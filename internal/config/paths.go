package config

// Runtime path helpers.
//
// Port of upstream/nanobot/nanobot/config/paths.py (71 lines).
//
// DELIBERATE DIVERGENCE (documented, not silent): the Python helpers return
// `pathlib.Path` objects produced by `ensure_dir()`, which CREATES the directory
// as a side effect, and they follow a mutable module-global config path
// (`loader._current_config_path`) so a second instance rooted elsewhere resolves
// its own data dir. The exported API here is a set of parameterless path
// getters, so they are pure: they neither create directories nor consult global
// mutable state. Directory creation is left to callers that actually write.
//
// Paths are returned ABSOLUTE with `~` expanded, matching what the reference
// actually yields (compat/python/dump_reference.py prints the expanded
// /home/<user>/.nanobot/... forms) and matching `get_workspace_path()`, which
// applies `.expanduser()`. `Load` and `Save` also accept a `~`-prefixed path, so
// either spelling works at the call site.

import (
	"os"
	"path/filepath"
	"strings"
)

// homeDir returns the current user's home directory, matching Python's
// `Path.home()` (which consults $HOME, then the password database).
func homeDir() string {
	if h := os.Getenv("HOME"); h != "" {
		return strings.TrimRight(h, string(os.PathSeparator))
	}
	if h, err := os.UserHomeDir(); err == nil && h != "" {
		return strings.TrimRight(h, string(os.PathSeparator))
	}
	return ""
}

// expandUser ports `Path.expanduser()`: a leading "~" (or "~user") is replaced
// by the user's home directory. Only the bare "~" form is supported here; a
// "~user" form that cannot be resolved is returned unchanged, like Python when
// the user does not exist.
func expandUser(path string) string {
	if path == "" || path[0] != '~' {
		return path
	}
	if path == "~" {
		return homeDir()
	}
	if strings.HasPrefix(path, "~/") || strings.HasPrefix(path, `~\`) {
		return filepath.Join(homeDir(), path[2:])
	}
	// "~user/..." — resolve only for the current user.
	rest := path[1:]
	user := rest
	if i := strings.IndexAny(rest, `/\`); i >= 0 {
		user = rest[:i]
	}
	if user == currentUserName() {
		return filepath.Join(homeDir(), rest[len(user):])
	}
	return path
}

func currentUserName() string {
	for _, k := range []string{"USER", "LOGNAME"} {
		if v := os.Getenv(k); v != "" {
			return v
		}
	}
	return ""
}

// resolvePath ports `path.expanduser().resolve(strict=False)`: expand `~` and
// make absolute relative to the working directory. Symlinks are NOT resolved,
// because Go has no direct equivalent of `Path.resolve()` that tolerates
// missing files without also resolving links; the reference resolves them, but
// no behavior in this package depends on the difference.
func resolvePath(path string) string {
	p := expandUser(path)
	if p == "" {
		return ""
	}
	if !filepath.IsAbs(p) {
		if abs, err := filepath.Abs(p); err == nil {
			p = abs
		}
	}
	return filepath.Clean(p)
}

// parentDir returns the parent directory of an already-resolved path.
func parentDir(path string) string { return filepath.Dir(path) }

// DefaultConfigPath returns the configuration file path.
// Prefers ~/.haosbot/config.json if it exists or if ~/.haosbot/ exists;
// otherwise falls back to ~/.nanobot/config.json for backward compatibility.
func DefaultConfigPath() string {
	haosCfg := filepath.Join(homeDir(), ".haosbot", "config.json")
	if _, err := os.Stat(haosCfg); err == nil {
		return haosCfg
	}
	haosDir := filepath.Join(homeDir(), ".haosbot")
	if _, err := os.Stat(haosDir); err == nil {
		return haosCfg
	}
	nanoCfg := filepath.Join(homeDir(), ".nanobot", "config.json")
	if _, err := os.Stat(nanoCfg); err == nil {
		return nanoCfg
	}
	return haosCfg
}

// DefaultDataDir returns the instance-level runtime data directory.
func DefaultDataDir() string { return parentDir(DefaultConfigPath()) }

// runtimeSubdir returns a named runtime subdirectory under the data dir.
func runtimeSubdir(name string) string { return filepath.Join(DefaultDataDir(), name) }

// DefaultWorkspace returns the agent workspace path.
func DefaultWorkspace() string {
	haosWs := filepath.Join(homeDir(), ".haosbot", "workspace")
	if _, err := os.Stat(haosWs); err == nil {
		return haosWs
	}
	nanoWs := filepath.Join(homeDir(), ".nanobot", "workspace")
	if _, err := os.Stat(nanoWs); err == nil {
		return nanoWs
	}
	return haosWs
}

// SessionsDir returns the session storage directory.
// Port of paths.get_legacy_sessions_dir (paths.py:69-71): the legacy global
// session directory, still the active `~/.nanobot/sessions` location.
func SessionsDir() string { return filepath.Join(homeDir(), ".nanobot", "sessions") }

// CronDir returns the cron storage directory (paths.py:36-38).
func CronDir() string { return runtimeSubdir("cron") }

// LogsDir returns the logs directory (paths.py:41-43).
func LogsDir() string { return runtimeSubdir("logs") }

// WebUIDir returns the directory for WebUI-only persisted display threads
// (paths.py:46-48).
func WebUIDir() string { return runtimeSubdir("webui") }

// MediaDir returns the media directory, optionally namespaced per channel.
// Port of paths.get_media_dir (paths.py:30-33): an empty channel returns the
// base media directory.
func MediaDir(channel string) string {
	base := runtimeSubdir("media")
	if channel == "" {
		return base
	}
	return filepath.Join(base, channel)
}

// CLIHistoryPath returns the shared CLI history file path.
// Port of paths.get_cli_history_path (paths.py:64-66).
func CLIHistoryPath() string {
	return filepath.Join(homeDir(), ".nanobot", "history", "cli_history")
}
