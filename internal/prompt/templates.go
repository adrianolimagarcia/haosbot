package prompt

// Bundled workspace templates.
//
// The reference ships a `templates/` directory inside the `nanobot` package and
// reads it at runtime through `importlib.resources`
// (utils/helpers.py:951 load_bundled_template). The ContextBuilder uses that
// directory for two distinct purposes:
//
//  1. to decide whether a workspace file is still the SHIPPED DEFAULT, via
//     _is_template_content (agent/context.py:224) — an unmodified AGENTS.md,
//     USER.md, SOUL.md or memory/MEMORY.md is deliberately withheld from the
//     prompt, because telling the model to follow boilerplate the user never
//     wrote is noise; and
//  2. to swap a workspace SOUL.md that still matches the LEGACY template for
//     the current one (agent/context.py:208-212).
//
// Neither is possible without the exact bytes of the shipped defaults, so those
// bytes are embedded here.
//
// WHERE THIS LIVES — `internal/prompt/` rather than `internal/workspaceprompt/`:
// workspaceprompt ports utils/workspace_prompts.py, which is a different
// concept: `<workspace>/prompts/<name>.md` files the USER writes to override a
// built-in prompt. Those live in the user's workspace and are looked up by
// name; the bundled templates live inside the binary and are looked up by
// package-relative path. Conflating them would put user-editable override
// handling and shipped-default content in one package.
//
// `internal/prompt/templates/` is already the embed root for the static agent
// templates (templates/agent/*.md), so mirroring the reference's
// `nanobot/templates/` layout under it keeps one embed root and one obvious
// mapping from reference path to embedded path. A grep confirms
// load_bundled_template has exactly ONE consumer in the whole reference —
// agent/context.py — so this package is also its only natural home.
//
// The embedded files are the frozen reference's templates at
// upstream/nanobot/nanobot/templates/ with the branding rewrites in
// brandingRewrites applied. TestBundledTemplatesMatchReference reverses those
// rewrites and re-checks the result against the checkout byte-for-byte, so drift
// — branded or not — cannot go unnoticed.

import (
	"embed"
	"io/fs"
	"strings"

	"github.com/adrianolimagarcia/nanobot-go/internal/textutil"
)

// bundledTemplates holds the workspace templates that ship with the binary.
//
// The list is explicit rather than a directory walk so that adding a file is a
// visible decision. All seven files of the reference's templates/ root are
// included, not just the five the ContextBuilder consults today:
//
//	AGENTS.md          consulted by _is_template_content (skippable default)
//	SOUL.md            consulted by the legacy-SOUL substitution target
//	USER.md            consulted by _is_template_content (skippable default)
//	legacy/SOUL.md     consulted by the legacy-SOUL substitution source
//	memory/MEMORY.md   consulted by _is_template_content (Memory section guard)
//	HEARTBEAT.md       not consulted by the prompt builder
//	prompts/README.md  not consulted by the prompt builder
//
// HEARTBEAT.md and prompts/README.md are carried because the reference's
// sync_workspace_templates (utils/helpers.py:897) copies every *.md in
// templates/ plus memory/MEMORY.md and prompts/README.md into a new workspace,
// and the "create only if missing" contract of that function can only be
// reproduced from the same bytes. Embedding them costs ~1.7 KB and keeps this
// directory a faithful mirror of the reference's templates/ root, which is what
// makes the digest test meaningful.
//
// The reference's templates/agent/ tree is embedded separately, by the
// per-file //go:embed directives in context.go and dream.go, and is not part of
// this FS.
//
//go:embed templates/AGENTS.md templates/SOUL.md templates/USER.md templates/HEARTBEAT.md templates/legacy/SOUL.md templates/memory/MEMORY.md templates/prompts/README.md
var bundledTemplates embed.FS

// bundledTemplatesRoot is the FS prefix mirroring the reference's
// `nanobot/templates/` directory, so a reference template path can be used as
// an embedded path unchanged.
const bundledTemplatesRoot = "templates"

// BundledTemplate returns the content of a bundled workspace template.
//
// The name is the path of the template RELATIVE to the reference's templates/
// directory, using forward slashes — for example "AGENTS.md", "SOUL.md",
// "legacy/SOUL.md" or "memory/MEMORY.md". It mirrors
// load_bundled_template (utils/helpers.py:951).
//
// ok is false when the template is not bundled, which is the reference's
// `None`. A template that exists but is empty reports ok=true with an empty
// string; the reference distinguishes those too (it checks `is_file()`, not
// truthiness), and callers that care about emptiness must test for it
// themselves — see the `or content` fallback in context.go.
func BundledTemplate(name string) (content string, ok bool) {
	data, err := fs.ReadFile(bundledTemplates, bundledTemplatesRoot+"/"+name)
	if err != nil {
		return "", false
	}
	return string(data), true
}

// brandingRewrites maps the wording this port SHIPS back to the frozen
// reference's wording. It is the ONLY permitted difference between a bundled
// template and its reference counterpart, and it is deliberately an explicit
// table rather than a hash or a "close enough" comparison: TestBundledTemplatesMatchReference
// derives the reference bytes from the shipped bytes through this table and
// compares them byte-for-byte against upstream/nanobot, so any OTHER drift — and
// any change to the table itself — fails the build.
//
// Why the reference bytes still matter after rebranding: IsTemplateContent
// decides whether a workspace file is still the SHIPPED DEFAULT and should be
// withheld from the prompt, and the legacy-SOUL substitution compares a
// workspace SOUL.md against the legacy template. A user who migrated a
// ~/.nanobot workspace never edited those files, so their bytes are the
// REFERENCE bytes. Recognising only the branded bytes would silently start
// injecting "I am nanobot ..." boilerplate into the prompt — the exact
// behaviour the reference withholds — and would break the differential prompt
// suite. The substitutions are reversed here instead of duplicating the
// reference files, so the shipped templates stay the single source of truth.
var brandingRewrites = []struct{ shipped, reference string }{
	// The identity line is a full rewrite, not a word swap: this port describes
	// itself differently from the reference. It MUST stay first — the
	// word-level entries below would otherwise rewrite its "haosbot" before
	// this entry could match the whole line.
	{
		shipped:   "I am haosbot 🤖, a autonomous infrastructure AI agent for HAOS.",
		reference: "I am nanobot 🐈, a personal AI assistant.",
	},
	// Every other occurrence is the bare product name, in both spellings the
	// templates use. These are word-level rather than per-line so that a new
	// mention of the product name cannot silently reintroduce the reference's
	// branding: reversing them still restores the reference bytes exactly,
	// which is what TestBundledTemplatesMatchReference checks.
	{shipped: "haosbot", reference: "nanobot"},
	{shipped: "Haosbot", reference: "Nanobot"},
}

// RebrandReferenceText rewrites the reference's product name to this port's in
// text that is kept byte-identical to the reference ON DISK.
//
// The built-in skills are a verbatim copy of upstream/nanobot/nanobot/skills/,
// verified by SHA-256 in internal/skills/builtin_test.go, so they cannot be
// rebranded at rest without losing that drift check. Their descriptions still
// reach the model through the skills section, so the rewrite happens here, on
// the way into the prompt. Rewrites are applied in table order for the same
// reason referenceVariant is: the identity line must be consumed before the
// word-level entries can touch it.
func RebrandReferenceText(content string) string {
	for _, rewrite := range brandingRewrites {
		if rewrite.shipped == "" || rewrite.reference == "" {
			continue
		}
		content = strings.ReplaceAll(content, rewrite.reference, rewrite.shipped)
	}
	return content
}

// brandingPlaceholder is what NormalizeBranding substitutes for both spellings
// of a branding line, so two prompts that differ ONLY in branding compare equal.
const brandingPlaceholder = "<BRANDING>"

// NormalizeBranding replaces this port's shipped branding and the frozen
// reference's wording with one placeholder.
//
// It exists for the differential harness: rebranding is deliberate, so a prompt
// built by this port can never be byte-identical to the reference's. Callers
// that compare prompts against the reference must use this to discount exactly
// the branding lines and nothing else — comparing raw bytes would fail on the
// intended difference, and normalising more broadly would hide real drift.
func NormalizeBranding(content string) string {
	for _, rewrite := range brandingRewrites {
		if rewrite.shipped != "" {
			content = strings.ReplaceAll(content, rewrite.shipped, brandingPlaceholder)
		}
		if rewrite.reference != "" {
			content = strings.ReplaceAll(content, rewrite.reference, brandingPlaceholder)
		}
	}
	// Dynamic runtime prompt lines use the all-caps HAOSBOT spelling even
	// though bundled templates use Haosbot/haosbot. It is the same deliberate
	// product rebrand and must be discounted by differential tests.
	content = strings.ReplaceAll(content, "HAOSBOT", brandingPlaceholder)
	return content
}

// ReferenceTemplate returns the frozen reference's bytes for a bundled template.
//
// ok is false when the template is not bundled, matching BundledTemplate. For a
// template this port ships unchanged, the result is the bundled content itself.
func ReferenceTemplate(name string) (string, bool) {
	content, ok := BundledTemplate(name)
	if !ok {
		return "", false
	}
	return referenceVariant(content), true
}

// referenceVariant rewrites shipped branding back to the reference's wording.
func referenceVariant(content string) string {
	for _, rewrite := range brandingRewrites {
		if rewrite.shipped == "" {
			continue
		}
		content = strings.ReplaceAll(content, rewrite.shipped, rewrite.reference)
	}
	return content
}

// IsTemplateContent reports whether content is identical to the bundled
// template at templatePath, i.e. whether the user has NOT customised it.
//
// Mirrors ContextBuilder._is_template_content (agent/context.py:224-229)
// exactly, including the two easy-to-miss details:
//
//   - BOTH sides are stripped with Python's str.strip(), not Go's
//     strings.TrimSpace. The two disagree on U+001C..U+001F, so a file holding
//     only "\x1c" is blank to Python but not to Go. A file that differs from
//     the template only in leading/trailing whitespace therefore still counts
//     as the template.
//   - A missing template yields FALSE, not true: "no bundled template to
//     compare against" means "not a template", so the content IS included. The
//     direction matters — getting it backwards would silently drop user files.
//
// A file matching EITHER the shipped (branded) template or the frozen reference
// template counts as unmodified. See brandingRewrites for why the reference
// variant must be recognised.
func IsTemplateContent(content, templatePath string) bool {
	tpl, ok := BundledTemplate(templatePath)
	if !ok {
		return false
	}
	stripped := textutil.PyStrip(content)
	if stripped == textutil.PyStrip(tpl) {
		return true
	}
	if ref := referenceVariant(tpl); ref != tpl {
		return stripped == textutil.PyStrip(ref)
	}
	return false
}
