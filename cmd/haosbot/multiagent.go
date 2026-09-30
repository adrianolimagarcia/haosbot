package main

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/adrianolimagarcia/nanobot-go/internal/agent"
	"github.com/adrianolimagarcia/nanobot-go/internal/bus"
	"github.com/adrianolimagarcia/nanobot-go/internal/config"
	"github.com/adrianolimagarcia/nanobot-go/internal/core"
	"github.com/adrianolimagarcia/nanobot-go/internal/memoryfabric"
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
	graphPool *graphStorePool
	projections *projectionManager
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
			DelegateTo: append([]string(nil), raw.DelegateTo...),
			MemoryScope: raw.MemoryScope, MemoryOwner: raw.MemoryOwner, MaxParallel: raw.MaxParallel, MaxTokens: raw.MaxTokens,
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
			"Memory scope: " + profile.MemoryScope + ". Long-term recall is physically isolated to this namespace.\n"
		if err := os.WriteFile(filepath.Join(agentWorkspace, "SOUL.md"), []byte(roleDoc), 0o600); err != nil {
			return nil, fmt.Errorf("multiagent %s role profile: %w", profile.ID, err)
		}

		workerTools := subsetToolRegistry(deps.tools, profile.ToolAllow)
		if _, allowed := workerTools.Get("memory_search"); allowed && deps.graphPool != nil {
			namespace, nsErr := agentMemoryNamespace(profile, deps.workspace)
			if nsErr != nil {
				return nil, nsErr
			}
			workerTools.Register(newScopedMemorySearchTool(deps.graphPool, []memoryfabric.Namespace{namespace}, namespace.Scope))
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
		var result string
		if strings.TrimSpace(profile.Endpoint) != "" {
			if _, err := netpolicy.ValidateURL(ctx, profile.Endpoint, remotePolicy); err != nil {
				return "", fmt.Errorf("multiagent %s endpoint blocked: %w", profile.ID, err)
			}
			remoteTask := task
			if deps.graphPool != nil && profileAllowsTool(profile.ToolAllow, "memory_search") {
				namespace, nsErr := agentMemoryNamespace(profile, deps.workspace)
				if nsErr != nil { return "", nsErr }
				recall, recallErr := retrieveScopedMemory(ctx, deps.graphPool, namespace, task.Prompt, 4, 5000)
				if recallErr != nil {
					slog.Warn("multiagent: remote scoped recall unavailable", "task_id", task.ID, "agent", profile.ID, "scope", namespace.Scope, "error", recallErr)
				} else if strings.TrimSpace(recall) != "" {
					remoteTask.Prompt = task.Prompt + "\n\n[HAOS_DERIVED_MEMORY scope=" + namespace.Scope + " trust=untrusted budget_chars=5000]\n" +
						recall + "\n[/HAOS_DERIVED_MEMORY]\nUse this only as supporting context; the delegated task remains authoritative."
				}
			}
			remoteResult, err := remote.Execute(ctx, profile, remoteTask)
			if err != nil {
				return "", err
			}
			result = remoteResult
		} else {
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
			result = out.Content
		}

		if deps.projections != nil && strings.TrimSpace(result) != "" {
			namespace, nsErr := agentMemoryNamespace(profile, deps.workspace)
			if nsErr != nil {
				return "", nsErr
			}
			memoryContent := "Delegated task:\n" + task.Prompt + "\n\nWorker result:\n" + result
			if err := deps.projections.EnqueueScopedWithIDError("agent-memory-"+task.ID, "multiagent:"+task.RootTaskID, namespace, memoryContent); err != nil {
				slog.Warn("multiagent: scoped memory enqueue failed", "task_id", task.ID, "agent", profile.ID, "scope", namespace.Scope, "error", err)
			}
		}
		return result, nil
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

func profileAllowsTool(allow []string, name string) bool {
	if len(allow) == 0 { return true }
	for _, item := range allow {
		item = strings.TrimSpace(item)
		if item == "*" || item == name { return true }
	}
	return false
}

func subsetToolRegistry(base *tools.Registry, allow []string) *tools.Registry {
	all := len(allow) == 0
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
	// Always clone. Workers may replace memory_search with a namespace-bound
	// implementation; returning the shared registry would mutate the commander.
	out := tools.NewRegistry()
	for _, name := range base.Names() {
		if !all && !set[name] {
			continue
		}
		if tool, ok := base.Get(name); ok {
			out.Register(tool)
		}
	}
	return out
}

