// Package prompt builds the system prompt and transcript sent to the model.
//
// Mirrors nanobot/agent/context.py:ContextBuilder at upstream 1bb712d3.
//
// TEMPLATE NOTE — the reference renders Jinja2 templates from
// nanobot/templates/agent/*.md. Rather than embed a template engine (which
// would mean a third-party dependency and a large attack surface), the
// conditional logic of the three non-static templates is reimplemented here in
// Go and the fully static templates are embedded verbatim. The rendered
// sections and their order match the reference; byte-exact Jinja2 whitespace
// is NOT claimed, because Jinja2's default (non-trim_blocks) environment
// emits the newline that follows each block tag and reproducing that exactly
// has no observable effect on model behavior.
//
// The workspace-level templates under nanobot/templates/ (AGENTS.md, SOUL.md,
// USER.md, legacy/SOUL.md, memory/MEMORY.md, ...) are embedded too, in
// templates.go. They are not rendered — they are compared against the
// workspace files to detect "still the shipped default", which is what
// _is_template_content does.
package prompt

import (
	_ "embed"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"

	"github.com/adrianolimagarcia/nanobot-go/internal/skills"
	"github.com/adrianolimagarcia/nanobot-go/internal/textutil"
)

//go:embed templates/agent/tool_contract.md
var toolContractTemplate string

//go:embed templates/agent/_snippets/untrusted_content.md
var untrustedContentTemplate string

//go:embed templates/agent/skills_section.md
var skillsSectionTemplate string

// sectionSeparator joins prompt sections, mirroring build_system_prompt's
// "\n\n---\n\n".join(parts) (context.py:152).
const sectionSeparator = "\n\n---\n\n"

// Summary is an archived conversation summary injected into the prompt.
// Mirrors SessionSummary.
type Summary struct {
	Text       string
	LastActive string
}

// Builder assembles system prompts and transcripts.
type Builder struct {
	// Workspace is the AGENT workspace (profile, memory, skills).
	Workspace string
	// DisabledSkills lists skill names to exclude.
	DisabledSkills []string
	// Timezone is the configured IANA timezone.
	Timezone string
	// BuiltinSkillsDir overrides where the built-in skill group is read from.
	//
	// Empty (the normal case) uses the embedded copy of the reference's
	// nanobot/skills/, which is the analogue of the reference's always-present
	// package directory. A non-empty value is passed through to
	// skills.NewWithBuiltinDir, which is what the differential harness uses to
	// point both implementations at one directory.
	BuiltinSkillsDir string
}

// New creates a Builder for an agent workspace.
func New(workspace string) *Builder {
	return &Builder{Workspace: workspace}
}

// BuildSystemPrompt assembles the system prompt.
//
// Section order mirrors build_system_prompt (context.py:101-152):
// identity, bootstrap files, tool contract, current project, memory,
// active skills, skills summary, archived context summary.
func (b *Builder) BuildSystemPrompt(channel string, summary *Summary, projectWorkspace string, includeMemory bool) string {
	root := projectWorkspace
	if root == "" {
		root = b.Workspace
	}

	parts := []string{b.identity(channel, root)}

	if bootstrap := b.LoadBootstrapFiles(root); bootstrap != "" {
		parts = append(parts, bootstrap)
	}

	parts = append(parts, strings.TrimRight(toolContractTemplate, "\n"))

	// The "Current Project" section appears only when the project workspace
	// differs from the agent workspace (context.py:119-125).
	projectPath := expandAndClean(root)
	agentPath := expandAndClean(b.Workspace)
	if projectPath != agentPath {
		parts = append(parts, fmt.Sprintf(
			"# Current Project\n\nWorking directory: %s\n"+
				"Use it as the default root for project files and relative tool paths.",
			projectPath))
	}

	// context.py:128-130. Two separate conditions, and neither is a stripped
	// emptiness test: `if memory` is Python truthiness on the RAW content, so a
	// memory file holding only whitespace or only "\x1c" is NOT empty and would
	// be appended — and a memory file that is still the shipped
	// memory/MEMORY.md template is withheld entirely, because the user has not
	// written anything yet.
	if includeMemory {
		memory := b.ReadMemory()
		if memory != "" && !IsTemplateContent(memory, "memory/MEMORY.md") {
			parts = append(parts, "# Memory\n\n## Long-term Memory\n"+memory)
		}
	}

	// context.py:132-142. The Active Skills section comes FIRST and its names
	// are then EXCLUDED from the summary below, so a skill that is always
	// active is not also advertised as a path to read.
	loader := b.SkillsLoader()
	activeSkills := loader.GetAlwaysSkills()
	if len(activeSkills) > 0 {
		if activeContent := loader.LoadSkillsForContext(activeSkills); activeContent != "" {
			parts = append(parts, "# Active Skills\n\n"+activeContent)
		}
	}

	exclude := make(map[string]bool, len(activeSkills))
	for _, name := range activeSkills {
		exclude[name] = true
	}
	// The reference raises ValueError out of build_system_prompt when a skill
	// path is not lexically under its group root (see skills.RelativeToError).
	// A server that crashes on a malformed workspace is worse than one that
	// omits the section, so the error is surfaced through SkillsLoader and the
	// section is dropped here. This is the ONLY input that can produce it.
	if summary, err := loader.BuildSkillsSummary(exclude, root); err == nil && summary != "" {
		parts = append(parts, strings.ReplaceAll(
			strings.TrimRight(skillsSectionTemplate, "\n"), "{{ skills_summary }}", summary))
	}

	if summary != nil && summary.Text != "" && summary.Text != "(nothing)" {
		parts = append(parts, "[Archived Context Summary]\n\n"+
			fmt.Sprintf("Previous conversation summary (last active %s):\n%s",
				summary.LastActive, summary.Text))
	}

	return strings.Join(parts, sectionSeparator)
}

// identity renders templates/agent/identity.md.
func (b *Builder) identity(channel, root string) string {
	workspacePath := expandAndClean(root)
	agentWorkspacePath := expandAndClean(b.Workspace)

	var sb strings.Builder
	sb.WriteString("## Runtime\n")
	sb.WriteString(b.runtimeDescription())
	sb.WriteString("\n\n## Workspace\n")

	if agentWorkspacePath != workspacePath {
		fmt.Fprintf(&sb, "Nanobot's agent workspace is at: %s\n", agentWorkspacePath)
		fmt.Fprintf(&sb, "- Agent profile: %s/SOUL.md and %s/USER.md\n", agentWorkspacePath, agentWorkspacePath)
		fmt.Fprintf(&sb, "- Long-term memory: %s/memory/MEMORY.md\n", agentWorkspacePath)
		fmt.Fprintf(&sb, "- History log: %s/memory/history.jsonl (append-only JSONL; prefer built-in `grep` for search).\n", agentWorkspacePath)
		fmt.Fprintf(&sb, "- Custom skills: %s/skills/{skill-name}/SKILL.md\n", agentWorkspacePath)
	} else {
		sb.WriteString("- Agent profile: SOUL.md and USER.md\n")
		sb.WriteString("- Long-term memory: memory/MEMORY.md\n")
		sb.WriteString("- History log: memory/history.jsonl (append-only JSONL; prefer built-in `grep` for search).\n")
		sb.WriteString("- Custom skills: skills/{skill-name}/SKILL.md\n")
	}

	sb.WriteString("\nOnly Dream memory-consolidation tasks may edit the profile and long-term memory files listed above.\n")
	sb.WriteString("\n")
	sb.WriteString(platformPolicy())
	sb.WriteString("\n")
	if hint := formatHint(channel); hint != "" {
		sb.WriteString("\n")
		sb.WriteString(hint)
		sb.WriteString("\n")
	}
	sb.WriteString("\n## External Content\n\n")
	sb.WriteString(strings.TrimRight(untrustedContentTemplate, "\n"))

	return sb.String()
}

// runtimeDescription mirrors _get_identity's runtime string (context.py:160).
//
// DIVERGENCE (intentional): the reference reports
// "<OS> <machine>, Python <version>". Reporting "Python" from a Go binary
// would be a lie, so this reports the Go toolchain instead. Everything else
// about the line matches.
func (b *Builder) runtimeDescription() string {
	return fmt.Sprintf("%s %s, Go %s", osName(), runtime.GOARCH, runtime.Version())
}

// osName mirrors platform.system() naming used by the reference.
func osName() string {
	switch runtime.GOOS {
	case "darwin":
		return "macOS"
	case "windows":
		return "Windows"
	case "linux":
		return "Linux"
	default:
		return runtime.GOOS
	}
}

// platformPolicy renders templates/agent/platform_policy.md.
func platformPolicy() string {
	if runtime.GOOS == "windows" {
		return "## Platform Policy (Windows)\n" +
			"- You are running on Windows. Do not assume GNU tools like `grep`, `sed`, or `awk` exist.\n" +
			"- Prefer Windows-native commands or file tools when they are more reliable.\n" +
			"- If terminal output is garbled, retry with UTF-8 output enabled."
	}
	return "## Platform Policy (POSIX)\n" +
		"- You are running on a POSIX system. Prefer UTF-8 and standard shell tools.\n" +
		"- Use file tools when they are simpler or more reliable than shell commands."
}

// formatHint renders the channel-specific Format Hint block (identity.md).
func formatHint(channel string) string {
	switch channel {
	case "telegram", "qq", "discord":
		return "## Format Hint\n" +
			"This conversation is on a messaging app. Use short paragraphs. Avoid large headings (#, ##). " +
			"Use **bold** sparingly. No tables — use plain lists."
	case "whatsapp", "sms":
		return "## Format Hint\n" +
			"This conversation is on a text messaging platform that does not render markdown. Use plain text only."
	case "email":
		return "## Format Hint\n" +
			"This conversation is via email. Structure with clear sections. Markdown may not render — keep formatting simple."
	case "cli", "mochat":
		return "## Format Hint\n" +
			"Output is rendered in a terminal. Avoid markdown headings and tables. Use plain text with minimal formatting."
	}
	return ""
}

// LoadBootstrapFiles loads project instructions plus the agent's global profile
// files, mirroring _load_bootstrap_files (context.py:194-221).
//
// Sources are AGENTS.md from the project workspace, then SOUL.md and USER.md
// from the AGENT workspace. Blank files are skipped.
//
// Three guards come from the reference and are easy to miss because they make
// the function depend on the SHIPPED templates rather than only on the
// workspace:
//
//   - a SOUL.md that is still the legacy bundled template is replaced by the
//     current one, so an upgrading user's workspace keeps working;
//   - a blank file is skipped (Python `.strip()`, not strings.TrimSpace);
//   - AGENTS.md and USER.md that are still the shipped default are skipped
//     entirely — a fresh install has both, byte-identical to the templates,
//     and the reference deliberately does not put boilerplate the user never
//     wrote into the prompt.
func (b *Builder) LoadBootstrapFiles(projectWorkspace string) string {
	root := projectWorkspace
	if root == "" {
		root = b.Workspace
	}
	type source struct {
		name string
		root string
	}
	sources := []source{
		{"AGENTS.md", root},
		{"SOUL.md", b.Workspace},
		{"USER.md", b.Workspace},
	}

	var parts []string
	for _, s := range sources {
		p := filepath.Join(expandPath(s.root), s.name)
		data, err := os.ReadFile(p)
		if err != nil {
			continue
		}
		content := string(data)

		// context.py:208-212. An unmodified legacy SOUL.md is upgraded in
		// memory to the current bundled SOUL.md. `or content` in the reference
		// is a FALSY check, not a None check: an empty bundled template also
		// falls back to the workspace content, so emptiness must be tested here
		// as well as presence.
		if s.name == "SOUL.md" && IsTemplateContent(content, "legacy/SOUL.md") {
			if tpl, ok := BundledTemplate("SOUL.md"); ok && tpl != "" {
				content = tpl
			}
		}

		// context.py:213-214. Python `.strip()`: U+001C..U+001F count as
		// whitespace and strings.TrimSpace would disagree.
		if textutil.PyStrip(content) == "" {
			continue
		}

		// context.py:215-217. Note the ORDER: the blank check above runs first,
		// so a whitespace-only AGENTS.md is skipped as blank rather than as a
		// template. Only AGENTS.md and USER.md are skippable — SOUL.md is
		// always kept.
		if s.name == "AGENTS.md" || s.name == "USER.md" {
			if IsTemplateContent(content, s.name) {
				continue
			}
		}

		parts = append(parts, fmt.Sprintf("## %s\n\n%s", s.name, content))
	}
	return strings.Join(parts, "\n\n")
}

// ReadMemory reads the long-term memory file.
// Mirrors memory.read_memory() reading workspace/memory/MEMORY.md.
func (b *Builder) ReadMemory() string {
	data, err := os.ReadFile(filepath.Join(expandPath(b.Workspace), "memory", "MEMORY.md"))
	if err != nil {
		return ""
	}
	return string(data)
}

// SkillsLoader builds the loader the reference constructs once in
// ContextBuilder.__init__ (context.py:99):
//
//	SkillsLoader(workspace, disabled_skills=set(disabled_skills) if disabled_skills else None)
//
// It is built per call rather than cached because the ported Loader is
// stateless — the reference's loader is too, apart from the Agent Plugin
// module-level cache, which the port deliberately does not reproduce.
func (b *Builder) SkillsLoader() *skills.Loader {
	workspace := expandPath(b.Workspace)
	var loader *skills.Loader
	if b.BuiltinSkillsDir != "" {
		loader = skills.NewWithBuiltinDir(workspace, b.BuiltinSkillsDir)
	} else {
		loader = skills.New(workspace)
	}
	if len(b.DisabledSkills) > 0 {
		disabled := make(map[string]bool, len(b.DisabledSkills))
		for _, name := range b.DisabledSkills {
			disabled[name] = true
		}
		loader.DisabledSkills = disabled
	}
	return loader
}

// expandPath expands a leading ~ to the user's home directory.
func expandPath(p string) string {
	if p == "" {
		return p
	}
	if p == "~" || strings.HasPrefix(p, "~/") {
		home, err := os.UserHomeDir()
		if err == nil {
			if p == "~" {
				return home
			}
			return filepath.Join(home, p[2:])
		}
	}
	return p
}

// expandAndClean expands ~ and resolves the path, mirroring the reference's
// Path.expanduser().resolve() used when comparing workspaces.
func expandAndClean(p string) string {
	expanded := expandPath(p)
	if abs, err := filepath.Abs(expanded); err == nil {
		return filepath.Clean(abs)
	}
	return filepath.Clean(expanded)
}
