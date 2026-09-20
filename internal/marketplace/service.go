package marketplace

import (
	"context"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

type Service struct {
	workspace string
	client    *http.Client
}

func NewService(workspace string) *Service {
	return &Service{
		workspace: workspace,
		client: &http.Client{
			Timeout: 45 * time.Second,
		},
	}
}

func (s *Service) installedSkills() map[string]bool {
	installed := make(map[string]bool)
	skillsDir := filepath.Join(s.workspace, "skills")
	entries, err := os.ReadDir(skillsDir)
	if err == nil {
		for _, e := range entries {
			if e.IsDir() {
				if _, err := os.Stat(filepath.Join(skillsDir, e.Name(), "SKILL.md")); err == nil {
					installed[e.Name()] = true
				}
			}
		}
	}
	return installed
}

func (s *Service) Trending(ctx context.Context, provider string, limit int) (*TrendingResponse, error) {
	if limit <= 0 {
		limit = 12
	}
	p := strings.ToLower(strings.TrimSpace(provider))
	if p == "" {
		p = "all"
	}

	var items []SkillItem
	var mu sync.Mutex
	var wg sync.WaitGroup

	if p == "all" || p == "skillhub" {
		wg.Add(1)
		go func() {
			defer wg.Done()
			res, err := fetchSkillHubTrending(ctx, s.client, limit)
			if err == nil && len(res) > 0 {
				mu.Lock()
				items = append(items, res...)
				mu.Unlock()
			}
		}()
	}

	if p == "all" || p == "skills_sh" {
		wg.Add(1)
		go func() {
			defer wg.Done()
			res, err := fetchSkillsShTrending(ctx, s.client, limit)
			if err == nil && len(res) > 0 {
				mu.Lock()
				items = append(items, res...)
				mu.Unlock()
			}
		}()
	}

	if p == "all" || p == "cliapps" {
		wg.Add(1)
		go func() {
			defer wg.Done()
			res, err := fetchCliAppsTrending(ctx, s.client, limit)
			if err == nil && len(res) > 0 {
				mu.Lock()
				items = append(items, res...)
				mu.Unlock()
			}
		}()
	}

	wg.Wait()

	installed := s.installedSkills()
	for i := range items {
		if installed[items[i].SkillID] {
			items[i].Installed = true
		}
	}

	return &TrendingResponse{
		Skills:           items,
		Provider:         p,
		InstallSupported: true,
	}, nil
}

func (s *Service) Search(ctx context.Context, query, provider string, limit int) (*SearchResponse, error) {
	q := strings.TrimSpace(query)
	if len(q) < 2 {
		return nil, fmt.Errorf("search query must contain at least 2 characters")
	}
	if limit <= 0 {
		limit = 20
	}
	p := strings.ToLower(strings.TrimSpace(provider))
	if p == "" {
		p = "all"
	}

	var items []SkillItem
	var mu sync.Mutex
	var wg sync.WaitGroup

	if p == "all" || p == "skillhub" {
		wg.Add(1)
		go func() {
			defer wg.Done()
			res, err := searchSkillHub(ctx, s.client, q, limit)
			if err == nil && len(res) > 0 {
				mu.Lock()
				items = append(items, res...)
				mu.Unlock()
			}
		}()
	}

	if p == "all" || p == "skills_sh" {
		wg.Add(1)
		go func() {
			defer wg.Done()
			res, err := searchSkillsSh(ctx, s.client, q, limit)
			if err == nil && len(res) > 0 {
				mu.Lock()
				items = append(items, res...)
				mu.Unlock()
			}
		}()
	}

	if p == "all" || p == "cliapps" {
		wg.Add(1)
		go func() {
			defer wg.Done()
			res, err := searchCliApps(ctx, s.client, q, limit)
			if err == nil && len(res) > 0 {
				mu.Lock()
				items = append(items, res...)
				mu.Unlock()
			}
		}()
	}

	wg.Wait()

	installed := s.installedSkills()
	for i := range items {
		if installed[items[i].SkillID] {
			items[i].Installed = true
		}
	}

	return &SearchResponse{
		Query:            q,
		Skills:           items,
		Provider:         p,
		InstallSupported: true,
	}, nil
}

func (s *Service) Install(ctx context.Context, req InstallRequest) (*InstallResponse, error) {
	skillID := strings.TrimSpace(req.SkillID)
	if skillID == "" {
		return nil, fmt.Errorf("skill_id is required")
	}
	p := strings.ToLower(strings.TrimSpace(req.Provider))
	switch p {
	case "skillhub":
		return installSkillHubSkill(ctx, s.client, s.workspace, skillID, req.Version)
	case "skills_sh":
		source := strings.TrimSpace(req.Source)
		if source == "" {
			source = skillID
		}
		return installSkillsShSkill(ctx, s.client, s.workspace, source, skillID)
	default:
		// Try skillhub first, then skills_sh
		resp, err := installSkillHubSkill(ctx, s.client, s.workspace, skillID, req.Version)
		if err == nil {
			return resp, nil
		}
		if req.Source != "" {
			return installSkillsShSkill(ctx, s.client, s.workspace, req.Source, skillID)
		}
		return nil, fmt.Errorf("installation failed: %w", err)
	}
}

func (s *Service) Uninstall(ctx context.Context, name string) error {
	name = strings.TrimSpace(name)
	if name == "" || strings.Contains(name, "/") || strings.Contains(name, "\\") || strings.Contains(name, "..") {
		return fmt.Errorf("invalid skill name %q", name)
	}
	targetDir := filepath.Join(s.workspace, "skills", name)
	if info, err := os.Stat(targetDir); err == nil && info.IsDir() {
		return os.RemoveAll(targetDir)
	}
	return os.ErrNotExist
}
