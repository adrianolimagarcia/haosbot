package a2a

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/adrianolimagarcia/nanobot-go/internal/agent"
	"github.com/adrianolimagarcia/nanobot-go/internal/config"
	"github.com/adrianolimagarcia/nanobot-go/internal/core"
)

// Task retention policy.
//
// A2A tasks are transient: a peer submits one, reads the reply, and may poll
// GetTask for a short while afterwards. Nothing ever removed an entry, so the
// store grew without bound — one entry per task, each holding the full input and
// output text, kept for the life of the process. The policy below bounds it:
//
//   - a terminal task (completed/failed/canceled/rejected) is retained for
//     taskRetentionTTL and then swept;
//   - the store is HARD bounded at maxRetainedTasks, so a burst that arrives
//     faster than the TTL can expire is bounded oldest-first even before any TTL
//     elapses. Admission is decided before the insert (see publish), so the
//     bound holds at every instant, including while N publishers are publishing
//     concurrently. The trim is amortised — one walk per batch of admissions,
//     not one per task — which is what keeps the hard bound cheap;
//   - sweeping is opportunistic (amortised on publish) rather than ticker
//     driven, so the handler owns no goroutine and needs no Close;
//   - the TTL pass runs at most once per taskSweepInterval, so a terminal task
//     lives at least taskRetentionTTL and at most taskRetentionTTL +
//     taskSweepInterval (the hard cap can evict it earlier).
//
// The values are deliberately not configurable: the reference has no A2A
// endpoint and the config schema has no key for them, so inventing one would
// create a second source of truth for a bound whose only job is to stop
// unbounded growth.
const (
	taskRetentionTTL  = 10 * time.Minute
	taskSweepInterval = time.Minute
	maxRetainedTasks  = 1024
	// taskTrimTarget is the low-water mark a cap trim aims for, so a burst costs
	// one store walk per batch instead of one walk per task.
	taskTrimTarget = maxRetainedTasks * 7 / 8
)

// ListTasks pagination bounds (proto ListTasksRequest): the default page is 50
// tasks and the service may not return more than 100.
const (
	defaultListPageSize = 50
	maxListPageSize     = 100
)

// Handler coordinates A2A endpoints and task execution
type Handler struct {
	cfg   *config.Config
	loop  *agent.Loop
	tasks sync.Map

	// cancels maps a task ID to the CancelFunc of the turn currently executing
	// it, which is what CancelTask needs: without it the only way to stop a task
	// was to close the client connection.
	cancels sync.Map

	// hubs maps a task ID to the fan-out point for its streaming subscribers.
	// Every published snapshot goes through it, so a stream sees exactly the
	// states the store holds, in the order they were published.
	hubs sync.Map

	// trimMu serialises the eviction walks, and nothing else. It is deliberately
	// NOT the admission path: publishers admit themselves with a CAS on retained
	// (see publish), so publishing stays parallel and only a publisher that finds
	// the store at its cap waits for a walk. The walk keeps its amortisation —
	// one walk per batch of admissions down to the low-water mark, instrumented
	// at 100 walks per 12800 sequential publishes, i.e. one walk per 128 — and a
	// sequential publish at the cap costs the same as before this change (min of
	// 5 runs: 908 ns/op here against 901 ns/op at the previous revision). A
	// concurrent comparison was NOT reliable on this shared build host (the same
	// benchmark varied 3.5-7.0 us/op between runs), so no claim is made about
	// concurrent throughput either way.
	trimMu sync.Mutex

	// startedAt is when this handler was constructed. The agent card is a pure
	// function of the configuration, which is loaded once at startup and never
	// changes while the process runs, so the handler's own construction time is
	// the honest Last-Modified for the card.
	startedAt time.Time

	// retained counts the live entries in tasks, and lastSweepNanos records the
	// last TTL pass. Both exist because sync.Map has no length and the cap has
	// to be enforced without walking the map on every request.
	//
	// retained is an UPPER BOUND on the entries in the map at every instant: a
	// publisher takes its slot with a CAS before it inserts (and releases it
	// again if the insert turned out to replace an existing key), and every
	// removal decrements only after the entry is gone. That ordering is what
	// makes the cap hard — a reader can never see a count below the number of
	// entries the store holds, and the store can never hold more than the cap.
	//
	// It is transiently ABOVE the map's size while a publisher sits between its
	// CAS and its insert, so it is a safe upper bound but not an exact length
	// under concurrent publishes; it is exact when nothing is publishing.
	retained       atomic.Int64
	lastSweepNanos atomic.Int64

	// taskSeq is the source of task and context identifiers.
	taskSeq atomic.Int64
}

// defaultAPIPort is the loader's own default for api.port
// (internal/config/schema.go defaultConfigSkeleton, 8900), resolved once and
// lazily. Taking it from the same source the loader uses — instead of a private
// literal — is what stops the fallback from drifting from the port the gateway
// actually binds and advertising an unreachable URL.
var defaultAPIPort = sync.OnceValue(func() int { return config.DefaultConfig().API.Port })

func NewHandler(cfg *config.Config, loop *agent.Loop) *Handler {
	return &Handler{
		cfg:       cfg,
		loop:      loop,
		startedAt: time.Now(),
	}
}

// publish stores an immutable snapshot of task and keeps the store bounded.
//
// Snapshots are copies: once published, a *Task is never mutated again. That is
// what lets handleGetTask serialise a task without a lock and without ever
// observing a half-written one (a completed status carrying the previous empty
// output, or a working status with a stale updatedAt).
//
// The cap is enforced BEFORE the insert, by taking a slot with a CAS, so the
// store can never hold more than maxRetainedTasks entries — not even while many
// publishers are publishing at once. The previous revision inserted first and
// trimmed afterwards, so N concurrent publishers held the store at up to cap+N
// entries until the next trim walked it back. Measured at that revision with 32
// publishers x 200 tasks: the retained count reached 1025..1029 against a cap of
// 1024 (TestTaskStoreNeverExceedsCapDuringBurst).
//
// The slot is taken before the insert rather than after it so that retained is
// an upper bound on the store at every instant, which is what lets a publisher
// below the cap proceed without any lock at all. Two publishers can race for the
// same task ID; the one whose Swap reports an existing entry releases the slot it
// took, so the count stays exact.
func (h *Handler) publish(task Task) {
	snapshot := task
	snapshot.updated = time.Now()

	// Re-publishing a retained task (the completion of a working task) replaces
	// an entry and must not be charged for a new slot. This is only an
	// optimisation: if the key disappears between this Load and the Store below,
	// that Store re-creates it where a delete just removed it, so the size of the
	// map does not grow on this path either way.
	if _, loaded := h.tasks.Load(task.ID); loaded {
		h.tasks.Store(task.ID, &snapshot)
		h.broadcast(snapshot)
		h.sweepTasks(time.Now())
		return
	}

	for {
		n := h.retained.Load()
		if n < maxRetainedTasks {
			if h.retained.CompareAndSwap(n, n+1) {
				break
			}
			continue
		}
		// At the cap: make room, then take the slot. trim is serialised, so a
		// burst above the cap costs one walk per batch rather than one walk per
		// publisher. This cannot spin forever: the only thing that can hold
		// retained above the number of entries in the map is another publisher
		// sitting between its CAS and its insert, which lasts a few
		// instructions, and the walk below removes every entry it can see.
		h.trim()
	}

	if _, loaded := h.tasks.Swap(task.ID, &snapshot); loaded {
		// The entry already existed: no new slot was needed after all.
		h.retained.Add(-1)
	}

	h.broadcast(snapshot)
	h.sweepTasks(time.Now())
}

// sweepTasks runs one retention pass. It takes no lock: every removal goes
// through deleteTask, whose LoadAndDelete guard keeps the count exact even when
// the TTL pass and a trim pick the same victim.
func (h *Handler) sweepTasks(now time.Time) {
	// The TTL pass walks the whole store, so it runs at most once per interval.
	// The hard cap is enforced by admission in publish, not here.
	last := h.lastSweepNanos.Load()
	if last == 0 || now.UnixNano()-last >= int64(taskSweepInterval) {
		if h.lastSweepNanos.CompareAndSwap(last, now.UnixNano()) {
			h.evictExpiredTasks(now)
		}
	}
	h.enforceTaskCap()
}

// evictExpiredTasks drops terminal tasks past their retention window.
func (h *Handler) evictExpiredTasks(now time.Time) {
	h.tasks.Range(func(key, value any) bool {
		task, ok := value.(*Task)
		if !ok {
			return true
		}
		if task.terminal() && now.Sub(task.updated) > taskRetentionTTL {
			h.deleteTask(key)
		}
		return true
	})
}

// enforceTaskCap is the backstop for the hard cap: publish keeps the store at or
// below maxRetainedTasks by admission, so this only fires if something grew the
// store without going through publish (a test that seeds h.tasks directly, or a
// future writer).
func (h *Handler) enforceTaskCap() {
	if h.retained.Load() <= maxRetainedTasks {
		return
	}
	h.trim()
}

// trim makes room in the store. It returns immediately when the count is
// already below the cap — another publisher has just trimmed — and otherwise
// evicts down to the low-water mark, so the next batch of admissions needs no
// walk. Re-checking against the CAP (not against the low-water mark) is what
// makes a burst cost one walk per batch: a queued publisher that found the store
// already back under the cap must not start a walk of its own.
func (h *Handler) trim() {
	h.trimMu.Lock()
	defer h.trimMu.Unlock()

	if h.retained.Load() < maxRetainedTasks {
		return
	}

	type candidate struct {
		key      any
		updated  time.Time
		terminal bool
	}
	candidates := make([]candidate, 0, maxRetainedTasks+1)
	h.tasks.Range(func(key, value any) bool {
		task, ok := value.(*Task)
		if !ok {
			return true
		}
		candidates = append(candidates, candidate{key: key, updated: task.updated, terminal: task.terminal()})
		return true
	})

	// Terminal tasks go first, least recently updated first. In-flight tasks are
	// dropped only when every retained task is still running. A peer that loses
	// its entry simply gets "Task not found" — the response of the send that owns
	// the task is unaffected.
	slices.SortFunc(candidates, func(a, b candidate) int {
		if a.terminal != b.terminal {
			if a.terminal {
				return -1
			}
			return 1
		}
		return a.updated.Compare(b.updated)
	})

	for _, victim := range candidates {
		if h.retained.Load() <= int64(taskTrimTarget) {
			return
		}
		h.deleteTask(victim.key)
	}
}

// deleteTask removes an entry, keeping the retained count exact when two walkers
// (a trim and the TTL pass) race for the same key.
func (h *Handler) deleteTask(key any) {
	if _, loaded := h.tasks.LoadAndDelete(key); loaded {
		h.retained.Add(-1)
	}
	// The hub goes with the task. A stream already open keeps its own channel and
	// simply stops receiving, which is what a subscriber to an evicted task should
	// observe; a later subscriber gets TaskNotFound.
	if id, ok := key.(string); ok {
		h.hubs.Delete(id)
	}
}

// loadTask reads the published snapshot for an ID.
func (h *Handler) loadTask(id string) (*Task, bool) {
	val, ok := h.tasks.Load(id)
	if !ok {
		return nil, false
	}
	task, ok := val.(*Task)
	return task, ok
}

// RegisterRoutes registers the A2A protocol routes on the provided mux.
//
// The JSON-RPC endpoint answers on both /a2a and /a2a/. The trailing-slash form
// is not something the spec mandates, but it is what real clients produce: the
// official TCK builds its HTTP client with the advertised interface URL as
// base_url, and httpx normalises that to "<url>/", so every request lands on
// /a2a/. Rejecting it makes this agent unreachable for the official conformance
// suite. Anything deeper than /a2a/ is refused, so the extra registration does
// not turn into a wildcard route.
func (h *Handler) RegisterRoutes(mux *http.ServeMux) {
	// A. Discovery Endpoint
	mux.HandleFunc("/.well-known/agent-card.json", h.handleAgentCard)

	// B. JSON-RPC 2.0 Task Endpoint
	mux.HandleFunc("/a2a", h.handleJSONRPC)
	mux.HandleFunc("/a2a/", h.handleJSONRPC)
}

// agentCardURL builds the address the card tells peers to use for this agent's
// A2A endpoint.
//
// api.publicBaseUrl wins when it is set. Behind a reverse proxy or NAT the
// locally bound host:port is not the address a peer can reach, so advertising
// it makes discovery wrong in exactly the deployment where it matters most.
// The configured value is a BASE, so a trailing slash is dropped before "/a2a"
// is appended and a path component is preserved:
//
//	https://example.com       -> https://example.com/a2a
//	https://example.com/      -> https://example.com/a2a
//	https://example.com/base  -> https://example.com/base/a2a
//
// With no public base URL configured the card advertises the effective bind
// address, byte for byte as it did before the key existed. That is why the
// public URL is a separate field rather than something written into
// cfg.API.Host/cfg.API.Port by the gateway runtime: those two carry the address
// the process actually listens on (cmd/haosbot/runtime.go writes the resolved
// --host/--port back into them), and overwriting them with the public URL would
// make the listener disagree with the config.
func (h *Handler) agentCardURL() string {
	if base := strings.TrimRight(h.cfg.API.PublicBaseURL, "/"); base != "" {
		return base + "/a2a"
	}

	host := h.cfg.API.Host
	if host == "" || host == "0.0.0.0" {
		host = "127.0.0.1"
	}
	port := h.cfg.API.Port
	if port <= 0 {
		port = defaultAPIPort()
	}
	return fmt.Sprintf("http://%s:%d/a2a", host, port)
}

// securitySchemeName is the key under which the bearer scheme is declared. It is
// referenced from AgentCard.security, which is what tells a client that a
// credential is required and how to present it.
const securitySchemeName = "bearerAuth"

// agentCard builds the discovery document.
//
// A2A 1.0 dropped the single top-level `url` in favour of the ordered
// `supportedInterfaces` list, made `defaultInputModes`/`defaultOutputModes`
// REQUIRED, and made `tags` REQUIRED on every skill. The card also has to
// declare the security scheme: the gateway protects /a2a with a bearer token
// whenever api.apiKey is set, and a card that omits securitySchemes tells a
// client the opposite, which is how an authenticated deployment becomes
// unreachable for a conformant client.
func (h *Handler) agentCard() AgentCard {
	card := AgentCard{
		Name:        "haosbot",
		Description: "Autonomous lightweight infrastructure, shell execution and Python/SQLite agent powered by haosbot",
		SupportedInterface: []AgentInterface{{
			URL:             h.agentCardURL(),
			ProtocolBinding: "JSONRPC",
			ProtocolVersion: ProtocolVersion,
		}},
		Version: "1.0.0",
		Capabilities: AgentCapabilities{
			// Declared explicitly rather than omitted so that a client asking for
			// a capability this agent does not have gets the specific error the
			// spec requires instead of MethodNotFound.
			Streaming:         true,
			PushNotifications: false,
			ExtendedAgentCard: false,
		},
		DefaultInputModes:  []string{MediaTypeText},
		DefaultOutputModes: []string{MediaTypeText},
		Skills: []AgentSkill{
			{
				ID:          "exec",
				Name:        "Terminal Command Execution",
				Description: "Execute shell commands, monitor system metrics, systemd and disk usage",
				Tags:        []string{"shell", "system", "diagnostics"},
			},
			{
				ID:          "python_exec",
				Name:        "Python SQLite Runner",
				Description: "Execute isolated Python scripts with SQLite, JSON and networking support",
				Tags:        []string{"python", "sqlite", "data"},
			},
			{
				ID:          "file_ops",
				Name:        "Workspace File Operations",
				Description: "Read, write and edit files safely inside the agent workspace",
				Tags:        []string{"files", "workspace", "editing"},
			},
		},
	}

	if h.cfg != nil && h.cfg.API.APIKey != "" {
		card.SecuritySchemes = map[string]any{
			securitySchemeName: map[string]any{
				"httpAuthSecurityScheme": map[string]any{
					"scheme":      "Bearer",
					"description": "Static bearer token from api.apiKey; send it as 'Authorization: Bearer <token>'.",
				},
			},
		}
		card.Security = []map[string]any{
			{"schemes": map[string]any{securitySchemeName: map[string]any{"list": []string{}}}},
		}
	}

	return card
}

func (h *Handler) handleAgentCard(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		w.Header().Set("Allow", "GET, HEAD")
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	body, err := json.Marshal(h.agentCard())
	if err != nil {
		http.Error(w, "Internal error", http.StatusInternalServerError)
		return
	}

	// The card is stable for the life of the process, so it is cacheable. The
	// spec lists these as SHOULD/MAY; without them every client re-fetches the
	// card before every call.
	etag := cardETag(body)
	lastModified := h.cardLastModified()

	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "public, max-age=300")
	w.Header().Set("ETag", etag)
	w.Header().Set("Last-Modified", lastModified.UTC().Format(http.TimeFormat))

	// Both validators are honoured, so a client that revalidates gets a bodyless
	// 304 instead of the card. If-None-Match is checked first because it is the
	// stronger validator: Last-Modified has one-second resolution and cannot tell
	// two cards apart within the same second.
	if matchesETag(r.Header.Get("If-None-Match"), etag) {
		w.WriteHeader(http.StatusNotModified)
		return
	}
	if ims := r.Header.Get("If-Modified-Since"); ims != "" {
		if since, err := http.ParseTime(ims); err == nil && !lastModified.Truncate(time.Second).After(since) {
			w.WriteHeader(http.StatusNotModified)
			return
		}
	}

	w.WriteHeader(http.StatusOK)
	if r.Method == http.MethodHead {
		return
	}
	_, _ = w.Write(body)
}

// cardLastModified reports when the advertised card last changed. A zero
// startedAt (a Handler built as a struct literal rather than by NewHandler) is
// reported as now, which keeps the header present without ever claiming the card
// is older than it is.
func (h *Handler) cardLastModified() time.Time {
	if h.startedAt.IsZero() {
		return time.Now()
	}
	return h.startedAt
}

// matchesETag reports whether an If-None-Match header covers etag. It handles
// the wildcard and the comma-separated list forms, and tolerates the weak
// prefix, since a client may send back any of them.
func matchesETag(header, etag string) bool {
	header = strings.TrimSpace(header)
	if header == "" {
		return false
	}
	if header == "*" {
		return true
	}
	for _, candidate := range strings.Split(header, ",") {
		candidate = strings.TrimSpace(candidate)
		candidate = strings.TrimPrefix(candidate, "W/")
		if candidate == etag {
			return true
		}
	}
	return false
}

// cardETag is a strong validator over the exact card bytes.
func cardETag(body []byte) string {
	return `"` + strconv.FormatUint(uint64(len(body)), 16) + "-" + strconv.FormatUint(fnv1a(body), 16) + `"`
}

// fnv1a is the 64-bit FNV-1a hash, inlined so the card endpoint does not pull
// hash/fnv into the request path for one call.
func fnv1a(b []byte) uint64 {
	const (
		offset = 14695981039346656037
		prime  = 1099511628211
	)
	h := uint64(offset)
	for _, c := range b {
		h ^= uint64(c)
		h *= prime
	}
	return h
}

// ---------------------------------------------------------------------------
// JSON-RPC dispatch
// ---------------------------------------------------------------------------

// supportedVersions are the A2A-Version values (Major.Minor) this interface
// accepts. 1.0 is what the card advertises; 0.3 is accepted because the legacy
// method aliases below let a pre-1.0 client keep working during the overlap
// period the spec describes.
var supportedVersions = map[string]bool{"1.0": true, "0.3": true}

// methodAliases maps the legacy dotted method names (v0.3 and the pre-0.3 names
// this handler used to expose) onto their v1.0 PascalCase equivalents. A2A 1.0
// names every method after its gRPC service method, so `tasks/get` is not a
// valid v1.0 method at all.
var methodAliases = map[string]string{
	"message/send": "SendMessage",
	"tasks/send":   "SendMessage",
	"tasks/create": "SendMessage",

	"tasks/get": "GetTask",

	"message/stream":    "SendStreamingMessage",
	"tasks/resubscribe": "SubscribeToTask",

	"tasks/cancel": "CancelTask",

	"tasks/list": "ListTasks",

	"tasks/pushNotificationConfig/set":    "CreateTaskPushNotificationConfig",
	"tasks/pushNotificationConfig/get":    "GetTaskPushNotificationConfig",
	"tasks/pushNotificationConfig/list":   "ListTaskPushNotificationConfigs",
	"tasks/pushNotificationConfig/delete": "DeleteTaskPushNotificationConfig",

	"agent/getAuthenticatedExtendedCard": "GetExtendedAgentCard",
}

func (h *Handler) handleJSONRPC(w http.ResponseWriter, r *http.Request) {
	// /a2a/ is registered as a subtree so the trailing-slash form works; only the
	// endpoint itself is a valid target.
	if path := r.URL.Path; path != "/a2a" && path != "/a2a/" {
		http.NotFound(w, r)
		return
	}

	if r.Method != http.MethodPost {
		w.Header().Set("Allow", "POST")
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	w.Header().Set("Content-Type", "application/json")

	var req JSONRPCRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		// The id is unknown when the body is unparseable, so the response carries
		// a null id, which is what JSON-RPC 2.0 prescribes for a parse error.
		h.writeError(w, nil, CodeJSONParseError, "Invalid JSON payload", nil)
		return
	}

	if req.JSONRPC != "" && req.JSONRPC != "2.0" {
		h.writeError(w, req.ID, CodeInvalidRequestError, "Request payload validation error",
			[]any{errorInfo("INVALID_REQUEST", map[string]string{"jsonrpc": req.JSONRPC})})
		return
	}

	// A2A-Version is a service parameter carried in a header (spec 3.6, 14.2.1).
	// A version this interface does not serve must be refused, not processed with
	// the wrong semantics.
	if version := strings.TrimSpace(r.Header.Get("A2A-Version")); version != "" && !supportedVersions[version] {
		h.writeError(w, req.ID, CodeVersionNotSupportedError, "Version not supported",
			[]any{errorInfo("VERSION_NOT_SUPPORTED", map[string]string{
				"requestedVersion": version,
				"supportedVersion": ProtocolVersion,
			})})
		return
	}

	method := req.Method
	if canonical, ok := methodAliases[method]; ok {
		method = canonical
	}

	switch method {
	case "SendMessage":
		h.handleSendMessage(r.Context(), w, req)
	case "GetTask":
		h.handleGetTask(w, req)
	case "ListTasks":
		h.handleListTasks(w, req)
	case "CancelTask":
		h.handleCancelTask(w, req)
	case "SendStreamingMessage":
		h.handleSendStreamingMessage(r.Context(), w, req)
	case "SubscribeToTask":
		h.handleSubscribeToTask(r.Context(), w, req)

	// Capability-gated operations. Each of these is answered with the error the
	// spec mandates for a capability the card declares as false, so a client can
	// tell "not supported by this agent" apart from "no such method".
	case "CreateTaskPushNotificationConfig", "GetTaskPushNotificationConfig",
		"ListTaskPushNotificationConfigs", "DeleteTaskPushNotificationConfig":
		h.writeError(w, req.ID, CodePushNotificationNotSupportedError, "This agent does not support push notifications",
			[]any{errorInfo("PUSH_NOTIFICATION_NOT_SUPPORTED", map[string]string{"capability": "pushNotifications"})})
	case "GetExtendedAgentCard":
		h.writeError(w, req.ID, CodeUnsupportedOperationError, "This agent has no extended agent card",
			[]any{errorInfo("UNSUPPORTED_OPERATION", map[string]string{"capability": "extendedAgentCard"})})

	default:
		h.writeError(w, req.ID, CodeMethodNotFoundError, "Method not found",
			[]any{errorInfo("METHOD_NOT_FOUND", map[string]string{"method": req.Method})})
	}
}

// writeJSON encodes a response, ignoring the write error: the connection is
// already gone if the encode fails, and there is nothing useful to do about it.
func (h *Handler) writeJSON(w http.ResponseWriter, resp JSONRPCResponse) {
	_ = json.NewEncoder(w).Encode(resp)
}

func (h *Handler) writeResult(w http.ResponseWriter, id any, result any) {
	h.writeJSON(w, JSONRPCResponse{JSONRPC: "2.0", ID: id, Result: result})
}

func (h *Handler) writeError(w http.ResponseWriter, id any, code int, message string, data []any) {
	h.writeJSON(w, JSONRPCResponse{
		JSONRPC: "2.0",
		ID:      id,
		Error:   &JSONRPCError{Code: code, Message: message, Data: data},
	})
}

// taskNotFound answers with the code the spec reserves for a missing task
// (-32001). The pre-1.0 handler used -32004, which the spec defines as
// UnsupportedOperationError, so a client could not tell a missing task from an
// unsupported one.
func (h *Handler) taskNotFound(w http.ResponseWriter, id any, taskID string) {
	h.writeError(w, id, CodeTaskNotFoundError, "Task not found",
		[]any{errorInfo("TASK_NOT_FOUND", map[string]string{"taskId": taskID})})
}

// requestTimeout is the upper bound applied to one A2A task.
//
// It mirrors the reference's per-request bound, `api.timeout`
// (config/schema.py:338 "Per-request timeout in seconds", default 120.0), which
// the reference applies to every API request (api/server.py:478-494). The
// default is therefore the 120s this handler used to hardcode.
func (h *Handler) requestTimeout() time.Duration {
	const fallback = 120 * time.Second
	if h == nil || h.cfg == nil || h.cfg.API.Timeout <= 0 {
		return fallback
	}
	return time.Duration(float64(h.cfg.API.Timeout) * float64(time.Second))
}

// ---------------------------------------------------------------------------
// SendMessage
// ---------------------------------------------------------------------------

// parseSendParams accepts both the v1.0 SendMessageRequest and the shapes the
// pre-1.0 handler exposed ({"message":{"text":...}} and {"input":...}).
//
// A2A 1.0 permits a server to accept the legacy request form during the overlap
// period, and the legacy form carries no messageId/role, so those are synthesised
// rather than rejected: refusing them would break existing callers for no
// protocol benefit. Responses are always emitted in the current form.
func (h *Handler) parseSendParams(raw json.RawMessage) (Message, *SendMessageConfiguration, error) {
	if len(raw) == 0 {
		return Message{}, nil, fmt.Errorf("params is required")
	}

	var wire struct {
		Tenant  string `json:"tenant"`
		Message struct {
			MessageID string `json:"messageId"`
			ContextID string `json:"contextId"`
			TaskID    string `json:"taskId"`
			Role      string `json:"role"`
			Parts     []Part `json:"parts"`
			Text      string `json:"text"`
		} `json:"message"`
		Configuration *SendMessageConfiguration `json:"configuration"`
		Input         string                    `json:"input"`
	}
	if err := json.Unmarshal(raw, &wire); err != nil {
		return Message{}, nil, fmt.Errorf("params is not a valid SendMessageRequest: %w", err)
	}

	msg := Message{
		MessageID: wire.Message.MessageID,
		ContextID: wire.Message.ContextID,
		TaskID:    wire.Message.TaskID,
		Role:      wire.Message.Role,
		Parts:     wire.Message.Parts,
	}

	// Legacy {"message":{"text":"..."}} / {"input":"..."} forms.
	if len(msg.Parts) == 0 {
		text := strings.TrimSpace(wire.Message.Text)
		if text == "" {
			text = strings.TrimSpace(wire.Input)
		}
		if text != "" {
			msg.Parts = []Part{NewTextPart(text)}
		}
	}

	if len(msg.Parts) == 0 {
		return Message{}, nil, fmt.Errorf("message.parts must contain at least one part")
	}
	if strings.TrimSpace(msg.MessageID) == "" {
		msg.MessageID = h.newID("msg")
	}
	if strings.TrimSpace(msg.Role) == "" {
		msg.Role = RoleUser
	}
	if msg.Role != RoleUser && msg.Role != RoleAgent {
		return Message{}, nil, fmt.Errorf("message.role must be %s or %s", RoleUser, RoleAgent)
	}

	return msg, wire.Configuration, nil
}

// newID mints an identifier. The spec only requires uniqueness, not a particular
// format, so a nanosecond stamp plus a process-local counter is enough and avoids
// pulling in a UUID dependency.
func (h *Handler) newID(prefix string) string {
	return fmt.Sprintf("%s-%d-%d", prefix, time.Now().UnixNano(), h.taskSeq.Add(1))
}

// handleSendMessage runs one task to a terminal state and answers with a
// SendMessageResponse.
//
// ctx is the INCOMING REQUEST's context: deriving the task from
// context.Background() meant a client that disconnected (or a proxy that timed
// out) left the agent calling the model and holding its concurrency slot until
// the bound elapsed, and broke cancellation propagation. The bound is kept, so a
// peer that stays connected still cannot pin a task open forever.
func (h *Handler) handleSendMessage(ctx context.Context, w http.ResponseWriter, req JSONRPCRequest) {
	plan, failure := h.prepareSend(req)
	if failure != nil {
		failure.write(w, req.ID)
		return
	}

	h.writeResult(w, req.ID, SendMessageResponse{Task: h.runTurn(ctx, plan)})
}

// sendPlan is a validated SendMessage request with its task and context already
// resolved. Splitting validation from execution is what lets the streaming
// handler answer a bad request with a plain JSON-RPC error instead of opening a
// stream it would immediately have to abandon.
type sendPlan struct {
	taskID    string
	contextID string
	input     string
	inbound   Message
}

// rpcFailure is a prepared JSON-RPC error.
type rpcFailure struct {
	code    int
	message string
	data    []any
}

func (f *rpcFailure) write(w http.ResponseWriter, id any) {
	_ = json.NewEncoder(w).Encode(JSONRPCResponse{
		JSONRPC: "2.0",
		ID:      id,
		Error:   &JSONRPCError{Code: f.code, Message: f.message, Data: f.data},
	})
}

// prepareSend validates a SendMessage request and resolves the task it targets.
func (h *Handler) prepareSend(req JSONRPCRequest) (sendPlan, *rpcFailure) {
	msg, _, err := h.parseSendParams(req.Params)
	if err != nil {
		return sendPlan{}, &rpcFailure{CodeInvalidParamsError, "Invalid parameters",
			[]any{errorInfo("INVALID_PARAMS", map[string]string{"detail": err.Error()})}}
	}

	if msg.HasUnsupportedContent() {
		return sendPlan{}, &rpcFailure{CodeContentTypeNotSupportedError, "Content type not supported",
			[]any{errorInfo("CONTENT_TYPE_NOT_SUPPORTED", map[string]string{
				"supportedInputModes": MediaTypeText,
			})}}
	}

	inputText := strings.TrimSpace(msg.Text())
	if inputText == "" {
		return sendPlan{}, &rpcFailure{CodeInvalidParamsError, "Invalid parameters",
			[]any{errorInfo("INVALID_PARAMS", map[string]string{
				"field": "message.parts", "detail": "at least one text part is required",
			})}}
	}

	// Continuing a task the server does not have is an error, not a new task:
	// silently creating one would make a client's follow-up look successful while
	// its context was dropped.
	//
	// The same guard applies to a task that already finished (a terminal task
	// cannot be resumed — the spec reserves UnsupportedOperationError for that)
	// and to a contextId that contradicts the task it is sent with, which would
	// otherwise silently file the turn under a context the task never belonged
	// to.
	taskID := msg.TaskID
	contextID := msg.ContextID
	if taskID != "" {
		existing, ok := h.loadTask(taskID)
		if !ok {
			return sendPlan{}, &rpcFailure{CodeTaskNotFoundError, "Task not found",
				[]any{errorInfo("TASK_NOT_FOUND", map[string]string{"taskId": taskID})}}
		}
		if existing.terminal() {
			return sendPlan{}, &rpcFailure{CodeUnsupportedOperationError,
				"Operation not supported for a task in a terminal state",
				[]any{errorInfo("UNSUPPORTED_OPERATION", map[string]string{
					"taskId": taskID,
					"state":  existing.Status.State,
				})}}
		}
		if contextID != "" && contextID != existing.ContextID {
			return sendPlan{}, &rpcFailure{CodeInvalidParamsError, "Invalid parameters",
				[]any{errorInfo("INVALID_PARAMS", map[string]string{
					"field":             "message.contextId",
					"detail":            "contextId does not match the task it was sent with",
					"taskId":            taskID,
					"expectedContextId": existing.ContextID,
				})}}
		}
		// The task already carries a context, so an omitted contextId is inferred
		// from it rather than minting a second one for the same conversation.
		contextID = existing.ContextID
	}

	if taskID == "" {
		taskID = h.newID("task")
	}
	if contextID == "" {
		contextID = h.newID("ctx")
	}

	return sendPlan{
		taskID:    taskID,
		contextID: contextID,
		input:     inputText,
		// The history is the inbound message exactly as it arrived, so a peer can
		// read back what it sent.
		inbound: Message{
			MessageID: msg.MessageID,
			ContextID: contextID,
			TaskID:    taskID,
			Role:      RoleUser,
			Parts:     msg.Parts,
		},
	}, nil
}

// runTurn executes one agent turn and publishes the task states it passes
// through: WORKING when accepted, then the terminal state. It never writes a
// response, so the blocking and streaming handlers can share it.
func (h *Handler) runTurn(ctx context.Context, plan sendPlan) *Task {
	task := Task{
		ID:        plan.taskID,
		ContextID: plan.contextID,
		Status:    taskStatus(TaskStateWorking),
		History:   []Message{plan.inbound},
	}
	h.publish(task)

	// The turn is cancellable from outside while it runs, which is what CancelTask
	// acts on. taskCtx is derived from the request context so a disconnected peer
	// still stops the work.
	taskCtx, cancel := context.WithTimeout(ctx, h.requestTimeout())
	h.cancels.Store(plan.taskID, cancel)
	defer func() {
		h.cancels.Delete(plan.taskID)
		cancel()
	}()

	var finalOutput string
	if h.loop != nil {
		in := core.InboundMessage{
			Channel:   "a2a",
			SenderID:  "a2a_peer",
			ChatID:    plan.contextID,
			Content:   plan.input,
			Timestamp: time.Now(),
			Metadata:  map[string]any{"source": "a2a_protocol", "taskId": plan.taskID},
		}

		out, runErr := h.loop.ProcessMessage(taskCtx, in)
		if runErr != nil {
			// A task canceled while running already carries its final state, and
			// that state must survive: overwriting it with FAILED would report a
			// deliberate cancellation as a crash.
			if current, ok := h.loadTask(plan.taskID); ok && current.terminal() {
				return current
			}
			task.Status = taskStatus(TaskStateFailed)
			task.Status.Message = &Message{
				MessageID: h.newID("msg"),
				ContextID: plan.contextID,
				TaskID:    plan.taskID,
				Role:      RoleAgent,
				Parts:     []Part{NewTextPart(runErr.Error())},
			}
			h.publish(task)
			return h.snapshotOr(task)
		}
		if out != nil {
			finalOutput = out.Content
		}
	} else {
		finalOutput = fmt.Sprintf("Haosbot received: %s (agent loop not attached)", plan.input)
	}

	// A cancellation that landed between the turn returning and this publish must
	// still win, otherwise the client that asked to cancel gets a completed task.
	if current, ok := h.loadTask(plan.taskID); ok && current.terminal() {
		return current
	}

	task.Status = taskStatus(TaskStateCompleted)
	task.Artifacts = []Artifact{{
		ArtifactID:  h.newID("artifact"),
		Name:        "response",
		Description: "Agent response text",
		Parts:       []Part{NewTextPart(finalOutput)},
	}}
	task.History = append(task.History, Message{
		MessageID: h.newID("msg"),
		ContextID: plan.contextID,
		TaskID:    plan.taskID,
		Role:      RoleAgent,
		Parts:     []Part{NewTextPart(finalOutput)},
	})
	h.publish(task)

	// The reply carries the same snapshot that was just published, so the caller
	// and a concurrent GetTask can never disagree about this task id.
	return h.snapshotOr(task)
}

// snapshotOr returns the published snapshot for a task, falling back to the
// local value when the entry was evicted between publish and read.
func (h *Handler) snapshotOr(task Task) *Task {
	if current, ok := h.loadTask(task.ID); ok {
		return current
	}
	snapshot := task
	snapshot.updated = time.Now()
	return &snapshot
}

// parseTaskID reads the id parameter shared by GetTask, CancelTask and
// SubscribeToTask.
func (h *Handler) parseTaskID(req JSONRPCRequest) (string, *rpcFailure) {
	var params struct {
		ID string `json:"id"`
	}
	if len(req.Params) > 0 {
		if err := json.Unmarshal(req.Params, &params); err != nil {
			return "", &rpcFailure{CodeInvalidParamsError, "Invalid parameters",
				[]any{errorInfo("INVALID_PARAMS", map[string]string{"detail": err.Error()})}}
		}
	}
	if strings.TrimSpace(params.ID) == "" {
		return "", &rpcFailure{CodeInvalidParamsError, "Invalid parameters",
			[]any{errorInfo("INVALID_PARAMS", map[string]string{"field": "id", "detail": "id is required"})}}
	}
	return params.ID, nil
}

// ---------------------------------------------------------------------------
// GetTask
// ---------------------------------------------------------------------------

func (h *Handler) handleGetTask(w http.ResponseWriter, req JSONRPCRequest) {
	var params struct {
		ID            string `json:"id"`
		HistoryLength *int   `json:"historyLength"`
	}
	if len(req.Params) > 0 {
		if err := json.Unmarshal(req.Params, &params); err != nil {
			h.writeError(w, req.ID, CodeInvalidParamsError, "Invalid parameters",
				[]any{errorInfo("INVALID_PARAMS", map[string]string{"detail": err.Error()})})
			return
		}
	}

	if strings.TrimSpace(params.ID) == "" {
		h.writeError(w, req.ID, CodeInvalidParamsError, "Invalid parameters",
			[]any{errorInfo("INVALID_PARAMS", map[string]string{"field": "id", "detail": "id is required"})})
		return
	}

	task, ok := h.loadTask(params.ID)
	if !ok {
		h.taskNotFound(w, req.ID, params.ID)
		return
	}

	h.writeResult(w, req.ID, taskWithHistoryLength(task, params.HistoryLength))
}

// taskWithHistoryLength applies the GetTask historyLength parameter: unset means
// no limit, zero means no history, and any other value caps the most recent
// messages returned (proto GetTaskRequest.history_length).
func taskWithHistoryLength(task *Task, historyLength *int) *Task {
	if historyLength == nil {
		return task
	}
	out := *task
	n := *historyLength
	switch {
	case n <= 0:
		out.History = nil
	case n < len(out.History):
		out.History = out.History[len(out.History)-n:]
	}
	return &out
}

// ---------------------------------------------------------------------------
// ListTasks
// ---------------------------------------------------------------------------

func (h *Handler) handleListTasks(w http.ResponseWriter, req JSONRPCRequest) {
	var params struct {
		ContextID            string `json:"contextId"`
		Status               string `json:"status"`
		PageSize             *int   `json:"pageSize"`
		PageToken            string `json:"pageToken"`
		HistoryLength        *int   `json:"historyLength"`
		IncludeArtifacts     *bool  `json:"includeArtifacts"`
		StatusTimestampAfter string `json:"statusTimestampAfter"`
	}
	if len(req.Params) > 0 {
		if err := json.Unmarshal(req.Params, &params); err != nil {
			h.writeError(w, req.ID, CodeInvalidParamsError, "Invalid parameters",
				[]any{errorInfo("INVALID_PARAMS", map[string]string{"detail": err.Error()})})
			return
		}
	}

	pageSize := defaultListPageSize
	if params.PageSize != nil {
		pageSize = *params.PageSize
		if pageSize < 1 {
			h.writeError(w, req.ID, CodeInvalidParamsError, "Invalid parameters",
				[]any{errorInfo("INVALID_PARAMS", map[string]string{"field": "pageSize", "detail": "pageSize must be at least 1"})})
			return
		}
		if pageSize > maxListPageSize {
			pageSize = maxListPageSize
		}
	}

	offset := 0
	if params.PageToken != "" {
		parsed, err := strconv.Atoi(params.PageToken)
		if err != nil || parsed < 0 {
			h.writeError(w, req.ID, CodeInvalidParamsError, "Invalid parameters",
				[]any{errorInfo("INVALID_PARAMS", map[string]string{"field": "pageToken", "detail": "pageToken is not a token issued by this agent"})})
			return
		}
		offset = parsed
	}

	includeArtifacts := params.IncludeArtifacts != nil && *params.IncludeArtifacts

	// Deterministic order: the store is a map, so listing without sorting would
	// hand out a different page order on every call and make pageToken meaningless.
	all := make([]*Task, 0, 64)
	h.tasks.Range(func(_, value any) bool {
		task, ok := value.(*Task)
		if !ok {
			return true
		}
		if params.ContextID != "" && task.ContextID != params.ContextID {
			return true
		}
		if params.Status != "" && task.Status.State != params.Status {
			return true
		}
		if params.StatusTimestampAfter != "" {
			after, err := time.Parse(time.RFC3339, params.StatusTimestampAfter)
			if err == nil {
				ts, err := time.Parse(time.RFC3339Nano, task.Status.Timestamp)
				if err == nil && ts.Before(after) {
					return true
				}
			}
		}
		all = append(all, task)
		return true
	})

	slices.SortFunc(all, func(a, b *Task) int {
		if a.updated.Equal(b.updated) {
			return strings.Compare(a.ID, b.ID)
		}
		return b.updated.Compare(a.updated)
	})

	total := len(all)
	if offset > total {
		offset = total
	}
	end := offset + pageSize
	if end > total {
		end = total
	}
	page := all[offset:end]

	out := make([]*Task, 0, len(page))
	for _, task := range page {
		view := taskWithHistoryLength(task, params.HistoryLength)
		if !includeArtifacts {
			// The proto defaults include_artifacts to false to keep the payload
			// small; the copy keeps the stored snapshot untouched.
			trimmed := *view
			trimmed.Artifacts = nil
			view = &trimmed
		}
		out = append(out, view)
	}

	next := ""
	if end < total {
		next = strconv.Itoa(end)
	}

	h.writeResult(w, req.ID, ListTasksResponse{
		Tasks:         out,
		NextPageToken: next,
		PageSize:      pageSize,
		TotalSize:     total,
	})
}

// ---------------------------------------------------------------------------
// CancelTask
// ---------------------------------------------------------------------------

// handleCancelTask stops a running task and answers with the resulting Task.
//
// Cancellation is idempotent for a task that is still retained: cancelling an
// already-canceled task returns it again rather than erroring. A task that
// reached any other terminal state is not cancelable, and the spec has a
// dedicated code for that (-32002) instead of the generic failure this handler
// used to return.
func (h *Handler) handleCancelTask(w http.ResponseWriter, req JSONRPCRequest) {
	var params struct {
		ID string `json:"id"`
	}
	if len(req.Params) > 0 {
		if err := json.Unmarshal(req.Params, &params); err != nil {
			h.writeError(w, req.ID, CodeInvalidParamsError, "Invalid parameters",
				[]any{errorInfo("INVALID_PARAMS", map[string]string{"detail": err.Error()})})
			return
		}
	}
	if strings.TrimSpace(params.ID) == "" {
		h.writeError(w, req.ID, CodeInvalidParamsError, "Invalid parameters",
			[]any{errorInfo("INVALID_PARAMS", map[string]string{"field": "id", "detail": "id is required"})})
		return
	}

	task, ok := h.loadTask(params.ID)
	if !ok {
		h.taskNotFound(w, req.ID, params.ID)
		return
	}

	switch task.Status.State {
	case TaskStateCanceled:
		h.writeResult(w, req.ID, task)
		return
	case TaskStateCompleted, TaskStateFailed, TaskStateRejected:
		h.writeError(w, req.ID, CodeTaskNotCancelableError, "Task cannot be canceled",
			[]any{errorInfo("TASK_NOT_CANCELABLE", map[string]string{
				"taskId": params.ID,
				"state":  task.Status.State,
			})})
		return
	}

	if cancel, ok := h.cancels.Load(params.ID); ok {
		if fn, ok := cancel.(context.CancelFunc); ok {
			fn()
		}
	}

	// Publish the canceled state here rather than waiting for the running turn to
	// notice: the caller is entitled to the post-cancellation Task in this
	// response, and the send path refuses to overwrite a terminal state.
	canceled := *task
	canceled.Status = taskStatus(TaskStateCanceled)
	h.publish(canceled)

	h.writeResult(w, req.ID, h.snapshotOr(canceled))
}
