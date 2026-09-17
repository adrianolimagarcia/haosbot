package memory

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/adrianolimagarcia/nanobot-go/internal/core"
	"github.com/adrianolimagarcia/nanobot-go/internal/prompt"
	"github.com/adrianolimagarcia/nanobot-go/internal/session"
	"github.com/adrianolimagarcia/nanobot-go/internal/textutil"
	"github.com/adrianolimagarcia/nanobot-go/internal/tools"
	"github.com/adrianolimagarcia/nanobot-go/internal/tools/builtin"
	"github.com/adrianolimagarcia/nanobot-go/internal/workspaceprompt"
)

// dreamPromptName is the workspace-override stem: workspace_prompt_file(ws, "dream").
const dreamPromptName = "dream"

// DreamPromptFile returns the workspace Dream prompt override path
// (dream_prompt_file, memory.py:510-512).
func (s *MemoryStore) DreamPromptFile() string {
	return workspaceprompt.File(s.workspace, dreamPromptName)
}

// HasDreamPromptOverride reports whether the workspace supplies a usable Dream
// prompt override (has_dream_prompt_override, memory.py:514-515).
func (s *MemoryStore) HasDreamPromptOverride() bool {
	return workspaceprompt.Has(s.DreamPromptFile())
}

// SkillCreatorPath is str(BUILTIN_SKILLS_DIR / "skill-creator" / "SKILL.md")
// (memory.py:524): the path substituted into the Dream template.
func SkillCreatorPath() string {
	return filepath.Join(BuiltinSkillsDir, "skill-creator", "SKILL.md")
}

// BuiltinSkillsDir is BUILTIN_SKILLS_DIR (agent/skills.py:15):
//
//	Path(__file__).parent.parent / "skills"
//
// i.e. the skills/ directory shipped inside the nanobot package.
//
// DIVERGENCE (this port): there is no bundled-skills directory here — see the
// scope note in internal/tools/builtin/doc.go, which records the same gap for
// read_file's builtin-skill path. Nothing on disk corresponds to the
// reference's value, so this default is the closest structural analogue: the
// skills/ directory beside the running executable. It is a variable so the
// eventual CLI wiring can point it at wherever the port decides to ship (or
// not ship) bundled skills, and so the differential test can substitute the
// reference's own value and compare the rendered prompt byte for byte.
var BuiltinSkillsDir = defaultBuiltinSkillsDir()

func defaultBuiltinSkillsDir() string {
	exe, err := os.Executable()
	if err != nil {
		return "skills"
	}
	return filepath.Join(filepath.Dir(exe), "skills")
}

// DefaultDreamPrompt renders the built-in Dream prompt
// (default_dream_prompt, memory.py:517-525).
//
// The reference imports BUILTIN_SKILLS_DIR inside the staticmethod; here the
// path comes from the package-level BuiltinSkillsDir so it can be overridden.
func DefaultDreamPrompt() string {
	return prompt.RenderDreamPrompt(SkillCreatorPath())
}

// dreamTemplate returns the workspace override when one exists, else the
// built-in prompt (_dream_template, memory.py:527-541).
//
// The reference logs a one-shot warning when the override exceeded the cap and
// was truncated. This port has no logger (the same choice store.go makes for
// its three other one-shot flags), so the flag is still recorded — it is the
// rate-limit state the reference keeps — and nothing is printed.
func (s *MemoryStore) dreamTemplate() string {
	text, originalChars, ok := workspaceprompt.Load(s.DreamPromptFile(), workspaceprompt.MaxChars)
	if ok {
		if originalChars > workspaceprompt.MaxChars && !s.dreamPromptOversizeLogged {
			s.dreamPromptOversizeLogged = true
		}
		return text
	}
	return DefaultDreamPrompt()
}

// dreamPromptEntryChars is the per-entry cap in the history block
// (truncate_text(e["content"], 1000), memory.py:558).
const dreamPromptEntryChars = 1000

// BuildDreamPrompt builds the Dream prompt with unprocessed history context
// (build_dream_prompt, memory.py:543-563).
//
// It returns ok=false when there is nothing unprocessed, which is the
// reference's None; lastCursor is then meaningless. The current contents of the
// durable memory files reach Dream through the normal agent system context,
// not through this prompt.
//
// max_entries reproduces Python's `entries[:max_entries]` slicing, negative
// values included: -1 drops the last entry rather than meaning "unlimited".
// When the slice is empty the reference raises IndexError from `batch[-1]`;
// this returns an error instead of an empty prompt, because a silent empty
// batch would advance the cursor past entries that were never shown.
func (s *MemoryStore) BuildDreamPrompt(maxEntries int) (string, int, bool, error) {
	lastCursor, err := s.GetLastDreamCursor()
	if err != nil {
		return "", 0, false, err
	}
	entries, err := s.ReadUnprocessedHistory(lastCursor)
	if err != nil {
		return "", 0, false, err
	}
	if len(entries) == 0 {
		return "", 0, false, nil
	}

	batch := entries
	switch {
	case maxEntries >= 0 && maxEntries < len(entries):
		batch = entries[:maxEntries]
	case maxEntries < 0:
		end := len(entries) + maxEntries
		if end < 0 {
			end = 0
		}
		batch = entries[:end]
	}
	if len(batch) == 0 {
		// The reference: IndexError: list index out of range (batch[-1]).
		return "", 0, false, fmt.Errorf(
			"memory: build dream prompt: max_entries=%d selects no entries from %d unprocessed (reference raises IndexError)",
			maxEntries, len(entries))
	}

	var b strings.Builder
	for i, entry := range batch {
		if i > 0 {
			b.WriteByte('\n')
		}
		timestamp, _ := entry["timestamp"].(string)
		content, _ := entry["content"].(string)
		fmt.Fprintf(&b, "[%s] %s", timestamp, textutil.TruncateText(content, dreamPromptEntryChars))
	}

	// The entries come back from ReadEntries, which decodes with UseNumber, so
	// the cursor is a json.Number here even though Python's is an int.
	// validCursor is the same extraction IterValidEntries already applied, so
	// it succeeds for every entry in the batch.
	last, ok := validCursor(batch[len(batch)-1]["cursor"])
	if !ok {
		return "", 0, false, fmt.Errorf("memory: build dream prompt: last entry has no valid cursor")
	}
	return s.dreamTemplate() + "\n\n## Conversation History\n" + b.String(), last, true, nil
}

// DreamDiffer is the slice of GitStore that DreamContentDiff needs.
//
// The reference holds a GitStore and calls exactly two methods on it
// (memory.py:571-573). internal/gitstore already declares both with these
// signatures, so the concrete *gitstore.GitStore satisfies this interface
// as-is; the interface exists so this package does not depend on a package
// that is still being written, and so the behaviour can be tested with a fake.
type DreamDiffer interface {
	// IsInitialized mirrors GitStore.is_initialized().
	IsInitialized() bool
	// SummarizeWorkingTree mirrors GitStore.summarize_working_tree(paths).
	SummarizeWorkingTree(paths []string) (string, error)
}

// SetDreamDiffer wires the repository used by DreamContentDiff.
//
// The reference constructs its GitStore in MemoryStore.__init__; this port
// cannot (it must not import internal/gitstore), so the dependency is injected.
// A store with no differ reports an empty diff, which is the same answer the
// reference gives for a workspace that is not a repository.
func (s *MemoryStore) SetDreamDiffer(d DreamDiffer) { s.git = d }

// DreamContentDiff returns the structured summary of uncommitted changes to the
// durable memory files (dream_content_diff, memory.py:565-573).
//
// It returns "" when git is unavailable or nothing changed; that empty string
// is what makes BuildDreamCommitMessage degrade to the bare prefix.
func (s *MemoryStore) DreamContentDiff() (string, error) {
	if s.git == nil || !s.git.IsInitialized() {
		return "", nil
	}
	return s.git.SummarizeWorkingTree(append([]string(nil), DreamContentPaths...))
}

// DreamRunCompleted reports whether a Dream run reached a normal terminal
// response (dream_run_completed, memory.py:618-626).
//
// The reference reads resp.metadata and requires it to be a dict whose
// "_stop_reason" is "completed"; anything else — including a missing response
// — is incomplete. A nil *core.OutboundMessage or a nil Metadata map is the
// reference's "metadata is not a dict" case.
func DreamRunCompleted(resp *core.OutboundMessage) bool {
	if resp == nil || resp.Metadata == nil {
		return false
	}
	reason, _ := resp.Metadata["_stop_reason"].(string)
	return reason == "completed"
}

// DreamIncompletionReason explains why a Dream run cannot advance
// (dream_incompletion_reason, memory.py:628-638).
func DreamIncompletionReason(resp *core.OutboundMessage) string {
	if resp == nil || resp.Metadata == nil {
		return "stop_reason: missing response metadata"
	}
	reason, present := resp.Metadata["_stop_reason"]
	if !present {
		reason = "unknown"
	}
	return "stop_reason: " + pyStr(reason)
}

// DreamSessionKey returns a unique session key for a Dream run, e.g.
// "dream:20260528-100000" (dream_session_key, memory.py:698-701).
//
// The reference formats datetime.now() with %Y%m%d-%H%M%S, so the key has
// one-second resolution and is not unique within a second there either.
func DreamSessionKey() string {
	return "dream:" + time.Now().Format("20060102-150405")
}

// BuildDreamCommitMessage builds a Dream commit message grounded in the real
// working-tree diff (build_dream_commit_message, memory.py:703-719).
//
// diffBody is a machine-derived summary of the actual file changes; the LLM
// narrative is deliberately excluded so the audit record reflects the
// filesystem rather than the model's self-report. An empty or whitespace-only
// diffBody yields the bare prefix, which auto_commit turns into a no-op.
func BuildDreamCommitMessage(prefix, diffBody string) string {
	diffBody = textutil.PyStrip(diffBody)
	if diffBody == "" {
		return prefix
	}
	return prefix + "\n\n" + diffBody
}

// PruneDreamSessions removes the oldest Dream session files, keeping only the
// keep most recent (prune_dream_sessions, memory.py:721-740).
//
// Only stems that decode to a key starting with "dream:" are considered;
// every other session file is never touched, and a file whose stem does not
// decode at all (the retired "dream_20260713-095959.jsonl" spelling) is left
// alone too.
//
// DELIBERATE DIVERGENCE: the reference sorts with
// `sort(key=lambda item: item[0].stat().st_mtime)` over `Path.glob`, whose
// order is the filesystem's (os.scandir). This walks the directory in Go's
// sorted-by-name order and sorts stably by mtime, so two files with an
// identical mtime are pruned by name here and by directory order there. Both
// orders are arbitrary; neither is reproducible from the other.
func PruneDreamSessions(store *session.Store, keep int) error {
	dir := store.Dir()
	if dir == "" {
		return fmt.Errorf("memory: prune dream sessions: session store has no directory")
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return err
	}

	type dreamFile struct {
		key     string
		modTime time.Time
	}
	var dreamFiles []dreamFile
	for _, entry := range entries {
		name := entry.Name()
		// Python's glob("*.jsonl") does not match dotfiles.
		if strings.HasPrefix(name, ".") || !strings.HasSuffix(name, ".jsonl") || entry.IsDir() {
			continue
		}
		key, ok := session.DecodeStorageKey(strings.TrimSuffix(name, ".jsonl"))
		if !ok || !strings.HasPrefix(key, "dream:") {
			continue
		}
		// entry.Info() is lstat, so a symlinked .jsonl contributes the link's
		// own mtime rather than its target's. The reference's path.stat()
		// follows the link; the two differ only for a symlink inside the
		// sessions directory.
		info, err := entry.Info()
		if err != nil {
			continue
		}
		dreamFiles = append(dreamFiles, dreamFile{key: key, modTime: info.ModTime()})
	}

	sort.SliceStable(dreamFiles, func(i, j int) bool {
		return dreamFiles[i].modTime.Before(dreamFiles[j].modTime)
	})

	// The reference slices with [: max(0, len(dream_files) - keep)]; Python
	// clamps an over-long slice bound, so a negative keep prunes everything
	// rather than panicking.
	excess := len(dreamFiles) - keep
	if excess < 0 {
		excess = 0
	}
	if excess > len(dreamFiles) {
		excess = len(dreamFiles)
	}
	var firstErr error
	for _, f := range dreamFiles[:excess] {
		if err := store.Delete(f.key); err != nil && firstErr == nil {
			firstErr = err
		}
	}
	return firstErr
}

// BuildDreamTools builds the restricted tool registry used by Dream runs
// (build_dream_tools, memory.py:575-616).
//
// The reference registers four tools, in this order:
//
//	read_file   -> builtin.NewReadFile    (workspace boundary, bundled skills readable)
//	edit_file   -> builtin.NewEditFile    (skills/ writable, plus the three memory files)
//	apply_patch -> builtin.NewApplyPatch  (same policy as edit_file)
//	write_file  -> builtin.NewWriteFile   (same policy as edit_file)
//
// Registration order is observable through Registry.Names, which mirrors the
// reference's tool_names (registry.py:204-206, list(self._tools.keys())), so it
// is matched deliberately. The order advertised to the model is separately
// sorted by Registry.Schemas, matching get_definitions (registry.py:86-108).
//
// The reference's FileStates (agent/tools/file_state.py) is still absent — see
// the scope note in internal/tools/builtin/doc.go — so no unchanged-file cache
// is shared between the four tools.
func (s *MemoryStore) BuildDreamTools() (*tools.Registry, error) {
	skillsDir := filepath.Join(s.workspace, "skills")
	if err := os.MkdirAll(skillsDir, 0o755); err != nil {
		return nil, fmt.Errorf("memory: create skills dir: %w", err)
	}

	// extra_read = [BUILTIN_SKILLS_DIR] if BUILTIN_SKILLS_DIR.exists() else None
	var extraRead []string
	if _, err := os.Stat(BuiltinSkillsDir); err == nil {
		extraRead = []string{BuiltinSkillsDir}
	}
	editableFiles := []string{s.memoryFile, s.soulFile, s.userFile}

	registry := tools.NewRegistry()
	registry.Register(builtin.NewReadFile(builtin.PathPolicy{
		Workspace:      s.workspace,
		AllowedDir:     s.workspace,
		ExtraReadDirs:  extraRead,
		ExtraReadFiles: nil,
	}))
	editPolicy := builtin.PathPolicy{
		Workspace:       s.workspace,
		AllowedDir:      skillsDir,
		ExtraWriteFiles: editableFiles,
	}
	registry.Register(builtin.NewEditFile(editPolicy))
	registry.Register(builtin.NewApplyPatch(editPolicy))
	registry.Register(builtin.NewWriteFile(editPolicy))
	return registry, nil
}
