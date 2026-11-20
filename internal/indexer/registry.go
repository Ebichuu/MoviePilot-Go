package indexer

import (
	"errors"
	"strings"
	"sync"
)

var (
	// ErrDownloadResolverNotFound 表示当前注册表没有对应站点下载解析器。
	ErrDownloadResolverNotFound = errors.New("站点下载解析器不存在")
	// ErrDownloadResolverVersionMismatch 表示执行计划与当前解析器语义不兼容。
	ErrDownloadResolverVersionMismatch = errors.New("站点下载解析器版本不匹配")
)

type Registry struct {
	mu       sync.RWMutex
	adapters []Adapter
}

func NewRegistry(adapters []Adapter) *Registry {
	registry := &Registry{}
	registry.Replace(adapters)
	return registry
}

func (r *Registry) Snapshot() []Adapter {
	if r == nil {
		return []Adapter{}
	}
	r.mu.RLock()
	defer r.mu.RUnlock()
	return append([]Adapter(nil), r.adapters...)
}

func (r *Registry) Replace(adapters []Adapter) {
	if r == nil {
		return
	}
	copyOfAdapters := append([]Adapter(nil), adapters...)
	r.mu.Lock()
	r.adapters = copyOfAdapters
	r.mu.Unlock()
}

// ResolveDownloadResolver 按站点、规范种类和版本取得当前解析器快照。
func (r *Registry) ResolveDownloadResolver(siteID int, kind string, version int16) (VersionedDownloadResolver, error) {
	kind = strings.ToLower(strings.TrimSpace(kind))
	if r == nil || siteID <= 0 || kind == "" || version <= 0 {
		return nil, ErrDownloadResolverNotFound
	}
	foundSite := false
	for _, adapter := range r.Snapshot() {
		identified, ok := adapter.(SiteIDAdapter)
		if !ok || identified.SiteID() != siteID {
			continue
		}
		foundSite = true
		resolver, ok := adapter.(VersionedDownloadResolver)
		if !ok {
			continue
		}
		if strings.EqualFold(resolver.ResolverKind(), kind) && resolver.ResolverVersion() == version {
			return resolver, nil
		}
	}
	if foundSite {
		return nil, ErrDownloadResolverVersionMismatch
	}
	return nil, ErrDownloadResolverNotFound
}
