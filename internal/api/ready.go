package api

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"time"

	"github.com/adrianolimagarcia/nanobot-go/internal/bus"
)

// Readiness is deliberately separate from liveness.
//
// /health answers "is the process up?", which is what a supervisor restarting a
// wedged process needs. /readyz answers "may this process receive traffic?",
// which is what a reverse proxy, a systemd unit or a container orchestrator
// needs before it routes a request here. Without that distinction an agent that
// is still assembling its runtime answers 200 and is handed traffic it cannot
// serve.
//
// What /readyz checks (Server.readiness):
//
//   - config:    a configuration is loaded. The gateway dereferences it on every
//     protected route.
//   - provider:  a model provider is attached. Without one
//     /v1/chat/completions answers 503 "No LLM provider configured".
//   - agentLoop: the agent loop is attached. Without one /api/agent/turn answers
//     503 and the A2A endpoint degrades to a stub that never calls a model.
//   - process:   the process owner has not taken the gateway out of rotation
//     through SetReady.
//   - bus:       the message bus is open AND something is consuming it. A closed
//     bus rejects every publish, and a bus nobody drains fills up and then
//     rejects them, so either one means channel messages cannot reach the agent.
//     See busState for what the check can and cannot prove.
//   - dataDir:   the directory the runtime writes under (the session store's
//     root lives inside it) accepts a REAL write, proven by creating and
//     removing a file rather than by stat-ing the path.
//
// Every check is bounded by readinessCheckTimeout and runs in its own goroutine,
// so a probe always answers: a check that does not finish in time is reported as
// a failure of that check instead of hanging the probe.
//
// What it does NOT check, and why:
//
//   - the session store itself: the api package holds no handle to it. The store
//     belongs to the runtime (cmd/haosbot/runtime.go builds a *session.Store and
//     adapts it for the loop), and nothing in this package can reach it without
//     inventing a second seam. Its dominant failure mode — a data directory that
//     cannot be written — is covered by the dataDir check, which probes the
//     parent of the store's root.
//   - queue depth: a non-empty queue is what a BUSY server looks like, and a
//     probe that went unhealthy under load would cause the outage it exists to
//     prevent. A full queue is also self-limiting (PublishInbound returns
//     ErrFull) rather than a state that needs a restart, so depth is deliberately
//     not a gate.
//   - whether channels are connected, whether MCP servers are reachable, and
//     whether the configured model provider is reachable: none of those are
//     observable here. SetReady is the seam for them: a runtime that must gate on
//     them calls SetReady(false) until they are up. The reference states the same
//     limitation for its own health endpoint
//     (docs/multiple-instances.md:123: "Readiness currently reflects only the
//     WebSocket channel").
type readinessReport struct {
	Status  string            `json:"status"`
	Runtime string            `json:"runtime"`
	Ready   bool              `json:"ready"`
	Checks  map[string]string `json:"checks"`
	Reasons []string          `json:"reasons,omitempty"`
}

const (
	readinessOK       = "ok"
	readinessDegraded = "degraded"
)

// Check states reported in the body. The gateway has no handle on the thing it
// was asked about ("missing"), the thing is in a state that cannot serve
// ("closed", "no_consumer", "unwritable", "not_ready"), or the check did not
// answer in time ("timeout").
const (
	readinessMissing    = "missing"
	readinessClosed     = "closed"
	readinessNoConsumer = "no_consumer"
	readinessUnwritable = "unwritable"
	readinessNotReady   = "not_ready"
	readinessTimeout    = "timeout"
)

// readinessCheckTimeout bounds ONE check.
//
// A probe is polled by a load balancer, so it has to answer quickly even when
// the thing it probes does not: each check runs in its own goroutine and is
// abandoned when the bound expires, which is reported as a failing check rather
// than as a hang. A local filesystem answers the write probe in microseconds, so
// the bound only matters for a hung mount.
//
// The bound cannot cancel a syscall already stuck in the kernel — a hung network
// mount ignores the goroutine blocked on it — so a probe against a dead
// filesystem leaves one goroutine behind until the call returns. That is the
// deliberate trade: the alternative is a probe that never answers, which turns a
// stuck filesystem into an outage nothing can detect.
const readinessCheckTimeout = 500 * time.Millisecond

// registerReady registers the readiness probe next to the liveness endpoint.
//
// Like /health it is deliberately outside the protected prefixes in security.go:
// a load balancer or an orchestrator probe has no bearer token, and the report
// exposes only coarse lifecycle state — no configuration, no secrets, no
// session data. Mutation and model-execution surfaces stay authenticated.
func (s *Server) registerReady(mux *http.ServeMux) {
	mux.HandleFunc("/readyz", s.handleReadyz)
}

func (s *Server) handleReadyz(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		w.Header().Set("Allow", "GET, HEAD")
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	report := s.readiness()
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	// 503 matches the convention the reference uses for its own readiness
	// endpoint (docs/multiple-instances.md:114-118) and the status the gateway
	// already returns for its own degraded surfaces.
	if !report.Ready {
		w.WriteHeader(http.StatusServiceUnavailable)
	} else {
		w.WriteHeader(http.StatusOK)
	}
	_ = json.NewEncoder(w).Encode(report)
}

// SetReady is the process-owned readiness gate. The api package cannot see the
// parts of startup that live above it (channel connections, MCP handshakes), so
// a runtime that gates on them takes the gateway out of rotation with
// SetReady(false) and puts it back with SetReady(true). The zero value means "no
// override", so a gateway that never calls it is governed by the built-in checks
// alone.
func (s *Server) SetReady(ready bool) {
	s.notReady.Store(!ready)
}

// Ready reports whether /readyz would answer 200 right now.
func (s *Server) Ready() bool {
	return s.readiness().Ready
}

func (s *Server) readiness() readinessReport {
	checks := map[string]string{
		"config":    readinessOK,
		"provider":  readinessOK,
		"agentLoop": readinessOK,
		"process":   readinessOK,
		"bus":       readinessOK,
		"dataDir":   readinessOK,
	}

	var reasons []string
	// fail names the failing check in the body twice over: as its state in
	// "checks" and as the prefix of its reason, so a probe that only logs the
	// reason still says which gate closed.
	fail := func(check, state, reason string) {
		checks[check] = state
		reasons = append(reasons, check+": "+reason)
	}

	if s == nil || s.cfg == nil {
		fail("config", readinessMissing, "no configuration loaded")
	}
	if s == nil || s.provider == nil {
		fail("provider", readinessMissing, "no model provider attached")
	}
	if s == nil || s.loop.Load() == nil {
		fail("agentLoop", readinessMissing, "agent loop not attached")
	}
	if s != nil && s.notReady.Load() {
		fail("process", readinessNotReady, "gateway taken out of rotation by the process owner (SetReady)")
	}

	if state, err := s.busState(); err != nil {
		fail("bus", state, err.Error())
	}
	if state, err := s.dataDirState(); err != nil {
		fail("dataDir", state, err.Error())
	}

	status := readinessOK
	if len(reasons) > 0 {
		status = readinessDegraded
	}
	return readinessReport{
		Status:  status,
		Runtime: "haosbot",
		Ready:   len(reasons) == 0,
		Checks:  checks,
		Reasons: reasons,
	}
}

// busState reports whether the message bus can still carry work to the agent.
func (s *Server) busState() (string, error) {
	var b *bus.Bus
	if s != nil {
		b = s.bus.Load()
	}
	if b == nil {
		return readinessMissing, errors.New("no message bus attached (SetBus)")
	}

	return boundedCheck(func() (string, error) {
		// Closed is read first: a closed bus ends Loop.Run, so a consumer count
		// taken after it would be meaningless.
		if b.Closed() {
			return readinessClosed, errors.New("the bus is closed: no channel message can reach the agent")
		}
		if !b.HasConsumer() {
			return readinessNoConsumer, errors.New("nothing is consuming the bus: the agent loop is not draining it")
		}
		return readinessOK, nil
	})
}

// dataDirState reports whether the runtime's data directory accepts a write.
func (s *Server) dataDirState() (string, error) {
	var dir string
	if s != nil {
		if p := s.dataDir.Load(); p != nil {
			dir = *p
		}
	}
	if dir == "" {
		return readinessMissing, errors.New("no data directory recorded (SetDataDir)")
	}

	return boundedCheck(func() (string, error) {
		if err := probeDirWritable(dir); err != nil {
			return readinessUnwritable, fmt.Errorf("data directory %s is not writable: %w", dir, err)
		}
		return readinessOK, nil
	})
}

// probeDirWritable proves a directory accepts a real write.
//
// Deliberately NOT os.Stat: a stat succeeds on a read-only mount, on a directory
// owned by another user, and on a full filesystem — precisely the states that
// stop the session store from saving — so a stat-based check would report "ok"
// for a gateway that cannot persist anything. Creating, writing, closing and
// removing a file is the cheapest operation that actually proves the property.
//
// The file is removed on every path, so a probe leaves nothing behind. There is
// no fsync: a probe must not wait for the disk, and durability is not what it is
// asking about.
func probeDirWritable(dir string) error {
	f, err := os.CreateTemp(dir, ".haosbot-readyz-*")
	if err != nil {
		return err
	}
	name := f.Name()
	defer func() { _ = os.Remove(name) }()

	if _, err := f.Write([]byte("readyz")); err != nil {
		_ = f.Close()
		return err
	}
	return f.Close()
}

// boundedCheck runs one readiness check under readinessCheckTimeout. The result
// channel is buffered, so a check that finishes after the deadline does not
// block its goroutine forever.
func boundedCheck(fn func() (string, error)) (string, error) {
	type result struct {
		state string
		err   error
	}

	done := make(chan result, 1)
	go func() {
		state, err := fn()
		done <- result{state: state, err: err}
	}()

	select {
	case r := <-done:
		return r.state, r.err
	case <-time.After(readinessCheckTimeout):
		return readinessTimeout, fmt.Errorf("check did not answer within %s", readinessCheckTimeout)
	}
}
