// Command nanobot is a Go reimplementation of the nanobot agent runtime.
//
// It is designed to read and write the same ~/.nanobot/ directory as the
// Python implementation. See docs/COMPATIBILITY.md for the exact state of that
// compatibility — this is NOT yet a drop-in replacement.
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"text/tabwriter"
	"time"

	"github.com/adrianolimagarcia/nanobot-go/internal/agent"
	"github.com/adrianolimagarcia/nanobot-go/internal/bus"
	"github.com/adrianolimagarcia/nanobot-go/internal/prompt"
	"github.com/adrianolimagarcia/nanobot-go/internal/tools"
)

// version is the nanobot-go version. It is deliberately distinct from the
// Python upstream version (0.3.5) so the two are never confused in bug
// reports; compatibilityTarget records which upstream commit this port tracks.
const (
	version             = "0.1.0-dev"
	compatibilityTarget = "HKUDS/nanobot@1bb712d3488915ca4ed9ccc1a93067ff722f5ab9 (v0.3.5)"
)

func main() {
	if len(os.Args) < 2 {
		usage(os.Stderr)
		os.Exit(2)
	}

	switch os.Args[1] {
	case "version", "--version", "-v":
		printVersion()
	case "paths":
		if err := printPaths(); err != nil {
			fatal(err)
		}
	case "help", "--help", "-h":
		usage(os.Stdout)
	case "selftest":
		if err := selftest(os.Args[2:]); err != nil {
			fatal(err)
		}
	case "run":
		if err := cmdRun(os.Args[2:]); err != nil {
			fatal(err)
		}
	case "chat":
		if err := cmdChat(os.Args[2:]); err != nil {
			fatal(err)
		}
	case "gateway":
		if err := cmdGateway(os.Args[2:]); err != nil {
			fatal(err)
		}
	default:
		fmt.Fprintf(os.Stderr, "nanobot: unknown command %q\n\n", os.Args[1])
		usage(os.Stderr)
		os.Exit(2)
	}
}

func printVersion() {
	fmt.Printf("nanobot-go %s\n", version)
	fmt.Printf("go         %s %s/%s\n", runtime.Version(), runtime.GOOS, runtime.GOARCH)
	fmt.Printf("compat     %s\n", compatibilityTarget)
}

// printPaths shows the resolved runtime layout, mirroring
// nanobot/config/paths.py. It is the fastest way to confirm that nanobot-go
// and the Python nanobot would read the same files.
func printPaths() error {
	home, err := os.UserHomeDir()
	if err != nil {
		return fmt.Errorf("resolve home directory: %w", err)
	}
	dataDir := filepath.Join(home, ".nanobot")

	type row struct{ name, path, note string }
	rows := []row{
		{"config", filepath.Join(dataDir, "config.json"), "main configuration"},
		{"data", dataDir, "instance runtime data"},
		{"workspace", filepath.Join(dataDir, "workspace"), "agent workspace (profile, memory, skills)"},
		{"sessions", filepath.Join(dataDir, "sessions"), "session JSONL, one subdir per workspace id"},
		{"cron", filepath.Join(dataDir, "cron"), "scheduled jobs"},
		{"logs", filepath.Join(dataDir, "logs"), "log output"},
		{"media", filepath.Join(dataDir, "media"), "per-channel media"},
		{"webui", filepath.Join(dataDir, "webui"), "WebUI display threads"},
		{"cli_history", filepath.Join(dataDir, "history", "cli_history"), "shared CLI history"},
	}

	sort.Slice(rows, func(i, j int) bool { return rows[i].name < rows[j].name })

	w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintln(w, "NAME\tPATH\tEXISTS\tNOTE")
	for _, r := range rows {
		exists := "no"
		if _, err := os.Stat(r.path); err == nil {
			exists = "yes"
		}
		fmt.Fprintf(w, "%s\t%s\t%s\t%s\n", r.name, r.path, exists, r.note)
	}
	return w.Flush()
}

func usage(w *os.File) {
	fmt.Fprint(w, `nanobot-go — Go runtime for the nanobot agent framework

Usage:
  nanobot run "<msg>"    Send one message and print the reply
  nanobot chat           Interactive REPL
  nanobot gateway        Run the agent loop until interrupted
  nanobot version        Print version and compatibility target
  nanobot paths          Show the resolved ~/.nanobot/ layout
  nanobot selftest       Build the core runtime in-process and report sizes
  nanobot help           Show this message

The API key is read from config.json only. The reference's
Config.get_api_key (config/schema.py:636-644) returns providers.<name>.apiKey
with no environment fallback, so this port does not read OPENAI_API_KEY
either. Configure the key in ~/.nanobot/config.json.

Compatibility: this port currently implements the agent core only.
See docs/COMPATIBILITY.md before relying on it.
`)
}

// selftest constructs the core runtime in-process, reports what it built, and
// optionally holds the process open so external tooling can sample resident
// memory.
//
// The hold exists because a process that exits in under a millisecond cannot
// be sampled by an external RSS poller; without it, memory measurement
// silently reports zero. Use: nanobot selftest --hold 1s
func selftest(args []string) error {
	fs := flag.NewFlagSet("selftest", flag.ContinueOnError)
	hold := fs.Duration("hold", 0, "keep the process alive for this long after setup, for RSS sampling")
	if err := fs.Parse(args); err != nil {
		return err
	}

	registry := tools.NewRegistry()
	registry.Register(&selftestTool{name: "read_file", readOnly: true})
	registry.Register(&selftestTool{name: "write_file", readOnly: false})

	messageBus := bus.New(bus.Options{})
	defer messageBus.Close()

	builder := prompt.New(filepath.Join(homeDir(), ".nanobot", "workspace"))
	sysPrompt := builder.BuildSystemPrompt("cli", nil, "", true)

	runner := agent.NewRunner()
	_ = runner

	var mem runtime.MemStats
	runtime.ReadMemStats(&mem)

	fmt.Printf("tools registered   %d\n", registry.Len())
	fmt.Printf("tool schemas       %d\n", len(registry.Schemas()))
	fmt.Printf("system prompt      %d bytes\n", len(sysPrompt))
	fmt.Printf("heap in use        %.2f MB\n", float64(mem.HeapInuse)/(1<<20))
	fmt.Printf("heap allocated     %.2f MB\n", float64(mem.HeapAlloc)/(1<<20))
	fmt.Printf("goroutines         %d\n", runtime.NumGoroutine())

	if *hold > 0 {
		fmt.Printf("holding %s for RSS sampling (pid %d)\n", *hold, os.Getpid())
		time.Sleep(*hold)
	}
	return nil
}

// selftestTool is a minimal tool used only to size the registry.
type selftestTool struct {
	tools.ReadOnlyBase
	name     string
	readOnly bool
}

func (t *selftestTool) Name() string { return t.name }
func (t *selftestTool) Description() string {
	return "selftest tool"
}
func (t *selftestTool) Parameters() json.RawMessage {
	return json.RawMessage(`{"type":"object","properties":{"path":{"type":"string"}},"required":["path"]}`)
}
func (t *selftestTool) Execute(context.Context, json.RawMessage) (tools.Result, error) {
	return tools.OK("ok"), nil
}

func homeDir() string {
	if h, err := os.UserHomeDir(); err == nil {
		return h
	}
	return "."
}

func fatal(err error) {
	fmt.Fprintf(os.Stderr, "nanobot: %v\n", err)
	os.Exit(1)
}
