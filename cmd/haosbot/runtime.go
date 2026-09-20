package main

import (
	"bufio"
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/adrianolimagarcia/nanobot-go/internal/agent"
	"github.com/adrianolimagarcia/nanobot-go/internal/api"
	"github.com/adrianolimagarcia/nanobot-go/internal/bus"
	"github.com/adrianolimagarcia/nanobot-go/internal/config"
	"github.com/adrianolimagarcia/nanobot-go/internal/core"
	cronruntime "github.com/adrianolimagarcia/nanobot-go/internal/cron"
	"github.com/adrianolimagarcia/nanobot-go/internal/memory"
	"github.com/adrianolimagarcia/nanobot-go/internal/memoryfabric"
	"github.com/adrianolimagarcia/nanobot-go/internal/mcpruntime"
	"github.com/adrianolimagarcia/nanobot-go/internal/tools/cliapps"
	"github.com/adrianolimagarcia/nanobot-go/internal/observability"
	"github.com/adrianolimagarcia/nanobot-go/internal/prompt"
	"github.com/adrianolimagarcia/nanobot-go/internal/provider"
	"github.com/adrianolimagarcia/nanobot-go/internal/provider/openai"
	"github.com/adrianolimagarcia/nanobot-go/internal/session"
	"github.com/adrianolimagarcia/nanobot-go/internal/tools"
	"github.com/adrianolimagarcia/nanobot-go/internal/tools/builtin"
	triggersruntime "github.com/adrianolimagarcia/nanobot-go/internal/triggers"
	wsbootstrap "github.com/adrianolimagarcia/nanobot-go/internal/workspace"

	"strconv"
)

type agentRuntime struct {
	cfg       *config.Config
	bus       *bus.Bus
	loop      *agent.Loop
	store     *session.Store
	metrics   *observability.Registry
	scheduler *cronruntime.Service
	triggers  *triggersruntime.Service
	closeF    func()
}

type transcriptStore struct{ s *session.Store }

func (t transcriptStore) Open(key string) (agent.Transcript, error) {
	if session.IsTransientKey(key) {
		return session.DefaultTransientStore().Open(key)
	}
	sess, err := t.s.Open(key)
	if err != nil {
		return nil, err
	}
	return sess, nil
}

func (t transcriptStore) List() ([]string, error) {
	return t.s.List()
}

func buildRuntime(cfg *config.Config) (*agentRuntime, error) {
	d := cfg.Agents.Defaults

	workspace, err := expandTilde(d.Workspace)
	if err != nil {
		return nil, fmt.Errorf("resolve workspace: %w", err)
	}
	if err := os.MkdirAll(workspace, 0o755); err != nil {
		return nil, fmt.Errorf("create workspace %s: %w", workspace, err)
	}
	if _, err := wsbootstrap.SyncTemplates(workspace, false); err != nil {
		return nil, fmt.Errorf("sync workspace templates: %w", err)
	}

	sessionsRoot := filepath.Join(config.DefaultDataDir(), "sessions")
	if isWithin(sessionsRoot, workspace) {
		return nil, fmt.Errorf(
			"session root %s is inside the agent workspace %s; refusing to start",
			sessionsRoot, workspace)
	}
	store := session.NewStore(workspace, sessionsRoot)

	prov, model, err := resolveProvider(cfg)
	if err != nil {
		return nil, err
	}

	execTimeout := time.Duration(cfg.Tools.Exec.Timeout) * time.Second
	if execTimeout <= 0 {
		execTimeout = 60 * time.Second
	}

	registry := tools.NewRegistry()
	builtin.Register(registry, builtin.Config{
		Files: builtin.PathPolicy{Workspace: workspace, AllowedDir: workspace},
		Exec: builtin.ExecOptions{
			Workspace:           workspace,
			RestrictToWorkspace: true,
			DefaultTimeout:      execTimeout,
			MaxTimeout:          600 * time.Second,
			DenyPatterns:        append([]string(nil), cfg.Tools.Exec.DenyPatterns...),
		},
		SSRFWhitelist:       append([]string(nil), cfg.Tools.SSRFWhitelist...),
		EnforceCapabilities: true,
		EnableFiles:         cfg.Tools.File.Enable,
		EnableExec:          cfg.Tools.Exec.Enable,
		EnableNetwork:       cfg.Tools.Web.Enable,
	})
	if cfg.Tools.CliApps.Enable {
		if execTool, ok := registry.Get("exec"); ok {
			registry.Register(cliapps.New(cliapps.Options{
				Workspace: workspace,
				RunTimeout: time.Duration(cfg.Tools.CliApps.RunTimeout) * time.Second,
				ExecTool: execTool,
			}))
		} else {
			slog.Warn("CLI Apps enabled but exec capability is disabled; run_cli_app not registered")
		}
	}
	mcpManager := mcpruntime.NewManager(mcpruntime.Options{SSRFWhitelist: append([]string(nil), cfg.Tools.SSRFWhitelist...)})
	if err := mcpManager.LoadAndRegister(context.Background(), registry, cfg.Tools.MCPServers); err != nil {
		slog.Warn("some MCP servers were not loaded", "error", err)
	}

	scheduler := cronruntime.NewService(filepath.Join(config.DefaultDataDir(), "cron", workspaceGraphNamespace(workspace), "jobs.json"), nil)
	if err := scheduler.Load(); err != nil {
		return nil, fmt.Errorf("load automation scheduler: %w", err)
	}
	triggerSvc := triggersruntime.NewService(filepath.Join(workspace, "triggers"), nil)
	registry.Register(cronruntime.NewTool(scheduler, d.Timezone))

	// Bounded by default: the queue limits come from gateway.maxInboundQueue /
	// gateway.maxOutboundQueue, which default to a non-zero cap. The reference
	// runs these queues unbounded; see internal/config/bus.go.
	messageBus := bus.New(cfg.BusOptions())
	profile := resolveResourceProfile()
	graphModeExplicit := strings.TrimSpace(os.Getenv(graphEmbedderModeEnv)) != ""
	graphEmbedder, err := resolveGraphEmbedder(context.Background(), profile.Name != "low" || graphModeExplicit)
	if err != nil {
		messageBus.Close()
		return nil, fmt.Errorf("load GraphRAG embedder: %w", err)
	}
	graphPool := newGraphStorePoolWithEmbedder(workspaceGraphRoot(config.DefaultDataDir(), workspace), 2, graphEmbedder)
	// Deep GraphRAG recall is explicit. Keeping it as a tool preserves hybrid
	// FTS/vector/graph capabilities without making every turn pay retrieval
	// latency before the provider starts.
	registry.Register(newMemorySearchTool(graphPool))
	metrics := observability.New()
	metrics.SetVectorEnabled(graphEmbedder != nil)
	metrics.SetEmbedderLoaded(graphEmbedder != nil)
	memoryFabric, err := memoryfabric.Open(context.Background(), memoryfabric.Config{
		Path: filepath.Join(config.DefaultDataDir(), "memory-fabric.db"),
		CacheKB: profile.MemoryCacheKB,
		MaxPending: profile.MemoryMaxPending,
		MaxPendingBytes: profile.MemoryMaxPendingBytes,
		MaxContentBytes: profile.MemoryMaxContentBytes,
		MaxDiskBytes: profile.FutureDiskBytes,
		MaxAttempts: 8,
		Projections: func() []string {
			if profile.ObsidianEnabled { return []string{memoryfabric.ProjectionGraph, memoryfabric.ProjectionObsidian} }
			return []string{memoryfabric.ProjectionGraph}
		}(),
	})
	if err != nil {
		_ = graphPool.Close()
		messageBus.Close()
		return nil, fmt.Errorf("open memory fabric: %w", err)
	}
	if err := migrateLegacyGraphOutbox(filepath.Join(config.DefaultDataDir(), "graph-outbox.jsonl"), memoryFabric); err != nil {
		_ = memoryFabric.Close()
		_ = graphPool.Close()
		messageBus.Close()
		return nil, fmt.Errorf("migrate legacy GraphRAG outbox: %w", err)
	}
	if err := ensureWorkspaceGraphProjection(context.Background(), config.DefaultDataDir(), workspace, memoryFabric); err != nil {
		_ = memoryFabric.Close()
		_ = graphPool.Close()
		messageBus.Close()
		return nil, fmt.Errorf("prepare workspace GraphRAG projection: %w", err)
	}
	projections, err := newProjectionManager(memoryFabric, graphPool, filepath.Join(config.DefaultDataDir(), "obsidian-memory"), profile.ProjectionWorkers, time.Duration(profile.ProjectionPollMs)*time.Millisecond, profile.ObsidianEnabled, metrics)
	if err != nil {
		_ = memoryFabric.Close()
		_ = graphPool.Close()
		messageBus.Close()
		return nil, fmt.Errorf("start memory projections: %w", err)
	}
	memoryMDProjection := newMemoryMDProjector(filepath.Join(workspace, "memory", "MEMORY.md"), graphPool, 2*time.Second)

	loop, err := agent.NewLoop(agent.LoopConfig{
		Bus:                   messageBus,
		Store:                 transcriptStore{store},
		Provider:              prov,
		Tools:                 registry,
		ToolsForMessage: func(msg core.InboundMessage) (*tools.Registry, error) {
			event, _ := msg.Metadata["_system_event"].(string)
			if event != "dream" { return nil, nil }
			memoryStore, err := memory.NewMemoryStore(workspace, memory.DefaultMaxHistory)
			if err != nil { return nil, err }
			return memoryStore.BuildDreamTools()
		},
		Prompt:                prompt.New(workspace),
		Model:                 model,
		MaxTokens:             d.MaxTokens,
		ContextWindowTokens:   d.ContextWindowTokens,
		AutoSummarizeTokens:   120_000,
		Temperature:           float64(d.Temperature),
		Workspace:             workspace,
		MaxIterations:         d.MaxToolIterations,
		MaxToolResultChars:    d.MaxToolResultChars,
		SequentialTools:       false,
		IncludeMemory:         profile.MemoryRetrievalEnabled,
		GraphMemoryEnqueue:           projections.Enqueue,
		GraphMemoryEnqueueWithID:     projections.EnqueueWithID,
		GraphMemoryEnqueueWithIDError: projections.EnqueueWithIDError,
		GraphMemoryMaxChars:       6000,
		Metrics:                   metrics,
	})
	if err == nil {
		err = loop.RecoverPendingGraphMemory()
	}
	if err == nil {
		err = scheduler.SetExecutor(func(ctx context.Context, job cronruntime.Job, runID string) (cronruntime.RunResult, error) {
			if job.Payload.Kind == cronruntime.PayloadSystemEvent {
				return executeSystemAutomation(ctx, loop, workspace, job, runID)
			}
			key := job.Payload.SessionKey
			metadata := make(map[string]any, len(job.Payload.OriginMetadata)+2)
			for k, v := range job.Payload.OriginMetadata {
				metadata[k] = v
			}
			metadata["_cron_trigger"] = map[string]any{
				"job_id": job.ID, "job_name": job.Name, "run_id": runID,
				"scheduled_for_ms": job.ScheduledForMS, "idempotency_key": job.IdempotencyKey,
			}
			metadata["_cron_defer_until_session_idle"] = true
			msg := core.InboundMessage{
				Channel: job.Payload.OriginChannel,
				SenderID: "cron",
				ChatID: job.Payload.OriginChatID,
				Content: "Scheduled automation triggered: " + job.Name + "\n\n" + job.Payload.Message,
				Metadata: metadata,
				SessionKeyOverride: &key,
			}

			for {
				out, runErr := loop.ProcessMessage(ctx, msg)
				if errors.Is(runErr, agent.ErrTurnActive) {
					timer := time.NewTimer(750 * time.Millisecond)
					select {
					case <-ctx.Done():
						if !timer.Stop() { select { case <-timer.C: default: } }
						return cronruntime.RunResult{RunID: runID}, ctx.Err()
					case <-timer.C:
						continue
					}
				}
				if runErr != nil {
					return cronruntime.RunResult{RunID: runID}, runErr
				}
				response := ""
				if out != nil {
					response = out.Content
					if job.Payload.OriginChannel != "webui" {
						if err := messageBus.PublishOutbound(ctx, *out); err != nil {
							return cronruntime.RunResult{RunID: runID, Response: response}, err
						}
					}
				}
				return cronruntime.RunResult{RunID: runID, Response: response}, nil
			}
		})
	}
	if err == nil {
		err = triggerSvc.SetExecutor(func(ctx context.Context, trigger triggersruntime.Trigger, delivery triggersruntime.Delivery) (string, error) {
			metadata := make(map[string]any, len(trigger.OriginMetadata)+2)
			for k, v := range trigger.OriginMetadata { metadata[k] = v }
			metadata["_local_trigger"] = map[string]any{
				"trigger_id": trigger.ID, "trigger_name": trigger.Name,
				"delivery_id": delivery.ID, "created_at_ms": delivery.CreatedAtMS,
			}
			key := trigger.SessionKey
			msg := core.InboundMessage{
				Channel: trigger.Channel, SenderID: trigger.SenderID, ChatID: trigger.ChatID,
				Content: "Local trigger received: " + trigger.Name + "\n\n" + delivery.Content,
				Metadata: metadata, SessionKeyOverride: &key,
			}
			for {
				out, runErr := loop.ProcessMessage(ctx, msg)
				if errors.Is(runErr, agent.ErrTurnActive) {
					timer := time.NewTimer(750 * time.Millisecond)
					select {
					case <-ctx.Done():
						if !timer.Stop() { select { case <-timer.C: default: } }
						return "", ctx.Err()
					case <-timer.C:
						continue
					}
				}
				if runErr != nil { return "", runErr }
				if out == nil { return "", nil }
				if trigger.Channel != "webui" {
					if err := messageBus.PublishOutbound(ctx, *out); err != nil { return out.Content, err }
				}
				return out.Content, nil
			}
		})
	}

	if err != nil {
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		_ = scheduler.Close(shutdownCtx)
		projections.Close(shutdownCtx)
		memoryMDProjection.Close()
		cancel()
		graphPool.LogShutdownStats()
		_ = graphPool.Close()
		if closeErr := memoryFabric.Close(); closeErr != nil {
			slog.Error("close memory fabric", "error", closeErr)
		}
		messageBus.Close()
		return nil, fmt.Errorf("build agent loop: %w", err)
	}

	return &agentRuntime{
		cfg:   cfg,
		bus:   messageBus,
		loop:  loop,
		store: store,
		metrics: metrics,
		scheduler: scheduler,
		triggers: triggerSvc,
		closeF: func() {
			shutdownCtx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
			_ = triggerSvc.Close(shutdownCtx)
			_ = scheduler.Close(shutdownCtx)
			_ = mcpManager.Close()
			projections.Close(shutdownCtx)
			memoryMDProjection.Close()
			cancel()
			// Logged after the indexer has drained and before Close, so the
			// counters describe the whole life of the pool: the graph store
			// pool is allowed to exceed its open-store limit while stores are
			// pinned, and this is where an operator sees by how much and how
			// often (graph_pool.go: OvershootStats).
			graphPool.LogShutdownStats()
			_ = graphPool.Close()
			if closeErr := memoryFabric.Close(); closeErr != nil {
				slog.Error("close memory fabric", "error", closeErr)
			}
			messageBus.Close()
			metrics.SetEmbedderLoaded(false)
		},
	}, nil
}

func (r *agentRuntime) Close() {
	if r.closeF != nil {
		r.closeF()
	}
}

func resolveProvider(cfg *config.Config) (provider.Provider, string, error) {
	d := cfg.Agents.Defaults
	model := d.Model
	providerName := strings.TrimSpace(d.Provider)

	prefix, rest := "", model
	if idx := strings.Index(model, "/"); idx > 0 {
		prefix, rest = model[:idx], model[idx+1:]
	}
	if providerName == "" || providerName == "auto" {
		if prefix != "" {
			providerName = prefix
		} else {
			providerName = "openai"
		}
	}
	if prefix != "" && strings.EqualFold(prefix, providerName) {
		model = rest
	}

	pc, err := providerConfigFor(cfg, providerName)
	if err != nil {
		return nil, "", err
	}

	apiKey := ""
	if pc.APIKey != nil {
		apiKey = *pc.APIKey
	}
	baseURL := ""
	if pc.APIBase != nil {
		baseURL = *pc.APIBase
	}

	client := openai.New(openai.Options{
		APIKey:        apiKey,
		BaseURL:       baseURL,
		Model:         model,
		ToolCallFormat: openai.ToolCallFormat(strings.TrimSpace(os.Getenv("NANOBOT_TOOL_CALL_FORMAT"))),
	})
	return client, model, nil
}

func providerConfigFor(cfg *config.Config, name string) (config.ProviderConfig, error) {
	p := cfg.Providers
	switch strings.ToLower(name) {
	case "openai", "":
		return p.OpenAI, nil
	case "openrouter":
		return p.OpenRouter, nil
	case "deepseek":
		return p.DeepSeek, nil
	case "groq":
		return p.Groq, nil
	case "anthropic":
		return p.Anthropic, nil
	case "custom":
		return p.Custom, nil
	default:
		return config.ProviderConfig{}, fmt.Errorf(
			"provider %q is not supported by this port (only OpenAI-compatible "+
				"chat completions are implemented); see docs/COMPATIBILITY.md", name)
	}
}

func cmdRun(args []string) error {
	if len(args) == 0 {
		return errors.New("usage: haosbot run \"<message>\"")
	}
	message := strings.Join(args, " ")

	cfg, err := config.LoadDefault()
	if err != nil {
		return err
	}
	rt, err := buildRuntime(cfg)
	if err != nil {
		return err
	}
	defer rt.Close()

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	msg := core.InboundMessage{
		Channel: "cli",
		ChatID:  "cli",
		Content: message,
	}
	out, err := rt.loop.ProcessMessage(ctx, msg)
	if err != nil {
		return err
	}
	if out != nil {
		fmt.Println(out.Content)
	}
	return nil
}

func cmdChat(args []string) error {
	cfg, err := config.LoadDefault()
	if err != nil {
		return err
	}
	rt, err := buildRuntime(cfg)
	if err != nil {
		return err
	}
	defer rt.Close()

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	fmt.Printf("haosbot %s — type /help for commands, Ctrl-D to exit\n", version)

	scanner := bufio.NewScanner(os.Stdin)
	scanner.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)

	for {
		fmt.Print("> ")
		if !scanner.Scan() {
			break
		}
		line := strings.TrimSpace(scanner.Text())
		if line == "" {
			continue
		}
		if line == "/quit" || line == "/exit" {
			break
		}

		out, err := rt.loop.ProcessMessage(ctx, core.InboundMessage{
			Channel: "cli",
			ChatID:  "cli",
			Content: line,
		})
		if err != nil {
			if errors.Is(err, context.Canceled) {
				break
			}
			fmt.Fprintf(os.Stderr, "error: %v\n", err)
			continue
		}
		if out != nil {
			fmt.Println(out.Content)
		}
	}
	if err := scanner.Err(); err != nil && !errors.Is(err, context.Canceled) {
		return err
	}
	return nil
}

func cmdGateway(args []string) error {
	fs := flag.NewFlagSet("gateway", flag.ExitOnError)
	hostFlag := fs.String("host", "", "HTTP gateway host/IP to bind (defaults to config api.host or 127.0.0.1)")
	portFlag := fs.String("port", "", "HTTP gateway port (defaults to config api.port or 8900)")
	if err := fs.Parse(args); err != nil {
		return err
	}

	cfg, err := config.LoadDefault()
	if err != nil {
		return err
	}
	rt, err := buildRuntime(cfg)
	if err != nil {
		return err
	}
	defer rt.Close()

	// Channels are a gateway concern: the reference constructs the
	// ChannelManager only in the gateway runtime (cli/gateway_runtime.py:715)
	// and starts it as one of the gateway's tasks (:941). `haosbot run` and
	// `haosbot chat` drive the agent loop directly and never touch a channel.
	if err := configureSystemAutomations(cfg, cfg.WorkspacePath(), rt.scheduler); err != nil {
		return fmt.Errorf("configure system automations: %w", err)
	}
	if err := rt.scheduler.Start(); err != nil {
		return fmt.Errorf("start automation scheduler: %w", err)
	}
	if err := rt.triggers.Start(); err != nil {
		return fmt.Errorf("start local trigger service: %w", err)
	}

	channelManager := buildChannelManager(cfg, rt.bus)
	if names := channelManager.EnabledChannels(); len(names) > 0 {
		fmt.Printf("haosbot %s channels enabled: %s\n", version, strings.Join(names, ", "))
	}
	// Registered after `defer rt.Close()` so that it runs BEFORE it (defers run
	// LIFO): the reference closes the channel transports first and only then
	// tears down the loop-owned resources (cli/gateway_runtime.py:305-308).
	defer func() {
		stopCtx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		_ = channelManager.StopAll(stopCtx)
	}()

	prov, _, err := resolveProvider(cfg)
	if err != nil {
		return err
	}

	apiServer := api.NewServer(cfg, prov, rt.loop)
	// /readyz gates on the bus being consumed and on the data directory being
	// writable. Both handles live here, not in the api package, so they are
	// injected the same way the loop is; leaving them out would make those two
	// checks report "missing" and take a healthy gateway out of rotation.
	apiServer.SetBus(rt.bus)
	apiServer.SetDataDir(config.DefaultDataDir())
	apiServer.SetMetrics(rt.metrics)
	apiServer.SetSessionStore(rt.store)
	apiServer.SetScheduler(rt.scheduler)
	apiServer.SetTriggerService(rt.triggers)

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	host := "127.0.0.1"
	if cfg.API.Host != "" {
		host = cfg.API.Host
	}
	if *hostFlag != "" {
		host = *hostFlag
	}

	port := "8900"
	if cfg.API.Port > 0 {
		port = fmt.Sprintf("%d", cfg.API.Port)
	}
	if *portFlag != "" {
		port = *portFlag
	}

	addr := fmt.Sprintf("%s:%s", host, port)
	fmt.Printf("haosbot %s gateway starting HTTP server on http://%s (endpoints: /v1/chat/completions, /v1/models, /health, /metrics, /readyz, /a2a)\n", version, addr)

	// Publish the EFFECTIVE address back into the config before the server
	// starts. --host/--port override cfg.API for the listener, but the A2A agent
	// card is built from cfg.API (internal/a2a/handler.go reads cfg.API.Host and
	// cfg.API.Port), so without this a gateway started as
	//     haosbot gateway --port 41877
	// listened on 41877 while advertising http://127.0.0.1:8900/a2a — A2A
	// discovery pointed every peer at a port nothing was listening on. Observed
	// before this fix: card url = http://127.0.0.1:8900/a2a for a server bound to
	// 127.0.0.1:41877. api.NewServer above stored the same *config.Config, and
	// a2a.NewHandler runs inside apiServer.Start below, so writing here is seen
	// by the card.
	cfg.API.Host = host
	if n, convErr := strconv.Atoi(port); convErr == nil {
		cfg.API.Port = n
	}

	errCh := make(chan error, 2)
	go func() {
		if err := apiServer.Start(addr); err != nil && !errors.Is(err, http.ErrServerClosed) {
			errCh <- err
		}
	}()
	go func() { errCh <- rt.loop.Run(ctx) }()
	// Fire-and-forget, like the reference's `asyncio.create_task(
	// channels.start_all(), name="nanobot-channels")` (cli/gateway_runtime.py:941).
	// StartAll returns once every channel has finished, and a channel that fails
	// to start is recorded and logged by the manager instead of taking the
	// gateway down (manager.py:373-389), so its result must NOT be an errCh
	// value: a nil from here would otherwise end the gateway's select.
	go func() { _ = channelManager.StartAll(ctx) }()

	select {
	case <-ctx.Done():
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		_ = apiServer.Shutdown(shutdownCtx)
		return nil
	case err := <-errCh:
		return err
	}
}

func expandTilde(path string) (string, error) {
	if path == "" {
		return config.DefaultWorkspace(), nil
	}
	if path != "~" && !strings.HasPrefix(path, "~"+string(filepath.Separator)) {
		return path, nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("expand %q: %w", path, err)
	}
	if path == "~" {
		return home, nil
	}
	return filepath.Join(home, path[2:]), nil
}

func isWithin(child, parent string) bool {
	rel, err := filepath.Rel(parent, child)
	if err != nil {
		return false
	}
	return rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
}
