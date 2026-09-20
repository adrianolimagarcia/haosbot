package cliapps

import (
	"context"
	"fmt"
	"time"
)

// CatalogFetcher deliberately abstracts transport. The manager owns cache
// validation/persistence while callers may source a catalog from HTTP, a local
// mirror, or a signed control-plane endpoint without coupling networking to
// lifecycle execution.
type CatalogFetcher func(context.Context) (Catalog, error)

// RefreshCatalog fetches, validates and atomically caches a catalog. A failed
// fetch or invalid catalog leaves the last known-good cache untouched.
func (m *Manager) RefreshCatalog(ctx context.Context, fetch CatalogFetcher) (Catalog, error) {
	if fetch == nil { return Catalog{}, fmt.Errorf("cliapps: catalog fetcher is required") }
	c, err := fetch(ctx)
	if err != nil { return Catalog{}, fmt.Errorf("cliapps: refresh catalog: %w", err) }
	if c.FetchedAt.IsZero() { c.FetchedAt = m.now().UTC() }
	if err := m.SaveCatalog(c); err != nil { return Catalog{}, fmt.Errorf("cliapps: refresh catalog: %w", err) }
	return c, nil
}

// CachedCatalog returns a fresh cache when possible and refreshes otherwise.
// If refresh fails and allowStale is true, the last known-good catalog is
// returned with stale=true and the refresh error for observability.
func (m *Manager) CachedCatalog(ctx context.Context, maxAge time.Duration, allowStale bool, fetch CatalogFetcher) (c Catalog, stale bool, err error) {
	cached, fresh, cacheErr := m.Catalog(maxAge)
	if cacheErr != nil { return Catalog{}, false, cacheErr }
	if fresh && !cached.FetchedAt.IsZero() { return cached, false, nil }
	updated, refreshErr := m.RefreshCatalog(ctx, fetch)
	if refreshErr == nil { return updated, false, nil }
	if allowStale && !cached.FetchedAt.IsZero() { return cached, true, refreshErr }
	return Catalog{}, false, refreshErr
}
