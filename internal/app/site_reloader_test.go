package app

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/moviepilot-go/moviepilot-go/internal/indexer"
)

type siteReloaderTestAdapter struct {
	name string
}

func (a siteReloaderTestAdapter) Name() string {
	return a.name
}

func (a siteReloaderTestAdapter) Search(context.Context, indexer.Query) ([]indexer.Resource, error) {
	return nil, nil
}

func TestSiteReloaderSerializesReloads(t *testing.T) {
	registry := indexer.NewRegistry(nil)
	var activeLoads atomic.Int32
	var maxActiveLoads atomic.Int32
	loadEntered := make(chan struct{}, 2)
	releaseLoad := make(chan struct{})
	reloader := newSiteReloader(registry, func(context.Context) ([]indexer.Adapter, error) {
		active := activeLoads.Add(1)
		for {
			maximum := maxActiveLoads.Load()
			if active <= maximum || maxActiveLoads.CompareAndSwap(maximum, active) {
				break
			}
		}
		loadEntered <- struct{}{}
		<-releaseLoad
		activeLoads.Add(-1)
		return nil, nil
	})

	start := make(chan struct{})
	var ready sync.WaitGroup
	var reloads sync.WaitGroup
	ready.Add(2)
	for range 2 {
		reloads.Add(1)
		go func() {
			defer reloads.Done()
			ready.Done()
			<-start
			if err := reloader.Reload(context.Background()); err != nil {
				t.Errorf("Reload() error = %v", err)
			}
		}()
	}
	ready.Wait()
	close(start)

	select {
	case <-loadEntered:
	case <-time.After(time.Second):
		t.Fatal("first load did not start")
	}
	select {
	case <-loadEntered:
		close(releaseLoad)
		reloads.Wait()
		t.Fatal("concurrent Reload calls entered load together")
	case <-time.After(50 * time.Millisecond):
	}

	close(releaseLoad)
	reloads.Wait()
	if got := maxActiveLoads.Load(); got != 1 {
		t.Fatalf("maximum concurrent loads = %d, want 1", got)
	}
}

func TestSiteReloaderKeepsRegistryOnLoadFailure(t *testing.T) {
	oldAdapter := siteReloaderTestAdapter{name: "old"}
	registry := indexer.NewRegistry([]indexer.Adapter{oldAdapter})
	loadErr := errors.New("load sites")
	reloader := newSiteReloader(registry, func(context.Context) ([]indexer.Adapter, error) {
		return []indexer.Adapter{siteReloaderTestAdapter{name: "new"}}, loadErr
	})

	if err := reloader.Reload(context.Background()); !errors.Is(err, loadErr) {
		t.Fatalf("Reload() error = %v, want %v", err, loadErr)
	}
	assertSiteReloaderSnapshot(t, registry, "old")
}

func TestSiteReloaderReplacesRegistryOnSuccess(t *testing.T) {
	registry := indexer.NewRegistry([]indexer.Adapter{siteReloaderTestAdapter{name: "old"}})
	reloader := newSiteReloader(registry, func(context.Context) ([]indexer.Adapter, error) {
		return []indexer.Adapter{siteReloaderTestAdapter{name: "new"}}, nil
	})

	if err := reloader.Reload(context.Background()); err != nil {
		t.Fatalf("Reload() error = %v", err)
	}
	assertSiteReloaderSnapshot(t, registry, "new")
}

func assertSiteReloaderSnapshot(t *testing.T, registry *indexer.Registry, wantName string) {
	t.Helper()
	snapshot := registry.Snapshot()
	if len(snapshot) != 1 || snapshot[0].Name() != wantName {
		t.Fatalf("registry snapshot = %#v, want one adapter named %q", snapshot, wantName)
	}
}
