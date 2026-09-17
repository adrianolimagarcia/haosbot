package skills

// CLI Apps skill aliases.
//
// Port of the tiny slice of nanobot/apps/cli/service.py that
// SkillsLoader._skill_aliases (agent/skills.py:65-72) consults:
//
//	CliAppManager(workspace=...).installed_skill_aliases()
//
// which is `{_skill_name(n, legacy=True): _skill_name(n) for n in
// installed_names()}` for the registry names recorded in
// `<data dir>/cli-apps/installed.json`.
//
// WHY IT IS WORTH PORTING AT ALL
//
// The aliases are consulted in three places on the prompt path, and each one
// changes observable output:
//
//   - list_skills expands a disabled name to its alias pair, so disabling
//     `cli-app-my-app` also disables the legacy `cli-app-my_app`;
//   - load_skill resolves a legacy name to the installed skill's directory, so
//     `$cli-app-my_app` still finds the skill;
//   - get_explicitly_invoked_skills does the same for `$name` references.
//
// DIVERGENCE (documented): the reference's CliAppManager constructor calls
// ensure_dir and therefore CREATES <data dir> and <data dir>/cli-apps on
// construction, even when nothing is installed. The port only reads; a missing
// registry file is an empty alias map, which is the same answer the reference
// computes from the empty directory it just made.

import (
	"encoding/json"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	"github.com/adrianolimagarcia/nanobot-go/internal/config"
)

// safeNameRe mirrors _SAFE_NAME_RE (service.py:43): runs of characters that are
// not [a-z0-9_-] collapse to a single dash.
var safeNameRe = regexp.MustCompile(`[^a-z0-9_-]+`)

// cliAppSkillName ports _skill_name (service.py:215-219).
func cliAppSkillName(name string, legacy bool) string {
	clean := strings.Trim(safeNameRe.ReplaceAllString(strings.ToLower(name), "-"), "-")
	if !legacy {
		clean = strings.ReplaceAll(clean, "_", "-")
	}
	if clean == "" {
		clean = "app"
	}
	return "cli-app-" + clean
}

// InstalledSkillAliases ports installed_skill_aliases (service.py:463-471).
//
// The returned map is legacy -> canonical and only contains entries where the
// two differ, i.e. names containing an underscore.
func InstalledSkillAliases(workspace string) map[string]string {
	aliases := map[string]string{}
	for _, name := range installedCLIAppNames() {
		legacy := cliAppSkillName(name, true)
		canonical := cliAppSkillName(name, false)
		if legacy != canonical {
			aliases[legacy] = canonical
		}
	}
	return aliases
}

// installedCLIAppNames ports installed_names (service.py:459-461):
// `sorted(str(name) for name in self._load_installed())`.
func installedCLIAppNames() []string {
	data := readInstalledRegistry()
	if len(data) == 0 {
		return nil
	}
	names := make([]string, 0, len(data))
	for name := range data {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

// readInstalledRegistry ports _read_json(installed_path) followed by
// _load_installed (service.py:361-366, :451-454).
func readInstalledRegistry() map[string]any {
	path := filepath.Join(config.DefaultDataDir(), "cli-apps", "installed.json")
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil
	}
	var parsed any
	if err := json.Unmarshal(raw, &parsed); err != nil {
		return nil
	}
	top, ok := parsed.(map[string]any)
	if !ok || len(top) == 0 {
		return nil
	}
	if apps, ok := top["apps"].(map[string]any); ok {
		return apps
	}
	return top
}
