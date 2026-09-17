// Package openai implements an OpenAI-compatible Chat Completions provider.
//
// It speaks the same HTTP/JSON as nanobot's reference OpenAICompatProvider
// (upstream/nanobot/nanobot/providers/openai_compat_provider.py:509, upstream @
// 1bb712d3488915ca4ed9ccc1a93067ff722f5ab9), which covers DeepSeek, Groq,
// Ollama, vLLM, OpenRouter and generic gateways.
//
// The reference drives the wire format through the official `openai` Python
// SDK, so some details live in the SDK rather than in nanobot's own code. Those
// are marked "SDK:" and were checked against the SDK source cached on this
// machine (openai 2.24.0, which satisfies nanobot's `openai>=2.8.0` pin).
package openai

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/adrianolimagarcia/nanobot-go/internal/core"
	"github.com/adrianolimagarcia/nanobot-go/internal/provider"
)

const (
	// DefaultBaseURL is used when Options.BaseURL is empty.
	//
	// The reference uses the provider spec's default_api_base verbatim and
	// never appends "/v1" (openai_compat_provider.py:541,606); registry
	// defaults already include it.
	DefaultBaseURL = "https://api.openai.com/v1"

	// DefaultModel mirrors OpenAICompatProvider(default_model="gpt-4o")
	// (openai_compat_provider.py:523).
	DefaultModel = "gpt-4o"

	// chatCompletionsPath is the relative path appended to the base URL,
	// matching the SDK's base_url/relative-path merge
	// (SDK: openai/_base_client.py:_prepare_url).
	chatCompletionsPath = "chat/completions"

	// requestTimeout mirrors _OPENAI_COMPAT_REQUEST_TIMEOUT_S
	// (openai_compat_provider.py:132).
	requestTimeout = 120 * time.Second

	// defaultStreamIdleTimeout and maxStreamIdleTimeout mirror
	// DEFAULT_STREAM_IDLE_TIMEOUT_S / MAX_STREAM_IDLE_TIMEOUT_S
	// (providers/base.py:29-30).
	defaultStreamIdleTimeout = 90 * time.Second
	maxStreamIdleTimeout     = 3600 * time.Second

	// streamIdleTimeoutEnv mirrors STREAM_IDLE_TIMEOUT_ENV (base.py:28).
	streamIdleTimeoutEnv = "NANOBOT_STREAM_IDLE_TIMEOUT_S"

	// noKeyPlaceholder mirrors `_api_key_for_client = api_key or "no-key"`
	// (openai_compat_provider.py:554): local servers such as Ollama accept
	// any bearer token, but the header must still be present.
	noKeyPlaceholder = "no-key"

	// maxErrorBodyBytes bounds how much of a non-2xx body is retained. The
	// reference stores the whole body; an API error body is small, so this
	// cap only guards against a hostile or misconfigured endpoint.
	maxErrorBodyBytes = 1 << 20
)

// Options configures a Client.
type Options struct {
	// APIKey is sent as "Authorization: Bearer <key>". Empty falls back to
	// the reference placeholder "no-key".
	APIKey string
	// BaseURL is the API root, e.g. "https://api.openai.com/v1" or
	// "http://localhost:11434/v1". Trailing slashes are tolerated. Empty
	// falls back to DefaultBaseURL.
	BaseURL string
	// Model is the default model used when a request does not name one.
	Model string
	// HTTPClient is used as-is when non-nil. The default client has no
	// overall timeout (that would abort long streams) but bounds dial, TLS
	// and response-header timeouts, plus a per-chunk stream idle timeout.
	HTTPClient *http.Client
	// ExtraHeaders are applied last, overriding defaults.
	ExtraHeaders map[string]string
	// ToolCallFormat selects the text parser for this provider. Native tool_calls
	// remain supported; Auto preserves legacy behavior.
	ToolCallFormat ToolCallFormat
	// ToolCallFormatsByModel overrides ToolCallFormat for exact model IDs.
	ToolCallFormatsByModel map[string]ToolCallFormat
}

// Client is an OpenAI-compatible chat provider.
//
// A Client is safe for concurrent use once constructed: it holds no mutable
// per-request state.
type Client struct {
	apiKey          string
	baseURL         string
	model           string
	httpClient      *http.Client
	extraHeaders         map[string]string
	toolCallFormat       ToolCallFormat
	toolCallFormatsModel map[string]ToolCallFormat
	sessionAffinity      string
}

var (
	_ provider.Provider          = (*Client)(nil)
	_ provider.StreamingProvider = (*Client)(nil)
)

// fallbackAPIKey mirrors `_api_key_for_client = api_key or "no-key"`
// (openai_compat_provider.py:554): the Authorization header is always present,
// which local servers such as Ollama require even though they ignore its value.
func fallbackAPIKey(key string) string {
	if key == "" {
		return noKeyPlaceholder
	}
	return key
}

// New builds a Client. It never fails: absent values fall back to the
// reference defaults, exactly as OpenAICompatProvider.__init__ does.
func New(opts Options) *Client {
	base := strings.TrimSpace(opts.BaseURL)
	if base == "" {
		base = DefaultBaseURL
	}
	// The SDK merges base_url with the relative request path without doubling
	// slashes (SDK: openai/_base_client.py:_prepare_url), so "https://x/v1"
	// and "https://x/v1/" both resolve to "https://x/v1/chat/completions".
	base = strings.TrimRight(base, "/")

	model := opts.Model
	if model == "" {
		model = DefaultModel
	}
	apiKey := fallbackAPIKey(opts.APIKey)
	httpClient := opts.HTTPClient
	if httpClient == nil {
		httpClient = defaultHTTPClient(base)
	}
	headers := make(map[string]string, len(opts.ExtraHeaders))
	for k, v := range opts.ExtraHeaders {
		headers[k] = v
	}
	modelFormats := make(map[string]ToolCallFormat, len(opts.ToolCallFormatsByModel))
	for k, v := range opts.ToolCallFormatsByModel {
		modelFormats[strings.ToLower(strings.TrimSpace(k))] = normalizeToolCallFormat(v)
	}
	return &Client{
		apiKey:              apiKey,
		baseURL:             base,
		model:               model,
		httpClient:          httpClient,
		extraHeaders:        headers,
		toolCallFormat:      normalizeToolCallFormat(opts.ToolCallFormat),
		toolCallFormatsModel: modelFormats,
		sessionAffinity:     randomHex(16),
	}
}

// Name identifies the provider. The reference's default provider_name is
// "openai" (openai_compat_provider.py:529).
func (c *Client) Name() string { return "openai" }

// Chat performs one non-streamed completion.
//
// A transport failure, a non-2xx status, or an unparseable body is returned as
// an error; a non-2xx status is always a *provider.HTTPError so the caller's
// retry policy can classify it.
func (c *Client) Chat(ctx context.Context, req provider.ChatRequest) (*core.Response, error) {
	body, err := c.buildBody(req, false)
	if err != nil {
		return nil, err
	}

	// The reference bounds every request with _OPENAI_COMPAT_REQUEST_TIMEOUT_S
	// = 120s (openai_compat_provider.py:132,210-212,606-611). For a
	// non-streaming call the body only arrives once the model has finished, so
	// that bound covers the whole exchange; an earlier caller deadline still
	// wins. The streaming path is bounded per chunk by the idle timer instead.
	ctx, cancel := context.WithTimeout(ctx, requestTimeout)
	defer cancel()

	resp, err := c.post(ctx, body)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	payload, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("openai: read response body: %w", err)
	}
	return parseResponseWithToolCallFormat(payload, c.toolCallFormatForModel(req.Model))
}

func (c *Client) toolCallFormatForModel(model string) ToolCallFormat {
	if model == "" {
		model = c.model
	}
	if format, ok := c.toolCallFormatsModel[strings.ToLower(strings.TrimSpace(model))]; ok {
		return format
	}
	return c.toolCallFormat
}

// endpoint returns the absolute chat-completions URL.
func (c *Client) endpoint() string { return c.baseURL + "/" + chatCompletionsPath }

// post sends body and returns the response for a 2xx status. Any other status
// is converted into a *provider.HTTPError with Retry-After parsed.
func (c *Client) post(ctx context.Context, body []byte) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.endpoint(), bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("openai: build request: %w", err)
	}
	c.setHeaders(req)

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		defer resp.Body.Close()
		payload, _ := io.ReadAll(io.LimitReader(resp.Body, maxErrorBodyBytes))
		return nil, &provider.HTTPError{
			StatusCode: resp.StatusCode,
			Status:     resp.Status,
			Body:       string(payload),
			RetryAfter: retryAfterFromHeaders(resp.Header),
		}
	}
	return resp, nil
}

// setHeaders applies the reference header set.
//
// Authorization comes from the SDK's auth_headers (SDK: openai/_client.py:615,
// `{"Authorization": f"Bearer {api_key}"}`) and is built by nanobot handing the
// key to AsyncOpenAI with `api_key or "no-key"` (openai_compat_provider.py:554,
// 604-612). "Accept: application/json" is the SDK default even for streamed
// requests (SDK: openai/_base_client.py:672) — no text/event-stream header is
// sent. "x-session-affinity" is nanobot's own default header
// (openai_compat_provider.py:543), and user extra_headers override last
// (openai_compat_provider.py:546-547).
func (c *Client) setHeaders(req *http.Request) {
	req.Header.Set("Authorization", "Bearer "+c.apiKey)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	if c.sessionAffinity != "" {
		req.Header.Set("x-session-affinity", c.sessionAffinity)
	}
	for k, v := range c.extraHeaders {
		req.Header.Set(k, v)
	}
}

// retryAfterFromHeaders mirrors LLMProvider._extract_retry_after_from_headers
// (providers/base.py:1642-1679): "retry-after-ms" wins, then a numeric
// "retry-after" in seconds, then an HTTP-date whose remaining time is used.
// A present value is never reported below 0.1s (_to_retry_seconds, base.py:1632).
func retryAfterFromHeaders(h http.Header) float64 {
	if h == nil {
		return 0
	}
	if raw := strings.TrimSpace(h.Get("retry-after-ms")); raw != "" {
		if v, err := strconv.ParseFloat(raw, 64); err == nil {
			if secs := v / 1000; secs > 0 {
				return secs
			}
		}
	}
	raw := strings.TrimSpace(h.Get("retry-after"))
	if raw == "" {
		return 0
	}
	if numericSecondsRe.MatchString(raw) {
		v, err := strconv.ParseFloat(raw, 64)
		if err != nil {
			return 0
		}
		return clampRetrySeconds(v)
	}
	when, err := http.ParseTime(raw)
	if err != nil {
		return 0
	}
	return clampRetrySeconds(time.Until(when).Seconds())
}

// numericSecondsRe matches the reference's numeric guard
// (`re.fullmatch(r"\d+(?:\.\d+)?", text)`, base.py:1670).
var numericSecondsRe = regexp.MustCompile(`^\d+(\.\d+)?$`)

func clampRetrySeconds(v float64) float64 {
	if v < 0.1 {
		return 0.1
	}
	return v
}

// resolveStreamIdleTimeout mirrors resolve_stream_idle_timeout_s
// (providers/base.py:39-60): NANOBOT_STREAM_IDLE_TIMEOUT_S, 90s default,
// invalid or non-positive values ignored, values above 3600s clamped.
func resolveStreamIdleTimeout() time.Duration {
	raw := strings.TrimSpace(os.Getenv(streamIdleTimeoutEnv))
	if raw == "" {
		return defaultStreamIdleTimeout
	}
	v, err := strconv.ParseFloat(raw, 64)
	if err != nil || v <= 0 {
		return defaultStreamIdleTimeout
	}
	if v > maxStreamIdleTimeout.Seconds() {
		return maxStreamIdleTimeout
	}
	return time.Duration(v * float64(time.Second))
}

// defaultHTTPClient builds the client used when Options.HTTPClient is nil.
func defaultHTTPClient(baseURL string) *http.Client {
	transport := &http.Transport{
		Proxy:                 http.ProxyFromEnvironment,
		DialContext:           (&net.Dialer{Timeout: 30 * time.Second, KeepAlive: 30 * time.Second}).DialContext,
		ForceAttemptHTTP2:     true,
		MaxIdleConns:          100,
		IdleConnTimeout:       90 * time.Second,
		TLSHandshakeTimeout:   10 * time.Second,
		ExpectContinueTimeout: 1 * time.Second,
		ResponseHeaderTimeout: requestTimeout,
	}
	if isLocalEndpoint(baseURL) {
		// Local model servers (Ollama, llama.cpp, vLLM) often close idle
		// connections before a client-side keepalive expires, and a host-level
		// HTTP_PROXY usually cannot reach localhost/LAN. The reference disables
		// keepalive and the proxy for local endpoints
		// (openai_compat_provider.py:580-600).
		transport.DisableKeepAlives = true
		transport.Proxy = nil
	}
	// No Client.Timeout: it would abort a long stream mid-flight. The reference
	// bounds a request with a 120s timeout kwarg
	// (openai_compat_provider.py:132,2125); the equivalents here are
	// ResponseHeaderTimeout and the per-chunk stream idle timer.
	return &http.Client{Transport: transport}
}

// isLocalEndpoint mirrors the spec-independent part of _is_local_endpoint
// (openai_compat_provider.py:372-400): localhost, host.docker.internal, or a
// loopback/private IP literal.
func isLocalEndpoint(raw string) bool {
	raw = strings.ToLower(strings.TrimSpace(raw))
	if raw == "" {
		return false
	}
	if !strings.Contains(raw, "://") {
		raw = "//" + raw
	}
	parsed, err := url.Parse(raw)
	if err != nil {
		return false
	}
	host := parsed.Hostname()
	if host == "localhost" || host == "host.docker.internal" {
		return true
	}
	if host == "" {
		return false
	}
	ip := net.ParseIP(host)
	if ip == nil {
		return false
	}
	return ip.IsLoopback() || ip.IsPrivate()
}

// randomHex returns n random bytes as lowercase hex (the SDK/httpx
// x-session-affinity value is uuid4().hex, openai_compat_provider.py:543).
// On the (practically unreachable) failure path the header is simply omitted.
func randomHex(n int) string {
	buf := make([]byte, n)
	if _, err := rand.Read(buf); err != nil {
		return ""
	}
	return hex.EncodeToString(buf)
}

// marshalNoHTMLEscape encodes v like json.Marshal but without Go's default
// HTML escaping, matching the reference's json.dumps(..., ensure_ascii=False)
// (base.py:163). Content and tool arguments are sent verbatim as UTF-8.
func marshalNoHTMLEscape(v any) ([]byte, error) {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(v); err != nil {
		return nil, err
	}
	return bytes.TrimRight(buf.Bytes(), "\n"), nil
}
