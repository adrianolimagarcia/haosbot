// Package observability provides allocation-light runtime diagnostics.
//
// Counters are atomics and are updated on hot paths without locks or background
// exporters. A JSON snapshot is rendered only when /metrics is requested.
package observability

import (
	"encoding/json"
	"net/http"
	"runtime"
	"sync/atomic"
	"time"
)

type Registry struct {
	started time.Time
	vectorEnabled atomic.Bool
	embedderLoaded atomic.Bool
	turns atomic.Uint64
	turnErrors atomic.Uint64
	providerTimeouts atomic.Uint64
	toolCalls atomic.Uint64
	toolErrors atomic.Uint64
	enqueueAccepted atomic.Uint64
	enqueueDeduplicated atomic.Uint64
	enqueueRejected atomic.Uint64
	claims atomic.Uint64
	projectionSuccess atomic.Uint64
	projectionRetries atomic.Uint64
	projectionDead atomic.Uint64
	projectionFailures atomic.Uint64
	projectionLatencyNanos atomic.Uint64
	projectionLatencySamples atomic.Uint64
	memoryPending atomic.Int64
	memoryPendingBytes atomic.Int64
	memoryRunning atomic.Int64
	memorySucceeded atomic.Int64
	memoryDead atomic.Int64
	memoryOldestAgeSecs atomic.Int64
}

type Snapshot struct {
	UptimeSeconds float64 `json:"uptime_seconds"`
	Goroutines int `json:"goroutines"`
	HeapAllocBytes uint64 `json:"heap_alloc_bytes"`
	HeapInUseBytes uint64 `json:"heap_inuse_bytes"`
	HeapObjects uint64 `json:"heap_objects"`
	SysBytes uint64 `json:"sys_bytes"`
	VectorEnabled bool `json:"vector_enabled"`
	EmbedderLoaded bool `json:"embedder_loaded"`
	Turns uint64 `json:"turns"`
	TurnErrors uint64 `json:"turn_errors"`
	ProviderTimeouts uint64 `json:"provider_timeouts"`
	ToolCalls uint64 `json:"tool_calls"`
	ToolErrors uint64 `json:"tool_errors"`
	EnqueueAccepted uint64 `json:"memory_enqueue_accepted"`
	EnqueueDeduplicated uint64 `json:"memory_enqueue_deduplicated"`
	EnqueueRejected uint64 `json:"memory_enqueue_rejected"`
	Claims uint64 `json:"projection_claims"`
	ProjectionSuccess uint64 `json:"projection_success"`
	ProjectionRetries uint64 `json:"projection_retries"`
	ProjectionDead uint64 `json:"projection_dead"`
	ProjectionFailures uint64 `json:"projection_failures"`
	ProjectionLatencyAvgMs float64 `json:"projection_latency_avg_ms"`
	MemoryPending int64 `json:"memory_pending"`
	MemoryPendingBytes int64 `json:"memory_pending_bytes"`
	MemoryRunning int64 `json:"memory_running"`
	MemorySucceeded int64 `json:"memory_succeeded"`
	MemoryDead int64 `json:"memory_dead"`
	MemoryOldestAgeSecs int64 `json:"memory_oldest_age_seconds"`
}

func New() *Registry { return &Registry{started: time.Now()} }

func (r *Registry) SetVectorEnabled(v bool) { r.vectorEnabled.Store(v) }
func (r *Registry) SetEmbedderLoaded(v bool) { r.embedderLoaded.Store(v) }
func (r *Registry) IncTurns() { r.turns.Add(1) }
func (r *Registry) IncTurnErrors() { r.turnErrors.Add(1) }
func (r *Registry) IncProviderTimeouts() { r.providerTimeouts.Add(1) }
func (r *Registry) IncToolCalls() { r.toolCalls.Add(1) }
func (r *Registry) AddToolCalls(n int) { if n > 0 { r.toolCalls.Add(uint64(n)) } }
func (r *Registry) IncToolErrors() { r.toolErrors.Add(1) }
func (r *Registry) IncEnqueueAccepted() { r.enqueueAccepted.Add(1) }
func (r *Registry) IncEnqueueDeduplicated() { r.enqueueDeduplicated.Add(1) }
func (r *Registry) IncEnqueueRejected() { r.enqueueRejected.Add(1) }
func (r *Registry) IncClaims() { r.claims.Add(1) }
func (r *Registry) IncProjectionSuccess(d time.Duration) { r.projectionSuccess.Add(1); r.projectionLatencyNanos.Add(uint64(d)); r.projectionLatencySamples.Add(1) }
func (r *Registry) IncProjectionRetry() { r.projectionRetries.Add(1); r.projectionFailures.Add(1) }
func (r *Registry) IncProjectionDead() { r.projectionDead.Add(1); r.projectionFailures.Add(1) }
func (r *Registry) SetMemoryStats(pending, running, succeeded, dead, oldestAgeSecs, pendingBytes int64) {
	r.memoryPending.Store(pending)
	r.memoryPendingBytes.Store(pendingBytes)
	r.memoryRunning.Store(running)
	r.memorySucceeded.Store(succeeded)
	r.memoryDead.Store(dead)
	r.memoryOldestAgeSecs.Store(oldestAgeSecs)
}

func (r *Registry) Snapshot() Snapshot {
	var mem runtime.MemStats
	runtime.ReadMemStats(&mem)
	samples := r.projectionLatencySamples.Load()
	avg := float64(0)
	if samples > 0 { avg = float64(r.projectionLatencyNanos.Load()) / float64(samples) / float64(time.Millisecond) }
	uptime := time.Since(r.started).Seconds()
	return Snapshot{
		UptimeSeconds: uptime, Goroutines: runtime.NumGoroutine(), HeapAllocBytes: mem.HeapAlloc,
		HeapInUseBytes: mem.HeapInuse, HeapObjects: mem.HeapObjects, SysBytes: mem.Sys,
		VectorEnabled: r.vectorEnabled.Load(), EmbedderLoaded: r.embedderLoaded.Load(),
		Turns: r.turns.Load(), TurnErrors: r.turnErrors.Load(), ProviderTimeouts: r.providerTimeouts.Load(),
		ToolCalls: r.toolCalls.Load(), ToolErrors: r.toolErrors.Load(), EnqueueAccepted: r.enqueueAccepted.Load(),
		EnqueueDeduplicated: r.enqueueDeduplicated.Load(), EnqueueRejected: r.enqueueRejected.Load(),
		Claims: r.claims.Load(), ProjectionSuccess: r.projectionSuccess.Load(), ProjectionRetries: r.projectionRetries.Load(),
		ProjectionDead: r.projectionDead.Load(), ProjectionFailures: r.projectionFailures.Load(), ProjectionLatencyAvgMs: avg,
		MemoryPending: r.memoryPending.Load(), MemoryPendingBytes: r.memoryPendingBytes.Load(), MemoryRunning: r.memoryRunning.Load(), MemorySucceeded: r.memorySucceeded.Load(),
		MemoryDead: r.memoryDead.Load(), MemoryOldestAgeSecs: r.memoryOldestAgeSecs.Load(),
	}
}

func (r *Registry) ServeHTTP(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(r.Snapshot())
}
