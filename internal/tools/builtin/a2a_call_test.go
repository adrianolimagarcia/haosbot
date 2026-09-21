package builtin

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// a2aTestPolicy allows the loopback address httptest binds to.
func a2aTestPolicy() []string { return []string{"127.0.0.1"} }

func a2aTool() *A2ACallTool { return NewA2ACall(a2aTestPolicy()) }

func runA2A(t *testing.T, tool *A2ACallTool, args string) string {
	t.Helper()
	res, err := tool.Execute(context.Background(), json.RawMessage(args))
	if err != nil {
		t.Fatalf("Execute(%s): %v", args, err)
	}
	return res.Content
}

// TestA2ADiscoverReportsV1Interfaces pins that discovery reads the v1.0 card
// shape. The card's address moved from a single top-level `url` to an ordered
// supportedInterfaces list, and the old client only looked at `url`.
func TestA2ADiscoverReportsV1Interfaces(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/.well-known/agent-card.json" {
			t.Errorf("discovery requested %q want the well-known card path", r.URL.Path)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{
			"name":"remote","description":"a remote agent","version":"1.0.0",
			"supportedInterfaces":[{"url":"http://example.test/a2a","protocolBinding":"JSONRPC","protocolVersion":"1.0"}],
			"skills":[{"id":"s1","name":"Skill One","description":"d","tags":["t"]}]
		}`)
	}))
	defer srv.Close()

	got := runA2A(t, a2aTool(), `{"action":"discover","agent_url":"`+srv.URL+`"}`)
	for _, want := range []string{"remote", "a remote agent", "http://example.test/a2a", "JSONRPC", "s1"} {
		if !strings.Contains(got, want) {
			t.Errorf("discovery output is missing %q:\n%s", want, got)
		}
	}
}

// TestA2ASendUsesTheV1SendMessageShape pins the request the client puts on the
// wire. The old client sent {"method":"tasks/send","params":{"message":{"text":…}}},
// a method name that appears in neither the v1.0 nor the v0.3 dispatch table, so
// no conformant agent would accept it.
func TestA2ASendUsesTheV1SendMessageShape(t *testing.T) {
	var captured map[string]any
	var path string

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/.well-known/agent-card.json" {
			// A card that advertises no interface: the client must still reach the
			// conventional /a2a path rather than posting to the bare base URL.
			w.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(w, `{"name":"remote"}`)
			return
		}

		path = r.URL.Path
		body, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(body, &captured)

		if r.Header.Get("A2A-Version") != a2aProtocolVersion {
			t.Errorf("A2A-Version=%q want %q", r.Header.Get("A2A-Version"), a2aProtocolVersion)
		}

		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"jsonrpc":"2.0","id":1,"result":{"task":{
			"id":"task-1","contextId":"ctx-1",
			"status":{"state":"TASK_STATE_COMPLETED","timestamp":"2026-01-01T00:00:00Z"},
			"artifacts":[{"artifactId":"a1","parts":[{"text":"remote answer"}]}]
		}}}`)
	}))
	defer srv.Close()

	got := runA2A(t, a2aTool(), `{"action":"send","agent_url":"`+srv.URL+`","task_message":"do the thing"}`)

	if captured["method"] != "SendMessage" {
		t.Errorf("method=%v want SendMessage", captured["method"])
	}
	params, _ := captured["params"].(map[string]any)
	message, _ := params["message"].(map[string]any)
	if message == nil {
		t.Fatalf("params.message is missing: %#v", captured)
	}
	if message["role"] != "ROLE_USER" {
		t.Errorf("message.role=%v want ROLE_USER", message["role"])
	}
	if id, _ := message["messageId"].(string); id == "" {
		t.Error("message.messageId is required")
	}
	parts, _ := message["parts"].([]any)
	if len(parts) != 1 {
		t.Fatalf("message.parts=%#v want exactly one part", message["parts"])
	}
	part, _ := parts[0].(map[string]any)
	if part["text"] != "do the thing" {
		t.Errorf("part.text=%v want the task message", part["text"])
	}
	if _, hasKind := part["kind"]; hasKind {
		t.Error("a v1.0 Part must not carry the removed v0.3 kind discriminator")
	}

	if path != "/a2a" {
		t.Errorf("posted to %q want /a2a", path)
	}
	if !strings.Contains(got, "remote answer") {
		t.Errorf("the artifact text was not surfaced:\n%s", got)
	}
	if !strings.Contains(got, "TASK_STATE_COMPLETED") {
		t.Errorf("the task state was not surfaced:\n%s", got)
	}
}

// TestA2ASendUsesTheInterfaceURLFromTheCard pins that the client posts to the
// address the peer advertises rather than guessing "<base>/a2a".
func TestA2ASendUsesTheInterfaceURLFromTheCard(t *testing.T) {
	var mux http.ServeMux
	var posted string

	mux.HandleFunc("/.well-known/agent-card.json", func(w http.ResponseWriter, r *http.Request) {
		base := "http://" + r.Host
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"name":"remote","supportedInterfaces":[
			{"url":"`+base+`/custom/rpc","protocolBinding":"JSONRPC","protocolVersion":"1.0"}]}`)
	})
	mux.HandleFunc("/custom/rpc", func(w http.ResponseWriter, r *http.Request) {
		posted = r.URL.Path
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"jsonrpc":"2.0","id":1,"result":{"message":{
			"messageId":"m","role":"ROLE_AGENT","parts":[{"text":"direct reply"}]}}}`)
	})

	srv := httptest.NewServer(&mux)
	defer srv.Close()

	got := runA2A(t, a2aTool(), `{"action":"send","agent_url":"`+srv.URL+`","task_message":"hi"}`)
	if posted != "/custom/rpc" {
		t.Errorf("posted to %q want the advertised /custom/rpc", posted)
	}
	if !strings.Contains(got, "direct reply") {
		t.Errorf("a Message reply was not surfaced:\n%s", got)
	}
}

// TestA2ASendFallsBackToTheV03MethodName pins the overlap-period tolerance: a
// peer that predates A2A 1.0 answers MethodNotFound, and the client retries with
// the v0.3 method name and its kind discriminator.
func TestA2ASendFallsBackToTheV03MethodName(t *testing.T) {
	var methods []string
	var legacyPart map[string]any

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/.well-known/agent-card.json" {
			w.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(w, `{"name":"legacy peer"}`)
			return
		}

		body, _ := io.ReadAll(r.Body)
		var req map[string]any
		_ = json.Unmarshal(body, &req)
		method, _ := req["method"].(string)
		methods = append(methods, method)

		w.Header().Set("Content-Type", "application/json")
		if method == "SendMessage" {
			_, _ = io.WriteString(w, `{"jsonrpc":"2.0","id":1,"error":{"code":-32601,"message":"Method not found"}}`)
			return
		}
		params, _ := req["params"].(map[string]any)
		message, _ := params["message"].(map[string]any)
		parts, _ := message["parts"].([]any)
		if len(parts) > 0 {
			legacyPart, _ = parts[0].(map[string]any)
		}
		_, _ = io.WriteString(w, `{"jsonrpc":"2.0","id":1,"result":{"id":"t1","status":"completed","output":"legacy answer"}}`)
	}))
	defer srv.Close()

	got := runA2A(t, a2aTool(), `{"action":"send","agent_url":"`+srv.URL+`","task_message":"hi"}`)

	if len(methods) != 2 || methods[0] != "SendMessage" || methods[1] != "message/send" {
		t.Fatalf("methods=%v want [SendMessage message/send]", methods)
	}
	if legacyPart == nil || legacyPart["kind"] != "text" {
		t.Errorf("the v0.3 retry must send a kind-discriminated part, got %#v", legacyPart)
	}
	if !strings.Contains(got, "legacy answer") {
		t.Errorf("a pre-1.0 flat response was not surfaced:\n%s", got)
	}
}

// TestA2ASendSurfacesRPCErrors keeps a peer's error visible instead of reporting
// an empty success.
func TestA2ASendSurfacesRPCErrors(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"jsonrpc":"2.0","id":1,"error":{"code":-32001,"message":"Task not found"}}`)
	}))
	defer srv.Close()

	got := runA2A(t, a2aTool(), `{"action":"send","agent_url":"`+srv.URL+`","task_message":"hi"}`)
	if !strings.Contains(got, "-32001") || !strings.Contains(got, "Task not found") {
		t.Errorf("the RPC error was not surfaced:\n%s", got)
	}
}

// TestA2ARejectsBlockedDestinations pins that the tool keeps honouring the
// outbound policy: loopback is denied unless explicitly allowlisted.
func TestA2ARejectsBlockedDestinations(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Error("a blocked destination must never be contacted")
	}))
	defer srv.Close()

	// No allowlist: the default policy denies loopback.
	got := runA2A(t, NewA2ACall(), `{"action":"send","agent_url":"`+srv.URL+`","task_message":"hi"}`)
	if !strings.Contains(got, "policy") {
		t.Errorf("a blocked destination was not refused by policy:\n%s", got)
	}
}

// TestA2AArgumentValidation pins the two argument errors the tool reports
// instead of performing a request.
func TestA2AArgumentValidation(t *testing.T) {
	tool := a2aTool()

	if got := runA2A(t, tool, `{"action":"send","agent_url":"http://example.test","task_message":"  "}`); !strings.Contains(got, "task_message is required") {
		t.Errorf("blank task_message: %s", got)
	}
	if got := runA2A(t, tool, `{"action":"nope","agent_url":"http://example.test"}`); !strings.Contains(got, "unknown action") {
		t.Errorf("unknown action: %s", got)
	}
	if got := runA2A(t, tool, `{"action":"send","agent_url":"","task_message":"x"}`); !strings.Contains(got, "agent_url is required") {
		t.Errorf("blank agent_url: %s", got)
	}
}
