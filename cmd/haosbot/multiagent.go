package main

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/adrianolimagarcia/nanobot-go/internal/agent"
	"github.com/adrianolimagarcia/nanobot-go/internal/bus"
	"github.com/adrianolimagarcia/nanobot-go/internal/config"
	"github.com/adrianolimagarcia/nanobot-go/internal/core"
	"github.com/adrianolimagarcia/nanobot-go/internal/multiagent"
	"github.com/adrianolimagarcia/nanobot-go/internal/netpolicy"
	"github.com/adrianolimagarcia/nanobot-go/internal/observability"
	"github.com/adrianolimagarcia/nanobot-go/internal/prompt"
	"github.com/adrianolimagarcia/nanobot-go/internal/provider"
	"github.com/adrianolimagarcia/nanobot-go/internal/session"
	"github.com/adrianolimagarcia/nanobot-go/internal/tools"
)

type multiAgentRuntimeDeps struct {
	bus       *bus.Bus
	store     *session.Store
	tools     *tools.Registry
	workspace string
	metrics   *observability.Registry
	provider  provider.Provider
	model     string
}

func buildMultiAgentManager(cfg *config.Config, deps multiAgentRuntimeDeps) (*multiagent.Manager, error) {
	settings := cfg.Agents.MultiAgent
	if !settings.Enabled {
		return nil, nil
	}

	profiles := make([]multiagent.Profile, 0, len(cfg.Agents.Profiles))
	for id, raw := range cfg.Agents.Profiles {
		profiles = append(profiles, multiagent.Profile{
			ID: id, Name: raw.Name, Role: raw.Role, Instructions: raw.Instructions,
			Model: raw.Model, Provider: raw.Provider, Endpoint: raw.Endpoint,
			TokenEnv: raw.TokenEnv, ToolAllow: append([]string(nil), raw.ToolAllow...),
			MemoryScope: raw.MemoryScope, MaxTokens: raw.MaxTokens,
			MaxToolIterations: raw.MaxToolIterations, Enabled: raw.Enabled,
		})
	}

	limits := multiagent.Limits{
		MaxDepth: settings.MaxDepth, MaxParallel: settings.MaxParallel,
		MaxChildren: settings.MaxChildren, MaxTasks: settings.MaxTasks,
		TaskTimeoutSecs: settings.TaskTimeoutSeconds,
		RetentionMinutes: settings.RetentionMinutes,
		TaskTimeout: time.Duration(settings.TaskTimeoutSeconds) * time.Second,
		Retention: time.Duration(settings.RetentionMinutes) * time.Minute,
	}

	var loopsMu sync.Mutex
	localLoops := map[string]*agent.Loop{}
	getLocalLoop := func(profile multiagent.Profile) (*agent.Loop, error) {
		loopsMu.Lock()
		defer loopsMu.Unlock()
		if loop := localLoops[profile.ID]; loop != nil {
			return loop, nil
		}

		workerCfg := *cfg
		workerCfg.Agents = cfg.Agents
		workerCfg.Agents.Defaults = cfg.Agents.Defaults
		if strings.TrimSpace(profile.Model) != "" {
			workerCfg.Agents.Defaults.Model = profile.Model
			// A model prefix should be allowed to select its provider when the
			// profile did not pin one explicitly.
			if strings.TrimSpace(profile.Provider) == "" {
				workerCfg.Agents.Defaults.Provider = "auto"
			}
		}
		if strings.TrimSpace(profile.Provider) != "" {
			workerCfg.Agents.Defaults.Provider = profile.Provider
		}
		if profile.MaxTokens > 0 {
			workerCfg.Agents.Defaults.MaxTokens = profile.MaxTokens
		}
		if profile.MaxToolIterations > 0 {
			workerCfg.Agents.Defaults.MaxToolIterations = profile.MaxToolIterations
		}

		prov, model := deps.provider, deps.model
		var err error
		// Profiles inherit the gateway provider/client by default. A dedicated
		// client is created only when the profile explicitly overrides routing.
		if strings.TrimSpace(profile.Model) != "" || strings.TrimSpace(profile.Provider) != "" {
			prov, model, err = resolveProvider(&workerCfg)
			if err != nil {
				return nil, fmt.Errorf("multiagent %s provider: %w", profile.ID, err)
			}
		}
		if prov == nil {
			return nil, fmt.Errorf("multiagent %s provider is unavailable", profile.ID)
		}

		agentWorkspace := filepath.Join(deps.workspace, ".haosbot", "agents", profile.ID)
		if err := os.MkdirAll(agentWorkspace, 0o700); err != nil {
			return nil, fmt.Errorf("multiagent %s workspace: %w", profile.ID, err)
		}
		roleDoc := "# Role\n\n" + strings.TrimSpace(profile.Instructions) +
			"\n\nYou are a delegated HAOS worker. Work only on the delegated task. " +
			"Return evidence and a concise result to the parent agent. " +
			"Do not claim work was completed unless tool results support it.\n\n" +
			"Memory scope: " + profile.MemoryScope + ".\n"
		if err := os.WriteFile(filepath.Join(agentWorkspace, "SOUL.md"), []byte(roleDoc), 0o600); err != nil {
			return nil, fmt.Errorf("multiagent %s role profile: %w", profile.ID, err)
		}

		workerTools := subsetToolRegistry(deps.tools, profile.ToolAllow)
		if strings.EqualFold(strings.TrimSpace(profile.MemoryScope), "private") {
			workerTools = withoutTool(workerTools, "memory_search")
		}
		builder := prompt.New(agentWorkspace)
		builder.DisabledSkills = append([]string(nil), workerCfg.Agents.Defaults.DisabledSkills...)
		builder.Timezone = workerCfg.Agents.Defaults.Timezone

		maxIterations := workerCfg.Agents.Defaults.MaxToolIterations
		maxTokens := workerCfg.Agents.Defaults.MaxTokens
		reasoning := ""
		if workerCfg.Agents.Defaults.ReasoningEffort != nil {
			reasoning = *workerCfg.Agents.Defaults.ReasoningEffort
		}
		workerSystemPrompt := strings.TrimSpace(profile.Instructions) + "\n\n" +
			"You are a delegated HAOS worker. Work only on the delegated task. " +
			"Return evidence and a concise result to the parent agent. " +
			"Do not claim work was completed unless tool results support it.\n\n" +
			builder.BuildSystemPrompt("multiagent", nil, deps.workspace, false)
		loop, err := agent.NewLoop(agent.LoopConfig{
			Bus: deps.bus, Store: transcriptStore{deps.store}, Provider: prov,
			Tools: workerTools, Prompt: builder, SystemPrompt: workerSystemPrompt, Model: model,
			MaxTokens: maxTokens,
			ContextWindowTokens: workerCfg.Agents.Defaults.ContextWindowTokens,
			AutoSummarizeTokens: 120_000,
			Temperature: float64(workerCfg.Agents.Defaults.Temperature),
			Workspace: agentWorkspace, ProjectWorkspace: deps.workspace,
			MaxIterations: maxIterations,
			MaxToolResultChars: workerCfg.Agents.Defaults.MaxToolResultChars,
			SequentialTools: false,
			ReasoningEffort: reasoning,
			// Worker transcripts are intentionally transient and are not directly
			// projected into canonical memory. Their returned result becomes part
			// of the parent turn, while memory_search still reads the shared graph.
			IncludeMemory: false,
			Metrics: deps.metrics,
		})
		if err != nil {
			return nil, fmt.Errorf("multiagent %s loop: %w", profile.ID, err)
		}
		localLoops[profile.ID] = loop
		return loop, nil
	}

	remotePolicy := netpolicy.Policy{Allowlist: append([]string(nil), cfg.Tools.SSRFWhitelist...)}
	remote := multiagent.A2AExecutor{
		Client: netpolicy.NewClient(time.Duration(settings.TaskTimeoutSeconds)*time.Second, remotePolicy),
		Token: func(profile multiagent.Profile) string {
			if strings.TrimSpace(profile.TokenEnv) == "" {
				return ""
			}
			return os.Getenv(profile.TokenEnv)
		},
	}

	executor := multiagent.ExecutorFunc(func(ctx context.Context, profile multiagent.Profile, task multiagent.Task) (string, error) {
		if strings.TrimSpace(profile.Endpoint) != "" {
			if _, err := netpolicy.ValidateURL(ctx, profile.Endpoint, remotePolicy); err != nil {
				return "", fmt.Errorf("multiagent %s endpoint blocked: %w", profile.ID, err)
			}
			return remote.Execute(ctx, profile, task)
		}
		loop, err := getLocalLoop(profile)
		if err != nil {
			return "", err
		}
		sessionKey := "webui:tmp_agent_" + profile.ID + "_" + task.ID
		msg := core.InboundMessage{
			Channel: "multiagent", SenderID: task.RequestedBy, ChatID: task.RootTaskID,
			Content: task.Prompt,
			Metadata: map[string]any{
				"_multiagent": map[string]any{
					"task_id": task.ID, "root_task_id": task.RootTaskID,
					"parent_task_id": task.ParentTaskID, "trace_id": task.TraceID,
					"agent_id": task.AgentID, "depth": task.Depth,
				},
			},
			SessionKeyOverride: &sessionKey,
		}
		out, err := loop.ProcessMessage(ctx, msg)
		if err != nil {
			return "", err
		}
		if out == nil {
			return "", fmt.Errorf("multiagent %s returned no outbound message", profile.ID)
		}
		return out.Content, nil
	})

	persistPath := ""
	if settings.PersistTasks {
		persistPath = filepath.Join(config.DefaultDataDir(), "multiagent", "tasks.json")
	}
	manager, err := multiagent.NewManager(profiles, limits, executor, persistPath, nil)
	if err != nil {
		return nil, err
	}
	deps.tools.Register(multiagent.NewTool(manager))
	return manager, nil
}

func subsetToolRegistry(base *tools.Registry, allow []string) *tools.Registry {
	if len(allow) == 0 {
		return base
	}
	all := false
	set := make(map[string]bool, len(allow))
	for _, name := range allow {
		name = strings.TrimSpace(name)
		if name == "*" {
			all = true
			break
		}
		if name != "" {
			set[name] = true
		}
	}
	if all {
		return base
	}
	out := tools.NewRegistry()
	for _, name := range base.Names() {
		if !set[name] {
			continue
		}
		if tool, ok := base.Get(name); ok {
			out.Register(tool)
		}
	}
	return out
}


func withoutTool(base *tools.Registry, denied string) *tools.Registry {
	out := tools.NewRegistry()
	for _, name := range base.Names() {
		if name == denied {
			continue
		}
		if tool, ok := base.Get(name); ok {
			out.Register(tool)
		}
	}
	return out
}
