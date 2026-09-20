package marketplace

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/adrianolimagarcia/nanobot-go/internal/netpolicy"
)

// marketplaceHosts is the fixed set of upstream hosts the catalogue talks to.
// Every request also carries the operator's SSRF whitelist so a deployment can
// extend the boundary without widening it for unrelated tools.
var marketplaceHosts = []string{
	"skills.sh",
	"api.skillhub.cn",
	"api.github.com",
	"codeload.github.com",
	"raw.githubusercontent.com",
	"clianything.cc",
}

// Options configures a catalogue service.
type Options struct {
	// Workspace is the agent workspace; skills are installed under <workspace>/skills.
	Workspace string
	// AllowRemoteInstall gates every code-writing operation. When false the
	// catalogue is read-only, which is the default.
	AllowRemoteInstall bool
	// SSRFWhitelist extends the outbound allowlist for the catalogue client.
	SSRFWhitelist []string
	// Client overrides the outbound client. Tests use it to reach local servers.
	Client *http.Client
}

type Service struct {
	workspace    string
	allowInstall bool
	client       *http.Client
}

// NewService builds a catalogue service. Remote installs are disabled unless
// explicitly enabled, so a read-only catalogue is the default posture.
func NewService(opts Options) *Service {
	client := opts.Client
	if client == nil {
		allowlist := make([]string, 0, len(marketplaceHosts)+len(opts.SSRFWhitelist))
		allowlist = append(allowlist, opts.SSRFWhitelist...)
		allowlist = append(allowlist, marketplaceHosts...)
		client = netpolicy.NewClient(45*time.Second, netpolicy.Policy{Allowlist: allowlist})
	}
	return &Service{
		workspace:    opts.Workspace,
		allowInstall: opts.AllowRemoteInstall,
		client:       client,
	}
}

// InstallSupported reports whether this service may write to the workspace.
func (s *Service) InstallSupported() bool { return s.allowInstall }

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

	items := make([]SkillItem, 0)
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
		// The operator gate is authoritative: a provider that cannot install at
		// all stays uninstallable even when remote installs are enabled.
		items[i].InstallSupported = items[i].InstallSupported && s.allowInstall
	}

	return &TrendingResponse{
		Skills:           items,
		Provider:         p,
		InstallSupported: s.allowInstall,
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

	items := make([]SkillItem, 0)
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
		items[i].InstallSupported = items[i].InstallSupported && s.allowInstall
	}

	return &SearchResponse{
		Query:            q,
		Skills:           items,
		Provider:         p,
		InstallSupported: s.allowInstall,
	}, nil
}

// ErrInstallDisabled is returned when remote installation is not enabled by the
// operator. Installing a skill writes files the agent will later execute, so it
// stays opt-in.
var ErrInstallDisabled = errors.New("remote skill installation is disabled by configuration (tools.webuiAllowRemotePackageInstall)")

func (s *Service) Install(ctx context.Context, req InstallRequest) (*InstallResponse, error) {
	if !s.allowInstall {
		return nil, ErrInstallDisabled
	}
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
	case "cliapps":
		// The CLI-Anything registry publishes catalogue entries only; there is
		// no archive to install. Saying so is better than silently trying an
		// unrelated provider and reporting its failure.
		return nil, fmt.Errorf("provider %q is a read-only catalogue and cannot be installed", p)
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
	if !s.allowInstall {
		return ErrInstallDisabled
	}
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
