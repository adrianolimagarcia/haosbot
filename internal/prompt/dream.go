package prompt

import (
	_ "embed"
	"strings"

	"github.com/adrianolimagarcia/nanobot-go/internal/textutil"
)

//go:embed templates/agent/dream.md
var dreamTemplate string

// dreamSkillCreatorPlaceholder is the only Jinja2 substitution in
// templates/agent/dream.md.
const dreamSkillCreatorPlaceholder = "{{ skill_creator_path }}"

// RenderDreamPrompt renders templates/agent/dream.md.
//
// Mirrors MemoryStore.default_dream_prompt (agent/memory.py:517-525), which
// calls render_template("agent/dream.md", strip=True,
// skill_creator_path=str(BUILTIN_SKILLS_DIR / "skill-creator" / "SKILL.md")).
//
// The template is embedded verbatim and only the substitution is reimplemented,
// following the policy documented in this package's doc comment. Two details of
// Jinja2's rendering are reproduced deliberately:
//
//   - `strip=True` is Python's str.rstrip(), so the trailing newline of the
//     file is removed (and any other trailing whitespace, which is why this is
//     textutil.PyRStrip rather than strings.TrimRight — the two disagree on
//     U+001C..U+001F).
//   - Jinja2's default keep_trailing_newline=False would drop the file's final
//     newline before rstrip runs. Both orders produce the same string for this
//     file, and the file has no {% %} blocks, so trim_blocks/lstrip_blocks have
//     nothing to act on. Verified byte-for-byte against the reference by
//     compat.TestDreamMatchesPythonReference.
func RenderDreamPrompt(skillCreatorPath string) string {
	return textutil.PyRStrip(strings.ReplaceAll(dreamTemplate, dreamSkillCreatorPlaceholder, skillCreatorPath))
}
