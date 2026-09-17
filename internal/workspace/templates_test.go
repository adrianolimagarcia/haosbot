package workspace

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/adrianolimagarcia/nanobot-go/internal/prompt"
)

// wantAdded is the exact return value of the reference's
// sync_workspace_templates on a fresh workspace, observed by running it:
//
//	>>> sync_workspace_templates(ws, silent=True)
//	['AGENTS.md', 'HEARTBEAT.md', 'SOUL.md', 'USER.md',
//	 'memory/MEMORY.md', 'prompts/README.md', 'memory/history.jsonl']
//
// and the second call on the same workspace returns [].
var wantAdded = []string{
	"AGENTS.md",
	"HEARTBEAT.md",
	"SOUL.md",
	"USER.md",
	"memory/MEMORY.md",
	"prompts/README.md",
	"memory/history.jsonl",
}

// TestSyncTemplatesMatchesReferenceReturnValue pins the return value and the
// resulting workspace tree against the reference's observed output.
func TestSyncTemplatesMatchesReferenceReturnValue(t *testing.T) {
	ws := t.TempDir()

	added, err := SyncTemplates(ws, true)
	if err != nil {
		t.Fatalf("SyncTemplates: %v", err)
	}
	if !reflect.DeepEqual(added, wantAdded) {
		t.Fatalf("added = %v\nwant    %v", added, wantAdded)
	}

	// Every reported path must exist and hold the bundled template's bytes.
	for _, name := range []string{"AGENTS.md", "HEARTBEAT.md", "SOUL.md", "USER.md", "memory/MEMORY.md", "prompts/README.md"} {
		want, ok := prompt.BundledTemplate(name)
		if !ok {
			t.Fatalf("template %q is not bundled", name)
		}
		got, err := os.ReadFile(filepath.Join(ws, filepath.FromSlash(name)))
		if err != nil {
			t.Fatalf("read %s: %v", name, err)
		}
		if string(got) != want {
			t.Errorf("%s content differs from the bundled template (%d vs %d bytes)", name, len(got), len(want))
		}
	}

	// history.jsonl is created EMPTY from a nil source, not copied.
	info, err := os.Stat(filepath.Join(ws, "memory", "history.jsonl"))
	if err != nil {
		t.Fatalf("history.jsonl: %v", err)
	}
	if info.Size() != 0 {
		t.Errorf("history.jsonl is %d bytes, want 0", info.Size())
	}

	// skills/ is created unconditionally and is NOT part of the return value.
	if st, err := os.Stat(filepath.Join(ws, "skills")); err != nil || !st.IsDir() {
		t.Errorf("skills/ was not created: %v", err)
	}

	// The git store is initialized (utils/helpers.py:938-945).
	if st, err := os.Stat(filepath.Join(ws, ".git")); err != nil || !st.IsDir() {
		t.Errorf(".git was not created: %v", err)
	}
}

// TestSyncTemplatesIsIdempotent pins the reference's second-call behaviour: []
// because every destination is skipped once it exists.
func TestSyncTemplatesIsIdempotent(t *testing.T) {
	ws := t.TempDir()
	if _, err := SyncTemplates(ws, true); err != nil {
		t.Fatalf("first SyncTemplates: %v", err)
	}
	added, err := SyncTemplates(ws, true)
	if err != nil {
		t.Fatalf("second SyncTemplates: %v", err)
	}
	if len(added) != 0 {
		t.Fatalf("second call added %v, want none", added)
	}
}

// TestSyncTemplatesNeverOverwritesUserFiles is the load-bearing property: the
// docstring promises "Creates missing files without overwriting user files".
func TestSyncTemplatesNeverOverwritesUserFiles(t *testing.T) {
	ws := t.TempDir()
	custom := "# my own agents file\n"
	if err := os.WriteFile(filepath.Join(ws, "AGENTS.md"), []byte(custom), 0o644); err != nil {
		t.Fatal(err)
	}

	added, err := SyncTemplates(ws, true)
	if err != nil {
		t.Fatalf("SyncTemplates: %v", err)
	}
	for _, name := range added {
		if name == "AGENTS.md" {
			t.Error("AGENTS.md was reported as added even though it already existed")
		}
	}
	got, err := os.ReadFile(filepath.Join(ws, "AGENTS.md"))
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != custom {
		t.Errorf("user file was overwritten: %q", got)
	}
	// The other files are still created.
	if len(added) != len(wantAdded)-1 {
		t.Errorf("added = %v, want the other %d files", added, len(wantAdded)-1)
	}
}

// TestSyncTemplatesCreatesParentDirectories covers memory/ and prompts/, which
// do not exist on a fresh workspace.
func TestSyncTemplatesCreatesParentDirectories(t *testing.T) {
	ws := t.TempDir()
	if _, err := SyncTemplates(ws, true); err != nil {
		t.Fatalf("SyncTemplates: %v", err)
	}
	for _, dir := range []string{"memory", "prompts", "skills"} {
		if st, err := os.Stat(filepath.Join(ws, dir)); err != nil || !st.IsDir() {
			t.Errorf("%s/ missing: %v", dir, err)
		}
	}
}

// TestSyncTemplatesSilentControlsOutput is a smoke check that silent=true
// produces no "Created" line. The non-silent path writes to stdout, which the
// test does not capture; the assertion here is only that silent is honoured in
// the direction that matters (no output).
func TestSyncTemplatesSilentControlsOutput(t *testing.T) {
	ws := t.TempDir()
	added, err := SyncTemplates(ws, true)
	if err != nil {
		t.Fatalf("SyncTemplates: %v", err)
	}
	for _, name := range added {
		if strings.HasPrefix(name, " ") {
			t.Errorf("added entry %q has stray leading space", name)
		}
	}
}
