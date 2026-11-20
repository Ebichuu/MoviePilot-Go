package storage

import (
	"context"
	"io"
	"strings"
	"sync"
)

// ProviderRegistry 按 V2 存储名把请求分发到已注册的存储实现。
type ProviderRegistry struct {
	mu        sync.RWMutex
	providers map[string]Provider
}

// NewProviderRegistry 创建空的分发注册表。
func NewProviderRegistry() *ProviderRegistry {
	return &ProviderRegistry{providers: map[string]Provider{}}
}

// Register 注册一个存储名对应的实现。
func (r *ProviderRegistry) Register(name string, provider Provider) {
	name = strings.TrimSpace(name)
	if r == nil || name == "" || provider == nil {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.providers[name] = provider
}

func (r *ProviderRegistry) lookup(name string) (Provider, error) {
	if r == nil {
		return nil, ErrUnsupportedStorage
	}
	r.mu.RLock()
	defer r.mu.RUnlock()
	provider, ok := r.providers[strings.TrimSpace(name)]
	if !ok || provider == nil {
		return nil, ErrUnsupportedStorage
	}
	return provider, nil
}

// Names 返回已注册的存储名（稳定排序）。
func (r *ProviderRegistry) Names() []string {
	if r == nil {
		return nil
	}
	r.mu.RLock()
	defer r.mu.RUnlock()
	names := make([]string, 0, len(r.providers))
	for name := range r.providers {
		names = append(names, name)
	}
	sortStrings(names)
	return names
}

// List 转发到对应存储实现。
func (r *ProviderRegistry) List(ctx context.Context, storageName, requested, sortKey, keyword string) ([]Item, error) {
	provider, err := r.lookup(storageName)
	if err != nil {
		return nil, err
	}
	return provider.List(ctx, storageName, requested, sortKey, keyword)
}

// Mkdir 转发到对应存储实现。
func (r *ProviderRegistry) Mkdir(ctx context.Context, storageName, requested, name string) (bool, error) {
	provider, err := r.lookup(storageName)
	if err != nil {
		return false, err
	}
	return provider.Mkdir(ctx, storageName, requested, name)
}

// Delete 转发到对应存储实现。
func (r *ProviderRegistry) Delete(ctx context.Context, storageName, requested string) (bool, error) {
	provider, err := r.lookup(storageName)
	if err != nil {
		return false, err
	}
	return provider.Delete(ctx, storageName, requested)
}

// Rename 转发到对应存储实现。
func (r *ProviderRegistry) Rename(ctx context.Context, storageName, requested, newName string, recursive bool) (bool, error) {
	provider, err := r.lookup(storageName)
	if err != nil {
		return false, err
	}
	return provider.Rename(ctx, storageName, requested, newName, recursive)
}

// Usage 转发到对应存储实现。
func (r *ProviderRegistry) Usage(ctx context.Context, storageName string) (Usage, error) {
	provider, err := r.lookup(storageName)
	if err != nil {
		return Usage{}, err
	}
	return provider.Usage(ctx, storageName)
}

// TransType 转发到对应存储实现。
func (r *ProviderRegistry) TransType(ctx context.Context, storageName string) (map[string]any, error) {
	provider, err := r.lookup(storageName)
	if err != nil {
		return nil, err
	}
	return provider.TransType(ctx, storageName)
}

// Open 转发到支持只读打开的存储实现。
func (r *ProviderRegistry) Open(ctx context.Context, storageName, requested string) (io.ReadCloser, string, error) {
	provider, err := r.lookup(storageName)
	if err != nil {
		return nil, "", err
	}
	opener, ok := provider.(FileOpener)
	if !ok {
		return nil, "", ErrUnsupportedStorage
	}
	return opener.Open(ctx, storageName, requested)
}

// QRCode 转发到支持登录的存储实现。
func (r *ProviderRegistry) QRCode(ctx context.Context, storageName string) (any, string, error) {
	provider, err := r.lookup(storageName)
	if err != nil {
		return nil, "存储实现未接入", err
	}
	manager, ok := provider.(AuthManager)
	if !ok {
		return nil, "该存储不支持扫码登录", nil
	}
	return manager.QRCode(ctx, storageName)
}

// AuthURL 转发到支持 OAuth 的存储实现。
func (r *ProviderRegistry) AuthURL(ctx context.Context, storageName string) (any, string, error) {
	provider, err := r.lookup(storageName)
	if err != nil {
		return nil, "存储实现未接入", err
	}
	manager, ok := provider.(AuthManager)
	if !ok {
		return nil, "该存储不支持 OAuth 授权", nil
	}
	return manager.AuthURL(ctx, storageName)
}

// CheckLogin 转发到支持登录确认的存储实现。
func (r *ProviderRegistry) CheckLogin(ctx context.Context, storageName, ck, token string) (any, string, error) {
	provider, err := r.lookup(storageName)
	if err != nil {
		return nil, "存储实现未接入", err
	}
	manager, ok := provider.(AuthManager)
	if !ok {
		return nil, "该存储无需登录确认", nil
	}
	return manager.CheckLogin(ctx, storageName, ck, token)
}

// SaveConfig 转发到支持配置保存的存储实现。
func (r *ProviderRegistry) SaveConfig(ctx context.Context, storageName string, conf map[string]any) error {
	provider, err := r.lookup(storageName)
	if err != nil {
		return err
	}
	if manager, ok := provider.(AuthManager); ok {
		return manager.SaveConfig(ctx, storageName, conf)
	}
	return nil
}

// ResetConfig 转发到支持配置重置的存储实现。
func (r *ProviderRegistry) ResetConfig(ctx context.Context, storageName string) error {
	provider, err := r.lookup(storageName)
	if err != nil {
		return err
	}
	if manager, ok := provider.(AuthManager); ok {
		return manager.ResetConfig(ctx, storageName)
	}
	return nil
}

var _ Provider = (*ProviderRegistry)(nil)
var _ FileOpener = (*ProviderRegistry)(nil)
var _ AuthManager = (*ProviderRegistry)(nil)
