package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"os"
	"path/filepath"
	"sync"

	micrographrag "github.com/adrianolimagarcia/micrographrag-go"
)

// graphStorePool physically isolates derived memory by session key. The
// previous single agent.db allowed semantic retrieval from one session to
// surface chunks written by another session because micrographrag SearchOptions
// currently has no source/tenant filter.
type graphStorePool struct {
	mu     sync.Mutex
	dir    string
	stores map[string]*micrographrag.Store
	closed bool
}

func newGraphStorePool(dir string) *graphStorePool {
	return &graphStorePool{dir: dir, stores: map[string]*micrographrag.Store{}}
}

func (p *graphStorePool) Store(ctx context.Context, sessionKey string) (*micrographrag.Store, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.closed {
		return nil, errors.New("graph store pool is closed")
	}
	if store, ok := p.stores[sessionKey]; ok {
		return store, nil
	}
	if err := os.MkdirAll(p.dir, 0o700); err != nil {
		return nil, err
	}

	sum := sha256.Sum256([]byte(sessionKey))
	name := hex.EncodeToString(sum[:]) + ".db"
	graphCfg := micrographrag.DefaultConfig(filepath.Join(p.dir, name))
	graphCfg.EnableVector = false
	graphCfg.EnableEmbeddingWorker = false
	store, err := micrographrag.Open(ctx, graphCfg, nil)
	if err != nil {
		return nil, err
	}
	p.stores[sessionKey] = store
	return store, nil
}

func (p *graphStorePool) Close() error {
	p.mu.Lock()
	if p.closed {
		p.mu.Unlock()
		return nil
	}
	p.closed = true
	stores := make([]*micrographrag.Store, 0, len(p.stores))
	for _, store := range p.stores {
		stores = append(stores, store)
	}
	p.stores = nil
	p.mu.Unlock()

	var joined error
	for _, store := range stores {
		if err := store.Close(); err != nil {
			joined = errors.Join(joined, err)
		}
	}
	return joined
}
