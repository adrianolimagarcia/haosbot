package main

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	micrographrag "github.com/adrianolimagarcia/micrographrag-go"
	"github.com/adrianolimagarcia/nanobot-go/internal/memoryfabric"
	"github.com/adrianolimagarcia/nanobot-go/internal/multiagent"
)

func TestScopedGraphStoresArePhysicallyIsolated(t *testing.T) {
	pool := newTestPool(t, 4)
	defer pool.Close()

	project := memoryfabric.Namespace{Scope: memoryfabric.ScopeProject, Owner: "project-a"}
	private := memoryfabric.Namespace{Scope: memoryfabric.ScopePrivate, Owner: "project-a:agent:coder"}
	projectKey, err := graphStoreKey(project)
	if err != nil { t.Fatal(err) }
	privateKey, err := graphStoreKey(private)
	if err != nil { t.Fatal(err) }
	if projectKey == privateKey {
		t.Fatal("project and private namespaces resolved to the same graph key")
	}

	projectStore, releaseProject, err := pool.Acquire(context.Background(), projectKey)
	if err != nil { t.Fatal(err) }
	if _, err := projectStore.AddMemory(context.Background(), micrographrag.MemoryInput{
		Kind: 1, Source: "scope-probe", Title: "project-only", Content: "project secret",
	}); err != nil {
		releaseProject()
		t.Fatal(err)
	}
	releaseProject()

	privateStore, releasePrivate, err := pool.Acquire(context.Background(), privateKey)
	if err != nil { t.Fatal(err) }
	defer releasePrivate()
	var count int
	if err := privateStore.DB().QueryRowContext(context.Background(),
		"SELECT COUNT(*) FROM documents WHERE source=?", "scope-probe").Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 0 {
		t.Fatalf("private store can see %d project documents", count)
	}
}

func TestBoundMemorySearchRejectsCrossScopeSelection(t *testing.T) {
	pool := newTestPool(t, 2)
	defer pool.Close()
	private := memoryfabric.Namespace{Scope: memoryfabric.ScopePrivate, Owner: "project:agent:coder"}
	tool := newScopedMemorySearchTool(pool, []memoryfabric.Namespace{private}, memoryfabric.ScopePrivate)
	res, err := tool.Execute(context.Background(), json.RawMessage(`{"query":"secret","scope":"global"}`))
	if err != nil { t.Fatal(err) }
	if !res.IsError || !strings.Contains(res.Content, "not available") {
		t.Fatalf("cross-scope result = %+v", res)
	}
}

func TestAgentMemoryNamespaceSemantics(t *testing.T) {
	workspace := "/tmp/project-a"
	private, err := agentMemoryNamespace(multiagent.Profile{ID: "coder", MemoryScope: "private"}, workspace)
	if err != nil { t.Fatal(err) }
	if private.Scope != memoryfabric.ScopePrivate || !strings.Contains(private.Owner, ":agent:coder") {
		t.Fatalf("private namespace = %+v", private)
	}
	team, err := agentMemoryNamespace(multiagent.Profile{ID: "reviewer", MemoryScope: "team", MemoryOwner: "engineering"}, workspace)
	if err != nil { t.Fatal(err) }
	if team.Scope != memoryfabric.ScopeTeam || !strings.HasSuffix(team.Owner, ":team:engineering") {
		t.Fatalf("team namespace = %+v", team)
	}
	project, err := agentMemoryNamespace(multiagent.Profile{ID: "planner", MemoryScope: "project"}, workspace)
	if err != nil { t.Fatal(err) }
	if project != projectMemoryNamespace(workspace) {
		t.Fatalf("project namespace = %+v want %+v", project, projectMemoryNamespace(workspace))
	}
	global, err := agentMemoryNamespace(multiagent.Profile{ID: "researcher", MemoryScope: "global"}, workspace)
	if err != nil { t.Fatal(err) }
	if global != globalMemoryNamespace() {
		t.Fatalf("global namespace = %+v want %+v", global, globalMemoryNamespace())
	}
}


func TestProjectionManagerRoutesJobsToScopedStores(t *testing.T) {
	pool := newTestPool(t, 4)
	defer pool.Close()
	manager := &projectionManager{graphPool: pool}
	project := memoryfabric.Namespace{Scope: memoryfabric.ScopeProject, Owner: "project-a"}
	private := memoryfabric.Namespace{Scope: memoryfabric.ScopePrivate, Owner: "project-a:agent:coder"}

	projectJob := memoryfabric.Job{
		ID: "project-task", RecordID: "project-task", SessionKey: "session-a",
		Scope: project.Scope, Owner: project.Owner, Content: "project result",
	}
	privateJob := memoryfabric.Job{
		ID: "private-task", RecordID: "private-task", SessionKey: "session-b",
		Scope: private.Scope, Owner: private.Owner, Content: "private result",
	}
	if err := manager.processGraph(context.Background(), projectJob); err != nil { t.Fatal(err) }
	if err := manager.processGraph(context.Background(), privateJob); err != nil { t.Fatal(err) }

	projectKey, _ := graphStoreKey(project)
	privateKey, _ := graphStoreKey(private)
	projectStore, releaseProject, err := pool.Acquire(context.Background(), projectKey)
	if err != nil { t.Fatal(err) }
	defer releaseProject()
	privateStore, releasePrivate, err := pool.Acquire(context.Background(), privateKey)
	if err != nil { t.Fatal(err) }
	defer releasePrivate()

	projectSource := "haosbot/memory/" + project.Scope + "/" + project.Owner + "/session/" + projectJob.SessionKey
	privateSource := "haosbot/memory/" + private.Scope + "/" + private.Owner + "/session/" + privateJob.SessionKey
	var projectOwn, projectLeak, privateOwn, privateLeak int
	if err := projectStore.DB().QueryRow("SELECT COUNT(*) FROM documents WHERE source=?", projectSource).Scan(&projectOwn); err != nil { t.Fatal(err) }
	if err := projectStore.DB().QueryRow("SELECT COUNT(*) FROM documents WHERE source=?", privateSource).Scan(&projectLeak); err != nil { t.Fatal(err) }
	if err := privateStore.DB().QueryRow("SELECT COUNT(*) FROM documents WHERE source=?", privateSource).Scan(&privateOwn); err != nil { t.Fatal(err) }
	if err := privateStore.DB().QueryRow("SELECT COUNT(*) FROM documents WHERE source=?", projectSource).Scan(&privateLeak); err != nil { t.Fatal(err) }
	if projectOwn != 1 || privateOwn != 1 || projectLeak != 0 || privateLeak != 0 {
		t.Fatalf("scope routing counts projectOwn=%d privateOwn=%d projectLeak=%d privateLeak=%d", projectOwn, privateOwn, projectLeak, privateLeak)
	}
}
