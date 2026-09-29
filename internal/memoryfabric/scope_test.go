package memoryfabric

import (
	"context"
	"database/sql"
	"path/filepath"
	"testing"
	"time"
)

func TestAppendTurnScopedKeepsNamespaceIdentity(t *testing.T) {
	project := Namespace{Scope: ScopeProject, Owner: "project-a"}
	s, err := Open(context.Background(), Config{
		Path: filepath.Join(t.TempDir(), "memory-fabric.db"),
		DefaultNamespace: project,
		MaxPending: 16, MaxPendingBytes: 1024 * 1024,
		MaxContentBytes: 1024, MaxDiskBytes: 50 * 1024 * 1024,
	})
	if err != nil { t.Fatal(err) }
	defer s.Close()

	if err := s.AppendTurn(context.Background(), "project-turn", "session", "project memory"); err != nil {
		t.Fatal(err)
	}
	private := Namespace{Scope: ScopePrivate, Owner: "project-a:agent:coder"}
	if err := s.AppendTurnScoped(context.Background(), "private-turn", "session", private, "private memory"); err != nil {
		t.Fatal(err)
	}

	seen := map[string]Job{}
	for i := 0; i < 2; i++ {
		job, ok, err := s.Claim(context.Background(), ProjectionGraph)
		if err != nil || !ok { t.Fatalf("claim %d: ok=%v err=%v", i, ok, err) }
		seen[job.ID] = job
		if err := s.Ack(context.Background(), ProjectionGraph, job.ID); err != nil { t.Fatal(err) }
	}
	if got := seen["project-turn"]; got.Scope != ScopeProject || got.Owner != "project-a" {
		t.Fatalf("project job namespace = %+v", got)
	}
	if got := seen["private-turn"]; got.Scope != ScopePrivate || got.Owner != private.Owner {
		t.Fatalf("private job namespace = %+v", got)
	}
	if err := s.AppendTurnScoped(context.Background(), "project-turn", "session", private, "project memory"); err == nil {
		t.Fatal("expected record identity conflict across namespaces")
	}
}

func TestOpenMigratesLegacyRecordsIntoDefaultProjectNamespace(t *testing.T) {
	path := filepath.Join(t.TempDir(), "memory-fabric.db")
	db, err := sql.Open("sqlite3", path)
	if err != nil { t.Fatal(err) }
	schema := "CREATE TABLE memory_records (" +
		"record_id TEXT PRIMARY KEY," +
		"session_key TEXT NOT NULL," +
		"content TEXT NOT NULL," +
		"content_hash BLOB NOT NULL," +
		"created_at INTEGER NOT NULL" +
		");" +
		"INSERT INTO memory_records(record_id,session_key,content,content_hash,created_at)" +
		" VALUES('legacy','session','hello',X'00',1);"
	if _, err = db.Exec(schema); err != nil { _ = db.Close(); t.Fatal(err) }
	if err := db.Close(); err != nil { t.Fatal(err) }

	ns := Namespace{Scope: ScopeProject, Owner: "workspace-123"}
	s, err := Open(context.Background(), Config{
		Path: path, DefaultNamespace: ns,
		MaxPending: 16, MaxPendingBytes: 1024 * 1024,
		MaxContentBytes: 1024, MaxDiskBytes: 50 * 1024 * 1024,
		Lease: time.Second,
	})
	if err != nil { t.Fatal(err) }
	defer s.Close()

	var scope, owner string
	if err := s.db.QueryRow("SELECT memory_scope,memory_owner FROM memory_records WHERE record_id='legacy'").Scan(&scope, &owner); err != nil {
		t.Fatal(err)
	}
	if scope != ScopeProject || owner != ns.Owner {
		t.Fatalf("legacy namespace = %s/%s, want %s/%s", scope, owner, ns.Scope, ns.Owner)
	}
}

func TestNormalizeNamespaceRejectsUnknownOrOwnerlessScopedMemory(t *testing.T) {
	if _, err := NormalizeNamespace(Namespace{Scope: "unknown", Owner: "x"}); err == nil {
		t.Fatal("expected invalid scope error")
	}
	if _, err := NormalizeNamespace(Namespace{Scope: ScopeTeam}); err == nil {
		t.Fatal("expected owner requirement for team scope")
	}
	global, err := NormalizeNamespace(Namespace{Scope: ScopeGlobal})
	if err != nil || global.Owner != ScopeGlobal {
		t.Fatalf("global namespace = %+v err=%v", global, err)
	}
}
