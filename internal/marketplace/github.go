package marketplace

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// GitHub is the last-resort installer: it needs no external CLI, so a skill can
// still be installed on hosts without npx. It resolves the repository default
// branch, walks the recursive tree once, and copies only the blobs that live
// under the skill directory, which keeps the install atomic and bounded.
const (
	githubAPIBase      = "https://api.github.com"
	maxSkillFileBytes  = 2 << 20  // 2 MiB per file
	maxSkillTotalBytes = 12 << 20 // 12 MiB per skill
	maxSkillFileCount  = 256
)

type githubRepoMeta struct {
	DefaultBranch string `json:"default_branch"`
}

type githubTreeEntry struct {
	Path string `json:"path"`
	Type string `json:"type"`
	URL  string `json:"url"`
	Size int64  `json:"size"`
}

type githubTree struct {
	Truncated bool              `json:"truncated"`
	Tree      []githubTreeEntry `json:"tree"`
}

type githubBlob struct {
	Encoding string `json:"encoding"`
	Content  string `json:"content"`
}

func fetchGitHubJSON(ctx context.Context, client *http.Client, target string, limit int64) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, target, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")
	req.Header.Set("User-Agent", "HAOSBOT-marketplace")

	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	switch {
	case resp.StatusCode == http.StatusNotFound:
		return nil, fmt.Errorf("repository or path not found on GitHub")
	case resp.StatusCode == http.StatusForbidden, resp.StatusCode == http.StatusTooManyRequests:
		return nil, errors.New("GitHub API rate limit reached; retry later or install through the skills CLI")
	case resp.StatusCode < 200 || resp.StatusCode > 299:
		return nil, fmt.Errorf("GitHub API returned HTTP %d", resp.StatusCode)
	}

	raw, err := io.ReadAll(io.LimitReader(resp.Body, limit+1))
	if err != nil {
		return nil, err
	}
	if int64(len(raw)) > limit {
		return nil, errors.New("GitHub response exceeded the size limit")
	}
	return raw, nil
}

// installGitHubSkill installs one skill directory from a GitHub repository.
// Everything is staged in a temporary directory inside the workspace skills
// root and renamed into place only after SKILL.md has been verified, so a
// partial download can never leave a half-installed skill behind.
func installGitHubSkill(ctx context.Context, client *http.Client, workspace, source, skillID string) (*InstallResponse, error) {
	return installGitHubSkillFrom(ctx, client, githubAPIBase, workspace, source, skillID)
}

// installGitHubSkillFrom takes the API base explicitly so tests can drive the
// installer against a local server instead of the public GitHub API.
func installGitHubSkillFrom(ctx context.Context, client *http.Client, apiBase, workspace, source, skillID string) (*InstallResponse, error) {
	if !sourceRE.MatchString(source) {
		return nil, fmt.Errorf("invalid skill source %q", source)
	}
	if !skillSlugRE.MatchString(skillID) {
		return nil, fmt.Errorf("invalid skill ID %q", skillID)
	}

	skillsRoot := filepath.Join(workspace, "skills")
	destDir := filepath.Join(skillsRoot, skillID)
	if _, err := os.Stat(filepath.Join(destDir, "SKILL.md")); err == nil {
		return &InstallResponse{
			Installed: true,
			Name:      skillID,
			Provider:  "skills_sh",
			Message:   "skill already installed",
		}, nil
	}

	metaRaw, err := fetchGitHubJSON(ctx, client, apiBase+"/repos/"+source, 1<<20)
	if err != nil {
		return nil, err
	}
	var meta githubRepoMeta
	if err := json.Unmarshal(metaRaw, &meta); err != nil {
		return nil, fmt.Errorf("invalid GitHub repository response: %w", err)
	}
	if meta.DefaultBranch == "" {
		return nil, errors.New("cannot resolve the repository default branch")
	}

	treeRaw, err := fetchGitHubJSON(ctx, client,
		apiBase+"/repos/"+source+"/git/trees/"+url.PathEscape(meta.DefaultBranch)+"?recursive=1", 8<<20)
	if err != nil {
		return nil, err
	}
	var tree githubTree
	if err := json.Unmarshal(treeRaw, &tree); err != nil {
		return nil, fmt.Errorf("invalid GitHub tree response: %w", err)
	}
	if tree.Truncated {
		return nil, errors.New("repository tree is truncated; the skill cannot be installed safely")
	}

	root, err := resolveSkillRoot(tree, skillID)
	if err != nil {
		return nil, err
	}

	type blobFile struct {
		path string
		url  string
		size int64
	}
	files := make([]blobFile, 0, 8)
	var total int64
	prefix := root + "/"
	for _, entry := range tree.Tree {
		if entry.Type != "blob" {
			continue
		}
		if entry.Path != root && !strings.HasPrefix(entry.Path, prefix) {
			continue
		}
		rel := strings.TrimPrefix(strings.TrimPrefix(entry.Path, root), "/")
		if rel == "" {
			continue
		}
		if entry.Size > maxSkillFileBytes {
			return nil, fmt.Errorf("skill file %q exceeds %d bytes", rel, int64(maxSkillFileBytes))
		}
		total += entry.Size
		if total > maxSkillTotalBytes {
			return nil, fmt.Errorf("skill exceeds the %d byte install budget", int64(maxSkillTotalBytes))
		}
		if len(files) >= maxSkillFileCount {
			return nil, fmt.Errorf("skill exceeds the %d file limit", maxSkillFileCount)
		}
		files = append(files, blobFile{path: rel, url: entry.URL, size: entry.Size})
	}
	if len(files) == 0 {
		return nil, errors.New("skill directory is empty")
	}

	if err := os.MkdirAll(skillsRoot, 0o755); err != nil {
		return nil, err
	}
	staging, err := os.MkdirTemp(skillsRoot, ".install-"+skillID+"-")
	if err != nil {
		return nil, err
	}
	defer os.RemoveAll(staging)

	for _, f := range files {
		blobRaw, err := fetchGitHubJSON(ctx, client, f.url, maxSkillFileBytes)
		if err != nil {
			return nil, err
		}
		var blob githubBlob
		if err := json.Unmarshal(blobRaw, &blob); err != nil {
			return nil, fmt.Errorf("invalid GitHub blob response: %w", err)
		}
		if blob.Encoding != "base64" {
			return nil, fmt.Errorf("unsupported GitHub blob encoding %q", blob.Encoding)
		}
		data, err := base64.StdEncoding.DecodeString(strings.ReplaceAll(blob.Content, "\n", ""))
		if err != nil {
			return nil, fmt.Errorf("decode %q: %w", f.path, err)
		}
		dest, err := safeJoin(staging, f.path)
		if err != nil {
			return nil, err
		}
		if err := os.MkdirAll(filepath.Dir(dest), 0o755); err != nil {
			return nil, err
		}
		if err := os.WriteFile(dest, data, 0o644); err != nil {
			return nil, err
		}
	}

	if _, err := os.Stat(filepath.Join(staging, "SKILL.md")); err != nil {
		return nil, errors.New("downloaded skill has no SKILL.md")
	}
	if err := os.Rename(staging, destDir); err != nil {
		return nil, err
	}

	return &InstallResponse{
		Installed: true,
		Name:      skillID,
		Provider:  "skills_sh",
		Message:   fmt.Sprintf("skill installed from %s (%d files)", source, len(files)),
	}, nil
}

// resolveSkillRoot picks the shortest directory that actually contains the
// requested SKILL.md, so nested monorepos resolve to the skill itself rather
// than to an enclosing directory with the same name.
func resolveSkillRoot(tree githubTree, skillID string) (string, error) {
	suffix := "/" + skillID + "/SKILL.md"
	candidates := make([]string, 0, 2)
	for _, entry := range tree.Tree {
		if entry.Type != "blob" {
			continue
		}
		if entry.Path == skillID+"/SKILL.md" || strings.HasSuffix(entry.Path, suffix) {
			candidates = append(candidates, strings.TrimSuffix(entry.Path, "/SKILL.md"))
		}
	}
	if len(candidates) == 0 {
		return "", fmt.Errorf("no %s/SKILL.md found in the repository", skillID)
	}
	sort.Slice(candidates, func(i, j int) bool {
		if len(candidates[i]) != len(candidates[j]) {
			return len(candidates[i]) < len(candidates[j])
		}
		return candidates[i] < candidates[j]
	})
	return candidates[0], nil
}

// safeJoin resolves rel under base and rejects anything that escapes it, so a
// hostile tree entry cannot write outside the staging directory.
func safeJoin(base, rel string) (string, error) {
	if rel == "" || filepath.IsAbs(rel) {
		return "", fmt.Errorf("unsafe skill path %q", rel)
	}
	dest := filepath.Join(base, filepath.FromSlash(rel))
	cleanBase, err := filepath.Abs(base)
	if err != nil {
		return "", err
	}
	cleanDest, err := filepath.Abs(dest)
	if err != nil {
		return "", err
	}
	relative, err := filepath.Rel(cleanBase, cleanDest)
	if err != nil || relative == ".." || strings.HasPrefix(relative, ".."+string(filepath.Separator)) {
		return "", fmt.Errorf("unsafe skill path %q", rel)
	}
	return cleanDest, nil
}
