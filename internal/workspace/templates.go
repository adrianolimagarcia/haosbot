// Package workspace ports the workspace bootstrap that the reference performs
// on CLI/gateway startup: sync_workspace_templates (utils/helpers.py:897-946).
//
// The port previously had NO bootstrap at all. cmd/nanobot/runtime.go only did
// os.MkdirAll on the workspace, so a workspace created by nanobot-go alone was
// empty, while the Python CLI creates seven files, a skills/ directory and a
// git store. The project's premise is that both runtimes share ~/.nanobot/, so
// a workspace already created by the Python CLI is the common case and the gap
// was invisible — but it is a real divergence on a fresh install, which is
// exactly the case the prompt guards were also found to mishandle.
//
// WHERE THIS LIVES — a package of its own rather than internal/prompt:
// the function needs two unrelated things, the embedded template BYTES
// (internal/prompt, which owns the embed.FS) and the git store
// (internal/gitstore). Putting it in internal/prompt would make the prompt
// builder depend on git; putting it in internal/gitstore would make the git
// store depend on the prompt templates. A small package that imports both keeps
// the dependency direction honest. internal/gitstore does not import
// internal/prompt, so there is no cycle.
package workspace

import (
	"fmt"
	"os"
	"path/filepath"

	"github.com/adrianolimagarcia/nanobot-go/internal/gitstore"
	"github.com/adrianolimagarcia/nanobot-go/internal/prompt"
)

// topLevelTemplates are the templates the reference copies into the workspace
// root. sync_workspace_templates filters the templates directory for top-level
// entries whose name ends in ".md" and does not start with "."; of the seven
// files in the reference's templates/ root, exactly these four pass that filter
// (agent/, legacy/, memory/ and prompts/ are directories, and __init__.py is
// not .md).
//
// The list is explicit rather than a walk of the embed.FS so that adding a
// bundled template is a visible decision, matching the precedent set by
// internal/prompt's own embed directive list.
//
// ORDER: the reference iterates `pkg_files("nanobot") / "templates"` with
// Path.iterdir(), i.e. os.scandir order, which is filesystem-dependent and NOT
// guaranteed. Observed on this checkout it is alphabetical, and embed.FS also
// returns lexically sorted entries, so the two agree here. The port is
// deliberately deterministic; a caller comparing against the reference should
// compare the returned SET, or accept that a different filesystem could order
// the reference's result differently.
var topLevelTemplates = []string{
	"AGENTS.md",
	"HEARTBEAT.md",
	"SOUL.md",
	"USER.md",
}

// trackedFiles is the tracked_files argument of the GitStore the reference
// constructs (utils/helpers.py:940-944). Note it is three files, not four:
// memory/.dream_cursor is NOT among them.
var trackedFiles = []string{
	"SOUL.md",
	"USER.md",
	"memory/MEMORY.md",
}

// SyncTemplates copies the bundled workspace templates into workspace, creating
// only files that do not already exist, and initializes the memory git store.
//
// It mirrors sync_workspace_templates (utils/helpers.py:897-946) and returns the
// workspace-relative paths it created, in creation order. A second call on the
// same workspace returns an empty slice — the function is idempotent because
// every destination is skipped once it exists.
//
// When silent is false each created path is printed as "  Created <path>",
// which is the plain-text rendering of the reference's rich
// `[dim]Created {name}[/dim]`. The port does not reproduce rich's ANSI dim
// styling.
//
// Errors: a missing bundled template is an error. The reference reads the
// template before testing whether the destination exists, so a missing source
// raises even when the destination is present; the reference's own templates
// always exist, so this is only reachable if the embed set is edited
// inconsistently. Failure to initialize the git store is NOT an error — the
// reference wraps that call in a bare `except Exception` and logs it, because a
// workspace without version control is still usable.
func SyncTemplates(workspace string, silent bool) ([]string, error) {
	added := make([]string, 0, len(topLevelTemplates)+3)

	// _write(src, dest) — note the reference reads src BEFORE the exists()
	// check, so a missing template raises even for an existing destination.
	write := func(name string, dest string) error {
		content, ok := prompt.BundledTemplate(name)
		if !ok {
			return fmt.Errorf("workspace: bundled template %q is missing from the embed set", name)
		}
		return writeIfMissing(workspace, dest, content, &added)
	}

	for _, name := range topLevelTemplates {
		if err := write(name, filepath.Join(workspace, name)); err != nil {
			return nil, err
		}
	}
	if err := write("memory/MEMORY.md", filepath.Join(workspace, "memory", "MEMORY.md")); err != nil {
		return nil, err
	}
	if err := write("prompts/README.md", filepath.Join(workspace, "prompts", "README.md")); err != nil {
		return nil, err
	}
	// _write(None, workspace / "memory" / "history.jsonl") — an EMPTY file, not a
	// template. The empty journal is what makes the first append work without a
	// separate creation step.
	if err := writeIfMissing(workspace, filepath.Join(workspace, "memory", "history.jsonl"), "", &added); err != nil {
		return nil, err
	}

	// (workspace / "skills").mkdir(exist_ok=True) — created unconditionally and
	// NOT reported in the return value, unlike the files above.
	if err := os.MkdirAll(filepath.Join(workspace, "skills"), 0o755); err != nil {
		return nil, fmt.Errorf("workspace: create skills dir: %w", err)
	}

	if len(added) > 0 && !silent {
		for _, name := range added {
			fmt.Printf("  Created %s\n", name)
		}
	}

	// Initialize git for memory version control. Errors are swallowed: the
	// reference logs and continues, so a git failure must not stop startup.
	if _, err := gitstore.New(workspace, trackedFiles).Init(); err != nil {
		// Deliberately ignored; see the doc comment. Reported by the caller's
		// logger in the reference, and this package has none.
		_ = err
	}

	return added, nil
}

// writeIfMissing implements the reference's inner _write: skip an existing
// destination, otherwise create parent directories and write content, recording
// the workspace-relative path.
func writeIfMissing(workspace, dest, content string, added *[]string) error {
	if _, err := os.Stat(dest); err == nil {
		return nil
	}
	if err := os.MkdirAll(filepath.Dir(dest), 0o755); err != nil {
		return fmt.Errorf("workspace: create parent of %s: %w", dest, err)
	}
	if err := os.WriteFile(dest, []byte(content), 0o644); err != nil {
		return fmt.Errorf("workspace: write %s: %w", dest, err)
	}
	rel, err := filepath.Rel(workspace, dest)
	if err != nil {
		return fmt.Errorf("workspace: relativize %s: %w", dest, err)
	}
	*added = append(*added, filepath.ToSlash(rel))
	return nil
}
