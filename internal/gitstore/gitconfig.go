package gitstore

import (
	"os"
	"path/filepath"
	"strings"
)

// init.defaultBranch support.
//
// dulwich does not hard-code the initial branch: Repo._init_maybe_bare asks
// StackedConfig for init.defaultBranch and only falls back to "master"
// (dulwich/repo.py:2224, DEFAULT_BRANCH) when the user has not configured one.
// That value is observable — it is what .git/HEAD points at after init — so a
// port that always wrote "master" would create a different repository than the
// reference on any machine whose git config sets init.defaultBranch.
//
// Only this one key is read. The file set mirrors dulwich's
// StackedConfig.default_backends: $GIT_CONFIG_GLOBAL, else ~/.gitconfig and
// $XDG_CONFIG_HOME/git/config; then $GIT_CONFIG_SYSTEM, else /etc/gitconfig
// unless $GIT_CONFIG_NOSYSTEM is set. The first file that defines the key wins.

// defaultBranchName returns the configured init.defaultBranch, or "master".
func defaultBranchName() string {
	for _, path := range gitConfigPaths() {
		if v, ok := readGitConfigValue(path, "init", "defaultbranch"); ok && v != "" {
			return v
		}
	}
	return "master"
}

// gitConfigPaths returns the configuration files to consult, in precedence
// order.
func gitConfigPaths() []string {
	var paths []string
	if p := os.Getenv("GIT_CONFIG_GLOBAL"); p != "" {
		paths = append(paths, p)
	} else {
		home, err := os.UserHomeDir()
		if err == nil {
			paths = append(paths, filepath.Join(home, ".gitconfig"))
			xdg := os.Getenv("XDG_CONFIG_HOME")
			if xdg == "" {
				xdg = filepath.Join(home, ".config")
			}
			paths = append(paths, filepath.Join(xdg, "git", "config"))
		}
	}
	if p := os.Getenv("GIT_CONFIG_SYSTEM"); p != "" {
		paths = append(paths, p)
	} else if os.Getenv("GIT_CONFIG_NOSYSTEM") == "" {
		paths = append(paths, "/etc/gitconfig")
	}
	return paths
}

// readGitConfigValue returns the value of section.name from a git config file.
//
// Sections and keys are case-insensitive (git-config(1)); "include" directives
// are not followed.
func readGitConfigValue(path, section, name string) (string, bool) {
	data, err := os.ReadFile(path)
	if err != nil {
		return "", false
	}
	cur := ""
	var value string
	var found bool
	for _, raw := range strings.Split(string(data), "\n") {
		line := strings.TrimSpace(strings.TrimSuffix(raw, "\r"))
		if line == "" || strings.HasPrefix(line, "#") || strings.HasPrefix(line, ";") {
			continue
		}
		if strings.HasPrefix(line, "[") {
			end := strings.Index(line, "]")
			if end < 0 {
				cur = ""
				continue
			}
			header := line[1:end]
			if i := strings.IndexByte(header, ' '); i >= 0 {
				header = header[:i] // drop a "subsection" qualifier
			}
			cur = strings.ToLower(strings.TrimSpace(header))
			continue
		}
		if cur != section {
			continue
		}
		eq := strings.IndexByte(line, '=')
		var key, val string
		if eq < 0 {
			key, val = line, "true" // a bare key means boolean true
		} else {
			key = strings.TrimSpace(line[:eq])
			val = strings.TrimSpace(line[eq+1:])
		}
		if !strings.EqualFold(key, name) {
			continue
		}
		val = strings.Trim(val, `"`)
		value, found = val, true
	}
	return value, found
}
