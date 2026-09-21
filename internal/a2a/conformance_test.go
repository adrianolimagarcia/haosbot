package a2a

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/adrianolimagarcia/nanobot-go/internal/config"
)

// ---------------------------------------------------------------------------
// Helpers
// ---------------------------------------------------------------------------

// rpcResult posts one JSON-RPC request and returns the raw result plus the
// decoded error, if any. It fails the test only when the body is not JSON.
func rpcResult(t *testing.T, client *http.Client, baseURL, method, params string) (json.RawMessage, *JSONRPCError, string) {
	t.Helper()

	if params == "" {
		params = "{}"
	}
	body := fmt.Sprintf(`{"jsonrpc":"2.0","id":"t","method":%q,"params":%s}`, method, params)

	resp, err := client.Post(baseURL+"/a2a", "application/json", strings.NewReader(body))
	if err != nil {
		t.Fatalf("POST /a2a (%s): %v", method, err)
	}
	defer func() { _ = resp.Body.Close() }()

	raw := make([]byte, 0, 4096)
	buf := make([]byte, 1024)
	for {
		n, err := resp.Body.Read(buf)
		raw = append(raw, buf[:n]...)
		if err != nil {
			break
		}
	}

	var envelope struct {
		JSONRPC string          `json:"jsonrpc"`
		Result  json.RawMessage `json:"result"`
		Error   *JSONRPCError   `json:"error"`
	}
	if err := json.Unmarshal(raw, &envelope); err != nil {
		t.Fatalf("%s answered with invalid JSON-RPC: %s", method, raw)
	}
	if envelope.JSONRPC != "2.0" {
		t.Fatalf("%s answered jsonrpc=%q want \"2.0\"", method, envelope.JSONRPC)
	}
	return envelope.Result, envelope.Error, string(raw)
}

// sendMessage drives one SendMessage and returns the decoded Task.
func sendMessage(t *testing.T, client *http.Client, baseURL, text string) *Task {
	t.Helper()

	params := fmt.Sprintf(`{"message":{"messageId":"msg-1","role":"ROLE_USER","parts":[{"text":%q}]}}`, text)
	result, rpcErr, raw := rpcResult(t, client, baseURL, "SendMessage", params)
	if rpcErr != nil {
		t.Fatalf("SendMessage failed: %d %s (%s)", rpcErr.Code, rpcErr.Message, raw)
	}

	// SendMessageResponse is a oneof: the result must be wrapped in a "task" or
	// "message" member, never a bare Task.
	var envelope map[string]json.RawMessage
	if err := json.Unmarshal(result, &envelope); err != nil {
		t.Fatalf("SendMessage result is not an object: %s", result)
	}
	taskRaw, ok := envelope["task"]
	if !ok {
		if _, isMessage := envelope["message"]; isMessage {
			t.Fatalf("SendMessage answered with a Message where the test expects a Task: %s", result)
		}
		t.Fatalf("SendMessage result is not a SendMessageResponse (no task/message member): %s", result)
	}

	var task Task
	if err := json.Unmarshal(taskRaw, &task); err != nil {
		t.Fatalf("decode task: %v (%s)", err, taskRaw)
	}
	return &task
}

// ---------------------------------------------------------------------------
// SendMessage / Task shape
// ---------------------------------------------------------------------------

// TestSendMessageReturnsAConformantTask pins the data model A2A 1.0 requires.
// The previous handler answered with a flat object — {"id","status":"completed",
// "input","output","createdAt","updatedAt"} — whose status was a bare string
// where the spec requires a TaskStatus object, and whose four extra members do
// not exist on Task at all. The official SDK client rejects exactly that shape.
func TestSendMessageReturnsAConformantTask(t *testing.T) {
	srv, _ := newTaskServer(t, config.DefaultConfig(), nil)

	task := sendMessage(t, srv.Client(), srv.URL, "hello")

	if task.ID == "" {
		t.Error("task.id is required")
	}
	if task.ContextID == "" {
		t.Error("task.contextId must be set so a peer can correlate a conversation")
	}
	if task.Status.State != TaskStateCompleted {
		t.Errorf("task.status.state=%q want %q", task.Status.State, TaskStateCompleted)
	}
	if task.Status.Timestamp == "" {
		t.Error("task.status.timestamp must be set")
	}
	if len(task.Artifacts) == 0 {
		t.Fatal("a completed task must carry its output as an artifact")
	}
	if task.Artifacts[0].ArtifactID == "" {
		t.Error("artifact.artifactId is required")
	}
	if len(task.Artifacts[0].Parts) == 0 {
		t.Fatal("an artifact must contain at least one part")
	}
	if got := task.Artifacts[0].Parts[0].Text; !strings.Contains(got, "hello") {
		t.Errorf("artifact text=%q want it to contain the input", got)
	}
	if len(task.History) == 0 {
		t.Error("task.history should carry the interaction")
	}
}

// TestTaskSerialisationHasNoLegacyFields guards the wire form itself: a client
// that validates the Task schema rejects the whole object when it carries
// members the schema does not define.
func TestTaskSerialisationHasNoLegacyFields(t *testing.T) {
	srv, _ := newTaskServer(t, config.DefaultConfig(), nil)

	_, _, raw := rpcResult(t, srv.Client(), srv.URL, "SendMessage",
		`{"message":{"messageId":"m","role":"ROLE_USER","parts":[{"text":"hi"}]}}`)

	var envelope struct {
		Result map[string]json.RawMessage `json:"result"`
	}
	if err := json.Unmarshal([]byte(raw), &envelope); err != nil {
		t.Fatalf("decode: %v", err)
	}
	taskRaw, ok := envelope.Result["task"]
	if !ok {
		t.Fatalf("result has no task member: %s", raw)
	}

	var fields map[string]json.RawMessage
	if err := json.Unmarshal(taskRaw, &fields); err != nil {
		t.Fatalf("decode task: %v", err)
	}
	allowed := map[string]bool{"id": true, "contextId": true, "status": true, "artifacts": true, "history": true, "metadata": true}
	for field := range fields {
		if !allowed[field] {
			t.Errorf("task carries %q, which is not a v1.0 Task field", field)
		}
	}

	// status must be an object carrying a state, not a string.
	var status map[string]json.RawMessage
	if err := json.Unmarshal(fields["status"], &status); err != nil {
		t.Fatalf("status is not an object (the spec requires TaskStatus): %s", fields["status"])
	}
	var state string
	if err := json.Unmarshal(status["state"], &state); err != nil {
		t.Fatalf("status.state missing: %s", fields["status"])
	}
	if !strings.HasPrefix(state, "TASK_STATE_") {
		t.Errorf("status.state=%q want a TASK_STATE_* enum name", state)
	}
}

// TestTimestampsAreUTCOffsetZ pins the timestamp rule. time.RFC3339 on a
// non-UTC time.Time emits a numeric offset, and the TCK rejects any offset other
// than 'Z'.
func TestTimestampsAreUTCOffsetZ(t *testing.T) {
	srv, _ := newTaskServer(t, config.DefaultConfig(), nil)

	task := sendMessage(t, srv.Client(), srv.URL, "hi")
	if !strings.HasSuffix(task.Status.Timestamp, "Z") {
		t.Fatalf("status.timestamp=%q want a UTC instant ending in Z", task.Status.Timestamp)
	}
	if _, err := time.Parse(time.RFC3339Nano, task.Status.Timestamp); err != nil {
		t.Fatalf("status.timestamp=%q is not RFC 3339: %v", task.Status.Timestamp, err)
	}
}

// TestPartHasNoKindDiscriminator pins the A2A 1.0 Part form. v0.3 used
// {"kind":"text","text":...}; 1.0 removed `kind` and made the member name the
// discriminator.
func TestPartHasNoKindDiscriminator(t *testing.T) {
	encoded, err := json.Marshal(NewTextPart("hello"))
	if err != nil {
		t.Fatalf("marshal part: %v", err)
	}
	if strings.Contains(string(encoded), "kind") {
		t.Errorf("part %s carries the removed v0.3 kind discriminator", encoded)
	}
	var decoded map[string]any
	if err := json.Unmarshal(encoded, &decoded); err != nil {
		t.Fatalf("decode part: %v", err)
	}
	if decoded["text"] != "hello" {
		t.Errorf("part=%s want a text member", encoded)
	}

	// An all-empty part must still serialise to a valid Part rather than {}.
	empty, err := json.Marshal(Part{})
	if err != nil {
		t.Fatalf("marshal empty part: %v", err)
	}
	if string(empty) == "{}" {
		t.Error("an empty Part serialised to {}, which is not a valid Part")
	}
}

// TestPartAcceptsLegacyKindForm pins the overlap-period tolerance the spec
// allows: a pre-1.0 peer sending {"kind":"text","text":...} must still be read.
func TestPartAcceptsLegacyKindForm(t *testing.T) {
	var part Part
	if err := json.Unmarshal([]byte(`{"kind":"text","text":"legacy"}`), &part); err != nil {
		t.Fatalf("unmarshal legacy text part: %v", err)
	}
	if part.Text != "legacy" {
		t.Errorf("text=%q want legacy", part.Text)
	}

	var file Part
	if err := json.Unmarshal([]byte(`{"kind":"file","file":{"name":"a.txt","mimeType":"text/plain","fileWithBytes":"aGk="}}`), &file); err != nil {
		t.Fatalf("unmarshal legacy file part: %v", err)
	}
	if file.Filename != "a.txt" || file.MediaType != "text/plain" || file.Raw != "aGk=" {
		t.Errorf("legacy file part decoded to %+v", file)
	}
}

// ---------------------------------------------------------------------------
// Method names
// ---------------------------------------------------------------------------

// TestV1MethodNamesAreDispatched pins that the PascalCase names A2A 1.0 defines
// reach their handlers. Before this change every one of them except SendMessage
// answered -32601, because the dispatcher only knew the dotted pre-1.0 names.
func TestV1MethodNamesAreDispatched(t *testing.T) {
	srv, _ := newTaskServer(t, config.DefaultConfig(), nil)
	client := srv.Client()

	created := sendMessage(t, client, srv.URL, "seed")

	cases := []struct {
		method string
		params string
	}{
		{"GetTask", fmt.Sprintf(`{"id":%q}`, created.ID)},
		{"ListTasks", `{}`},
		{"CancelTask", fmt.Sprintf(`{"id":%q}`, created.ID)},
	}
	for _, tc := range cases {
		t.Run(tc.method, func(t *testing.T) {
			_, rpcErr, raw := rpcResult(t, client, srv.URL, tc.method, tc.params)
			if rpcErr != nil && rpcErr.Code == CodeMethodNotFoundError {
				t.Fatalf("%s is a v1.0 method but answered MethodNotFound: %s", tc.method, raw)
			}
		})
	}
}

// TestLegacyMethodAliasesStillWork pins that the pre-1.0 dotted names this
// handler used to expose keep working, which the spec's overlap period allows.
func TestLegacyMethodAliasesStillWork(t *testing.T) {
	srv, _ := newTaskServer(t, config.DefaultConfig(), nil)
	client := srv.Client()

	for _, method := range []string{"tasks/send", "message/send", "tasks/create"} {
		t.Run(method, func(t *testing.T) {
			result, rpcErr, raw := rpcResult(t, client, srv.URL, method,
				`{"message":{"text":"legacy call"}}`)
			if rpcErr != nil {
				t.Fatalf("%s failed: %d %s", method, rpcErr.Code, rpcErr.Message)
			}
			if !strings.Contains(string(result), `"task"`) {
				t.Fatalf("%s must answer with a SendMessageResponse, got %s", method, raw)
			}
		})
	}

	// tasks/get is the legacy spelling of GetTask.
	created := sendMessage(t, client, srv.URL, "seed")
	result, rpcErr, _ := rpcResult(t, client, srv.URL, "tasks/get", fmt.Sprintf(`{"id":%q}`, created.ID))
	if rpcErr != nil {
		t.Fatalf("tasks/get failed: %d %s", rpcErr.Code, rpcErr.Message)
	}
	var got Task
	if err := json.Unmarshal(result, &got); err != nil {
		t.Fatalf("decode tasks/get result: %v", err)
	}
	if got.ID != created.ID {
		t.Errorf("tasks/get returned task %q want %q", got.ID, created.ID)
	}
}

// TestUnknownMethodIsMethodNotFound keeps the standard JSON-RPC code for a
// method that genuinely does not exist.
func TestUnknownMethodIsMethodNotFound(t *testing.T) {
	srv, _ := newTaskServer(t, config.DefaultConfig(), nil)

	_, rpcErr, _ := rpcResult(t, srv.Client(), srv.URL, "NoSuchMethod", `{}`)
	if rpcErr == nil {
		t.Fatal("an unknown method must return an error")
	}
	if rpcErr.Code != CodeMethodNotFoundError {
		t.Errorf("code=%d want %d", rpcErr.Code, CodeMethodNotFoundError)
	}
	if len(rpcErr.Data) == 0 {
		t.Error("A2A errors must carry a data array with a @type key")
	}
}

// ---------------------------------------------------------------------------
// Error codes
// ---------------------------------------------------------------------------

// TestErrorCodesMatchTheSpec pins the codes the spec reserves for A2A errors.
// The previous handler answered "Task not found" with -32004, which the spec
// defines as UnsupportedOperationError.
func TestErrorCodesMatchTheSpec(t *testing.T) {
	srv, _ := newTaskServer(t, config.DefaultConfig(), nil)
	client := srv.Client()

	created := sendMessage(t, client, srv.URL, "seed")

	cases := []struct {
		name   string
		method string
		params string
		want   int
	}{
		{"missing task", "GetTask", `{"id":"does-not-exist"}`, CodeTaskNotFoundError},
		{"missing task via legacy alias", "tasks/get", `{"id":"does-not-exist"}`, CodeTaskNotFoundError},
		{"cancel a completed task", "CancelTask", fmt.Sprintf(`{"id":%q}`, created.ID), CodeTaskNotCancelableError},
		{"streaming is not a declared capability", "SendStreamingMessage", `{"message":{"messageId":"m","role":"ROLE_USER","parts":[{"text":"x"}]}}`, CodeUnsupportedOperationError},
		{"subscribe is not a declared capability", "SubscribeToTask", `{"id":"x"}`, CodeUnsupportedOperationError},
		{"push notifications are not a declared capability", "CreateTaskPushNotificationConfig", `{"id":"x"}`, CodePushNotificationNotSupportedError},
		{"no extended card is configured", "GetExtendedAgentCard", `{}`, CodeUnsupportedOperationError},
		{"missing id", "GetTask", `{}`, CodeInvalidParamsError},
		{"no parts", "SendMessage", `{"message":{"messageId":"m","role":"ROLE_USER","parts":[]}}`, CodeInvalidParamsError},
		{"unsupported content", "SendMessage", `{"message":{"messageId":"m","role":"ROLE_USER","parts":[{"raw":"aGk="}]}}`, CodeContentTypeNotSupportedError},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, rpcErr, raw := rpcResult(t, client, srv.URL, tc.method, tc.params)
			if rpcErr == nil {
				t.Fatalf("%s(%s) succeeded, want error %d: %s", tc.method, tc.params, tc.want, raw)
			}
			if rpcErr.Code != tc.want {
				t.Fatalf("%s(%s) code=%d want %d (%s)", tc.method, tc.params, rpcErr.Code, tc.want, rpcErr.Message)
			}
		})
	}
}

// TestErrorCarriesErrorInfoDetail pins the error.data shape from spec 9.5: an
// array of objects, each carrying a @type key.
func TestErrorCarriesErrorInfoDetail(t *testing.T) {
	srv, _ := newTaskServer(t, config.DefaultConfig(), nil)

	_, rpcErr, _ := rpcResult(t, srv.Client(), srv.URL, "GetTask", `{"id":"nope"}`)
	if rpcErr == nil {
		t.Fatal("want an error")
	}
	if len(rpcErr.Data) == 0 {
		t.Fatal("error.data must carry at least one detail object")
	}
	detail, ok := rpcErr.Data[0].(map[string]any)
	if !ok {
		t.Fatalf("error.data[0] is not an object: %#v", rpcErr.Data[0])
	}
	if _, ok := detail["@type"]; !ok {
		t.Errorf("every error.data entry MUST include a @type key: %#v", detail)
	}
}

// TestUnsupportedProtocolVersionIsRefused pins the A2A-Version service
// parameter. A version this interface does not serve must be refused rather than
// processed with the wrong semantics.
func TestUnsupportedProtocolVersionIsRefused(t *testing.T) {
	srv, _ := newTaskServer(t, config.DefaultConfig(), nil)
	client := srv.Client()

	post := func(version string) (int, string) {
		body := `{"jsonrpc":"2.0","id":"t","method":"ListTasks","params":{}}`
		req, err := http.NewRequest(http.MethodPost, srv.URL+"/a2a", strings.NewReader(body))
		if err != nil {
			t.Fatalf("build request: %v", err)
		}
		req.Header.Set("Content-Type", "application/json")
		if version != "" {
			req.Header.Set("A2A-Version", version)
		}
		resp, err := client.Do(req)
		if err != nil {
			t.Fatalf("POST: %v", err)
		}
		defer func() { _ = resp.Body.Close() }()

		var envelope struct {
			Error *JSONRPCError `json:"error"`
		}
		if err := json.NewDecoder(resp.Body).Decode(&envelope); err != nil {
			t.Fatalf("decode: %v", err)
		}
		if envelope.Error == nil {
			return 0, ""
		}
		return envelope.Error.Code, envelope.Error.Message
	}

	if code, _ := post("99.9"); code != CodeVersionNotSupportedError {
		t.Errorf("A2A-Version 99.9 answered code=%d want %d", code, CodeVersionNotSupportedError)
	}
	for _, version := range []string{"", "1.0", "0.3"} {
		if code, msg := post(version); code != 0 {
			t.Errorf("A2A-Version %q must be accepted, got code=%d %s", version, code, msg)
		}
	}
}

// ---------------------------------------------------------------------------
// GetTask / ListTasks / CancelTask behaviour
// ---------------------------------------------------------------------------

// TestSendMessageToUnknownTaskIsNotFound pins that continuing a task the server
// does not hold is an error. Silently creating a new task would make a client's
// follow-up look successful while its context was dropped.
func TestSendMessageToUnknownTaskIsNotFound(t *testing.T) {
	srv, _ := newTaskServer(t, config.DefaultConfig(), nil)

	_, rpcErr, _ := rpcResult(t, srv.Client(), srv.URL, "SendMessage",
		`{"message":{"messageId":"m","role":"ROLE_USER","taskId":"ghost","parts":[{"text":"hi"}]}}`)
	if rpcErr == nil {
		t.Fatal("want an error when continuing an unknown task")
	}
	if rpcErr.Code != CodeTaskNotFoundError {
		t.Errorf("code=%d want %d", rpcErr.Code, CodeTaskNotFoundError)
	}
}

// TestGetTaskHistoryLength pins the GetTask historyLength parameter: unset means
// no limit and zero means no history.
func TestGetTaskHistoryLength(t *testing.T) {
	srv, _ := newTaskServer(t, config.DefaultConfig(), nil)
	client := srv.Client()

	created := sendMessage(t, client, srv.URL, "hi")

	unset, rpcErr, _ := rpcResult(t, client, srv.URL, "GetTask", fmt.Sprintf(`{"id":%q}`, created.ID))
	if rpcErr != nil {
		t.Fatalf("GetTask: %d %s", rpcErr.Code, rpcErr.Message)
	}
	var full Task
	if err := json.Unmarshal(unset, &full); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(full.History) == 0 {
		t.Fatal("historyLength unset must not truncate the history")
	}

	zero, rpcErr, _ := rpcResult(t, client, srv.URL, "GetTask", fmt.Sprintf(`{"id":%q,"historyLength":0}`, created.ID))
	if rpcErr != nil {
		t.Fatalf("GetTask: %d %s", rpcErr.Code, rpcErr.Message)
	}
	var none Task
	if err := json.Unmarshal(zero, &none); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(none.History) != 0 {
		t.Errorf("historyLength=0 returned %d history entries, want none", len(none.History))
	}
}

// TestListTasksPaginates pins the paging contract: a stable order, a nextPageToken
// while more remain, and the four REQUIRED response fields.
func TestListTasksPaginates(t *testing.T) {
	srv, _ := newTaskServer(t, config.DefaultConfig(), nil)
	client := srv.Client()

	const total = 5
	for i := 0; i < total; i++ {
		sendMessage(t, client, srv.URL, fmt.Sprintf("task %d", i))
	}

	first, rpcErr, raw := rpcResult(t, client, srv.URL, "ListTasks", `{"pageSize":2}`)
	if rpcErr != nil {
		t.Fatalf("ListTasks: %d %s", rpcErr.Code, rpcErr.Message)
	}
	var page ListTasksResponse
	if err := json.Unmarshal(first, &page); err != nil {
		t.Fatalf("decode ListTasks: %v (%s)", err, raw)
	}
	if len(page.Tasks) != 2 {
		t.Fatalf("pageSize=2 returned %d tasks", len(page.Tasks))
	}
	if page.TotalSize != total {
		t.Errorf("totalSize=%d want %d", page.TotalSize, total)
	}
	if page.PageSize != 2 {
		t.Errorf("pageSize=%d want 2", page.PageSize)
	}
	if page.NextPageToken == "" {
		t.Fatal("nextPageToken must be set while more tasks remain")
	}
	// includeArtifacts defaults to false, which is what keeps the page small.
	if len(page.Tasks[0].Artifacts) != 0 {
		t.Error("artifacts must be omitted unless includeArtifacts is true")
	}

	second, rpcErr, _ := rpcResult(t, client, srv.URL, "ListTasks",
		fmt.Sprintf(`{"pageSize":2,"pageToken":%q}`, page.NextPageToken))
	if rpcErr != nil {
		t.Fatalf("ListTasks page 2: %d %s", rpcErr.Code, rpcErr.Message)
	}
	var page2 ListTasksResponse
	if err := json.Unmarshal(second, &page2); err != nil {
		t.Fatalf("decode page 2: %v", err)
	}
	if len(page2.Tasks) != 2 {
		t.Fatalf("page 2 returned %d tasks", len(page2.Tasks))
	}
	for _, a := range page.Tasks {
		for _, b := range page2.Tasks {
			if a.ID == b.ID {
				t.Errorf("task %s appeared on both pages", a.ID)
			}
		}
	}
}

// TestListTasksRejectsForeignPageToken keeps an opaque token from being read as
// anything but a token this agent issued.
func TestListTasksRejectsForeignPageToken(t *testing.T) {
	srv, _ := newTaskServer(t, config.DefaultConfig(), nil)

	_, rpcErr, _ := rpcResult(t, srv.Client(), srv.URL, "ListTasks", `{"pageToken":"not-a-token"}`)
	if rpcErr == nil {
		t.Fatal("a foreign pageToken must be rejected")
	}
	if rpcErr.Code != CodeInvalidParamsError {
		t.Errorf("code=%d want %d", rpcErr.Code, CodeInvalidParamsError)
	}
}

// TestCancelTaskStopsARunningTask pins that CancelTask actually interrupts the
// running turn. Before this change there was no way to stop a task short of
// closing the connection: the handler kept no handle on the turn's context.
func TestCancelTaskStopsARunningTask(t *testing.T) {
	prov := newGateProvider()
	srv, h := newTaskServer(t, config.DefaultConfig(), newTestLoop(t, prov))
	client := srv.Client()

	id, response := beginTask(t, h, client, srv.URL, prov)

	result, rpcErr, raw := rpcResult(t, client, srv.URL, "CancelTask", fmt.Sprintf(`{"id":%q}`, id))
	if rpcErr != nil {
		t.Fatalf("CancelTask: %d %s (%s)", rpcErr.Code, rpcErr.Message, raw)
	}
	var canceled Task
	if err := json.Unmarshal(result, &canceled); err != nil {
		t.Fatalf("decode CancelTask result: %v", err)
	}
	if canceled.Status.State != TaskStateCanceled {
		t.Fatalf("CancelTask returned state %q want %q", canceled.Status.State, TaskStateCanceled)
	}

	// The turn must actually stop: the provider reports the cancellation.
	select {
	case <-prov.cancelled:
	case <-time.After(5 * time.Second):
		t.Fatal("the running turn was not interrupted by CancelTask")
	}

	// The in-flight SendMessage must not overwrite the cancellation with a
	// completed task.
	if body := <-response; body != nil {
		var envelope struct {
			Result struct {
				Task *Task `json:"task"`
			} `json:"result"`
		}
		if err := json.Unmarshal(body, &envelope); err == nil && envelope.Result.Task != nil {
			if envelope.Result.Task.Status.State == TaskStateCompleted {
				t.Fatalf("the canceled task was reported as completed: %s", body)
			}
		}
	}
}

// TestCancelTaskIsIdempotent pins that cancelling an already-canceled task
// returns it again rather than erroring, which is what the spec's idempotency
// rule for CancelTask requires while the task is still retained.
func TestCancelTaskIsIdempotent(t *testing.T) {
	prov := newGateProvider()
	srv, h := newTaskServer(t, config.DefaultConfig(), newTestLoop(t, prov))
	client := srv.Client()

	id, response := beginTask(t, h, client, srv.URL, prov)
	params := fmt.Sprintf(`{"id":%q}`, id)

	if _, rpcErr, _ := rpcResult(t, client, srv.URL, "CancelTask", params); rpcErr != nil {
		t.Fatalf("first CancelTask: %d %s", rpcErr.Code, rpcErr.Message)
	}
	<-response

	result, rpcErr, _ := rpcResult(t, client, srv.URL, "CancelTask", params)
	if rpcErr != nil {
		t.Fatalf("second CancelTask: %d %s", rpcErr.Code, rpcErr.Message)
	}
	var task Task
	if err := json.Unmarshal(result, &task); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if task.Status.State != TaskStateCanceled {
		t.Errorf("state=%q want %q", task.Status.State, TaskStateCanceled)
	}
}

// TestCanceledTaskIsTerminal pins that CANCELED counts as a terminal state for
// the retention policy. Treating only COMPLETED/FAILED as terminal left canceled
// tasks in the store as if they were still running.
func TestCanceledTaskIsTerminal(t *testing.T) {
	for _, state := range []string{TaskStateCompleted, TaskStateFailed, TaskStateCanceled, TaskStateRejected} {
		if !(Task{Status: TaskStatus{State: state}}).terminal() {
			t.Errorf("state %q must be terminal", state)
		}
	}
	for _, state := range []string{TaskStateSubmitted, TaskStateWorking, TaskStateInputRequired, TaskStateAuthRequired} {
		if (Task{Status: TaskStatus{State: state}}).terminal() {
			t.Errorf("state %q must not be terminal", state)
		}
	}
}

// TestSendMessageToTerminalTaskIsRefused pins CORE-SEND-002: a task that already
// reached a terminal state cannot be resumed, and the spec reserves
// UnsupportedOperationError for it. Accepting the message silently restarted
// work the peer believed was finished.
func TestSendMessageToTerminalTaskIsRefused(t *testing.T) {
	srv, _ := newTaskServer(t, config.DefaultConfig(), nil)
	client := srv.Client()

	created := sendMessage(t, client, srv.URL, "seed")
	if created.Status.State != TaskStateCompleted {
		t.Fatalf("precondition: task state=%q want terminal", created.Status.State)
	}

	_, rpcErr, raw := rpcResult(t, client, srv.URL, "SendMessage",
		fmt.Sprintf(`{"message":{"messageId":"m2","role":"ROLE_USER","taskId":%q,"parts":[{"text":"again"}]}}`, created.ID))
	if rpcErr == nil {
		t.Fatalf("SendMessage to a terminal task succeeded, want UnsupportedOperationError: %s", raw)
	}
	if rpcErr.Code != CodeUnsupportedOperationError {
		t.Errorf("code=%d want %d", rpcErr.Code, CodeUnsupportedOperationError)
	}
}

// TestSendMessageRejectsMismatchingContext pins CORE-MULTI-006: a message that
// names a task and a contextId the task does not belong to is contradictory, and
// accepting it would file the turn under the wrong conversation.
func TestSendMessageRejectsMismatchingContext(t *testing.T) {
	prov := newGateProvider()
	srv, h := newTaskServer(t, config.DefaultConfig(), newTestLoop(t, prov))
	client := srv.Client()

	id, response := beginTask(t, h, client, srv.URL, prov)
	running, ok := h.loadTask(id)
	if !ok {
		t.Fatal("the in-flight task was not published")
	}

	_, rpcErr, raw := rpcResult(t, client, srv.URL, "SendMessage",
		fmt.Sprintf(`{"message":{"messageId":"m3","role":"ROLE_USER","taskId":%q,"contextId":"ctx-wrong","parts":[{"text":"hi"}]}}`, id))
	if rpcErr == nil {
		t.Fatalf("SendMessage with a mismatching contextId succeeded, want an error: %s", raw)
	}
	if rpcErr.Code != CodeInvalidParamsError {
		t.Errorf("code=%d want %d", rpcErr.Code, CodeInvalidParamsError)
	}

	prov.releaseAll()
	<-response

	// The matching context must still be accepted, so the guard cannot be a blanket
	// refusal of contextId.
	if _, rpcErr, _ := rpcResult(t, client, srv.URL, "SendMessage",
		fmt.Sprintf(`{"message":{"messageId":"m4","role":"ROLE_USER","taskId":%q,"contextId":%q,"parts":[{"text":"hi"}]}}`,
			id, running.ContextID)); rpcErr != nil && rpcErr.Code == CodeInvalidParamsError {
		t.Errorf("SendMessage with the task's own contextId was rejected: %d %s", rpcErr.Code, rpcErr.Message)
	}
}

// TestSendMessageInfersContextFromTask pins CORE-MULTI-005: a follow-up that
// names only the task must inherit that task's context rather than minting a
// second one for the same conversation.
func TestSendMessageInfersContextFromTask(t *testing.T) {
	prov := newGateProvider()
	srv, h := newTaskServer(t, config.DefaultConfig(), newTestLoop(t, prov))
	client := srv.Client()

	id, response := beginTask(t, h, client, srv.URL, prov)
	running, ok := h.loadTask(id)
	if !ok {
		t.Fatal("the in-flight task was not published")
	}

	result, rpcErr, raw := rpcResult(t, client, srv.URL, "SendMessage",
		fmt.Sprintf(`{"message":{"messageId":"m5","role":"ROLE_USER","taskId":%q,"parts":[{"text":"follow-up"}]}}`, id))
	if rpcErr != nil {
		t.Fatalf("follow-up with only a taskId was rejected: %d %s (%s)", rpcErr.Code, rpcErr.Message, raw)
	}

	var envelope struct {
		Task *Task `json:"task"`
	}
	if err := json.Unmarshal(result, &envelope); err != nil || envelope.Task == nil {
		t.Fatalf("decode follow-up response: %v (%s)", err, result)
	}
	if envelope.Task.ContextID != running.ContextID {
		t.Errorf("follow-up contextId=%q want the task's %q", envelope.Task.ContextID, running.ContextID)
	}
	if envelope.Task.ID != id {
		t.Errorf("follow-up created task %q want the named task %q", envelope.Task.ID, id)
	}

	prov.releaseAll()
	<-response
}

// TestAgentCardCacheValidators pins the caching contract on the discovery
// endpoint: the validators must be present and must actually be honoured, so a
// revalidating client gets a bodyless 304 instead of the card.
func TestAgentCardCacheValidators(t *testing.T) {
	srv, _ := newTaskServer(t, config.DefaultConfig(), nil)
	client := srv.Client()
	cardURL := srv.URL + "/.well-known/agent-card.json"

	get := func(headers map[string]string) *http.Response {
		t.Helper()
		req, err := http.NewRequest(http.MethodGet, cardURL, nil)
		if err != nil {
			t.Fatalf("build request: %v", err)
		}
		for k, v := range headers {
			req.Header.Set(k, v)
		}
		resp, err := client.Do(req)
		if err != nil {
			t.Fatalf("GET card: %v", err)
		}
		t.Cleanup(func() { _ = resp.Body.Close() })
		return resp
	}

	resp := get(nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status=%d want 200", resp.StatusCode)
	}
	etag := resp.Header.Get("ETag")
	if etag == "" {
		t.Error("ETag is required for a cacheable card")
	}
	if cc := resp.Header.Get("Cache-Control"); !strings.Contains(cc, "max-age") {
		t.Errorf("Cache-Control=%q want a max-age directive", cc)
	}
	lastModified := resp.Header.Get("Last-Modified")
	if lastModified == "" {
		t.Fatal("Last-Modified must be present")
	}
	if _, err := http.ParseTime(lastModified); err != nil {
		t.Errorf("Last-Modified=%q is not a valid HTTP date: %v", lastModified, err)
	}

	if resp := get(map[string]string{"If-None-Match": etag}); resp.StatusCode != http.StatusNotModified {
		t.Errorf("If-None-Match with the current ETag: status=%d want 304", resp.StatusCode)
	}
	if resp := get(map[string]string{"If-None-Match": `"stale"`}); resp.StatusCode != http.StatusOK {
		t.Errorf("If-None-Match with a stale ETag: status=%d want 200", resp.StatusCode)
	}
	if resp := get(map[string]string{"If-None-Match": "W/" + etag}); resp.StatusCode != http.StatusNotModified {
		t.Errorf("a weak If-None-Match must still match: status=%d want 304", resp.StatusCode)
	}
	if resp := get(map[string]string{"If-Modified-Since": lastModified}); resp.StatusCode != http.StatusNotModified {
		t.Errorf("If-Modified-Since with the current value: status=%d want 304", resp.StatusCode)
	}
	if resp := get(map[string]string{"If-Modified-Since": "Thu, 01 Jan 1970 00:00:00 GMT"}); resp.StatusCode != http.StatusOK {
		t.Errorf("If-Modified-Since far in the past: status=%d want 200", resp.StatusCode)
	}
}

// TestAgentCardMethodHandling pins GET/HEAD acceptance and rejection of anything
// that could mutate the card.
func TestAgentCardMethodHandling(t *testing.T) {
	srv, _ := newTaskServer(t, config.DefaultConfig(), nil)

	head, err := srv.Client().Head(srv.URL + "/.well-known/agent-card.json")
	if err != nil {
		t.Fatalf("HEAD card: %v", err)
	}
	defer func() { _ = head.Body.Close() }()
	if head.StatusCode != http.StatusOK {
		t.Errorf("HEAD status=%d want 200", head.StatusCode)
	}

	for _, method := range []string{http.MethodPost, http.MethodPut, http.MethodDelete} {
		req, err := http.NewRequest(method, srv.URL+"/.well-known/agent-card.json", nil)
		if err != nil {
			t.Fatalf("build %s: %v", method, err)
		}
		resp, err := srv.Client().Do(req)
		if err != nil {
			t.Fatalf("%s card: %v", method, err)
		}
		_ = resp.Body.Close()
		if resp.StatusCode != http.StatusMethodNotAllowed {
			t.Errorf("%s status=%d want 405", method, resp.StatusCode)
		}
	}
}

// TestTrailingSlashEndpointMatchesTheCanonicalOne pins that the extra /a2a/
// route is the same handler, not a second, weaker one.
func TestTrailingSlashEndpointMatchesTheCanonicalOne(t *testing.T) {
	srv, _ := newTaskServer(t, config.DefaultConfig(), nil)

	body := `{"jsonrpc":"2.0","id":"1","method":"ListTasks","params":{}}`
	for _, path := range []string{"/a2a", "/a2a/"} {
		resp, err := srv.Client().Post(srv.URL+path, "application/json", strings.NewReader(body))
		if err != nil {
			t.Fatalf("POST %s: %v", path, err)
		}
		raw, _ := io.ReadAll(resp.Body)
		_ = resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			t.Errorf("POST %s: status=%d want 200 (%s)", path, resp.StatusCode, raw)
		}
		if !strings.Contains(string(raw), `"tasks"`) {
			t.Errorf("POST %s did not reach the JSON-RPC handler: %s", path, raw)
		}
	}

	// Anything below /a2a/ is not an endpoint.
	resp, err := srv.Client().Post(srv.URL+"/a2a/deeper", "application/json", strings.NewReader(body))
	if err != nil {
		t.Fatalf("POST /a2a/deeper: %v", err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("POST /a2a/deeper: status=%d want 404", resp.StatusCode)
	}
}
