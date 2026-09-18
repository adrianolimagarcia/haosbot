package observability

import (
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestSnapshotAndEndpoint(t *testing.T) {
	r := New()
	r.SetVectorEnabled(true)
	r.SetEmbedderLoaded(true)
	r.IncTurns()
	r.IncProjectionSuccess(4 * time.Millisecond)
	r.ObserveGraphSearch(2 * time.Millisecond)
	r.ObserveProviderTTFT(3 * time.Millisecond)
	r.ObserveProviderTotal(10 * time.Millisecond)
	r.ObserveTools(1 * time.Millisecond)
	r.ObservePersistence(4 * time.Millisecond)
	r.ObserveTurn(20 * time.Millisecond)
	r.SetMemoryStats(2, 1, 3, 0, 7, 9)
	if got := r.Snapshot(); !got.VectorEnabled || got.Turns != 1 || got.MemoryPending != 2 || got.MemoryPendingBytes != 9 || got.ProjectionSuccess != 1 {
		t.Fatalf("snapshot=%+v", got)
	}
	got := r.Snapshot()
	if got.GraphSearchAvgMs != 2 || got.ProviderTTFTAvgMs != 3 || got.ProviderTotalAvgMs != 10 || got.PersistenceAvgMs != 4 {
		t.Fatalf("latency snapshot=%+v", got)
	}
	w := httptest.NewRecorder()
	r.ServeHTTP(w, httptest.NewRequest("GET", "/metrics", nil))
	if w.Code != 200 || !strings.Contains(w.Body.String(), `"memory_pending":2`) {
		t.Fatalf("status=%d body=%s", w.Code, w.Body.String())
	}
}
