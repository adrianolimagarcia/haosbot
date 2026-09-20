package marketplace

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"
)

const (
	cliAnythingRegistryURL = "https://clianything.cc/public_registry.json"
)

type cliAppEntry struct {
	Name        string `json:"name"`
	Description string `json:"description"`
	NpmPackage  string `json:"npm_package"`
	Version     string `json:"version"`
	URL         string `json:"url"`
	EntryPoint  string `json:"entry_point"`
	Args        []string `json:"args"`
}

type cliAppsRegistryCache struct {
	mu       sync.Mutex
	cachedAt time.Time
	entries  []cliAppEntry
}

var registryCache cliAppsRegistryCache

func fetchCliAppsRegistry(ctx context.Context, client *http.Client) ([]cliAppEntry, error) {
	registryCache.mu.Lock()
	if time.Since(registryCache.cachedAt) < 10*time.Minute && len(registryCache.entries) > 0 {
		entries := registryCache.entries
		registryCache.mu.Unlock()
		return entries, nil
	}
	registryCache.mu.Unlock()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, cliAnythingRegistryURL, nil)
	if err != nil {
		return nil, err
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("fetch cliapps registry: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("cliapps registry HTTP %d", resp.StatusCode)
	}

	var payload struct {
		CLIs []cliAppEntry `json:"clis"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 8<<20)).Decode(&payload); err != nil {
		return nil, fmt.Errorf("decode cliapps registry: %w", err)
	}

	registryCache.mu.Lock()
	registryCache.cachedAt = time.Now()
	registryCache.entries = payload.CLIs
	registryCache.mu.Unlock()

	return payload.CLIs, nil
}

func fetchCliAppsTrending(ctx context.Context, client *http.Client, limit int) ([]SkillItem, error) {
	clis, err := fetchCliAppsRegistry(ctx, client)
	if err != nil {
		return nil, err
	}
	items := make([]SkillItem, 0)
	for _, cli := range clis {
		if strings.TrimSpace(cli.Name) == "" {
			continue
		}
		items = append(items, convertCliApp(cli))
		if limit > 0 && len(items) >= limit {
			break
		}
	}
	return items, nil
}

func searchCliApps(ctx context.Context, client *http.Client, query string, limit int) ([]SkillItem, error) {
	clis, err := fetchCliAppsRegistry(ctx, client)
	if err != nil {
		return nil, err
	}
	q := strings.ToLower(strings.TrimSpace(query))
	items := make([]SkillItem, 0)
	for _, cli := range clis {
		name := strings.ToLower(cli.Name)
		desc := strings.ToLower(cli.Description)
		if strings.Contains(name, q) || strings.Contains(desc, q) {
			items = append(items, convertCliApp(cli))
			if limit > 0 && len(items) >= limit {
				break
			}
		}
	}
	return items, nil
}

func convertCliApp(cli cliAppEntry) SkillItem {
	desc := cli.Description
	if desc == "" {
		desc = "CLI Tool application"
	}
	return SkillItem{
		ID:               "cliapps:" + cli.Name,
		SkillID:          cli.Name,
		Name:             cli.Name,
		Description:      desc,
		Source:           "CLI-Anything",
		Provider:         "cliapps",
		URL:              cli.URL,
		Version:          cli.Version,
		InstallSupported: true,
	}
}
