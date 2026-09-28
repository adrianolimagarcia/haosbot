package multiagent

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/adrianolimagarcia/nanobot-go/internal/tools"
)

type Tool struct {
	tools.Base
	manager *Manager
}

func NewTool(manager *Manager) *Tool { return &Tool{manager: manager} }

func (t *Tool) Name() string { return "agents" }

func (t *Tool) Description() string {
	return "Manage HAOS multi-agent work. Actions: list agents; delegate a task; inspect, wait, read result, or cancel a task. Delegate only when specialization or parallel work materially helps."
}

func (t *Tool) Parameters() json.RawMessage {
	return json.RawMessage("{\"type\":\"object\",\"properties\":{\"action\":{\"type\":\"string\",\"enum\":[\"list\",\"delegate\",\"status\",\"wait\",\"result\",\"cancel\"]},\"agent\":{\"type\":\"string\",\"description\":\"Target agent id for delegate.\"},\"prompt\":{\"type\":\"string\",\"description\":\"Self-contained task for the target agent.\"},\"task_id\":{\"type\":\"string\",\"description\":\"Task id for status/wait/result/cancel.\"},\"wait\":{\"type\":\"boolean\",\"description\":\"For delegate, wait for completion. Defaults true.\"},\"timeout_seconds\":{\"type\":\"integer\",\"minimum\":1,\"description\":\"Optional task/wait timeout, capped by runtime policy.\"}},\"required\":[\"action\"],\"additionalProperties\":false}")
}

func (t *Tool) ConcurrencySafe() bool { return true }
func (t *Tool) ReadOnly() bool        { return false }
func (t *Tool) Exclusive() bool       { return false }

func (t *Tool) Execute(ctx context.Context, raw json.RawMessage) (tools.Result, error) {
	if t.manager == nil {
		return tools.Errf("multi-agent runtime is unavailable"), nil
	}
	var req struct {
		Action         string `json:"action"`
		Agent          string `json:"agent"`
		Prompt         string `json:"prompt"`
		TaskID         string `json:"task_id"`
		Wait           *bool  `json:"wait"`
		TimeoutSeconds int    `json:"timeout_seconds"`
	}
	if err := json.Unmarshal(raw, &req); err != nil {
		return tools.Errf("invalid agents arguments: %v", err), nil
	}
	req.Action = strings.ToLower(strings.TrimSpace(req.Action))
	timeout := time.Duration(req.TimeoutSeconds) * time.Second

	switch req.Action {
	case "list":
		return jsonResult(map[string]any{"agents": t.manager.Profiles(), "limits": t.manager.Limits()})
	case "delegate":
		wait := true
		if req.Wait != nil {
			wait = *req.Wait
		}
		task, err := t.manager.Delegate(ctx, DelegateRequest{
			AgentID: req.Agent, Prompt: req.Prompt, Wait: wait, Timeout: timeout, Detach: !wait,
		})
		if err != nil {
			if task != nil {
				return jsonResult(map[string]any{"task": task, "error": err.Error()})
			}
			return tools.Errf("delegate failed: %v", err), nil
		}
		return jsonResult(map[string]any{"task": task})
	case "status":
		task, ok := t.manager.Get(strings.TrimSpace(req.TaskID))
		if !ok {
			return tools.Errf("task %q not found", req.TaskID), nil
		}
		return jsonResult(map[string]any{"task": task})
	case "wait":
		if strings.TrimSpace(req.TaskID) == "" {
			return tools.Errf("task_id is required"), nil
		}
		waitCtx := ctx
		var cancel context.CancelFunc
		if timeout > 0 {
			waitCtx, cancel = context.WithTimeout(ctx, timeout)
			defer cancel()
		}
		task, err := t.manager.Wait(waitCtx, strings.TrimSpace(req.TaskID))
		if err != nil {
			return jsonResult(map[string]any{"task": task, "error": err.Error()})
		}
		return jsonResult(map[string]any{"task": task})
	case "result":
		task, ok := t.manager.Get(strings.TrimSpace(req.TaskID))
		if !ok {
			return tools.Errf("task %q not found", req.TaskID), nil
		}
		if !isTerminal(task.Status) {
			return jsonResult(map[string]any{"task": task, "ready": false})
		}
		return jsonResult(map[string]any{"task": task, "ready": true, "result": task.Result, "error": task.Error})
	case "cancel":
		task, err := t.manager.Cancel(strings.TrimSpace(req.TaskID))
		if err != nil {
			return tools.Errf("cancel failed: %v", err), nil
		}
		return jsonResult(map[string]any{"task": task})
	default:
		return tools.Errf("unsupported action %q", req.Action), nil
	}
}

func jsonResult(v any) (tools.Result, error) {
	data, err := json.Marshal(v)
	if err != nil {
		return tools.Result{}, fmt.Errorf("multiagent: encode tool result: %w", err)
	}
	return tools.OK(string(data)), nil
}
