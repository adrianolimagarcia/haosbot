package main

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"flag"
	"github.com/adrianolimagarcia/nanobot-go/internal/api"
	"net/http"

	"github.com/adrianolimagarcia/nanobot-go/internal/agent"
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
//
// The adapter exists because Store.Open returns the concrete *session.Session
// while the loop's interface requires the agent.Transcript interface; Go
// requires the return types to match exactly, so a one-method shim is needed.
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

	// sync_workspace_templates (utils/helpers.py:897-946), called at startup by
	// every reference entry point (cli/agent.py:154, cli/commands.py:233,385,
	// cli/gateway_runtime.py:410, cli/webui.py:178) with silent defaulting to
	// false. It creates the missing workspace files without overwriting user
	// files, makes skills/, writes an empty memory/history.jsonl and initializes
	// the memory git store. The port previously did none of this, so a workspace
	// created by nanobot-go alone was empty.
	if _, err := wsbootstrap.SyncTemplates(workspace, false); err != nil {
		return nil, fmt.Errorf("sync workspace templates: %w", err)
	}

	// Session storage must live OUTSIDE the agent workspace. The reference
	// raises RuntimeError when the session root is inside the workspace,
	// because the agent could otherwise read or corrupt its own transcripts.
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

	registry := tools.NewRegistry()
	builtin.Register(registry, builtin.Config{
		Files: builtin.PathPolicy{Workspace: workspace, AllowedDir: workspace},
		Exec: builtin.ExecOptions{
			Workspace:           workspace,
			RestrictToWorkspace: true,
			DefaultTimeout:      60 * time.Second,
			MaxTimeout:          600 * time.Second,
		},
	})

	messageBus := bus.New(bus.Options{})

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
	})
	if err != nil {
		messageBus.Close()
		return nil, fmt.Errorf("build agent loop: %w", err)
	}

	return &agentRuntime{
		cfg:    cfg,
		bus:    messageBus,
		loop:   loop,
		store:  store,
		closeF: messageBus.Close,
	}, nil
}

func (r *agentRuntime) Close() {
	if r.closeF != nil {
		r.closeF()
	}
}

// resolveProvider picks the provider and model from config.
//
// This is a deliberately small subset of the reference's router: it honours an
// explicit provider when configured, otherwise it infers the provider from the
// "provider/model" prefix, and finally falls back to the OpenAI-compatible
// endpoint. Providers beyond OpenAI-compatible chat completions are NOT
// implemented — see docs/COMPATIBILITY.md.
func resolveProvider(cfg *config.Config) (provider.Provider, string, error) {
	d := cfg.Agents.Defaults
	model := d.Model
	providerName := strings.TrimSpace(d.Provider)

	// The reference model string is "<provider>/<model>"; the provider part is
	// a routing prefix and must NOT be sent as part of the model name.
	// Config.get_provider (config/schema.py) matches on that prefix, and the
	// provider receives only the remainder.
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
	// Strip the prefix whenever it names the provider we resolved, so
	// "openai/gpt-4o" sends model "gpt-4o".
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
		// Unknown names are treated as OpenAI-compatible when an apiBase is
		// present, which is how the reference handles user-defined providers.
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
		return errors.New("usage: nanobot run \"<message>\"")
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

	fmt.Printf("nanobot-go %s — type /help for commands, Ctrl-D to exit\n", version)

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
//
// This is the minimal gateway: it drains the bus and dispatches to the loop.
// Channels (Telegram, Discord, ...) are NOT implemented, so in practice only
// locally published messages are processed.
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
	fmt.Printf("nanobot-go %s gateway starting HTTP server on http://%s (endpoints: /v1/chat/completions, /v1/models, /health)\n", version, addr)

	errCh := make(chan error, 1)
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

// expandTilde resolves a leading "~" to the user home directory, matching
// nanobot.config.paths.expand_user. A bare "~" or "~/..." is expanded; "~user"
// is deliberately NOT expanded (the reference does not either).
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

// isWithin reports whether child is inside parent.
func isWithin(child, parent string) bool {
	rel, err := filepath.Rel(parent, child)
	if err != nil {
		return false
	}
	return rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
}
