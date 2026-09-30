package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/adrianolimagarcia/nanobot-go/internal/memoryfabric"
)

type fakeMemoryAdmin struct {
	snapshot memoryfabric.AdminSnapshot
	retried bool
	rebuilt string
	pruned bool
	vacuumed bool
}

func (f *fakeMemoryAdmin) Snapshot(context.Context, int) (memoryfabric.AdminSnapshot, error) { return f.snapshot, nil }
func (f *fakeMemoryAdmin) RetryDead(context.Context, string, string) error { f.retried = true; return nil }
func (f *fakeMemoryAdmin) Rebuild(_ context.Context, projection string) error { f.rebuilt = projection; return nil }
func (f *fakeMemoryAdmin) Prune(context.Context, time.Time, *memoryfabric.Namespace, int) (int64, error) { f.pruned = true; return 3, nil }
func (f *fakeMemoryAdmin) Vacuum(context.Context) error { f.vacuumed = true; return nil }

func TestMemoryAdminAPIGetAndActions(t *testing.T) {
	admin := &fakeMemoryAdmin{snapshot: memoryfabric.AdminSnapshot{
		Stats: memoryfabric.Stats{Pending: 2, Dead: 1},
		Namespaces: []memoryfabric.NamespaceStats{{Scope: memoryfabric.ScopeProject, Owner: "p", Records: 4, Bytes: 128}},
	}}
	s := &Server{memoryAdmin: admin}

	getReq := httptest.NewRequest(http.MethodGet, "/api/webui/memory/admin", nil)
	getRec := httptest.NewRecorder()
	s.handleWebUIMemoryAdmin(getRec, getReq)
	if getRec.Code != http.StatusOK { t.Fatalf("GET status=%d body=%s", getRec.Code, getRec.Body.String()) }
	var snap memoryfabric.AdminSnapshot
	if err := json.Unmarshal(getRec.Body.Bytes(), &snap); err != nil { t.Fatal(err) }
	if snap.Stats.Pending != 2 || len(snap.Namespaces) != 1 { t.Fatalf("snapshot=%+v", snap) }

	cases := []struct{
		name string
		body string
		check func() bool
	}{
		{"retry", `{"action":"retry_dead","projection":"graph","job_id":"j1"}`, func() bool { return admin.retried }},
		{"rebuild", `{"action":"rebuild","projection":"graph"}`, func() bool { return admin.rebuilt == "graph" }},
		{"prune", `{"action":"prune","before_days":30,"scope":"global","max_records":10}`, func() bool { return admin.pruned }},
		{"vacuum", `{"action":"vacuum"}`, func() bool { return admin.vacuumed }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodPost, "/api/webui/memory/admin", strings.NewReader(tc.body))
			rec := httptest.NewRecorder()
			s.handleWebUIMemoryAdmin(rec, req)
			if rec.Code != http.StatusOK { t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String()) }
			if !tc.check() { t.Fatalf("action %s not observed", tc.name) }
		})
	}
}
