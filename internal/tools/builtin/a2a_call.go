package builtin

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/adrianolimagarcia/nanobot-go/internal/netpolicy"
	"github.com/adrianolimagarcia/nanobot-go/internal/tools"
)

// A2ACallTool allows the nanobot agent to discover and delegate tasks to another
// A2A agent.
type A2ACallTool struct {
	params json.RawMessage
	policy netpolicy.Policy
}

// a2aProtocolVersion is the A2A specification version this client speaks, sent
// as the A2A-Version service parameter on every request.
const a2aProtocolVersion = "1.0"

// NewA2ACall constructs the A2A tool. The optional allowlist is intentionally
// explicit: private, loopback and link-local destinations are denied by default.
// Existing callers that pass no argument retain the secure default.
func NewA2ACall(allowlist ...[]string) *A2ACallTool {
	raw := json.RawMessage(`{
		"type": "object",
		"properties": {
			"action": {
				"type": "string",
				"description": "Action to perform: 'discover' to read remote agent-card.json, or 'send' to delegate a task."
			},
			"agent_url": {
				"type": "string",
				"description": "The base URL or endpoint of the remote agent. Private/local destinations require an explicit SSRF allowlist entry."
			},
			"task_message": {
				"type": "string",
				"description": "The prompt, instruction or question to send to the remote agent (required when action is 'send')."
			}
		},
		"required": ["action", "agent_url"]
	}`)
	policy := netpolicy.Policy{}
	if len(allowlist) > 0 {
		policy.Allowlist = append([]string(nil), allowlist[0]...)
	}
	return &A2ACallTool{params: raw, policy: policy}
}

func (t *A2ACallTool) Name() string {
	return "a2a_call"
}

func (t *A2ACallTool) Description() string {
	return "Delegate a task to a remote AI agent using the official Agent2Agent (A2A) protocol. Can fetch agent card for discovery or send tasks via JSON-RPC."
}

func (t *A2ACallTool) Parameters() json.RawMessage {
	return t.params
}

// ---------------------------------------------------------------------------
// Minimal A2A wire model
//
// These types are duplicated rather than imported from internal/a2a because that
// package imports internal/agent, which imports internal/tools — importing it
// here would close an import cycle. Only the fields this client reads are
// declared.
// ---------------------------------------------------------------------------

type a2aPart struct {
	Text string `json:"text"`
}

type a2aMessage struct {
	MessageID string    `json:"messageId"`
	Role      string    `json:"role"`
	Parts     []a2aPart `json:"parts"`
}

type a2aArtifact struct {
	Name  string    `json:"name"`
	Parts []a2aPart `json:"parts"`
}

type a2aTaskStatus struct {
	State   string      `json:"state"`
	Message *a2aMessage `json:"message"`
}

type a2aTask struct {
	ID        string        `json:"id"`
	ContextID string        `json:"contextId"`
	Status    a2aTaskStatus `json:"status"`
	Artifacts []a2aArtifact `json:"artifacts"`
	History   []a2aMessage  `json:"history"`
}

// a2aSendResult is SendMessageResponse: a oneof of Task or Message.
type a2aSendResult struct {
	Task    *a2aTask    `json:"task"`
	Message *a2aMessage `json:"message"`

	// Legacy pre-1.0 fields, read only so a peer running an older build of this
	// same agent is still understood.
	ID     string `json:"id"`
	Output string `json:"output"`
}

type a2aRPCError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

type a2aRPCResponse struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      any             `json:"id"`
	Result  json.RawMessage `json:"result"`
	Error   *a2aRPCError    `json:"error"`
}

type a2aAgentInterface struct {
	URL             string `json:"url"`
	ProtocolBinding string `json:"protocolBinding"`
	ProtocolVersion string `json:"protocolVersion"`
}

type a2aAgentCard struct {
	Name               string              `json:"name"`
	Description        string              `json:"description"`
	Version            string              `json:"version"`
	SupportedInterface []a2aAgentInterface `json:"supportedInterfaces"`
	DefaultInputModes  []string            `json:"defaultInputModes"`
	DefaultOutputModes []string            `json:"defaultOutputModes"`
	Skills             []struct {
		ID          string   `json:"id"`
		Name        string   `json:"name"`
		Description string   `json:"description"`
		Tags        []string `json:"tags"`
	} `json:"skills"`

	// url is the v0.3 discovery field, kept so an older peer still resolves.
	URL string `json:"url"`
}

// jsonRPCEndpoint returns the interface URL the client should POST to.
func (c a2aAgentCard) jsonRPCEndpoint() string {
	for _, iface := range c.SupportedInterface {
		if strings.EqualFold(iface.ProtocolBinding, "JSONRPC") && iface.URL != "" {
			return iface.URL
		}
	}
	// A card that declares no JSONRPC interface may still be a v0.3 card, whose
	// only address is the top-level url.
	return c.URL
}

// ---------------------------------------------------------------------------

func (t *A2ACallTool) Execute(ctx context.Context, args json.RawMessage) (tools.Result, error) {
	var input struct {
		Action      string `json:"action"`
		AgentURL    string `json:"agent_url"`
		TaskMessage string `json:"task_message"`
	}

	if err := json.Unmarshal(args, &input); err != nil {
		return tools.Result{Content: fmt.Sprintf("invalid arguments: %v", err)}, nil
	}

	baseURL := strings.TrimRight(strings.TrimSpace(input.AgentURL), "/")
	if baseURL == "" {
		return tools.Result{Content: "agent_url is required"}, nil
	}

	client := netpolicy.NewClient(60*time.Second, t.policy)

	switch strings.ToLower(input.Action) {
	case "discover":
		card, _, err := t.fetchCard(ctx, client, baseURL)
		if err != nil {
			return tools.Result{Content: err.Error()}, nil
		}
		return tools.Result{Content: formatCard(card)}, nil

	case "send":
		if strings.TrimSpace(input.TaskMessage) == "" {
			return tools.Result{Content: "task_message is required for send action"}, nil
		}
		// Prefer the address the peer advertises. Falling back to "<base>/a2a"
		// when it advertises none is deliberate: posting to the bare base URL
		// would silently target the wrong path on a peer that does serve /a2a,
		// and "/" is not a JSON-RPC endpoint anywhere in the spec.
		endpoint := baseURL
		if card, err := t.discoverCard(ctx, client, baseURL); err == nil {
			if advertised := card.jsonRPCEndpoint(); advertised != "" {
				endpoint = advertised
			}
		}
		if endpoint == baseURL && !strings.HasSuffix(endpoint, "/a2a") {
			endpoint = strings.TrimRight(endpoint, "/") + "/a2a"
		}
		return t.sendMessage(ctx, client, endpoint, input.TaskMessage)

	default:
		return tools.Result{
			Content: fmt.Sprintf("unknown action: %s. Supported actions: 'discover', 'send'", input.Action),
		}, nil
	}
}

// cardDiscoveryURL resolves the well-known card location for a base URL. When the
// caller already passed a card URL it is used verbatim.
func cardDiscoveryURL(baseURL string) string {
	if strings.HasSuffix(baseURL, "/.well-known/agent-card.json") {
		return baseURL
	}
	// The card lives at the authority root, so a path on the base URL (such as
	// "/a2a") must be dropped rather than prefixed.
	if u, err := url.Parse(baseURL); err == nil && u.Scheme != "" && u.Host != "" {
		return fmt.Sprintf("%s://%s/.well-known/agent-card.json", u.Scheme, u.Host)
	}
	return baseURL + "/.well-known/agent-card.json"
}

// discoverCard fetches and parses the remote agent card. A failure is returned
// so the caller can fall back to a default endpoint instead of aborting.
func (t *A2ACallTool) discoverCard(ctx context.Context, client *http.Client, baseURL string) (a2aAgentCard, error) {
	card, _, err := t.fetchCard(ctx, client, baseURL)
	return card, err
}

func (t *A2ACallTool) fetchCard(ctx context.Context, client *http.Client, baseURL string) (a2aAgentCard, string, error) {
	discoveryURL := cardDiscoveryURL(baseURL)
	if _, err := netpolicy.ValidateURL(ctx, discoveryURL, t.policy); err != nil {
		return a2aAgentCard{}, "", fmt.Errorf("discovery blocked by outbound policy: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, discoveryURL, nil)
	if err != nil {
		return a2aAgentCard{}, "", err
	}
	req.Header.Set("A2A-Version", a2aProtocolVersion)

	resp, err := client.Do(req)
	if err != nil {
		return a2aAgentCard{}, "", fmt.Errorf("discovery failed: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	body, err := readLimitedResponse(resp.Body, netpolicy.MaxResponseBytes(t.policy))
	if err != nil {
		return a2aAgentCard{}, "", fmt.Errorf("discovery response rejected: %w", err)
	}

	var card a2aAgentCard
	if err := json.Unmarshal(body, &card); err != nil {
		return a2aAgentCard{}, "", fmt.Errorf("agent card is not valid JSON: %w", err)
	}
	return card, string(body), nil
}

func formatCard(card a2aAgentCard) string {
	var b strings.Builder
	b.WriteString("Remote Agent Discovery:\n")
	fmt.Fprintf(&b, "  name: %s\n", card.Name)
	if card.Description != "" {
		fmt.Fprintf(&b, "  description: %s\n", card.Description)
	}
	if card.Version != "" {
		fmt.Fprintf(&b, "  version: %s\n", card.Version)
	}
	if len(card.SupportedInterface) > 0 {
		b.WriteString("  interfaces:\n")
		for _, iface := range card.SupportedInterface {
			fmt.Fprintf(&b, "    - %s (%s, protocol %s)\n", iface.URL, iface.ProtocolBinding, iface.ProtocolVersion)
		}
	} else if card.URL != "" {
		// v0.3 card: report it as the interface it effectively declares.
		fmt.Fprintf(&b, "  interface: %s (legacy v0.3 card)\n", card.URL)
	}
	if len(card.Skills) > 0 {
		b.WriteString("  skills:\n")
		for _, skill := range card.Skills {
			fmt.Fprintf(&b, "    - %s: %s\n", skill.ID, skill.Name)
		}
	}
	return b.String()
}

// sendMessage performs one A2A SendMessage call.
//
// The request is the v1.0 shape: method SendMessage, params.message carrying
// messageId, role and a parts array, with the A2A-Version service parameter in a
// header. The pre-1.0 client sent `tasks/send` with `params.message.text`, a
// method name that exists in neither the v1.0 nor the v0.3 dispatch table, so no
// conformant agent would accept it.
func (t *A2ACallTool) sendMessage(ctx context.Context, client *http.Client, endpoint, text string) (tools.Result, error) {
	messageID := func() string { return fmt.Sprintf("haosbot-%d", time.Now().UnixNano()) }

	rpc, err := t.call(ctx, client, endpoint, "SendMessage", map[string]any{
		"message": map[string]any{
			"messageId": messageID(),
			"role":      "ROLE_USER",
			"parts":     []map[string]any{{"text": text}},
		},
	})
	if err != nil {
		return tools.Result{Content: err.Error()}, nil
	}

	// A peer that predates A2A 1.0 answers MethodNotFound for SendMessage. Retrying
	// once with the v0.3 method name and its `kind` discriminator keeps the tool
	// usable against an un-migrated agent, which the spec's overlap period allows.
	if rpc.Error != nil && rpc.Error.Code == -32601 {
		rpc, err = t.call(ctx, client, endpoint, "message/send", map[string]any{
			"message": map[string]any{
				"messageId": messageID(),
				"role":      "user",
				"parts":     []map[string]any{{"kind": "text", "text": text}},
			},
		})
		if err != nil {
			return tools.Result{Content: err.Error()}, nil
		}
	}

	if rpc.Error != nil {
		return tools.Result{Content: fmt.Sprintf("A2A error %d: %s", rpc.Error.Code, rpc.Error.Message)}, nil
	}

	reply, ok := extractReply(rpc.Result)
	if !ok {
		return tools.Result{Content: fmt.Sprintf("A2A response carried no task or message: %s", string(rpc.Result))}, nil
	}
	return tools.Result{Content: fmt.Sprintf("A2A Response from %s:\n%s", endpoint, reply)}, nil
}

// call posts one JSON-RPC request and decodes the reply into a fresh response
// value. Decoding into a fresh value matters: encoding/json leaves fields absent
// from a payload untouched, so reusing one struct across the v0.3 retry would
// carry the first attempt's error into the second attempt's result.
func (t *A2ACallTool) call(ctx context.Context, client *http.Client, endpoint, method string, params any) (a2aRPCResponse, error) {
	body, err := t.rpc(ctx, client, endpoint, method, params)
	if err != nil {
		return a2aRPCResponse{}, err
	}
	var rpc a2aRPCResponse
	if err := json.Unmarshal(body, &rpc); err != nil {
		return a2aRPCResponse{}, fmt.Errorf("A2A response is not valid JSON: %w", err)
	}
	return rpc, nil
}

// rpc posts one JSON-RPC request and returns the raw response body.
func (t *A2ACallTool) rpc(ctx context.Context, client *http.Client, endpoint, method string, params any) ([]byte, error) {
	if _, err := netpolicy.ValidateURL(ctx, endpoint, t.policy); err != nil {
		return nil, fmt.Errorf("task delegation blocked by outbound policy: %w", err)
	}

	payload, err := json.Marshal(map[string]any{
		"jsonrpc": "2.0",
		"id":      time.Now().UnixNano(),
		"method":  method,
		"params":  params,
	})
	if err != nil {
		return nil, err
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(payload))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("A2A-Version", a2aProtocolVersion)

	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("task delegation failed: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	body, err := readLimitedResponse(resp.Body, netpolicy.MaxResponseBytes(t.policy))
	if err != nil {
		return nil, fmt.Errorf("A2A response rejected: %w", err)
	}
	return body, nil
}

// extractReply turns a SendMessageResponse into the text a model can read.
//
// A2A 1.0 puts a task's output in artifacts and, for an interrupted or failed
// task, in status.message; a plain reply is a Message. The pre-1.0 flat
// {id,output} object is still recognised so a peer running an older build of
// this agent keeps working.
func extractReply(raw json.RawMessage) (string, bool) {
	var result a2aSendResult
	if err := json.Unmarshal(raw, &result); err != nil {
		return "", false
	}

	switch {
	case result.Task != nil:
		return formatTask(*result.Task), true
	case result.Message != nil:
		return joinParts(result.Message.Parts), true
	case result.Output != "":
		return result.Output, true
	}
	return "", false
}

func formatTask(task a2aTask) string {
	var b strings.Builder
	fmt.Fprintf(&b, "task %s [%s]", task.ID, task.Status.State)

	var texts []string
	for _, artifact := range task.Artifacts {
		if text := joinParts(artifact.Parts); text != "" {
			texts = append(texts, text)
		}
	}
	if len(texts) == 0 && task.Status.Message != nil {
		if text := joinParts(task.Status.Message.Parts); text != "" {
			texts = append(texts, text)
		}
	}
	if len(texts) == 0 && len(task.History) > 0 {
		last := task.History[len(task.History)-1]
		if text := joinParts(last.Parts); text != "" {
			texts = append(texts, text)
		}
	}
	if len(texts) > 0 {
		b.WriteString("\n")
		b.WriteString(strings.Join(texts, "\n"))
	}
	return b.String()
}

func joinParts(parts []a2aPart) string {
	var texts []string
	for _, part := range parts {
		if strings.TrimSpace(part.Text) != "" {
			texts = append(texts, part.Text)
		}
	}
	return strings.Join(texts, "\n")
}

func readLimitedResponse(r io.Reader, limit int64) ([]byte, error) {
	body, err := io.ReadAll(io.LimitReader(r, limit+1))
	if err != nil {
		return nil, err
	}
	if int64(len(body)) > limit {
		return nil, fmt.Errorf("response exceeds %d bytes", limit)
	}
	return body, nil
}
