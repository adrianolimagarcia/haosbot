package multiagent

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"encoding/json"
	"time"
)

func TestManagerDelegateLifecycleAndLimits(t *testing.T) {
	var manager *Manager
	exec := ExecutorFunc(func(ctx context.Context, profile Profile, task Task) (string, error) {
		meta, ok := ExecutionMetaFromContext(ctx)
		if !ok || meta.TaskID != task.ID || meta.AgentID != profile.ID {
			t.Fatalf("missing execution meta: ok=%v meta=%+v task=%+v", ok, meta, task)
		}
		return "done:" + profile.ID, nil
	})
	var err error
	manager, err = NewManager([]Profile{{ID: "coder", Enabled: true, DelegateTo: []string{"coder"}}}, Limits{
		MaxDepth: 2, MaxParallel: 1, MaxChildren: 2, MaxTasks: 16,
		TaskTimeout: time.Second, Retention: time.Hour,
	}, exec, "", nil)
	if err != nil { t.Fatal(err) }

	task, err := manager.Delegate(context.Background(), DelegateRequest{AgentID: "coder", Prompt: "implement", Wait: true})
	if err != nil { t.Fatal(err) }
	if task.Status != TaskCompleted || task.Result != "done:coder" || task.RootTaskID != task.ID || task.Depth != 1 {
		t.Fatalf("unexpected task: %+v", task)
	}

	cycleCtx := WithExecutionMeta(context.Background(), ExecutionMeta{
		TaskID: "parent", RootTaskID: "root", TraceID: "trace", AgentID: "coder", Depth: 1,
		Ancestry: []string{"planner"},
	})
	if _, err := manager.Delegate(cycleCtx, DelegateRequest{AgentID: "coder", Prompt: "loop"}); err == nil {
		t.Fatal("expected cycle rejection")
	}

	depthCtx := WithExecutionMeta(context.Background(), ExecutionMeta{
		TaskID: "parent", RootTaskID: "root", TraceID: "trace", AgentID: "planner", Depth: 2,
	})
	if _, err := manager.Delegate(depthCtx, DelegateRequest{AgentID: "coder", Prompt: "too deep"}); err == nil {
		t.Fatal("expected max-depth rejection")
	}
}

func TestManagerCancelAndPersistenceRecovery(t *testing.T) {
	path := filepath.Join(t.TempDir(), "tasks.json")
	started := make(chan struct{})
	exec := ExecutorFunc(func(ctx context.Context, profile Profile, task Task) (string, error) {
		close(started)
		<-ctx.Done()
		return "", ctx.Err()
	})
	manager, err := NewManager([]Profile{{ID: "worker", Enabled: true}}, Limits{
		MaxDepth: 3, MaxParallel: 1, MaxChildren: 2, MaxTasks: 16,
		TaskTimeout: time.Minute, Retention: time.Hour,
	}, exec, path, nil)
	if err != nil { t.Fatal(err) }

	task, err := manager.Delegate(context.Background(), DelegateRequest{
		AgentID: "worker", Prompt: "block", Wait: false, Detach: true,
	})
	if err != nil { t.Fatal(err) }
	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("worker did not start")
	}
	if _, err := manager.Cancel(task.ID); err != nil { t.Fatal(err) }
	final, err := manager.Wait(context.Background(), task.ID)
	if err == nil || final.Status != TaskCanceled {
		t.Fatalf("cancel result: task=%+v err=%v", final, err)
	}

	// A new manager must recover terminal tasks from the durable snapshot.
	reloaded, err := NewManager([]Profile{{ID: "worker", Enabled: true}}, Limits{
		MaxDepth: 3, MaxParallel: 1, MaxChildren: 2, MaxTasks: 16,
		TaskTimeout: time.Minute, Retention: time.Hour,
	}, ExecutorFunc(func(context.Context, Profile, Task) (string, error) {
		return "", errors.New("should not execute recovered terminal task")
	}), path, nil)
	if err != nil { t.Fatal(err) }
	got, ok := reloaded.Get(task.ID)
	if !ok || got.Status != TaskCanceled {
		t.Fatalf("recovered task = %+v ok=%v", got, ok)
	}
}

func TestInvalidProfileID(t *testing.T) {
	_, err := NewManager([]Profile{{ID: "../escape", Enabled: true}}, Limits{}, ExecutorFunc(func(context.Context, Profile, Task) (string, error) {
		return "", nil
	}), "", nil)
	if err == nil {
		t.Fatal("expected invalid profile id error")
	}
}


func TestNestedDelegateWaitYieldsSingleWorkerSlot(t *testing.T) {
	var manager *Manager
	var agentTool *Tool
	exec := ExecutorFunc(func(ctx context.Context, profile Profile, task Task) (string, error) {
		if profile.ID == "child" {
			return "child-result", nil
		}
		raw := json.RawMessage(`{"action":"delegate","agent":"child","prompt":"child task","wait":true}`)
		result, err := agentTool.Execute(ctx, raw)
		if err != nil {
			return "", err
		}
		if result.IsError {
			return "", errors.New(result.Content)
		}
		return result.Content, nil
	})
	var err error
	manager, err = NewManager([]Profile{
		{ID: "parent", Enabled: true, DelegateTo: []string{"child"}},
		{ID: "child", Enabled: true},
	}, Limits{
		MaxDepth: 3, MaxParallel: 1, MaxChildren: 2, MaxTasks: 16,
		TaskTimeout: 2 * time.Second, Retention: time.Hour,
	}, exec, "", nil)
	if err != nil { t.Fatal(err) }
	agentTool = NewTool(manager)

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	task, err := manager.Delegate(ctx, DelegateRequest{AgentID: "parent", Prompt: "parent task", Wait: true})
	if err != nil { t.Fatal(err) }
	if task.Status != TaskCompleted || !strings.Contains(task.Result, "child-result") {
		t.Fatalf("nested task = %+v", task)
	}
}


func TestDelegationRBACRejectsUnlistedTarget(t *testing.T) {
	manager, err := NewManager([]Profile{
		{ID: "planner", Enabled: true, DelegateTo: []string{"researcher"}},
		{ID: "coder", Enabled: true},
	}, Limits{MaxDepth: 3, MaxParallel: 2, MaxChildren: 2, MaxTasks: 16, TaskTimeout: time.Second, Retention: time.Hour},
		ExecutorFunc(func(context.Context, Profile, Task) (string, error) { return "ok", nil }), "", nil)
	if err != nil { t.Fatal(err) }
	ctx := WithExecutionMeta(context.Background(), ExecutionMeta{TaskID: "p", RootTaskID: "p", TraceID: "t", AgentID: "planner", Depth: 1})
	if _, err := manager.Delegate(ctx, DelegateRequest{AgentID: "coder", Prompt: "blocked"}); err == nil {
		t.Fatal("expected delegation RBAC rejection")
	}
}
