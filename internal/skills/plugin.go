package skills

// Agent Plugin skills.
//
// Port of the part of nanobot/agent/plugins.py that SkillsLoader.list_skills
// consults: enabled_agent_plugin_skills (plugins.py:82-104) and everything it
// transitively needs — manifest discovery, package fingerprints, the
// activation marker, and the per-plugin data directory.
//
// The rest of plugins.py (MCP server components, logos, the enable/disable UI
// API, discover_agent_plugins) is NOT ported: nothing on the prompt path reads
// it, and the WebUI that consumes it has no Go counterpart yet.
//
// WHY THE FINGERPRINT IS REPRODUCED EXACTLY
//
// A plugin counts as enabled only when its activation marker matches either the
// current package fingerprint or the legacy form (the bare package root). The
// marker file is SHARED STATE on disk: the reference writes it, and a Go
// process reading the same workspace has to agree byte for byte or it will
// treat an enabled plugin as disabled and silently delete the marker
// (plugins.py:441-447). Reproducing sha256 over the sorted rglob of the package
// is therefore not optional fidelity — it is what keeps the two
// implementations from fighting over one file.

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"unicode/utf8"

	"github.com/adrianolimagarcia/nanobot-go/internal/config"
)

// agentPluginSchema is AGENT_PLUGIN_SCHEMA (plugins.py:21).
const agentPluginSchema = "https://agent-plugins.org/schemas/1.0.0/plugin.schema.json"

// pluginNamePattern mirrors _PLUGIN_NAME (plugins.py:24) minus the lookahead,
// which isValidPluginName reproduces.
//
//	^(?!.*(?:--|\.\.))[a-z0-9](?:[a-z0-9.-]*[a-z0-9])?$
var pluginNamePattern = regexp.MustCompile(`\A[a-z0-9](?:[a-z0-9.-]*[a-z0-9])?\z`)

// PluginSkill is one (name, SKILL.md path) pair contributed by an enabled
// plugin, mirroring the tuple enabled_agent_plugin_skills returns.
type PluginSkill struct {
	Name string
	Path string
}

// agentPlugin is the subset of the AgentPlugin dataclass the skill path needs.
type agentPlugin struct {
	Name string
	Root string
}

// EnabledAgentPluginSkills ports enabled_agent_plugin_skills (plugins.py:82-104).
//
// Skills come back in plugin discovery order (plugins sorted by name, then each
// plugin's skill directories sorted by name), and only from plugins whose
// activation marker validates.
//
// DIVERGENCE (documented): the reference memoises the result in a module-level
// _SKILL_CACHE and invalidates it from the enable/disable API. The port keeps no
// cache, so every call re-reads the package — the same answer, without the
// cache-coherence failure modes.
func EnabledAgentPluginSkills(workspace string) []PluginSkill {
	var out []PluginSkill
	for _, plugin := range installedPlugins(workspace) {
		skills := discoverPluginSkills(plugin.Name, plugin.Root)
		if len(skills) == 0 {
			continue
		}
		if _, ok := enabledPackageFingerprint(workspace, plugin); !ok {
			continue
		}
		out = append(out, skills...)
	}
	return out
}

// installedPlugins ports _installed_plugins (plugins.py:52-72).
func installedPlugins(workspace string) []agentPlugin {
	resolved := pyResolve(workspace)
	root, ok := contained(pyJoin(resolved, "plugins"), resolved, true)
	if !ok {
		return nil
	}
	order := []string{}
	byName := map[string]*agentPlugin{}
	for _, candidate := range children(root) {
		pluginRoot, ok := contained(candidate, root, true)
		if !ok {
			continue
		}
		plugin, ok := loadPluginManifest(pluginRoot)
		if !ok {
			continue
		}
		if _, dup := byName[plugin.Name]; dup {
			// A duplicate identity invalidates every plugin with that name,
			// mirroring `plugins[plugin.name] = None`.
			byName[plugin.Name] = nil
			continue
		}
		byName[plugin.Name] = &plugin
		order = append(order, plugin.Name)
	}
	out := make([]agentPlugin, 0, len(order))
	for _, name := range order {
		if byName[name] != nil {
			out = append(out, *byName[name])
		}
	}
	return out
}

// loadPluginManifest ports _load_manifest (plugins.py:225-252), reduced to the
// fields the skill path needs. Every validation that can reject a plugin is
// kept, because rejecting one here removes its skills from the prompt.
func loadPluginManifest(pluginRoot string) (agentPlugin, bool) {
	payload, ok := readPluginObject(pyJoin(pluginRoot, "plugin.json"), pluginRoot)
	if !ok {
		return agentPlugin{}, false
	}
	if schema, _ := payload["$schema"].(string); schema != agentPluginSchema {
		return agentPlugin{}, false
	}
	name, ok := payload["name"].(string)
	if !ok || utf8.RuneCountInString(name) > 64 || !isValidPluginName(name) {
		return agentPlugin{}, false
	}
	return agentPlugin{Name: name, Root: pluginRoot}, true
}

// isValidPluginName reproduces `_PLUGIN_NAME.fullmatch(name)`; RE2 has no
// lookahead, so `(?!.*(?:--|\.\.))` is checked directly. As with skill names,
// Python's `.` does not cross a newline, so only the first line is inspected.
func isValidPluginName(name string) bool {
	firstLine := name
	if i := strings.IndexByte(name, '\n'); i >= 0 {
		firstLine = name[:i]
	}
	if strings.Contains(firstLine, "--") || strings.Contains(firstLine, "..") {
		return false
	}
	return pluginNamePattern.MatchString(name)
}

// discoverPluginSkills ports _discover_plugin_skills (plugins.py:478-502).
func discoverPluginSkills(pluginName, pluginRoot string) []PluginSkill {
	skillsRoot, ok := contained(pyJoin(pluginRoot, "skills"), pluginRoot, true)
	if !ok {
		return nil
	}
	var out []PluginSkill
	for _, candidate := range children(skillsRoot) {
		skillRoot, ok := contained(candidate, skillsRoot, true)
		if !ok {
			continue
		}
		skillFile, ok := contained(pyJoin(skillRoot, "SKILL.md"), pluginRoot, false)
		if !ok {
			continue
		}
		content, readErr := readTextFile(skillFile)
		if readErr != nil {
			continue
		}
		metadata := ParseSkillMetadata(content)
		if metadata == nil || !ValidSkillMetadata(metadata, filepath.Base(candidate)) {
			continue
		}
		out = append(out, PluginSkill{Name: filepath.Base(candidate), Path: skillFile})
	}
	return out
}

// enabledPackageFingerprint ports _enabled_package_fingerprint (plugins.py:417-448).
//
// Note the side effects, which are reproduced deliberately: a marker whose
// content is neither the current activation nor the legacy package root is
// DELETED, and a legacy marker is REWRITTEN in the current form.
func enabledPackageFingerprint(workspace string, plugin agentPlugin) (string, bool) {
	dataDir, err := pluginDataDir(workspace, plugin.Name, false)
	if err != nil {
		return "", false
	}
	marker := pyJoin(dataDir, "enabled")
	info, statErr := os.Stat(marker)
	if statErr != nil || info.IsDir() {
		return "", false
	}
	current, readErr := readTextFile(marker)
	if readErr != nil {
		return "", false
	}
	activation, ok := activationMarker(plugin.Root)
	if !ok {
		_ = os.Remove(marker)
		return "", false
	}
	var payload map[string]any
	if err := json.Unmarshal([]byte(activation), &payload); err != nil {
		return "", false
	}
	fingerprint, ok := payload["fingerprint"].(string)
	if !ok {
		return "", false
	}
	if current == activation {
		return fingerprint, true
	}
	if current == plugin.Root {
		if writeErr := os.WriteFile(marker, []byte(activation), 0o600); writeErr == nil {
			return fingerprint, true
		}
		return "", false
	}
	_ = os.Remove(marker)
	return "", false
}

// activationMarker ports _activation_marker (plugins.py:455-465).
func activationMarker(root string) (string, bool) {
	fingerprint, ok := packageFingerprint(root)
	if !ok {
		return "", false
	}
	return `{"fingerprint":` + pyJSONString(fingerprint) + `,"root":` + pyJSONString(root) + `}`, true
}

// packageFingerprint ports _package_fingerprint (plugins.py:167-188): a SHA-256
// over every path under root, in sorted path order, tagged by kind.
//
// The traversal does NOT follow directory symlinks, matching Path.rglob's
// default; a symlink is hashed by its link target instead. An entry that is
// neither a file, a directory nor a symlink makes the whole fingerprint
// unavailable, which is Python's `return None`.
func packageFingerprint(root string) (string, bool) {
	var rels []string
	var walk func(dir, rel string) bool
	walk = func(dir, rel string) bool {
		entries, err := os.ReadDir(dir)
		if err != nil {
			return false
		}
		for _, entry := range entries {
			childRel := entry.Name()
			if rel != "" {
				childRel = rel + "/" + entry.Name()
			}
			rels = append(rels, childRel)
			if entry.IsDir() {
				if !walk(filepath.Join(dir, entry.Name()), childRel) {
					return false
				}
			}
		}
		return true
	}
	if !walk(root, "") {
		return "", false
	}
	sort.Strings(rels)

	digest := sha256.New()
	for _, rel := range rels {
		full := filepath.Join(root, filepath.FromSlash(rel))
		info, err := os.Lstat(full)
		if err != nil {
			return "", false
		}
		digest.Write([]byte(rel))
		switch {
		case info.Mode()&os.ModeSymlink != 0:
			target, linkErr := os.Readlink(full)
			if linkErr != nil {
				return "", false
			}
			digest.Write([]byte("\x00link\x00"))
			digest.Write([]byte(filepath.ToSlash(target)))
		case info.Mode().IsRegular():
			data, readErr := os.ReadFile(full)
			if readErr != nil {
				return "", false
			}
			digest.Write([]byte("\x00file\x00"))
			digest.Write(data)
		case info.IsDir():
			digest.Write([]byte("\x00dir\x00"))
		default:
			return "", false
		}
		digest.Write([]byte("\x00"))
	}
	return hex.EncodeToString(digest.Sum(nil)), true
}

// pluginDataDir ports _plugin_data_dir (plugins.py:398-414).
//
// create=false never creates anything; the reference's create=true path (used
// by the enable API) is not ported.
func pluginDataDir(workspace, name string, create bool) (string, error) {
	workspaceID := sha256.Sum256([]byte(pyResolve(workspace)))
	digest := hex.EncodeToString(workspaceID[:])[:12]

	current := filepath.Dir(pyResolve(config.DefaultConfigPath()))
	for _, segment := range []string{"plugin-data", digest, name} {
		path := pyJoin(current, segment)
		if create {
			if err := os.MkdirAll(path, 0o700); err != nil {
				return "", err
			}
		}
		resolved, err := resolveStrict(path)
		if err != nil {
			if !create {
				// Python's resolve(strict=False) tolerates a missing tail.
				resolved = pyResolve(path)
			} else {
				return "", err
			}
		}
		if !isRelativeTo(resolved, current) {
			return "", os.ErrPermission
		}
		current = resolved
	}
	return current, nil
}

// --------------------------------------------------------------------------
// small helpers shared with the loader
// --------------------------------------------------------------------------

// resolveStrict ports Path.resolve(strict=True): every component must exist.
func resolveStrict(path string) (string, error) {
	expanded := pyExpandUser(path)
	abs := expanded
	if !filepath.IsAbs(abs) {
		wd, err := os.Getwd()
		if err != nil {
			return "", err
		}
		abs = filepath.Join(wd, abs)
	}
	return filepath.EvalSymlinks(abs)
}

// isRelativeTo ports PurePath.is_relative_to: lexical, no filesystem access.
func isRelativeTo(path, root string) bool {
	_, ok := pyRelativeTo(path, root)
	return ok
}

// contained ports _contained (plugins.py:517-525): resolve strictly, require
// the expected kind, and require the result to be under root.
func contained(path, root string, directory bool) (string, bool) {
	resolved, err := resolveStrict(path)
	if err != nil {
		return "", false
	}
	info, statErr := os.Stat(resolved)
	if statErr != nil {
		return "", false
	}
	if info.IsDir() != directory {
		return "", false
	}
	if !isRelativeTo(resolved, root) {
		return "", false
	}
	return resolved, true
}

// children ports _children (plugins.py:505-512): directory entries SORTED by
// name. This is the opposite of _skill_entries_from_dir, which does not sort,
// and the difference is observable in the prompt.
func children(root string) []string {
	entries, err := os.ReadDir(root)
	if err != nil {
		return nil
	}
	out := make([]string, 0, len(entries))
	for _, entry := range entries {
		out = append(out, entry.Name())
	}
	sort.Strings(out)
	paths := make([]string, 0, len(out))
	for _, name := range out {
		paths = append(paths, pyJoin(root, name))
	}
	return paths
}

// readPluginObject ports _read_object (plugins.py:527-537): a contained JSON
// file whose top level is an object.
func readPluginObject(path, root string) (map[string]any, bool) {
	containedPath, ok := contained(path, root, false)
	if !ok {
		return nil, false
	}
	content, err := readTextFile(containedPath)
	if err != nil {
		return nil, false
	}
	var parsed any
	if err := json.Unmarshal([]byte(content), &parsed); err != nil {
		return nil, false
	}
	obj, ok := parsed.(map[string]any)
	if !ok {
		return nil, false
	}
	return obj, true
}

// readTextFile reads a file the way Path.read_text(encoding="utf-8") does:
// strict UTF-8 decoding and universal-newline translation.
//
// The newline translation is not cosmetic. `Path.read_text` opens in text mode
// with newline=None, so "\r\n" and a lone "\r" both become "\n" before any
// regex sees them. Reading the bytes verbatim would make a CRLF SKILL.md parse
// differently from the reference.
func readTextFile(path string) (string, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	if !utf8.Valid(data) {
		return "", &fs.PathError{Op: "read", Path: path, Err: errInvalidUTF8}
	}
	return normalizeNewlines(string(data)), nil
}

type invalidUTF8Error struct{}

func (invalidUTF8Error) Error() string { return "invalid UTF-8 (UnicodeDecodeError)" }

var errInvalidUTF8 error = invalidUTF8Error{}

// normalizeNewlines ports Python's universal-newline translation.
func normalizeNewlines(s string) string {
	if !strings.ContainsRune(s, '\r') {
		return s
	}
	s = strings.ReplaceAll(s, "\r\n", "\n")
	return strings.ReplaceAll(s, "\r", "\n")
}
