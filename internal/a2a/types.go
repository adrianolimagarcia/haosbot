package a2a

import (
	"encoding/json"
	"time"
)

// ProtocolVersion is the A2A specification version this server implements. It is
// reported per interface in AgentCard.supportedInterfaces (proto AgentInterface,
// protocol_version) and is the value matched against the client's A2A-Version
// service parameter.
const ProtocolVersion = "1.0"

// TaskState values (proto TaskState). The wire form is the protobuf enum name,
// not the v0.3 lowercase spelling: A2A 1.0 serialises enums with ProtoJSON, which
// emits the symbolic name. A client that receives "completed" where it expects
// TASK_STATE_COMPLETED rejects the whole Task object.
const (
	TaskStateUnspecified   = "TASK_STATE_UNSPECIFIED"
	TaskStateSubmitted     = "TASK_STATE_SUBMITTED"
	TaskStateWorking       = "TASK_STATE_WORKING"
	TaskStateCompleted     = "TASK_STATE_COMPLETED"
	TaskStateFailed        = "TASK_STATE_FAILED"
	TaskStateCanceled      = "TASK_STATE_CANCELED"
	TaskStateInputRequired = "TASK_STATE_INPUT_REQUIRED"
	TaskStateRejected      = "TASK_STATE_REJECTED"
	TaskStateAuthRequired  = "TASK_STATE_AUTH_REQUIRED"
)

// Role values (proto Role).
const (
	RoleUser  = "ROLE_USER"
	RoleAgent = "ROLE_AGENT"
)

// Media types the agent accepts and produces. These are advertised in the Agent
// Card as defaultInputModes/defaultOutputModes, which are REQUIRED fields.
const (
	MediaTypeText = "text/plain"
)

// A2A-specific JSON-RPC error codes (spec 5.4, "Error Code Mappings"). The
// standard JSON-RPC codes are listed alongside them because the two ranges share
// the same error object.
const (
	CodeJSONParseError      = -32700
	CodeInvalidRequestError = -32600
	CodeMethodNotFoundError = -32601
	CodeInvalidParamsError  = -32602
	CodeInternalError       = -32603

	CodeTaskNotFoundError                   = -32001
	CodeTaskNotCancelableError              = -32002
	CodePushNotificationNotSupportedError   = -32003
	CodeUnsupportedOperationError           = -32004
	CodeContentTypeNotSupportedError        = -32005
	CodeInvalidAgentResponseError           = -32006
	CodeExtendedAgentCardNotConfiguredError = -32007
	CodeExtensionSupportRequiredError       = -32008
	CodeVersionNotSupportedError            = -32009
)

// a2aErrorDomain is the ErrorInfo.domain value for this agent's errors, as shown
// in the spec's error example (9.5).
const a2aErrorDomain = "a2a-protocol.org"

// AgentCard is the A2A discovery document served at
// /.well-known/agent-card.json (proto AgentCard).
//
// The v1.0.0 field set is not the v0.3 one: the single top-level `url` was
// replaced by the ordered `supportedInterfaces` list, `protocolVersion` moved
// into each interface, and `defaultInputModes`/`defaultOutputModes` became
// REQUIRED. A card carrying the legacy `url` is rejected outright by the
// official JSON Schema (additionalProperties: false), and a card without
// `supportedInterfaces` makes the official TCK abort before it issues a single
// request — it cannot tell which transports to exercise.
type AgentCard struct {
	Name               string            `json:"name"`
	Description        string            `json:"description"`
	SupportedInterface []AgentInterface  `json:"supportedInterfaces"`
	Version            string            `json:"version"`
	Capabilities       AgentCapabilities `json:"capabilities"`
	DefaultInputModes  []string          `json:"defaultInputModes"`
	DefaultOutputModes []string          `json:"defaultOutputModes"`
	Skills             []AgentSkill      `json:"skills"`
	SecuritySchemes    map[string]any    `json:"securitySchemes,omitempty"`
	Security           []map[string]any  `json:"security,omitempty"`
	Provider           *AgentProvider    `json:"provider,omitempty"`
	Signatures         []json.RawMessage `json:"signatures,omitempty"`
	DocumentationURL   string            `json:"documentationUrl,omitempty"`
	IconURL            string            `json:"iconUrl,omitempty"`
}

// AgentInterface describes one transport at one URL (proto AgentInterface). The
// first entry is the preferred interface.
type AgentInterface struct {
	URL             string `json:"url"`
	ProtocolBinding string `json:"protocolBinding"`
	ProtocolVersion string `json:"protocolVersion"`
	Tenant          string `json:"tenant,omitempty"`
}

// AgentCapabilities advertises optional protocol features (proto
// AgentCapabilities). Every field is optional in the proto, but the presence of
// a false value is what makes a capability-specific error code mandatory: a
// client that sees pushNotifications:false and still calls a push method must be
// answered with PushNotificationNotSupportedError, not MethodNotFound.
type AgentCapabilities struct {
	Streaming         bool `json:"streaming"`
	PushNotifications bool `json:"pushNotifications"`
	ExtendedAgentCard bool `json:"extendedAgentCard"`
}

// AgentProvider identifies the organisation behind the agent (proto
// AgentProvider); both of its fields are REQUIRED when the object is present.
type AgentProvider struct {
	URL          string `json:"url"`
	Organization string `json:"organization"`
}

// AgentSkill describes one ability of the agent (proto AgentSkill). `tags` is
// REQUIRED: the official card schema and the TCK both reject a skill without it.
type AgentSkill struct {
	ID          string   `json:"id"`
	Name        string   `json:"name"`
	Description string   `json:"description"`
	Tags        []string `json:"tags"`
	Examples    []string `json:"examples,omitempty"`
	InputModes  []string `json:"inputModes,omitempty"`
	OutputModes []string `json:"outputModes,omitempty"`
}

// Part is one piece of message or artifact content (proto Part).
//
// A2A 1.0 removed the `kind` discriminator that v0.3 used: the member name
// itself now identifies the variant, so a text part is {"text":"..."} and a file
// part is {"raw":...,"filename":...,"mediaType":...}. MarshalJSON enforces that
// exactly one member is emitted, because a struct with four omitempty string
// fields would silently serialise an empty text part as {} — which is not a
// valid Part.
type Part struct {
	Text      string          `json:"-"`
	Raw       string          `json:"-"`
	URL       string          `json:"-"`
	Data      json.RawMessage `json:"-"`
	Metadata  map[string]any  `json:"metadata,omitempty"`
	Filename  string          `json:"filename,omitempty"`
	MediaType string          `json:"mediaType,omitempty"`
}

// NewTextPart builds the only part variant this agent produces.
func NewTextPart(text string) Part {
	return Part{Text: text, MediaType: MediaTypeText}
}

// MarshalJSON emits exactly one content member, chosen by which field is set.
// The order matches the proto oneof declaration; an all-empty Part falls back to
// an empty text part so the result is always a valid Part.
func (p Part) MarshalJSON() ([]byte, error) {
	type partAlias struct {
		Text      *string         `json:"text,omitempty"`
		Raw       *string         `json:"raw,omitempty"`
		URL       *string         `json:"url,omitempty"`
		Data      json.RawMessage `json:"data,omitempty"`
		Metadata  map[string]any  `json:"metadata,omitempty"`
		Filename  string          `json:"filename,omitempty"`
		MediaType string          `json:"mediaType,omitempty"`
	}

	out := partAlias{
		Metadata:  p.Metadata,
		Filename:  p.Filename,
		MediaType: p.MediaType,
	}
	switch {
	case p.Text != "" || (p.Raw == "" && p.URL == "" && len(p.Data) == 0):
		text := p.Text
		out.Text = &text
	case p.Raw != "":
		raw := p.Raw
		out.Raw = &raw
	case p.URL != "":
		url := p.URL
		out.URL = &url
	default:
		out.Data = p.Data
	}
	return json.Marshal(out)
}

// UnmarshalJSON accepts both the current flat form and the legacy v0.3 form
// ({"kind":"text","text":...}, {"kind":"file","file":{...}},
// {"kind":"data","data":...}), so a peer that has not migrated yet is still
// understood. A2A 1.0 explicitly allows a server to accept both during the
// overlap period; responses are always emitted in the current form.
func (p *Part) UnmarshalJSON(b []byte) error {
	var wire struct {
		Text      string          `json:"text"`
		Raw       string          `json:"raw"`
		URL       string          `json:"url"`
		Data      json.RawMessage `json:"data"`
		Metadata  map[string]any  `json:"metadata"`
		Filename  string          `json:"filename"`
		MediaType string          `json:"mediaType"`
		Kind      string          `json:"kind"`
		File      struct {
			Name          string `json:"name"`
			MimeType      string `json:"mimeType"`
			Bytes         string `json:"bytes"`
			FileWithBytes string `json:"fileWithBytes"`
			FileWithURI   string `json:"fileWithUri"`
			URI           string `json:"uri"`
		} `json:"file"`
	}
	if err := json.Unmarshal(b, &wire); err != nil {
		return err
	}

	*p = Part{
		Text:      wire.Text,
		Raw:       wire.Raw,
		URL:       wire.URL,
		Data:      wire.Data,
		Metadata:  wire.Metadata,
		Filename:  wire.Filename,
		MediaType: wire.MediaType,
	}

	// Legacy file part: {"kind":"file","file":{"name","mimeType","fileWithBytes"|"fileWithUri"}}
	if wire.Kind == "file" {
		if p.Filename == "" {
			p.Filename = wire.File.Name
		}
		if p.MediaType == "" {
			p.MediaType = wire.File.MimeType
		}
		if p.Raw == "" {
			switch {
			case wire.File.FileWithBytes != "":
				p.Raw = wire.File.FileWithBytes
			case wire.File.Bytes != "":
				p.Raw = wire.File.Bytes
			}
		}
		if p.URL == "" {
			switch {
			case wire.File.FileWithURI != "":
				p.URL = wire.File.FileWithURI
			case wire.File.URI != "":
				p.URL = wire.File.URI
			}
		}
	}
	return nil
}

// Message is one turn of content from a peer or from the agent (proto Message).
// messageId and role are REQUIRED, as is a non-empty parts list.
type Message struct {
	MessageID        string         `json:"messageId"`
	ContextID        string         `json:"contextId,omitempty"`
	TaskID           string         `json:"taskId,omitempty"`
	Role             string         `json:"role"`
	Parts            []Part         `json:"parts"`
	Metadata         map[string]any `json:"metadata,omitempty"`
	Extensions       []string       `json:"extensions,omitempty"`
	ReferenceTaskIDs []string       `json:"referenceTaskIds,omitempty"`
}

// Text returns the concatenation of every text part, which is the only content
// this agent can consume.
func (m Message) Text() string {
	var b []byte
	for _, p := range m.Parts {
		if p.Text == "" {
			continue
		}
		if len(b) > 0 {
			b = append(b, '\n')
		}
		b = append(b, p.Text...)
	}
	return string(b)
}

// HasUnsupportedContent reports whether the message carries a part this agent
// cannot process (file or structured data). Callers answer those with
// ContentTypeNotSupportedError rather than silently dropping the content.
func (m Message) HasUnsupportedContent() bool {
	for _, p := range m.Parts {
		if p.Raw != "" || p.URL != "" || len(p.Data) > 0 {
			return true
		}
	}
	return false
}

// TaskStatus is the current state of a Task (proto TaskStatus). `state` is
// REQUIRED; `timestamp` is an ISO 8601 instant and, per spec 5.6, MUST NOT carry
// a timezone offset other than 'Z'.
type TaskStatus struct {
	State     string   `json:"state"`
	Message   *Message `json:"message,omitempty"`
	Timestamp string   `json:"timestamp,omitempty"`
}

// Artifact is an output produced by a Task (proto Artifact). artifactId and a
// non-empty parts list are REQUIRED.
type Artifact struct {
	ArtifactID  string         `json:"artifactId"`
	Name        string         `json:"name,omitempty"`
	Description string         `json:"description,omitempty"`
	Parts       []Part         `json:"parts"`
	Metadata    map[string]any `json:"metadata,omitempty"`
	Extensions  []string       `json:"extensions,omitempty"`
}

// Task is the unit of work tracked by the protocol (proto Task).
//
// updated is bookkeeping for the retention policy and is deliberately not part
// of the wire form: the protocol's own timestamp lives in status.timestamp.
type Task struct {
	ID        string         `json:"id"`
	ContextID string         `json:"contextId,omitempty"`
	Status    TaskStatus     `json:"status"`
	Artifacts []Artifact     `json:"artifacts,omitempty"`
	History   []Message      `json:"history,omitempty"`
	Metadata  map[string]any `json:"metadata,omitempty"`

	updated time.Time
}

// terminal reports whether a task reached a final state (spec 4.1.3). CANCELED
// and REJECTED are terminal just like COMPLETED and FAILED; treating only the
// latter two as terminal kept canceled tasks in the store as if they were still
// running, which is what the retention sweep keys on.
func (t Task) terminal() bool {
	switch t.Status.State {
	case TaskStateCompleted, TaskStateFailed, TaskStateCanceled, TaskStateRejected:
		return true
	default:
		return false
	}
}

// rfc3339UTC formats an instant the way the spec requires for JSON timestamps:
// RFC 3339 in UTC, so the offset suffix is always 'Z'. time.RFC3339Nano on a
// non-UTC time.Time emits the numeric offset instead (e.g. "+05:30"), which the
// official TCK rejects.
func rfc3339UTC(t time.Time) string {
	return t.UTC().Format(time.RFC3339Nano)
}

// taskStatus builds a status value stamped with the current time.
func taskStatus(state string) TaskStatus {
	return TaskStatus{State: state, Timestamp: rfc3339UTC(time.Now())}
}

// ---------------------------------------------------------------------------
// JSON-RPC envelope
// ---------------------------------------------------------------------------

// JSONRPCRequest is an incoming JSON-RPC 2.0 request.
type JSONRPCRequest struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      any             `json:"id"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params"`
}

// JSONRPCResponse is an outgoing JSON-RPC 2.0 response.
type JSONRPCResponse struct {
	JSONRPC string        `json:"jsonrpc"`
	ID      any           `json:"id"`
	Result  any           `json:"result,omitempty"`
	Error   *JSONRPCError `json:"error,omitempty"`
}

// JSONRPCError is the JSON-RPC 2.0 error object. Data carries the A2A error
// detail array described in spec 9.5.
type JSONRPCError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
	Data    []any  `json:"data,omitempty"`
}

// errorInfo builds a google.rpc.ErrorInfo detail object, which the spec names as
// the well-known type to use for A2A errors. Every object in `data` MUST carry a
// @type key.
func errorInfo(reason string, metadata map[string]string) map[string]any {
	info := map[string]any{
		"@type":  "type.googleapis.com/google.rpc.ErrorInfo",
		"reason": reason,
		"domain": a2aErrorDomain,
	}
	if len(metadata) > 0 {
		info["metadata"] = metadata
	}
	return info
}

// SendMessageRequest is the payload of SendMessage (proto SendMessageRequest).
type SendMessageRequest struct {
	Tenant        string                    `json:"tenant,omitempty"`
	Message       Message                   `json:"message"`
	Configuration *SendMessageConfiguration `json:"configuration,omitempty"`
	Metadata      map[string]any            `json:"metadata,omitempty"`
}

// SendMessageConfiguration carries per-request options (proto
// SendMessageConfiguration). Only returnImmediately changes behaviour here:
// false (the default) means the call must block until the task reaches a
// terminal state, which is what this handler does.
type SendMessageConfiguration struct {
	AcceptedOutputModes        []string `json:"acceptedOutputModes,omitempty"`
	HistoryLength              *int     `json:"historyLength,omitempty"`
	ReturnImmediately          bool     `json:"returnImmediately,omitempty"`
	TaskPushNotificationConfig any      `json:"taskPushNotificationConfig,omitempty"`
}

// SendMessageResponse is the payload of a successful SendMessage (proto
// SendMessageResponse). It is a oneof: exactly one of Task or Message is set, so
// the JSON result is {"task":{...}} or {"message":{...}} — never a bare Task.
type SendMessageResponse struct {
	Task    *Task    `json:"task,omitempty"`
	Message *Message `json:"message,omitempty"`
}

// ListTasksResponse is the payload of ListTasks (proto ListTasksResponse). All
// four fields are REQUIRED.
type ListTasksResponse struct {
	Tasks         []*Task `json:"tasks"`
	NextPageToken string  `json:"nextPageToken"`
	PageSize      int     `json:"pageSize"`
	TotalSize     int     `json:"totalSize"`
}
