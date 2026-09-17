package prompt

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestSystemPromptStructure(t *testing.T) {
	ws := t.TempDir()
	b := New(ws)
	got := b.BuildSystemPrompt("cli", nil, ws, true)

	for _, want := range []string{
		"## Runtime",
		"## Workspace",
		"# Tool Usage Notes",
		"## Platform Policy",
		"## External Content",
		"## Format Hint",
		"Only Dream memory-consolidation tasks",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("system prompt missing %q", want)
		}
	}
	// CLI channel must get the terminal format hint.
	if !strings.Contains(got, "Output is rendered in a terminal") {
		t.Error("cli format hint missing")
	}
	// Agent workspace == project workspace, so the project section is omitted.
	if strings.Contains(got, "# Current Project") {
		t.Error("Current Project section should be absent when workspaces match")
	}
}

func TestFormatHintPerChannel(t *testing.T) {
	ws := t.TempDir()
	b := New(ws)
	cases := map[string]string{
		"telegram": "messaging app",
		"discord":  "messaging app",
		"whatsapp": "does not render markdown",
		"email":    "via email",
		"cli":      "rendered in a terminal",
		"unknown":  "",
	}
	for channel, want := range cases {
		got := b.BuildSystemPrompt(channel, nil, ws, true)
		if want == "" {
			if strings.Contains(got, "## Format Hint") {
				t.Errorf("channel %q should have no format hint", channel)
			}
			continue
		}
		if !strings.Contains(got, want) {
			t.Errorf("channel %q: missing %q", channel, want)
		}
	}
}

func TestCurrentProjectSectionWhenWorkspacesDiffer(t *testing.T) {
	agentWS := t.TempDir()
	projectWS := t.TempDir()
	b := New(agentWS)

	got := b.BuildSystemPrompt("cli", nil, projectWS, true)
	if !strings.Contains(got, "# Current Project") {
		t.Fatal("Current Project section missing when workspaces differ")
	}
	if !strings.Contains(got, "Working directory: "+projectWS) {
		t.Errorf("working directory not reported; got:\n%s", got)
	}
	// The agent workspace must be advertised separately.
	if !strings.Contains(got, "Nanobot's agent workspace is at: ") {
		t.Error("agent workspace line missing when workspaces differ")
	}
}

func TestBootstrapFilesLoaded(t *testing.T) {
	agentWS := t.TempDir()
	projectWS := t.TempDir()

	mustWrite(t, filepath.Join(projectWS, "AGENTS.md"), "PROJECT INSTRUCTIONS")
	mustWrite(t, filepath.Join(agentWS, "SOUL.md"), "AGENT SOUL")
	mustWrite(t, filepath.Join(agentWS, "USER.md"), "AGENT USER")

	b := New(agentWS)
	got := b.BuildSystemPrompt("cli", nil, projectWS, true)

	for _, want := range []string{
		"## AGENTS.md\n\nPROJECT INSTRUCTIONS",
		"## SOUL.md\n\nAGENT SOUL",
		"## USER.md\n\nAGENT USER",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("bootstrap section missing: %q", want)
		}
	}

	// Order must be AGENTS.md, SOUL.md, USER.md.
	iAgents := strings.Index(got, "## AGENTS.md")
	iSoul := strings.Index(got, "## SOUL.md")
	iUser := strings.Index(got, "## USER.md")
	if !(iAgents < iSoul && iSoul < iUser) {
		t.Errorf("bootstrap order wrong: agents=%d soul=%d user=%d", iAgents, iSoul, iUser)
	}
}

func TestBootstrapSkipsEmptyFiles(t *testing.T) {
	agentWS := t.TempDir()
	mustWrite(t, filepath.Join(agentWS, "SOUL.md"), "   \n\t\n")

	b := New(agentWS)
	got := b.BuildSystemPrompt("cli", nil, agentWS, true)
	if strings.Contains(got, "## SOUL.md") {
		t.Error("whitespace-only bootstrap file should be skipped")
	}
}

func TestMemoryInjected(t *testing.T) {
	agentWS := t.TempDir()
	if err := os.MkdirAll(filepath.Join(agentWS, "memory"), 0o755); err != nil {
		t.Fatal(err)
	}
	mustWrite(t, filepath.Join(agentWS, "memory", "MEMORY.md"), "REMEMBERED FACT")

	b := New(agentWS)
	got := b.BuildSystemPrompt("cli", nil, agentWS, true)
	if !strings.Contains(got, "# Memory\n\n## Long-term Memory\nREMEMBERED FACT") {
		t.Errorf("memory section missing:\n%s", got)
	}

	// includeMemory=false must omit it.
	got = b.BuildSystemPrompt("cli", nil, agentWS, false)
	if strings.Contains(got, "REMEMBERED FACT") {
		t.Error("memory included despite includeMemory=false")
	}
}

func TestSkillsSummary(t *testing.T) {
	agentWS := t.TempDir()
	// Created in a NON-alphabetical order on purpose. _skill_entries_from_dir
	// uses base.iterdir() and never sorts, so the reference lists these as
	// mid/zeta/alpha; a port that sorted (or that used os.ReadDir) would emit
	// alpha/mid/zeta and tell the model a different order.
	for _, name := range []string{"mid", "zeta", "alpha", "notaskill"} {
		if err := os.MkdirAll(filepath.Join(agentWS, "skills", name), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	for _, name := range []string{"mid", "zeta", "alpha"} {
		mustWrite(t, filepath.Join(agentWS, "skills", name, "SKILL.md"), "# "+name)
	}
	// notaskill has no SKILL.md and must be excluded.

	b := New(agentWS)
	got := b.BuildSystemPrompt("cli", nil, agentWS, true)

	if !strings.Contains(got, "# Skills") {
		t.Fatal("skills section missing")
	}
	for _, name := range []string{"mid", "zeta", "alpha"} {
		if !strings.Contains(got, name+"/SKILL.md") {
			t.Errorf("skill %s not listed:\n%s", name, got)
		}
	}
	if strings.Contains(got, "notaskill") {
		t.Error("directory without SKILL.md should be excluded")
	}

	// Directory order, NOT sorted order. The reference's order is whatever the
	// filesystem returns; on this fixture that is creation order.
	iMid := strings.Index(got, "mid/SKILL.md")
	iZeta := strings.Index(got, "zeta/SKILL.md")
	iAlpha := strings.Index(got, "alpha/SKILL.md")
	if !(iMid < iZeta && iZeta < iAlpha) {
		t.Errorf("workspace skills are not in directory order: mid=%d zeta=%d alpha=%d", iMid, iZeta, iAlpha)
	}
	if strings.Index(got, "alpha/SKILL.md") < strings.Index(got, "mid/SKILL.md") {
		t.Error("workspace skills were sorted; the reference does not sort them")
	}

	// The group header names the RELATIVE root, because the agent and project
	// workspaces are the same here (build_skills_summary's use_relative_roots).
	if !strings.Contains(got, "### Workspace skills (`skills`)") {
		t.Errorf("workspace group header missing or not relative:\n%s", got)
	}
}

func TestDisabledSkillsExcluded(t *testing.T) {
	agentWS := t.TempDir()
	for _, name := range []string{"github", "weather"} {
		if err := os.MkdirAll(filepath.Join(agentWS, "skills", name), 0o755); err != nil {
			t.Fatal(err)
		}
		mustWrite(t, filepath.Join(agentWS, "skills", name, "SKILL.md"), "# "+name)
	}

	b := New(agentWS)
	b.DisabledSkills = []string{"weather"}
	got := b.BuildSystemPrompt("cli", nil, agentWS, true)

	if strings.Contains(got, "weather/SKILL.md") {
		t.Error("disabled skill still listed")
	}
	if !strings.Contains(got, "github/SKILL.md") {
		t.Error("enabled skill missing")
	}
}

func TestBuiltinSkillsAlwaysPresent(t *testing.T) {
	// The reference's BUILTIN_SKILLS_DIR lives inside the Python package, so a
	// workspace with no skills/ of its own still gets a "# Skills" section
	// listing the built-in group. The port's embedded copy of
	// nanobot/skills/ is the analogue; before it existed the Go prompt emitted
	// NOTHING here, which is the reported bug.
	ws := t.TempDir()
	b := New(ws)
	got := b.BuildSystemPrompt("cli", nil, ws, true)

	if !strings.Contains(got, "# Skills") {
		t.Fatal("the built-in skills section is absent; a fresh install must still list its skills")
	}
	if !strings.Contains(got, "### Built-in skills (`skills`)") {
		t.Errorf("built-in group header missing:\n%s", got)
	}
	if !strings.Contains(got, "- **cron**") {
		t.Errorf("the built-in cron skill is not listed:\n%s", got)
	}
	// No workspace group when the workspace has no skills/.
	if strings.Contains(got, "### Workspace skills") {
		t.Error("a workspace group appeared for a workspace with no skills/")
	}
}

func TestActiveSkillsSectionFromAlwaysWorkspaceSkill(t *testing.T) {
	// get_always_skills returns [] for the BUNDLED skills, so the "# Active
	// Skills" section is absent on a fresh install for both implementations. It
	// only becomes observable once a workspace skill sets `always`.
	ws := t.TempDir()
	mustWrite(t, filepath.Join(ws, "skills", "pinned", "SKILL.md"),
		"---\nname: pinned\ndescription: Always active.\nalways: true\n---\n\nPINNED BODY\n")
	mustWrite(t, filepath.Join(ws, "skills", "lazy", "SKILL.md"),
		"---\nname: lazy\ndescription: Not always active.\n---\n\nLAZY BODY\n")

	b := New(ws)
	got := b.BuildSystemPrompt("cli", nil, ws, true)

	if !strings.Contains(got, "# Active Skills") {
		t.Fatalf("the # Active Skills section is missing:\n%s", got)
	}
	if !strings.Contains(got, "### Skill: pinned\n\nPINNED BODY") {
		t.Errorf("the always-active skill's body was not inlined:\n%s", got)
	}
	if strings.Contains(got, "LAZY BODY") {
		t.Error("a skill without always:true was inlined")
	}
	// Active skills are excluded from the summary below, so they are not ALSO
	// advertised as a path to read.
	if strings.Contains(got, "- **pinned**") {
		t.Error("the always-active skill is still advertised in the skills summary")
	}
	if !strings.Contains(got, "- **lazy**") {
		t.Error("the non-always skill should still be listed in the summary")
	}
	// Section order: Active Skills comes BEFORE the summary (context.py:132-142).
	if strings.Index(got, "# Active Skills") > strings.Index(got, "# Skills") {
		t.Error("Active Skills must precede the Skills summary")
	}
}

func TestArchivedSummaryInjected(t *testing.T) {
	ws := t.TempDir()
	b := New(ws)

	got := b.BuildSystemPrompt("cli", &Summary{Text: "earlier stuff", LastActive: "2026-01-01"}, ws, true)
	if !strings.Contains(got, "[Archived Context Summary]") {
		t.Fatal("archived summary section missing")
	}
	if !strings.Contains(got, "earlier stuff") {
		t.Error("summary text missing")
	}

	// The literal "(nothing)" sentinel must be suppressed, matching
	// context.py:145.
	got = b.BuildSystemPrompt("cli", &Summary{Text: "(nothing)"}, ws, true)
	if strings.Contains(got, "[Archived Context Summary]") {
		t.Error("(nothing) summary should be suppressed")
	}
}

func TestSectionSeparator(t *testing.T) {
	agentWS := t.TempDir()
	projectWS := t.TempDir()
	mustWrite(t, filepath.Join(projectWS, "AGENTS.md"), "X")

	b := New(agentWS)
	got := b.BuildSystemPrompt("cli", nil, projectWS, true)
	if !strings.Contains(got, "\n\n---\n\n") {
		t.Error("sections not joined with the reference separator")
	}
}

func TestExpandPath(t *testing.T) {
	home, err := os.UserHomeDir()
	if err != nil {
		t.Skip("no home dir")
	}
	if got := expandPath("~/x"); got != filepath.Join(home, "x") {
		t.Errorf("expandPath(~/x) = %q", got)
	}
	if got := expandPath("~"); got != home {
		t.Errorf("expandPath(~) = %q", got)
	}
	if got := expandPath("/abs/path"); got != "/abs/path" {
		t.Errorf("absolute path changed: %q", got)
	}
	if got := expandPath("rel/path"); got != "rel/path" {
		t.Errorf("relative path changed: %q", got)
	}
}

func mustWrite(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}
