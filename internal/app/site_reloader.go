package app

import (
	"context"
	"sync"

	"github.com/moviepilot-go/moviepilot-go/internal/indexer"
)

type siteReloader struct {
	mu       sync.Mutex
	registry *indexer.Registry
	load     func(context.Context) ([]indexer.Adapter, error)
}

func newSiteReloader(
	registry *indexer.Registry,
	load func(context.Context) ([]indexer.Adapter, error),
) *siteReloader {
	return &siteReloader{
		registry: registry,
		load:     load,
	}
}

func (r *siteReloader) Reload(ctx context.Context) error {
	r.mu.Lock()
	defer r.mu.Unlock()

	adapters, err := r.load(ctx)
	if err != nil {
		return err
	}
	r.registry.Replace(adapters)
	return nil
}
