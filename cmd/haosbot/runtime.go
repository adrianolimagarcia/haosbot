package main

import (
	"bufio"
	"context"
	"errors"
	"flag"
	"fmt"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	micrographrag "github.com/adrianolimagarcia/micrographrag-go"

	"github.com/adrianolimagarcia/nanobot-go/internal/agent"
	"github.com/adrianolimagarcia/nanobot-go/internal/api"
	"github.com/adrianolimagarcia/nanobot-go/internal/bus"
	"github.com/adrianolimagarcia/nanobot-go/internal/config"
	"github.com/adrianolimagarcia/nanobot-go/internal/core"
	"github.com/adrianolimagarcia/nanobot-go/internal/prompt"
	"github.com/adrianolimagarcia/nanobot-go/internal/provider"
	"github.com/adrianolimagarcia/nanobot-go/internal/provider/openai"
	"github.com/adrianolimagarcia/nanobot-go/internal/session"
	"github.com/adrianolimagarcia/nanobot-go/internal/tools"
	"github.com/adrianolimagarcia/nanobot-go/internal/tools/builtin"
	// Aliased because buildRuntime has a local `workspace` string that would
	// otherwise shadow the package name.
	wsbootstrap "github.com/adrianolimagarcia/nanobot-go/internal/workspace"
)

// runtime is the assembled agent: config, bus, session store, provider, tools
// and the loop that ties them together.
type agentRuntime struct {
	cfg    *config.Config
	bus    *bus.Bus
	loop   *agent.Loop
	store  *session.Store
	closeF func()
}

// transcriptStore adapts *session.Store to agent.TranscriptStore.
type transcriptStore struct{ s *session.Store }

func (t transcriptStore) Open(key string) (agent.Transcript, error) {
	sess, err := t.s.Open(key)
	if err != nil {
		return nil, err
	}
	return sess, nil
}

// buildRuntime assembles the runtime from a loaded config.
func buildRuntime(cfg *config.Config) (*agentRuntime, error) {
	d := cfg.Agents.Defaults

	// The agent workspace is where the agent reads and writes files.
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

	// Session storage must live OUTSIDE the agent workspace.
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

	messageBus := bus.New(bus.Options{})

	// MicroGraphRAG is a derived index. It is optional at runtime and uses FTS5
	// by default; vector embeddings remain disabled unless explicitly provisioned.
	graphCfg := micrographrag.DefaultConfig(filepath.Join(config.DefaultDataDir(), "agent.db"))
	graphCfg.EnableVector = false
	graphCfg.EnableEmbeddingWorker = false
	graphStore, graphErr := micrographrag.Open(context.Background(), graphCfg, nil)
	if graphErr != nil {
		fmt.Fprintf(os.Stderr, "haosbot: graph memory disabled: %v\n", graphErr)
		graphStore = nil
	}

	loop, err := agent.NewLoop(agent.LoopConfig{
		Bus:                 messageBus,
		Store:               transcriptStore{store},
		Provider:            prov,
		Tools:               registry,
		Prompt:              prompt.New(workspace),
		Model:               model,
		MaxTokens:           d.MaxTokens,
		ContextWindowTokens: d.ContextWindowTokens,
		Temperature:         float64(d.Temperature),
		Workspace:           workspace,
		MaxIterations:       d.MaxToolIterations,
		MaxToolResultChars:  d.MaxToolResultChars,
		SequentialTools:     false,
		GraphMemory:         graphStore,
		GraphMemoryMaxChars: 6000,
	})
	if err != nil {
		messageBus.Close()
		return nil, fmt.Errorf("build agent loop: %w", err)
	}

	return &agentRuntime{
		cfg:   cfg,
		bus:   messageBus,
		loop:  loop,
		store: store,
		closeF: func() {
			if graphStore != nil {
				_ = graphStore.Close()
			}
			messageBus.Close()
		},
	}, nil
}

func (r *agentRuntime) Close() {
	if r.closeF != nil {
		r.closeF()
	}
}

// resolveProvider picks the provider and model from config.
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
		APIKey:  apiKey,
		BaseURL: baseURL,
		Model:   model,
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

// ---------------------------------------------------------------------------
// Commands
// ---------------------------------------------------------------------------

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

// cmdGateway runs the loop as a service until interrupted.
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

	prov, _, err := resolveProvider(cfg)
	if err != nil {
		return err
	}

	apiServer := api.NewServer(cfg, prov, rt.loop)

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
	fmt.Printf("haosbot %s gateway starting HTTP server on http://%s (endpoints: /v1/chat/completions, /v1/models, /health)\n", version, addr)

	errCh := make(chan error, 2)
	go func() {
		if err := apiServer.Start(addr); err != nil && !errors.Is(err, http.ErrServerClosed) {
			errCh <- err
		}
	}()

	go func() { errCh <- rt.loop.Run(ctx) }()

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

// ---------------------------------------------------------------------------
// helpers
// ---------------------------------------------------------------------------

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
