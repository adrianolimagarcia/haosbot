package skills

// Bundled built-in skills.
//
// The reference ships `nanobot/skills/` INSIDE the Python package
// (BUILTIN_SKILLS_DIR = Path(__file__).parent.parent / "skills",
// agent/skills.py:15), so the directory is always present and the built-in
// group of build_skills_summary is always populated. The Go port is a single
// binary with no package directory, so the same 18 files are embedded here and
// serve as the "always present" analogue.
//
// internal/skills/builtin/ is a byte-for-byte copy of
// upstream/nanobot/nanobot/skills/ at 1bb712d3, verified by SHA-256 in
// builtin_test.go. The drift check matters because the built-in group's
// descriptions and the `(unavailable: ...)` suffixes shown to the model come
// straight out of those files.

import (
	"embed"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
)

// builtinSkillsFS is the embedded copy of the reference's nanobot/skills/.
//
// The pattern is the whole directory rather than a file list so that the
// reference tree is mirrored exactly, including my/references/examples.md,
// README.md and the two skills' script files. BuiltinSkillNames is written out
// by hand in builtin_test.go so that dropping a skill from this tree is a test
// failure rather than a silently smaller set.
//
//go:embed builtin
var builtinSkillsFS embed.FS

// builtinSkillsEmbedRoot is the FS prefix mirroring the reference's
// `nanobot/skills/` directory.
const builtinSkillsEmbedRoot = "builtin"

// DefaultBuiltinSkillsDir is the closest structural analogue of
// BUILTIN_SKILLS_DIR for a single binary: the skills/ directory beside the
// running executable.
//
// It is only a DEFAULT. When the directory does not exist the loader falls back
// to the embedded copy above, which is what makes the port's built-in group
// behave like the reference's always-present package directory.
func DefaultBuiltinSkillsDir() string {
	exe, err := os.Executable()
	if err != nil {
		return "skills"
	}
	return filepath.Join(filepath.Dir(exe), "skills")
}

// BuiltinSkillsDir is the canonical built-in skills root for the whole port.
//
// CALLERS SHOULD USE THIS ONE. internal/memory/dream.go:64 declares its own
// BuiltinSkillsDir for the Dream prompt and the Dream tool's read policy; the
// two are the same value by default but are separate variables, so overriding
// one does not affect the other. See the package doc for the reconciliation
// recommendation.
var BuiltinSkillsDir = DefaultBuiltinSkillsDir()

// BundledSkillFile returns the embedded content of one file under the built-in
// skills root, addressed by its path relative to the reference's skills/
// directory and using forward slashes — for example "cron/SKILL.md" or
// "my/references/examples.md".
//
// ok is false when no such file is embedded.
func BundledSkillFile(rel string) (string, bool) {
	data, err := fs.ReadFile(builtinSkillsFS, builtinSkillsEmbedRoot+"/"+rel)
	if err != nil {
		return "", false
	}
	return string(data), true
}

// BundledSkillNames returns the names of the embedded built-in skill
// directories — the directories that hold a SKILL.md — in the order embed.FS
// reports them (lexicographic).
//
// The reference's own order comes from os directory listing and is NOT sorted
// (see Loader.entriesFromDir); the embedded FS has no such order to preserve.
// builtin_test.go pins the two orders against each other for the shipped tree.
func BundledSkillNames() []string {
	entries, err := fs.ReadDir(builtinSkillsFS, builtinSkillsEmbedRoot)
	if err != nil {
		return nil
	}
	names := make([]string, 0, len(entries))
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		if _, err := fs.Stat(builtinSkillsFS, builtinSkillsEmbedRoot+"/"+entry.Name()+"/SKILL.md"); err != nil {
			continue
		}
		names = append(names, entry.Name())
	}
	return names
}

// BundledSkillFiles returns every embedded path under the built-in skills root,
// relative to that root, in lexicographic order. It exists for the drift test.
func BundledSkillFiles() []string {
	var out []string
	_ = fs.WalkDir(builtinSkillsFS, builtinSkillsEmbedRoot, func(path string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return nil
		}
		rel, relErr := filepath.Rel(builtinSkillsEmbedRoot, path)
		if relErr != nil {
			return nil
		}
		out = append(out, filepath.ToSlash(rel))
		return nil
	})
	sort.Strings(out)
	return out
}

// ExtractBuiltinSkills writes the embedded built-in skills to dest, creating
// directories as needed and never overwriting an existing file.
//
// It exists so a deployment can put the tree on disk where the reference keeps
// it — the Dream prompt names `<BUILTIN_SKILLS_DIR>/skill-creator/SKILL.md` and
// the Dream tool's read policy allow-lists that directory
// (internal/memory/dream.go:41-67, :346), both of which need real files. The
// reference never needs this because its package directory already exists.
func ExtractBuiltinSkills(dest string) error {
	return fs.WalkDir(builtinSkillsFS, builtinSkillsEmbedRoot, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, relErr := filepath.Rel(builtinSkillsEmbedRoot, path)
		if relErr != nil {
			return relErr
		}
		if rel == "." {
			return nil
		}
		target := filepath.Join(dest, rel)
		if d.IsDir() {
			return os.MkdirAll(target, 0o755)
		}
		data, readErr := fs.ReadFile(builtinSkillsFS, path)
		if readErr != nil {
			return readErr
		}
		if _, statErr := os.Stat(target); statErr == nil {
			return nil
		}
		if mkErr := os.MkdirAll(filepath.Dir(target), 0o755); mkErr != nil {
			return mkErr
		}
		if writeErr := os.WriteFile(target, data, 0o644); writeErr != nil {
			return fmt.Errorf("skills: extract %s: %w", target, writeErr)
		}
		return nil
	})
}
