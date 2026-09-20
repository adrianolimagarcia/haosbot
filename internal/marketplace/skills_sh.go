package marketplace

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
)

const (
	skillsShAPIBase = "https://skills.sh"
)

var sourceRE = regexp.MustCompile(`^[A-Za-z0-9](?:[A-Za-z0-9-]{0,37}[A-Za-z0-9])?/[A-Za-z0-9](?:[A-Za-z0-9_.-]{0,98}[A-Za-z0-9])?$`)

type skillsShRow struct {
	Source   string `json:"source"`
	SkillID  string `json:"skillId"`
	Name     string `json:"name"`
	Installs int64  `json:"installs"`
}

func npxAvailable() bool {
	_, err := exec.LookPath("npx")
	return err == nil
}

func fetchSkillsShTrending(ctx context.Context, client *http.Client, limit int) ([]SkillItem, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, skillsShAPIBase+"/api/skills/trending/0", nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "application/json")
	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("skills.sh trending: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("skills.sh trending: HTTP %d", resp.StatusCode)
	}

	var payload struct {
		Skills []skillsShRow `json:"skills"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 4<<20)).Decode(&payload); err != nil {
		return nil, fmt.Errorf("skills.sh decode: %w", err)
	}

	canInstall := npxAvailable()
	items := make([]SkillItem, 0)
	seenSources := map[string]bool{}
	for _, row := range payload.Skills {
		if seenSources[row.Source] {
			continue
		}
		item := convertSkillsShRow(row, canInstall)
		if item != nil {
			seenSources[row.Source] = true
			items = append(items, *item)
			if limit > 0 && len(items) >= limit {
				break
			}
		}
	}
	return items, nil
}

func searchSkillsSh(ctx context.Context, client *http.Client, query string, limit int) ([]SkillItem, error) {
	u := fmt.Sprintf("%s/api/search?q=%s&limit=%d", skillsShAPIBase, url.QueryEscape(query), limit)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "application/json")
	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("skills.sh search: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("skills.sh search: HTTP %d", resp.StatusCode)
	}

	var payload struct {
		Skills []skillsShRow `json:"skills"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 4<<20)).Decode(&payload); err != nil {
		return nil, fmt.Errorf("skills.sh decode: %w", err)
	}

	canInstall := npxAvailable()
	items := make([]SkillItem, 0)
	for _, row := range payload.Skills {
		item := convertSkillsShRow(row, canInstall)
		if item != nil {
			items = append(items, *item)
			if limit > 0 && len(items) >= limit {
				break
			}
		}
	}
	return items, nil
}

func convertSkillsShRow(row skillsShRow, canInstall bool) *SkillItem {
	source := strings.TrimSpace(row.Source)
	skillID := strings.TrimSpace(row.SkillID)
	if !sourceRE.MatchString(source) || !skillSlugRE.MatchString(skillID) {
		return nil
	}
	name := strings.TrimSpace(row.Name)
	if name == "" {
		name = skillID
	}

	return &SkillItem{
		ID:               fmt.Sprintf("%s/%s", source, skillID),
		SkillID:          skillID,
		Name:             name,
		Description:      fmt.Sprintf("Agent skill from %s (%s)", source, skillID),
		Source:           source,
		Provider:         "skills_sh",
		Installs:         row.Installs,
		URL:              fmt.Sprintf("https://skills.sh/%s/%s", source, skillID),
		InstallSupported: canInstall,
	}
}

func installSkillsShSkill(ctx context.Context, client *http.Client, workspace, source, skillID string) (*InstallResponse, error) {
	if !sourceRE.MatchString(source) {
		return nil, fmt.Errorf("invalid skill source %q", source)
	}
	if !skillSlugRE.MatchString(skillID) {
		return nil, fmt.Errorf("invalid skill ID %q", skillID)
	}

	destDir := filepath.Join(workspace, "skills", skillID)
	if _, err := os.Stat(filepath.Join(destDir, "SKILL.md")); err == nil {
		return &InstallResponse{
			Installed: true,
			Name:      skillID,
			Provider:  "skills_sh",
			Message:   "skill already installed",
		}, nil
	}

	if npxAvailable() {
		cmd := exec.CommandContext(ctx, "npx", "--yes", "skills@latest", "add", source, "--skill", skillID, "--agent", "openclaw", "--copy", "--yes")
		cmd.Dir = workspace
		cmd.Env = append(os.Environ(), "DISABLE_TELEMETRY=1")
		output, err := cmd.CombinedOutput()
		if err != nil {
			return nil, fmt.Errorf("npx skills add failed: %w (output: %s)", err, strings.TrimSpace(string(output)))
		}
		return &InstallResponse{
			Installed: true,
			Name:      skillID,
			Provider:  "skills_sh",
			Message:   "skill installed via skills CLI",
		}, nil
	}

	// Fallback: fetch directly from GitHub raw content
	branches := []string{"main", "master"}
	paths := []string{
		fmt.Sprintf("skills/%s/SKILL.md", skillID),
		fmt.Sprintf("%s/SKILL.md", skillID),
		"SKILL.md",
	}

	var content []byte
	for _, branch := range branches {
		for _, p := range paths {
			rawURL := fmt.Sprintf("https://raw.githubusercontent.com/%s/%s/%s", source, branch, p)
			req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
			if err != nil {
				continue
			}
			resp, err := client.Do(req)
			if err != nil || resp.StatusCode != http.StatusOK {
				if resp != nil {
					resp.Body.Close()
				}
				continue
			}
			data, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20)) // 1 MiB
			resp.Body.Close()
			if err == nil && len(data) > 0 {
				content = data
				break
			}
		}
		if len(content) > 0 {
			break
		}
	}

	if len(content) == 0 {
		return nil, fmt.Errorf("could not download skill from %s (npx is not installed and GitHub raw fetch was not found)", source)
	}

	if err := os.MkdirAll(destDir, 0o755); err != nil {
		return nil, err
	}
	if err := os.WriteFile(filepath.Join(destDir, "SKILL.md"), content, 0o644); err != nil {
		return nil, err
	}

	return &InstallResponse{
		Installed: true,
		Name:      skillID,
		Provider:  "skills_sh",
		Message:   "skill installed directly from source repository",
	}, nil
}
