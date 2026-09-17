// Package skills ports nanobot/agent/skills.py:SkillsLoader at upstream
// 1bb712d3 (v0.3.5).
//
// # WHAT IT IS FOR
//
// The system prompt tells the model which skills exist, what each one is for,
// which are unusable because a required CLI or environment variable is missing,
// and — for skills marked `always: true` — their full instructions. On a fresh
// install that is an ~1.9 KB "# Skills" section the model never sees without
// this package: the port's previous stand-in (a 25-line scan of
// <workspace>/skills printing bare relative paths) emitted nothing at all for
// the built-in skills, which are the only skills a fresh install has.
//
// LAYOUT
//
//	loader.go       SkillsLoader itself
//	frontmatter.go  parse_skill_metadata / valid_skill_metadata / _strip_frontmatter
//	yaml.go         the YAML subset yaml.safe_load needs for frontmatter
//	plugin.go       enabled_agent_plugin_skills and its transitive helpers
//	builtin.go      the embedded copy of nanobot/skills/ and its extraction
//	pypath.go       pathlib semantics (join, resolve, relative_to)
//	pyvalue.go      Python truthiness / str() / json escaping
//	aliases.go      _skill_aliases (the CLI Apps installed registry)
//
// CALLERS SHOULD USE Loader.BuiltinSkillsRoot's source, not a second copy of
// the built-in directory: internal/memory/dream.go declares its own
// BuiltinSkillsDir for the Dream prompt, and the canonical one is
// skills.BuiltinSkillsDir here. See the note on that variable.
package skills

import (
	"fmt"
	"os"
	"os/exec"
	"strings"
	"unicode"
	"unicode/utf8"
)

// Skill sources, mirroring the `source` strings in list_skills.
const (
	SourceWorkspace = "workspace"
	SourcePlugin    = "plugin"
	SourceBuiltin   = "builtin"
)

// Skill is one entry of list_skills: a name, the SKILL.md path the reference
// would stringify, and which group it came from.
type Skill struct {
	Name   string
	Path   string
	Source string
}

// Requirements is get_skill_requirements' return shape.
type Requirements struct {
	Bins        []string
	Env         []string
	MissingBins []string
	MissingEnv  []string
}

// ExplicitSkillContext mirrors RuntimeContextBlock(source="explicit_skills").
//
// The reference returns nanobot.runtime_context.RuntimeContextBlock. The port
// has no such type yet (internal/runtimecontext ports a different half of that
// module), so this is the minimal structural equivalent; a caller adapts it at
// the boundary.
type ExplicitSkillContext struct {
	Source  string
	Content string
}

// RelativeToError reports the ValueError that `Path.relative_to(root)` raises
// in build_skills_summary (skills.py:260) when a skill's path is not lexically
// under its group root.
//
// This is reachable: plugin skill paths are RESOLVED by _contained while the
// plugin group root is `self.workspace / "plugins"` unresolved, so a workspace
// spelled with a symlinked or relative prefix makes the two disagree and the
// reference raises ValueError out of build_system_prompt. The port reports it
// here instead of crashing the process; internal/prompt omits the section.
type RelativeToError struct {
	Path string
	Root string
}

func (e *RelativeToError) Error() string {
	return fmt.Sprintf("skills: %q is not under %q (pathlib relative_to ValueError)", e.Path, e.Root)
}

// Loader ports SkillsLoader.
type Loader struct {
	// Workspace is the AGENT workspace, exactly as passed to the reference
	// (`self.workspace = workspace`, NOT resolved).
	Workspace string

	// DisabledSkills ports the disabled_skills set.
	DisabledSkills map[string]bool

	// SkillAliases overrides _skill_aliases(). nil uses the ported default,
	// which reads the CLI Apps installed registry (aliases.go).
	SkillAliases func(workspace string) map[string]string

	workspaceSkills string
	builtinDir      string
	builtinExplicit bool
}

// New creates a loader for an agent workspace whose built-in skills come from
// the embedded copy of the reference's nanobot/skills/.
//
// This is the production constructor. It differs from the reference in one
// way only: the reference's built-in directory always exists because it ships
// inside the Python package, whereas a Go binary has no package directory, so
// the embedded tree stands in.
func New(workspace string) *Loader {
	return &Loader{
		Workspace:       workspace,
		workspaceSkills: pyJoin(workspace, "skills"),
	}
}

// NewWithBuiltinDir creates a loader whose built-in group is read from an
// explicit on-disk directory, mirroring
// `SkillsLoader(workspace, builtin_skills_dir=...)`.
//
// A directory that does not exist yields NO built-in skills, which is the
// reference's `if self.builtin_skills and self.builtin_skills.exists()` branch.
// This constructor is what makes a byte-for-byte differential comparison
// possible: both sides can be pointed at the same directory.
func NewWithBuiltinDir(workspace, builtinDir string) *Loader {
	return &Loader{
		Workspace:       workspace,
		workspaceSkills: pyJoin(workspace, "skills"),
		builtinDir:      builtinDir,
		builtinExplicit: true,
	}
}

// WorkspaceSkills is `self.workspace_skills`, i.e. workspace/"skills".
func (l *Loader) WorkspaceSkills() string { return l.workspaceSkills }

// BuiltinSkillsRoot is `self.builtin_skills`.
func (l *Loader) BuiltinSkillsRoot() string {
	if l.builtinExplicit {
		return l.builtinDir
	}
	if l.builtinDir != "" {
		return l.builtinDir
	}
	return BuiltinSkillsDir
}

// builtinEmbedded reports whether the built-in group is served from the
// embedded tree rather than from disk, and whether the group exists at all.
func (l *Loader) builtinEmbedded() (embedded, exists bool) {
	root := l.BuiltinSkillsRoot()
	if info, err := os.Stat(root); err == nil && info.IsDir() {
		return false, true
	}
	if l.builtinExplicit {
		// The reference has no fallback: a missing explicit directory simply
		// contributes no skills.
		return false, false
	}
	return true, true
}

// ListSkills ports list_skills (skills.py:90-130).
//
// filterUnavailable is the DEFAULT-TRUE parameter that build_skills_summary
// deliberately inverts; getting it backwards changes which skills appear at all.
func (l *Loader) ListSkills(filterUnavailable bool) []Skill {
	skills := l.entriesFromDir(l.workspaceSkills, SourceWorkspace, nil)
	seen := map[string]bool{}
	for _, skill := range skills {
		seen[skill.Name] = true
	}
	for _, pluginSkill := range EnabledAgentPluginSkills(l.Workspace) {
		if seen[pluginSkill.Name] {
			continue
		}
		skills = append(skills, Skill{Name: pluginSkill.Name, Path: pluginSkill.Path, Source: SourcePlugin})
		seen[pluginSkill.Name] = true
	}
	if embedded, exists := l.builtinEmbedded(); exists {
		if embedded {
			for _, name := range BundledSkillNames() {
				if seen[name] {
					continue
				}
				skills = append(skills, Skill{
					Name:   name,
					Path:   pyJoin(l.BuiltinSkillsRoot(), name, "SKILL.md"),
					Source: SourceBuiltin,
				})
			}
		} else {
			skills = append(skills, l.entriesFromDir(l.BuiltinSkillsRoot(), SourceBuiltin, seen)...)
		}
	}

	if len(l.DisabledSkills) > 0 {
		disabled := map[string]bool{}
		for name, off := range l.DisabledSkills {
			disabled[name] = off
		}
		for legacy, canonical := range l.skillAliases() {
			if disabled[legacy] || disabled[canonical] {
				disabled[legacy] = true
				disabled[canonical] = true
			}
		}
		kept := make([]Skill, 0, len(skills))
		for _, skill := range skills {
			if !disabled[skill.Name] {
				kept = append(kept, skill)
			}
		}
		skills = kept
	}

	if filterUnavailable {
		kept := make([]Skill, 0, len(skills))
		for _, skill := range skills {
			if l.CheckRequirements(l.GetSkillMeta(skill.Name)) {
				kept = append(kept, skill)
			}
		}
		return kept
	}
	return skills
}

// entriesFromDir ports _skill_entries_from_dir (skills.py:74-88).
//
// THE ORDER IS NOT SORTED. `base.iterdir()` yields entries in the directory's
// own order (creation order on ext4 here, not name order), and the reference
// never sorts them — a fixture that creates zeta, alpha, mid gets exactly that
// order in the prompt. os.ReadDir would sort; the file handle's ReadDir(-1)
// does not, which is the Go analogue of iterdir().
//
// Path.is_dir() and Path.exists() follow symlinks, so a symlinked skill
// directory counts and a dangling one does not.
func (l *Loader) entriesFromDir(base, source string, skip map[string]bool) []Skill {
	if _, err := os.Stat(base); err != nil {
		return nil
	}
	handle, err := os.Open(base)
	if err != nil {
		return nil
	}
	defer handle.Close()
	entries, err := handle.ReadDir(-1)
	if err != nil {
		return nil
	}
	var out []Skill
	for _, entry := range entries {
		name := entry.Name()
		if skip != nil && skip[name] {
			continue
		}
		info, statErr := os.Stat(pyJoin(base, name))
		if statErr != nil || !info.IsDir() {
			continue
		}
		skillFile := pyJoin(base, name, "SKILL.md")
		if fileInfo, fileErr := os.Stat(skillFile); fileErr != nil || fileInfo.IsDir() {
			continue
		}
		out = append(out, Skill{Name: name, Path: skillFile, Source: source})
	}
	return out
}

// LoadSkill ports load_skill (skills.py:132-146).
//
// ok=false is the reference's None: no skill of that name, OR a file that could
// not be read or decoded. DIVERGENCE (documented): the reference lets an
// OSError / UnicodeDecodeError propagate out of read_text, so a corrupt
// SKILL.md crashes build_system_prompt. Degrading to "no such skill" keeps a
// long-running server alive; LoadSkillStrict exposes the distinction for
// callers that want it.
func (l *Loader) LoadSkill(name string) (string, bool) {
	content, found, err := l.LoadSkillStrict(name)
	if err != nil {
		return "", false
	}
	return content, found
}

// LoadSkillStrict is LoadSkill with the read error surfaced separately.
func (l *Loader) LoadSkillStrict(name string) (content string, found bool, err error) {
	skills := l.ListSkills(false)
	available := map[string]bool{}
	for _, skill := range skills {
		available[skill.Name] = true
	}
	resolved := name
	if !available[name] {
		if canonical, ok := l.skillAliases()[name]; ok {
			resolved = canonical
		}
	}
	for _, skill := range skills {
		if skill.Name == resolved {
			text, readErr := l.readSkillFile(skill)
			if readErr != nil {
				return "", false, readErr
			}
			return text, true, nil
		}
	}
	return "", false, nil
}

// readSkillFile reads a skill's SKILL.md from wherever its group lives.
func (l *Loader) readSkillFile(skill Skill) (string, error) {
	if skill.Source == SourceBuiltin {
		if embedded, _ := l.builtinEmbedded(); embedded {
			rel, ok := pyRelativeTo(skill.Path, l.BuiltinSkillsRoot())
			if ok {
				if content, found := BundledSkillFile(rel); found {
					return content, nil
				}
			}
			return "", os.ErrNotExist
		}
	}
	return readTextFile(skill.Path)
}

// LoadSkillsForContext ports load_skills_for_context (skills.py:148-163).
//
// The walrus in the comprehension is a TRUTHINESS test, so an empty SKILL.md is
// dropped rather than rendered as an empty section.
func (l *Loader) LoadSkillsForContext(names []string) string {
	parts := make([]string, 0, len(names))
	for _, name := range names {
		markdown, ok := l.LoadSkill(name)
		if !ok || markdown == "" {
			continue
		}
		parts = append(parts, fmt.Sprintf("### Skill: %s\n\n%s", name, StripFrontmatter(markdown)))
	}
	return strings.Join(parts, "\n\n---\n\n")
}

// BuildSkillsSummary ports build_skills_summary (skills.py:204-263).
//
// exclude is a set (Python set[str]); workspace is the effective PROJECT
// workspace, which decides whether the group roots are shown relative or
// absolute. The error is a *RelativeToError where the reference raises
// ValueError.
func (l *Loader) BuildSkillsSummary(exclude map[string]bool, workspace string) (string, error) {
	all := l.ListSkills(false)
	if len(all) == 0 {
		return "", nil
	}

	agentWorkspace := pyResolve(l.Workspace)
	projectWorkspace := l.Workspace
	if workspace != "" {
		projectWorkspace = workspace
	}
	projectWorkspace = pyResolve(projectWorkspace)
	useRelativeRoots := projectWorkspace == agentWorkspace

	type group struct {
		label  string
		source string
		root   string
	}
	groups := []group{
		{"Workspace skills", SourceWorkspace, l.workspaceSkills},
		{"Agent Plugin skills", SourcePlugin, pyJoin(l.Workspace, "plugins")},
		{"Built-in skills", SourceBuiltin, l.BuiltinSkillsRoot()},
	}

	var sections []string
	for _, g := range groups {
		var entries []Skill
		for _, skill := range all {
			if skill.Source != g.source {
				continue
			}
			if len(exclude) > 0 && exclude[skill.Name] {
				continue
			}
			entries = append(entries, skill)
		}
		if len(entries) == 0 {
			continue
		}

		resolvedRoot := pyResolve(g.root)
		displayRoot := resolvedRoot
		if useRelativeRoots {
			if g.source == SourcePlugin {
				displayRoot = "plugins"
			} else {
				displayRoot = "skills"
			}
		}
		lines := []string{fmt.Sprintf("### %s (`%s`)", g.label, displayRoot)}
		for _, skill := range entries {
			meta := l.GetSkillMeta(skill.Name)
			available := l.CheckRequirements(meta)
			desc := l.GetSkillDescription(skill.Name)
			suffix := ""
			if !available {
				if missing := l.GetMissingRequirements(meta); missing != "" {
					suffix = " (unavailable: " + missing + ")"
				} else {
					suffix = " (unavailable)"
				}
			}
			relativePath, ok := pyRelativeTo(skill.Path, g.root)
			if !ok {
				return "", &RelativeToError{Path: skill.Path, Root: g.root}
			}
			lines = append(lines, fmt.Sprintf("- **%s** \u2014 %s%s  `%s`", skill.Name, desc, suffix, relativePath))
		}
		sections = append(sections, strings.Join(lines, "\n"))
	}
	return strings.Join(sections, "\n\n"), nil
}

// requirementLists ports _requirement_lists (skills.py:265-275).
//
// Every branch is a tolerance for a wrong shape: `requires` must be a mapping,
// and bins/env must be lists of non-blank strings. A skill that gets this wrong
// is treated as having no requirements, i.e. available.
func requirementLists(skillMeta map[string]any) (bins, env []string) {
	raw, present := skillMeta["requires"]
	if !present || !pyTruthy(raw) {
		return nil, nil
	}
	if !pyIsInstanceDict(raw) {
		return nil, nil
	}
	requires := raw.(map[string]any)

	binsRaw := requires["bins"]
	if !pyTruthy(binsRaw) {
		binsRaw = nil
	}
	if pyIsInstanceList(binsRaw) {
		for _, value := range binsRaw.([]any) {
			if pyStrippedTruthy(value) {
				bins = append(bins, value.(string))
			}
		}
	}
	envRaw := requires["env"]
	if !pyTruthy(envRaw) {
		envRaw = nil
	}
	if pyIsInstanceList(envRaw) {
		for _, value := range envRaw.([]any) {
			if pyStrippedTruthy(value) {
				env = append(env, value.(string))
			}
		}
	}
	return bins, env
}

// GetMissingRequirements ports _get_missing_requirements (skills.py:277-283).
func (l *Loader) GetMissingRequirements(skillMeta map[string]any) string {
	bins, env := requirementLists(skillMeta)
	missing := make([]string, 0, len(bins)+len(env))
	for _, command := range bins {
		if !which(command) {
			missing = append(missing, "CLI: "+command)
		}
	}
	for _, name := range env {
		if os.Getenv(name) == "" {
			missing = append(missing, "ENV: "+name)
		}
	}
	return strings.Join(missing, ", ")
}

// GetSkillAvailability ports get_skill_availability (skills.py:285-289).
func (l *Loader) GetSkillAvailability(name string) (bool, string) {
	meta := l.GetSkillMeta(name)
	available := l.CheckRequirements(meta)
	if available {
		return true, ""
	}
	return false, l.GetMissingRequirements(meta)
}

// GetSkillRequirements ports get_skill_requirements (skills.py:291-299).
func (l *Loader) GetSkillRequirements(name string) Requirements {
	bins, env := requirementLists(l.GetSkillMeta(name))
	req := Requirements{Bins: bins, Env: env}
	for _, value := range bins {
		if !which(value) {
			req.MissingBins = append(req.MissingBins, value)
		}
	}
	for _, value := range env {
		if os.Getenv(value) == "" {
			req.MissingEnv = append(req.MissingEnv, value)
		}
	}
	return req
}

// GetSkillDescription ports get_skill_description (skills.py:301-307).
//
// `isinstance(description, str) and description` is a TRUTHINESS test, so an
// empty description falls back to the skill name.
func (l *Loader) GetSkillDescription(name string) string {
	meta := l.GetSkillMetadata(name)
	if meta != nil {
		if description, ok := meta["description"].(string); ok && description != "" {
			return description
		}
	}
	return name
}

// GetSkillMeta ports _get_skill_meta (skills.py:345-348).
func (l *Loader) GetSkillMeta(name string) map[string]any {
	rawMeta := l.GetSkillMetadata(name)
	if rawMeta == nil {
		rawMeta = map[string]any{}
	}
	return ParseNanobotMetadata(rawMeta["metadata"])
}

// GetAlwaysSkills ports get_always_skills (skills.py:350-360).
//
// Three details are load-bearing:
//   - the list comes from list_skills(filter_unavailable=True), so a skill whose
//     requirements are unmet is never auto-activated even if it says always;
//   - `(meta := ... or {})` makes an EMPTY frontmatter mapping falsy, so a skill
//     with no frontmatter is excluded even though `always` would be absent
//     either way;
//   - the test is a truthiness check on the nested `metadata.always` OR the
//     top-level `always`, so `always: "false"` (a string) ACTIVATES the skill
//     while `always: 0` does not.
func (l *Loader) GetAlwaysSkills() []string {
	var out []string
	for _, skill := range l.ListSkills(true) {
		meta := l.GetSkillMetadata(skill.Name)
		if len(meta) == 0 {
			continue
		}
		nested := ParseNanobotMetadata(meta["metadata"])
		if pyTruthy(nested["always"]) || pyTruthy(meta["always"]) {
			out = append(out, skill.Name)
		}
	}
	return out
}

// GetSkillMetadata ports get_skill_metadata (skills.py:362-372).
//
// nil is the reference's None.
func (l *Loader) GetSkillMetadata(name string) map[string]any {
	content, _ := l.LoadSkill(name)
	return ParseSkillMetadata(content)
}

// CheckRequirements ports _check_requirements (skills.py:338-343).
func (l *Loader) CheckRequirements(skillMeta map[string]any) bool {
	bins, env := requirementLists(skillMeta)
	for _, command := range bins {
		if !which(command) {
			return false
		}
	}
	for _, name := range env {
		// `os.environ.get(var)` is a truthiness test: an environment variable
		// set to the empty string does NOT satisfy the requirement.
		if os.Getenv(name) == "" {
			return false
		}
	}
	return true
}

// skillAliases ports _skill_aliases (skills.py:65-72).
func (l *Loader) skillAliases() map[string]string {
	if l.SkillAliases != nil {
		return l.SkillAliases(l.Workspace)
	}
	return InstalledSkillAliases(l.Workspace)
}

// GetExplicitlyInvokedSkills ports get_explicitly_invoked_skills
// (skills.py:165-180).
func (l *Loader) GetExplicitlyInvokedSkills(text string) []string {
	if text == "" {
		return nil
	}
	available := map[string]bool{}
	for _, skill := range l.ListSkills(true) {
		available[skill.Name] = true
	}
	aliases := l.skillAliases()
	var invoked []string
	for _, requested := range findSkillReferences(text) {
		name := requested
		if !available[name] {
			if canonical, ok := aliases[name]; ok {
				name = canonical
			}
		}
		if available[name] && !containsString(invoked, name) {
			invoked = append(invoked, name)
		}
	}
	return invoked
}

// BuildExplicitSkillRuntimeContext ports build_explicit_skill_runtime_context
// (skills.py:182-202).
//
// nil is the reference's None.
func (l *Loader) BuildExplicitSkillRuntimeContext(text string) *ExplicitSkillContext {
	names := l.GetExplicitlyInvokedSkills(text)
	if len(names) == 0 {
		return nil
	}
	alwaysActive := map[string]bool{}
	for _, name := range l.GetAlwaysSkills() {
		alwaysActive[name] = true
	}
	remaining := make([]string, 0, len(names))
	for _, name := range names {
		if !alwaysActive[name] {
			remaining = append(remaining, name)
		}
	}
	content := l.LoadSkillsForContext(remaining)
	if content == "" {
		return nil
	}
	return &ExplicitSkillContext{
		Source: "explicit_skills",
		Content: "[Active Skills \u2014 instructions for this user turn]\n" +
			content + "\n[/Active Skills]",
	}
}

// findSkillReferences ports `_SKILL_REFERENCE.finditer(text)`:
//
//	(?<![\w$])\$([A-Za-z0-9_-]+)
//
// RE2 has no lookbehind, so the negative assertion is evaluated directly. Python's
// `\w` is Unicode-aware (letters, digits, underscore); the port uses
// unicode.IsLetter || unicode.IsNumber, which differs from str.isalnum() only
// for the handful of Unicode numeric letters that Go classifies as letters
// anyway.
func findSkillReferences(text string) []string {
	var out []string
	for i := 0; i < len(text); i++ {
		if text[i] != '$' {
			continue
		}
		if i > 0 {
			prev, _ := utf8.DecodeLastRuneInString(text[:i])
			if prev == '$' || prev == '_' || unicode.IsLetter(prev) || unicode.IsNumber(prev) {
				continue
			}
		}
		j := i + 1
		for j < len(text) && isSkillReferenceChar(text[j]) {
			j++
		}
		if j > i+1 {
			out = append(out, text[i+1:j])
		}
		i = j - 1
	}
	return out
}

func isSkillReferenceChar(c byte) bool {
	return c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '_' || c == '-'
}

// which ports shutil.which (POSIX): a command containing a separator is tested
// directly, otherwise PATH is searched for an executable regular file.
func which(command string) bool {
	if command == "" {
		return false
	}
	_, err := exec.LookPath(command)
	return err == nil
}

func containsString(values []string, want string) bool {
	for _, value := range values {
		if value == want {
			return true
		}
	}
	return false
}
