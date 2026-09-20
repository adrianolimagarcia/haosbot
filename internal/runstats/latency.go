package runstats

import (
	"sync"
	"time"
)

// TurnLatency records the three user-visible latency phases without coupling
// measurement to a provider implementation. Zero values mean the phase was not
// observed (for example a failed request that never emitted a token).
type TurnLatency struct {
	PreProvider time.Duration `json:"pre_provider"`
	TTFB        time.Duration `json:"ttfb"`
	Total       time.Duration `json:"total"`
}

// LatencyRecorder is safe for callbacks from different goroutines. A clock is
// injected so tests and offline harnesses are deterministic and need no sleeps.
type LatencyRecorder struct {
	mu sync.Mutex
	now func() time.Time
	started time.Time
	provider time.Time
	firstByte time.Time
	finished time.Time
}

func NewLatencyRecorder(now func() time.Time) *LatencyRecorder {
	if now == nil { now = time.Now }
	r := &LatencyRecorder{now: now}
	r.started = now()
	return r
}

func (r *LatencyRecorder) ProviderStart() {
	r.mu.Lock(); defer r.mu.Unlock()
	if r.provider.IsZero() { r.provider = r.now() }
}

func (r *LatencyRecorder) FirstByte() {
	r.mu.Lock(); defer r.mu.Unlock()
	if r.firstByte.IsZero() { r.firstByte = r.now() }
}

func (r *LatencyRecorder) Finish() {
	r.mu.Lock(); defer r.mu.Unlock()
	if r.finished.IsZero() { r.finished = r.now() }
}

func (r *LatencyRecorder) Snapshot() TurnLatency {
	r.mu.Lock(); defer r.mu.Unlock()
	var out TurnLatency
	if !r.provider.IsZero() && !r.started.IsZero() { out.PreProvider = nonNegative(r.provider.Sub(r.started)) }
	if !r.firstByte.IsZero() && !r.started.IsZero() { out.TTFB = nonNegative(r.firstByte.Sub(r.started)) }
	if !r.finished.IsZero() && !r.started.IsZero() { out.Total = nonNegative(r.finished.Sub(r.started)) }
	return out
}

func nonNegative(d time.Duration) time.Duration { if d < 0 { return 0 }; return d }
