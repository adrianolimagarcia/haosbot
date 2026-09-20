package marketplace

import (
	"archive/zip"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

const (
	skillhubAPIBase = "https://api.skillhub.cn"
	maxZipDownload  = 25 << 20  // 25 MiB
	maxZipUnpacked  = 100 << 20 // 100 MiB
	maxZipEntries   = 1000
)

var skillSlugRE = regexp.MustCompile(`^[a-z0-9]+(?:-[a-z0-9]+)*$`)

type skillhubSkillRow struct {
	Slug        string `json:"slug"`
	Name        string `json:"name"`
	DisplayName string `json:"displayName"`
	Description string `json:"description"`
	Installs    int64  `json:"installs"`
	Downloads   int64  `json:"downloads"`
	Version     string `json:"version"`
	Namespace   struct {
		Handle string `json:"handle"`
	} `json:"namespace"`
	OwnerName string `json:"owner_name"`
	Publisher struct {
		Verified bool `json:"verified"`
	} `json:"publisher"`
}

func fetchSkillHubTrending(ctx context.Context, client *http.Client, limit int) ([]SkillItem, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, skillhubAPIBase+"/api/v1/showcase/trending", nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "application/json")
	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("skillhub trending: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("skillhub trending: HTTP %d", resp.StatusCode)
	}

	var payload struct {
		Skills []skillhubSkillRow `json:"skills"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 4<<20)).Decode(&payload); err != nil {
		return nil, fmt.Errorf("skillhub decode: %w", err)
	}

	var items []SkillItem
	for _, row := range payload.Skills {
		item := convertSkillHubRow(row)
		if item != nil {
			items = append(items, *item)
			if limit > 0 && len(items) >= limit {
				break
			}
		}
	}
	return items, nil
}

func searchSkillHub(ctx context.Context, client *http.Client, query string, limit int) ([]SkillItem, error) {
	u := fmt.Sprintf("%s/api/v1/search?q=%s&limit=%d", skillhubAPIBase, url.QueryEscape(query), limit)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "application/json")
	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("skillhub search: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("skillhub search: HTTP %d", resp.StatusCode)
	}

	var payload struct {
		Skills []skillhubSkillRow `json:"skills"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 4<<20)).Decode(&payload); err != nil {
		return nil, fmt.Errorf("skillhub decode: %w", err)
	}

	var items []SkillItem
	for _, row := range payload.Skills {
		item := convertSkillHubRow(row)
		if item != nil {
			items = append(items, *item)
			if limit > 0 && len(items) >= limit {
				break
			}
		}
	}
	return items, nil
}

func convertSkillHubRow(row skillhubSkillRow) *SkillItem {
	slug := strings.TrimSpace(row.Slug)
	if slug == "" || !skillSlugRE.MatchString(slug) {
		return nil
	}
	displayName := strings.TrimSpace(row.DisplayName)
	if displayName == "" {
		displayName = strings.TrimSpace(row.Name)
	}
	if displayName == "" {
		displayName = slug
	}
	handle := strings.TrimSpace(row.Namespace.Handle)
	if handle == "" {
		handle = strings.TrimSpace(row.OwnerName)
	}
	if handle == "" {
		handle = "community"
	}

	return &SkillItem{
		ID:               "skillhub:" + slug,
		SkillID:          slug,
		Name:             displayName,
		Description:      strings.TrimSpace(row.Description),
		Source:           "@" + handle + "/" + slug,
		Provider:         "skillhub",
		Installs:         row.Installs,
		Downloads:        row.Downloads,
		URL:              fmt.Sprintf("https://skillhub.cn/%s/%s", url.PathEscape(handle), url.PathEscape(slug)),
		Version:          strings.TrimSpace(row.Version),
		InstallSupported: true,
		Verified:         row.Publisher.Verified,
	}
}

func installSkillHubSkill(ctx context.Context, client *http.Client, workspace, skillID, version string) (*InstallResponse, error) {
	if !skillSlugRE.MatchString(skillID) {
		return nil, fmt.Errorf("invalid skill name %q", skillID)
	}
	destDir := filepath.Join(workspace, "skills", skillID)
	if _, err := os.Stat(filepath.Join(destDir, "SKILL.md")); err == nil {
		return &InstallResponse{
			Installed: true,
			Name:      skillID,
			Provider:  "skillhub",
			Message:   "skill already installed",
		}, nil
	}

	downloadURL := fmt.Sprintf("%s/api/v1/download?slug=%s", skillhubAPIBase, url.QueryEscape(skillID))
	if version != "" {
		downloadURL += "&version=" + url.QueryEscape(version)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, downloadURL, nil)
	if err != nil {
		return nil, err
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("download skillhub: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("skillhub download HTTP %d", resp.StatusCode)
	}

	data, err := io.ReadAll(io.LimitReader(resp.Body, maxZipDownload+1))
	if err != nil {
		return nil, fmt.Errorf("read skillhub archive: %w", err)
	}
	if len(data) > maxZipDownload {
		return nil, fmt.Errorf("skillhub archive exceeded maximum allowed size (%d bytes)", maxZipDownload)
	}

	zr, err := zip.NewReader(bytes.NewReader(data), int64(len(data)))
	if err != nil {
		return nil, fmt.Errorf("invalid zip archive: %w", err)
	}

	if len(zr.File) > maxZipEntries {
		return nil, fmt.Errorf("too many files in archive (limit %d)", maxZipEntries)
	}

	// Staging directory inside skills
	skillsDir := filepath.Join(workspace, "skills")
	if err := os.MkdirAll(skillsDir, 0o755); err != nil {
		return nil, err
	}
	stageDir, err := os.MkdirTemp(skillsDir, ".skillhub-stage-*")
	if err != nil {
		return nil, err
	}
	defer os.RemoveAll(stageDir)

	var totalUnpacked int64
	for _, f := range zr.File {
		clean := filepath.Clean(f.Name)
		if strings.HasPrefix(clean, "..") || filepath.IsAbs(clean) || strings.HasPrefix(clean, "/") {
			return nil, fmt.Errorf("unsafe path in archive: %q", f.Name)
		}
		target := filepath.Join(stageDir, clean)
		if f.FileInfo().IsDir() {
			if err := os.MkdirAll(target, 0o755); err != nil {
				return nil, err
			}
			continue
		}

		if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
			return nil, err
		}

		rc, err := f.Open()
		if err != nil {
			return nil, err
		}

		out, err := os.OpenFile(target, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, f.Mode()&0o755)
		if err != nil {
			rc.Close()
			return nil, err
		}

		n, err := io.Copy(out, io.LimitReader(rc, maxZipUnpacked-totalUnpacked+1))
		rc.Close()
		out.Close()
		if err != nil {
			return nil, err
		}
		totalUnpacked += n
		if totalUnpacked > maxZipUnpacked {
			return nil, fmt.Errorf("unpacked archive exceeded limit of %d bytes", maxZipUnpacked)
		}
	}

	// Atomic replace
	_ = os.RemoveAll(destDir)
	if err := os.Rename(stageDir, destDir); err != nil {
		return nil, fmt.Errorf("deploy skill: %w", err)
	}

	return &InstallResponse{
		Installed: true,
		Name:      skillID,
		Provider:  "skillhub",
		Message:   "skill successfully installed",
	}, nil
}
