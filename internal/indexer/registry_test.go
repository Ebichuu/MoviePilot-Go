package indexer

import (
	"context"
	"errors"
	"testing"
)

type registryTestAdapter struct{ name string }

func (a registryTestAdapter) Name() string { return a.name }
func (a registryTestAdapter) Search(context.Context, Query) ([]Resource, error) {
	return nil, nil
}

type registryDownloadAdapter struct {
	registryTestAdapter
	siteID  int
	kind    string
	version int16
}

func (a registryDownloadAdapter) SiteID() int            { return a.siteID }
func (a registryDownloadAdapter) ResolverKind() string   { return a.kind }
func (a registryDownloadAdapter) ResolverVersion() int16 { return a.version }
func (a registryDownloadAdapter) ResolveDownload(context.Context, string) (ResolvedDownload, error) {
	return ResolvedDownload{}, nil
}

func TestRegistryUsesImmutableSnapshots(t *testing.T) {
	registry := NewRegistry([]Adapter{registryTestAdapter{name: "old"}})
	oldSnapshot := registry.Snapshot()
	registry.Replace([]Adapter{registryTestAdapter{name: "new"}})
	newSnapshot := registry.Snapshot()
	if len(oldSnapshot) != 1 || oldSnapshot[0].Name() != "old" {
		t.Fatalf("old snapshot changed: %#v", oldSnapshot)
	}
	if len(newSnapshot) != 1 || newSnapshot[0].Name() != "new" {
		t.Fatalf("new snapshot missing replacement: %#v", newSnapshot)
	}
	oldSnapshot[0] = registryTestAdapter{name: "mutated"}
	if registry.Snapshot()[0].Name() != "new" {
		t.Fatal("caller mutated registry storage")
	}
}

func TestRegistryResolveDownloadResolver(t *testing.T) {
	t.Parallel()

	resolver := registryDownloadAdapter{
		registryTestAdapter: registryTestAdapter{name: "fixture"},
		siteID:              42,
		kind:                "haidan",
		version:             1,
	}
	tests := []struct {
		name     string
		registry *Registry
		siteID   int
		kind     string
		version  int16
		wantErr  error
	}{
		{name: "精确匹配", registry: NewRegistry([]Adapter{resolver}), siteID: 42, kind: "haidan", version: 1},
		{name: "种类大小写和空白规范化", registry: NewRegistry([]Adapter{resolver}), siteID: 42, kind: " HAIDAN ", version: 1},
		{name: "站点不存在", registry: NewRegistry([]Adapter{resolver}), siteID: 99, kind: "haidan", version: 1, wantErr: ErrDownloadResolverNotFound},
		{name: "种类不匹配", registry: NewRegistry([]Adapter{resolver}), siteID: 42, kind: "hddolby", version: 1, wantErr: ErrDownloadResolverVersionMismatch},
		{name: "版本不匹配", registry: NewRegistry([]Adapter{resolver}), siteID: 42, kind: "haidan", version: 2, wantErr: ErrDownloadResolverVersionMismatch},
		{name: "站点没有版本化解析器", registry: NewRegistry([]Adapter{registryTestSiteAdapter{registryTestAdapter{name: "plain"}, 42}}), siteID: 42, kind: "haidan", version: 1, wantErr: ErrDownloadResolverVersionMismatch},
		{name: "空注册表", registry: nil, siteID: 42, kind: "haidan", version: 1, wantErr: ErrDownloadResolverNotFound},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			got, err := tt.registry.ResolveDownloadResolver(tt.siteID, tt.kind, tt.version)
			if !errors.Is(err, tt.wantErr) {
				t.Fatalf("ResolveDownloadResolver() error = %v, want %v", err, tt.wantErr)
			}
			if tt.wantErr == nil && got != resolver {
				t.Fatalf("ResolveDownloadResolver() = %#v, want fixture resolver", got)
			}
		})
	}
}

type registryTestSiteAdapter struct {
	registryTestAdapter
	siteID int
}

func (a registryTestSiteAdapter) SiteID() int { return a.siteID }
