package memory

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/adrianolimagarcia/nanobot-go/internal/core"
	"github.com/adrianolimagarcia/nanobot-go/internal/session"
	"github.com/adrianolimagarcia/nanobot-go/internal/textutil"
	"github.com/adrianolimagarcia/nanobot-go/internal/workspaceprompt"
)

// fakeDiffer is the GitStore slice DreamContentDiff needs.
type fakeDiffer struct {
	initialized bool
	summary     string
	err         error

	calls int
	paths []string
}

func (f *fakeDiffer) IsInitialized() bool { return f.initialized }

func (f *fakeDiffer) SummarizeWorkingTree(paths []string) (string, error) {
	f.calls++
	f.paths = append([]string(nil), paths...)
	return f.summary, f.err
}

// TestDreamContentDiff pins dream_content_diff (memory.py:565-573): an
// uninitialized repository is "" and never reaches summarize_working_tree.
func TestDreamContentDiff(t *testing.T) {
	t.Run("no_differ", func(t *testing.T) {
		store := newStore(t)
		got, err := store.DreamContentDiff()
		if err != nil || got != "" {
			t.Fatalf("got %q, %v; want \"\", nil", got, err)
		}
	})

	t.Run("not_initialized", func(t *testing.T) {
		store := newStore(t)
		differ := &fakeDiffer{initialized: false, summary: "should not be used"}
		store.SetDreamDiffer(differ)
		got, err := store.DreamContentDiff()
		if err != nil || got != "" {
			t.Fatalf("got %q, %v; want \"\", nil", got, err)
		}
		if differ.calls != 0 {
			t.Errorf("summarize was called %d times on an uninitialized repo", differ.calls)
		}
	})

	t.Run("initialized", func(t *testing.T) {
		store := newStore(t)
		differ := &fakeDiffer{initialized: true, summary: "SOUL.md: +1 -0"}
		store.SetDreamDiffer(differ)
		got, err := store.DreamContentDiff()
		if err != nil {
			t.Fatalf("DreamContentDiff: %v", err)
		}
		if got != "SOUL.md: +1 -0" {
			t.Errorf("got %q", got)
		}
		if want := strings.Join(DreamContentPaths, ","); strings.Join(differ.paths, ",") != want {
			t.Errorf("paths = %v, want %v", differ.paths, DreamContentPaths)
		}
		// The returned slice must be a copy: a caller mutating it must not be
		// able to change the package-level list.
		differ.paths[0] = "MUTATED"
		if DreamContentPaths[0] != "SOUL.md" {
			t.Errorf("DreamContentPaths was mutated through the differ: %v", DreamContentPaths)
		}
	})

	t.Run("error_propagates", func(t *testing.T) {
		store := newStore(t)
		boom := errors.New("boom")
		store.SetDreamDiffer(&fakeDiffer{initialized: true, err: boom})
		if _, err := store.DreamContentDiff(); !errors.Is(err, boom) {
			t.Fatalf("err = %v, want %v", err, boom)
		}
	})
}

// TestBuildDreamCommitMessage pins the empty-diff degradation: the bare prefix
// is what makes auto_commit a no-op.
func TestBuildDreamCommitMessage(t *testing.T) {
	cases := []struct{ prefix, body, want string }{
		{"dream: manual run", "SOUL.md: +1 -0", "dream: manual run\n\nSOUL.md: +1 -0"},
		{"dream: manual run", "", "dream: manual run"},
		{"dream: manual run", "   ", "dream: manual run"},
		{"dream: manual run", "\n\t ", "dream: manual run"},
		{"dream: manual run", "  SOUL.md: +1 -0  ", "dream: manual run\n\nSOUL.md: +1 -0"},
		{"", "body", "\n\nbody"},
	}
	for _, tc := range cases {
		if got := BuildDreamCommitMessage(tc.prefix, tc.body); got != tc.want {
			t.Errorf("BuildDreamCommitMessage(%q, %q) = %q, want %q", tc.prefix, tc.body, got, tc.want)
		}
	}

	// Python's str.strip(), not strings.TrimSpace: U+001C is whitespace to
	// Python only, so this body is EMPTY to the reference and the prefix is
	// returned bare.
	if got := BuildDreamCommitMessage("p", "\x1c"); got != "p" {
		t.Errorf("U+001C was not stripped: %q", got)
	}
}

// TestDreamSessionKeyShape pins the key format without pinning the clock.
func TestDreamSessionKeyShape(t *testing.T) {
	key := DreamSessionKey()
	if !strings.HasPrefix(key, "dream:") {
		t.Fatalf("key %q has no dream: prefix", key)
	}
	stamp := strings.TrimPrefix(key, "dream:")
	if len(stamp) != len("20060102-150405") {
		t.Fatalf("stamp %q has length %d, want 15", stamp, len(stamp))
	}
	if _, err := time.Parse("20060102-150405", stamp); err != nil {
		t.Fatalf("stamp %q does not parse: %v", stamp, err)
	}
}

// TestDreamRunStatus pins dream_run_completed / dream_incompletion_reason.
//
// A nil response and a nil metadata map both mean "missing response metadata";
// an EMPTY metadata map is a different answer, because Python's isinstance({},
// dict) is True and .get("_stop_reason", "unknown") then yields "unknown".
func TestDreamRunStatus(t *testing.T) {
	cases := []struct {
		name      string
		resp      *core.OutboundMessage
		completed bool
		reason    string
	}{
		{"completed", &core.OutboundMessage{Metadata: map[string]any{"_stop_reason": "completed"}}, true, "stop_reason: completed"},
		{"other_reason", &core.OutboundMessage{Metadata: map[string]any{"_stop_reason": "max_iterations"}}, false, "stop_reason: max_iterations"},
		{"null_reason", &core.OutboundMessage{Metadata: map[string]any{"_stop_reason": nil}}, false, "stop_reason: None"},
		{"int_reason", &core.OutboundMessage{Metadata: map[string]any{"_stop_reason": 123}}, false, "stop_reason: 123"},
		{"empty_metadata", &core.OutboundMessage{Metadata: map[string]any{}}, false, "stop_reason: unknown"},
		{"nil_metadata", &core.OutboundMessage{}, false, "stop_reason: missing response metadata"},
		{"nil_response", nil, false, "stop_reason: missing response metadata"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := DreamRunCompleted(tc.resp); got != tc.completed {
				t.Errorf("DreamRunCompleted = %v, want %v", got, tc.completed)
			}
			if got := DreamIncompletionReason(tc.resp); got != tc.reason {
				t.Errorf("DreamIncompletionReason = %q, want %q", got, tc.reason)
			}
		})
	}
}

// TestPruneDreamSessionsKeepsMostRecent is the reference's own scenario
// (tests/agent/test_dream_session.py): 15 Dream sessions, keep 10.
func TestPruneDreamSessionsKeepsMostRecent(t *testing.T) {
	store := session.NewStore("", t.TempDir())
	dir := store.Dir()
	base := time.Now().Add(-time.Hour)

	var dreamPaths []string
	for i := 0; i < 15; i++ {
		key := fmt.Sprintf("dream:20260528-%06d", 100000+i)
		path := writeSessionFile(t, dir, key)
		stamp := base.Add(time.Duration(i) * time.Second)
		if err := os.Chtimes(path, stamp, stamp); err != nil {
			t.Fatalf("chtimes: %v", err)
		}
		dreamPaths = append(dreamPaths, path)
	}
	normalPath := writeSessionFile(t, dir, "telegram:123")
	// A retired spelling: the stem is not canonical base64, so it decodes to
	// nothing and must never be considered.
	legacyPath := filepath.Join(dir, "dream_20260713-095959.jsonl")
	if err := os.WriteFile(legacyPath, []byte("{}\n"), 0o644); err != nil {
		t.Fatalf("write legacy: %v", err)
	}

	if err := PruneDreamSessions(store, 10); err != nil {
		t.Fatalf("PruneDreamSessions: %v", err)
	}

	for i, path := range dreamPaths {
		_, err := os.Stat(path)
		if i < 5 && err == nil {
			t.Errorf("dream session %d should have been pruned", i)
		}
		if i >= 5 && err != nil {
			t.Errorf("dream session %d should have survived: %v", i, err)
		}
	}
	if _, err := os.Stat(normalPath); err != nil {
		t.Errorf("non-dream session was touched: %v", err)
	}
	if _, err := os.Stat(legacyPath); err != nil {
		t.Errorf("legacy-named file was touched: %v", err)
	}
}

func TestPruneDreamSessionsNoops(t *testing.T) {
	t.Run("empty_dir", func(t *testing.T) {
		store := session.NewStore("", t.TempDir())
		if err := PruneDreamSessions(store, 10); err != nil {
			t.Fatalf("PruneDreamSessions: %v", err)
		}
	})
	t.Run("under_limit", func(t *testing.T) {
		store := session.NewStore("", t.TempDir())
		for i := 0; i < 3; i++ {
			writeSessionFile(t, store.Dir(), fmt.Sprintf("dream:20260528-%06d", 100000+i))
		}
		if err := PruneDreamSessions(store, 10); err != nil {
			t.Fatalf("PruneDreamSessions: %v", err)
		}
		entries, err := os.ReadDir(store.Dir())
		if err != nil {
			t.Fatalf("readdir: %v", err)
		}
		jsonl := 0
		for _, e := range entries {
			if strings.HasSuffix(e.Name(), ".jsonl") {
				jsonl++
			}
		}
		if jsonl != 3 {
			t.Errorf("got %d session files, want 3", jsonl)
		}
	})
	t.Run("negative_keep_prunes_all", func(t *testing.T) {
		// The reference slices with [: max(0, len - keep)], and Python clamps
		// an over-long slice bound instead of raising. A Go port that slices
		// naively panics here.
		store := session.NewStore("", t.TempDir())
		for i := 0; i < 3; i++ {
			writeSessionFile(t, store.Dir(), fmt.Sprintf("dream:20260528-%06d", 100000+i))
		}
		if err := PruneDreamSessions(store, -1); err != nil {
			t.Fatalf("PruneDreamSessions: %v", err)
		}
		entries, err := os.ReadDir(store.Dir())
		if err != nil {
			t.Fatalf("readdir: %v", err)
		}
		for _, e := range entries {
			if strings.HasSuffix(e.Name(), ".jsonl") {
				t.Errorf("session %s survived a negative keep", e.Name())
			}
		}
	})
}

func writeSessionFile(t *testing.T, dir, key string) string {
	t.Helper()
	path := filepath.Join(dir, session.StorageKey(key)+".jsonl")
	doc := fmt.Sprintf("{\"_type\": \"metadata\", \"key\": %q}\n", key)
	if err := os.WriteFile(path, []byte(doc), 0o644); err != nil {
		t.Fatalf("write session %s: %v", key, err)
	}
	return path
}

// TestBuildDreamTools checks the wiring of the four tools build_dream_tools
// registers (memory.py:592-614), including their observable ordering.
func TestBuildDreamTools(t *testing.T) {
	store := newStore(t)
	registry, err := store.BuildDreamTools()
	if err != nil {
		t.Fatalf("BuildDreamTools: %v", err)
	}

	for _, name := range []string{"read_file", "edit_file", "apply_patch", "write_file"} {
		if _, ok := registry.Get(name); !ok {
			t.Errorf("tool %q is missing", name)
		}
	}
	if registry.Len() != 4 {
		t.Errorf("registry has %d tools, want 4", registry.Len())
	}
	// Registration order mirrors build_dream_tools (memory.py:592-614), which is
	// observable through Registry.Names == the reference's tool_names
	// (registry.py:204-206, list(self._tools.keys())).
	if got := strings.Join(registry.Names(), ","); got != "read_file,edit_file,apply_patch,write_file" {
		t.Errorf("registration order = %s, want read_file,edit_file,apply_patch,write_file", got)
	}
	// The advertised order is sorted by Registry.Schemas, matching
	// get_definitions (registry.py:86-108). Verified by running the reference:
	// build_dream_tools().get_definitions() yields
	// [apply_patch, edit_file, read_file, write_file].
	var advertised []string
	for _, sc := range registry.Schemas() {
		advertised = append(advertised, sc.Name)
	}
	if got := strings.Join(advertised, ","); got != "apply_patch,edit_file,read_file,write_file" {
		t.Errorf("advertised order = %s, want apply_patch,edit_file,read_file,write_file", got)
	}

	skillsDir := filepath.Join(store.workspace, "skills")
	if info, err := os.Stat(skillsDir); err != nil || !info.IsDir() {
		t.Errorf("skills directory was not created: %v", err)
	}

	// The policy must let Dream edit the three durable memory files even
	// though they live outside skills/.
	writeTool, ok := registry.Get("write_file")
	if !ok {
		t.Fatal("write_file missing")
	}
	memoryFile, _, soulFile, userFile, _, _ := store.Paths()
	for _, path := range []string{memoryFile, soulFile, userFile} {
		raw, err := json.Marshal(map[string]any{"path": path, "content": "written\n"})
		if err != nil {
			t.Fatalf("marshal: %v", err)
		}
		result, err := writeTool.Execute(context.Background(), raw)
		if err != nil {
			t.Fatalf("execute write_file on %s: %v", path, err)
		}
		if result.IsError {
			t.Errorf("write_file refused %s: %s", path, result.Content)
		}
		if data, err := os.ReadFile(path); err != nil || string(data) != "written\n" {
			t.Errorf("%s = %q, %v", path, data, err)
		}
	}

	// A path outside both boundaries must be refused.
	outside := filepath.Join(store.workspace, "outside.txt")
	raw, err := json.Marshal(map[string]any{"path": outside, "content": "nope\n"})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	result, err := writeTool.Execute(context.Background(), raw)
	if err != nil {
		t.Fatalf("execute: %v", err)
	}
	if !result.IsError {
		t.Errorf("write outside the boundary succeeded: %s", result.Content)
	}
	if _, err := os.Stat(outside); err == nil {
		t.Error("the outside file was created")
	}
}

// appendEntries seeds a history journal and returns the cursor of the last one.
func appendEntries(t *testing.T, store *MemoryStore, contents ...string) int {
	t.Helper()
	last := 0
	for _, content := range contents {
		cursor, err := store.AppendHistory(content, nil, "")
		if err != nil {
			t.Fatalf("AppendHistory: %v", err)
		}
		last = cursor
	}
	return last
}

// TestBuildDreamPrompt pins build_dream_prompt (memory.py:543).
func TestBuildDreamPrompt(t *testing.T) {
	t.Run("nothing_unprocessed", func(t *testing.T) {
		store := newStore(t)
		_, _, ok, err := store.BuildDreamPrompt(20)
		if err != nil {
			t.Fatalf("BuildDreamPrompt: %v", err)
		}
		if ok {
			t.Error("ok = true with an empty journal")
		}
	})

	t.Run("cursor_advanced", func(t *testing.T) {
		store := newStore(t)
		last := appendEntries(t, store, "first", "second", "third")
		if err := store.SetLastDreamCursor(last); err != nil {
			t.Fatalf("SetLastDreamCursor: %v", err)
		}
		_, _, ok, err := store.BuildDreamPrompt(20)
		if err != nil {
			t.Fatalf("BuildDreamPrompt: %v", err)
		}
		if ok {
			t.Error("ok = true after the cursor consumed every entry")
		}
	})

	t.Run("batch_and_cursor", func(t *testing.T) {
		store := newStore(t)
		appendEntries(t, store, "first", "second", "third")
		prompt, cursor, ok, err := store.BuildDreamPrompt(20)
		if err != nil || !ok {
			t.Fatalf("BuildDreamPrompt: ok=%v err=%v", ok, err)
		}
		if cursor != 3 {
			t.Errorf("cursor = %d, want 3", cursor)
		}
		head := DefaultDreamPrompt() + "\n\n## Conversation History\n"
		if !strings.HasPrefix(prompt, head) {
			t.Errorf("prompt does not start with the template + history header:\n%q", prompt)
		}
		// Each line is "[<timestamp>] <content>": the history journal holds
		// consolidated text, not transcript messages.
		history := strings.TrimPrefix(prompt, head)
		if !strings.HasSuffix(history, "] third") {
			t.Errorf("history block = %q", history)
		}
		if lines := strings.Split(history, "\n"); len(lines) != 3 {
			t.Errorf("history block has %d lines, want 3: %q", len(lines), history)
		}
		for i, want := range []string{"first", "second", "third"} {
			if !strings.HasSuffix(strings.Split(history, "\n")[i], "] "+want) {
				t.Errorf("line %d = %q, want it to end with %q", i, strings.Split(history, "\n")[i], want)
			}
		}
	})

	t.Run("max_entries_limits_the_batch", func(t *testing.T) {
		store := newStore(t)
		appendEntries(t, store, "first", "second", "third")
		prompt, cursor, ok, err := store.BuildDreamPrompt(1)
		if err != nil || !ok {
			t.Fatalf("BuildDreamPrompt: ok=%v err=%v", ok, err)
		}
		if cursor != 1 {
			t.Errorf("cursor = %d, want 1", cursor)
		}
		if strings.Contains(prompt, "second") {
			t.Errorf("batch was not limited: %q", prompt)
		}
	})

	t.Run("negative_max_entries_drops_the_tail", func(t *testing.T) {
		// Python slices entries[: -1], which keeps all but the LAST entry —
		// not "unlimited".
		store := newStore(t)
		appendEntries(t, store, "first", "second", "third")
		prompt, cursor, ok, err := store.BuildDreamPrompt(-1)
		if err != nil || !ok {
			t.Fatalf("BuildDreamPrompt: ok=%v err=%v", ok, err)
		}
		if cursor != 2 {
			t.Errorf("cursor = %d, want 2", cursor)
		}
		if strings.Contains(prompt, "third") {
			t.Errorf("the last entry should have been dropped: %q", prompt)
		}
	})

	t.Run("empty_batch_is_an_error", func(t *testing.T) {
		// The reference raises IndexError from batch[-1]; an empty prompt
		// would silently advance the cursor past unseen entries.
		store := newStore(t)
		appendEntries(t, store, "first")
		if _, _, _, err := store.BuildDreamPrompt(0); err == nil {
			t.Error("max_entries=0 returned no error")
		}
		if _, _, _, err := store.BuildDreamPrompt(-5); err == nil {
			t.Error("max_entries=-5 returned no error")
		}
	})

	t.Run("entry_content_is_capped", func(t *testing.T) {
		store := newStore(t)
		appendEntries(t, store, strings.Repeat("x", 5000))
		prompt, _, ok, err := store.BuildDreamPrompt(20)
		if err != nil || !ok {
			t.Fatalf("BuildDreamPrompt: ok=%v err=%v", ok, err)
		}
		if !strings.HasSuffix(prompt, textutil.TruncatedSuffix) {
			t.Error("the entry was not truncated")
		}
		head := DefaultDreamPrompt() + "\n\n## Conversation History\n["
		if !strings.HasPrefix(prompt, head) {
			t.Fatalf("unexpected prompt head: %q", prompt[:60])
		}
		body := strings.TrimSuffix(strings.TrimPrefix(prompt, head), textutil.TruncatedSuffix)
		// "[<timestamp>] " then exactly 1000 characters of content.
		if got := strings.Count(body, "x"); got != dreamPromptEntryChars {
			t.Errorf("entry content has %d characters, want %d", got, dreamPromptEntryChars)
		}
	})
}

// TestDreamTemplateOverride pins _dream_template: a usable override wins, an
// empty one falls back, and an oversized one is truncated and flagged once.
func TestDreamTemplateOverride(t *testing.T) {
	t.Run("override_wins", func(t *testing.T) {
		store := newStore(t)
		path := store.DreamPromptFile()
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatalf("mkdir: %v", err)
		}
		if err := os.WriteFile(path, []byte("CUSTOM\n\n"), 0o644); err != nil {
			t.Fatalf("write override: %v", err)
		}
		if !store.HasDreamPromptOverride() {
			t.Error("HasDreamPromptOverride = false for a non-empty file")
		}
		appendEntries(t, store, "entry")
		prompt, _, ok, err := store.BuildDreamPrompt(20)
		if err != nil || !ok {
			t.Fatalf("BuildDreamPrompt: ok=%v err=%v", ok, err)
		}
		if !strings.HasPrefix(prompt, "CUSTOM\n\n## Conversation History\n") {
			t.Errorf("override not used: %q", prompt)
		}
		if store.dreamPromptOversizeLogged {
			t.Error("oversize flag set for a small override")
		}
	})

	t.Run("blank_override_falls_back", func(t *testing.T) {
		store := newStore(t)
		path := store.DreamPromptFile()
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatalf("mkdir: %v", err)
		}
		if err := os.WriteFile(path, []byte("   \n"), 0o644); err != nil {
			t.Fatalf("write override: %v", err)
		}
		if store.HasDreamPromptOverride() {
			t.Error("HasDreamPromptOverride = true for a whitespace-only file")
		}
		if got := store.dreamTemplate(); got != DefaultDreamPrompt() {
			t.Error("a blank override must fall back to the built-in prompt")
		}
	})

	t.Run("oversize_override_is_truncated_and_flagged_once", func(t *testing.T) {
		store := newStore(t)
		path := store.DreamPromptFile()
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatalf("mkdir: %v", err)
		}
		if err := os.WriteFile(path, []byte(strings.Repeat("z", workspaceprompt.MaxChars+500)), 0o644); err != nil {
			t.Fatalf("write override: %v", err)
		}
		got := store.dreamTemplate()
		if !strings.HasSuffix(got, textutil.TruncatedSuffix) {
			t.Error("the oversize override was not truncated")
		}
		if !store.dreamPromptOversizeLogged {
			t.Error("the one-shot oversize flag was not set")
		}
		// Second call: same text, flag already set (rate limit is one-shot).
		if again := store.dreamTemplate(); again != got {
			t.Error("the template changed between calls")
		}
		if !store.dreamPromptOversizeLogged {
			t.Error("the flag was cleared")
		}
	})
}

// TestDefaultDreamPromptUsesBuiltinSkillsDir pins the substitution source.
func TestDefaultDreamPromptUsesBuiltinSkillsDir(t *testing.T) {
	saved := BuiltinSkillsDir
	defer func() { BuiltinSkillsDir = saved }()

	BuiltinSkillsDir = filepath.Join(string(filepath.Separator), "opt", "nanobot", "skills")
	want := filepath.Join(BuiltinSkillsDir, "skill-creator", "SKILL.md")
	if got := SkillCreatorPath(); got != want {
		t.Errorf("SkillCreatorPath() = %q, want %q", got, want)
	}
	if !strings.Contains(DefaultDreamPrompt(), "`"+want+"` for format") {
		t.Error("the prompt does not reference the substituted path")
	}
}
