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
	r.SetMemoryStats(2, 1, 3, 0, 7, 9)
	if got := r.Snapshot(); !got.VectorEnabled || got.Turns != 1 || got.MemoryPending != 2 || got.MemoryPendingBytes != 9 || got.ProjectionSuccess != 1 {
		t.Fatalf("snapshot=%+v", got)
	}
	w := httptest.NewRecorder()
	r.ServeHTTP(w, httptest.NewRequest("GET", "/metrics", nil))
	if w.Code != 200 || !strings.Contains(w.Body.String(), `"memory_pending":2`) {
		t.Fatalf("status=%d body=%s", w.Code, w.Body.String())
	}
}
