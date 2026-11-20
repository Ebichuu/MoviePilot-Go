package storage

import (
	"context"
	"errors"
	"io"
	"strings"
	"testing"
)

// registryPlainProvider 只实现 Provider，不实现 FileOpener / AuthManager，
// 用来验证注册表对「可选能力缺失」的处理。
type registryPlainProvider struct {
	calls []string
}

func (p *registryPlainProvider) record(name string) { p.calls = append(p.calls, name) }

func (p *registryPlainProvider) List(_ context.Context, storageName, requested, sortKey, keyword string) ([]Item, error) {
	p.record("List:" + storageName + ":" + requested + ":" + sortKey + ":" + keyword)
	return []Item{{Name: "listed"}}, nil
}

func (p *registryPlainProvider) Mkdir(_ context.Context, storageName, requested, name string) (bool, error) {
	p.record("Mkdir:" + storageName + ":" + requested + ":" + name)
	return true, nil
}

func (p *registryPlainProvider) Delete(_ context.Context, storageName, requested string) (bool, error) {
	p.record("Delete:" + storageName + ":" + requested)
	return true, nil
}

func (p *registryPlainProvider) Rename(_ context.Context, storageName, requested, newName string, recursive bool) (bool, error) {
	p.record("Rename:" + storageName + ":" + requested + ":" + newName)
	return recursive, nil
}

func (p *registryPlainProvider) Usage(_ context.Context, storageName string) (Usage, error) {
	p.record("Usage:" + storageName)
	return Usage{Total: 100}, nil
}

func (p *registryPlainProvider) TransType(_ context.Context, storageName string) (map[string]any, error) {
	p.record("TransType:" + storageName)
	return map[string]any{"name": storageName}, nil
}

// registryAuthProvider 额外实现登录/配置能力与只读打开。
type registryAuthProvider struct {
	registryPlainProvider
}

func (p *registryAuthProvider) Open(_ context.Context, storageName, requested string) (io.ReadCloser, string, error) {
	p.record("Open:" + storageName + ":" + requested)
	return io.NopCloser(strings.NewReader("payload")), "text/plain", nil
}

func (p *registryAuthProvider) QRCode(_ context.Context, storageName string) (any, string, error) {
	return map[string]any{"storage": storageName}, "扫码", nil
}

func (p *registryAuthProvider) AuthURL(_ context.Context, storageName string) (any, string, error) {
	return map[string]any{"storage": storageName}, "授权", nil
}

func (p *registryAuthProvider) CheckLogin(_ context.Context, storageName, ck, token string) (any, string, error) {
	return map[string]any{"ck": ck, "token": token}, "确认", nil
}

func (p *registryAuthProvider) SaveConfig(_ context.Context, storageName string, conf map[string]any) error {
	p.record("SaveConfig:" + storageName)
	return nil
}

func (p *registryAuthProvider) ResetConfig(_ context.Context, storageName string) error {
	p.record("ResetConfig:" + storageName)
	return nil
}

func TestProviderRegistryRegisterAndNames(t *testing.T) {
	registry := NewProviderRegistry()
	registry.Register("  local  ", &registryPlainProvider{})
	registry.Register("alist", &registryPlainProvider{})
	// 空名与 nil 实现都必须被忽略，不能产生不可用的注册项。
	registry.Register("", &registryPlainProvider{})
	registry.Register("blank", nil)

	names := registry.Names()
	if len(names) != 2 || names[0] != "alist" || names[1] != "local" {
		t.Fatalf("注册名 = %v，期望排序后的 [alist local]", names)
	}
	// 同名重复注册以最后一次为准。
	replacement := &registryPlainProvider{}
	registry.Register("local", replacement)
	if _, err := registry.List(context.Background(), "local", "p", "", ""); err != nil {
		t.Fatalf("重复注册后应可用: %v", err)
	}
	if len(replacement.calls) != 1 {
		t.Fatalf("应使用最后一次注册的实现，实际调用 %v", replacement.calls)
	}

	var nilRegistry *ProviderRegistry
	nilRegistry.Register("x", &registryPlainProvider{}) // 必须安全
	if names := nilRegistry.Names(); names != nil {
		t.Fatalf("nil 注册表应返回 nil，实际 %v", names)
	}
}

func TestProviderRegistryUnknownStorage(t *testing.T) {
	registry := NewProviderRegistry()
	registry.Register("local", &registryPlainProvider{})

	if _, err := registry.List(context.Background(), "nope", "", "", ""); !errors.Is(err, ErrUnsupportedStorage) {
		t.Fatalf("未知存储应返回 ErrUnsupportedStorage，实际 %v", err)
	}
	var nilRegistry *ProviderRegistry
	if _, err := nilRegistry.List(context.Background(), "local", "", "", ""); !errors.Is(err, ErrUnsupportedStorage) {
		t.Fatalf("nil 注册表应返回 ErrUnsupportedStorage，实际 %v", err)
	}
	// 名字两端空白不影响查找。
	if _, err := registry.List(context.Background(), "  local  ", "p", "", ""); err != nil {
		t.Fatalf("带空格的存储名应可用: %v", err)
	}
}

func TestProviderRegistryDelegatesCoreCalls(t *testing.T) {
	provider := &registryPlainProvider{}
	registry := NewProviderRegistry()
	registry.Register("local", provider)
	ctx := context.Background()

	items, err := registry.List(ctx, "local", "p", "name", "kw")
	if err != nil || len(items) != 1 || items[0].Name != "listed" {
		t.Fatalf("List = %v %v", items, err)
	}
	if created, err := registry.Mkdir(ctx, "local", "p", "新目录"); err != nil || !created {
		t.Fatalf("Mkdir = %v %v", created, err)
	}
	if deleted, err := registry.Delete(ctx, "local", "p/a"); err != nil || !deleted {
		t.Fatalf("Delete = %v %v", deleted, err)
	}
	if renamed, err := registry.Rename(ctx, "local", "p/a", "p/b", true); err != nil || !renamed {
		t.Fatalf("Rename 应透传 recursive = %v %v", renamed, err)
	}
	if usage, err := registry.Usage(ctx, "local"); err != nil || usage.Total != 100 {
		t.Fatalf("Usage = %+v %v", usage, err)
	}
	if types, err := registry.TransType(ctx, "local"); err != nil || types["name"] != "local" {
		t.Fatalf("TransType = %v %v", types, err)
	}

	want := []string{
		"List:local:p:name:kw",
		"Mkdir:local:p:新目录",
		"Delete:local:p/a",
		"Rename:local:p/a:p/b",
		"Usage:local",
		"TransType:local",
	}
	if len(provider.calls) != len(want) {
		t.Fatalf("调用序列 = %v", provider.calls)
	}
	for index, call := range want {
		if provider.calls[index] != call {
			t.Fatalf("第 %d 次调用 = %q，期望 %q", index, provider.calls[index], call)
		}
	}
}

func TestProviderRegistryOpenNeedsFileOpener(t *testing.T) {
	registry := NewProviderRegistry()
	registry.Register("plain", &registryPlainProvider{})

	if reader, _, err := registry.Open(context.Background(), "plain", "a.txt"); !errors.Is(err, ErrUnsupportedStorage) || reader != nil {
		t.Fatalf("不支持打开的实现应返回 ErrUnsupportedStorage，实际 %v %v", reader, err)
	}

	auth := &registryAuthProvider{}
	registry.Register("auth", auth)
	reader, contentType, err := registry.Open(context.Background(), "auth", "a.txt")
	if err != nil {
		t.Fatalf("Open 失败: %v", err)
	}
	defer func() { _ = reader.Close() }()
	if contentType != "text/plain" {
		t.Fatalf("内容类型 = %q", contentType)
	}
}

func TestProviderRegistryAuthFallbacks(t *testing.T) {
	registry := NewProviderRegistry()
	registry.Register("plain", &registryPlainProvider{})
	ctx := context.Background()

	// 没有登录能力的存储：返回稳定说明且不算错误，前端据此隐藏入口。
	if payload, message, err := registry.QRCode(ctx, "plain"); err != nil || payload != nil || message == "" {
		t.Fatalf("QRCode = %v %q %v", payload, message, err)
	}
	if payload, message, err := registry.AuthURL(ctx, "plain"); err != nil || payload != nil || message == "" {
		t.Fatalf("AuthURL = %v %q %v", payload, message, err)
	}
	if payload, message, err := registry.CheckLogin(ctx, "plain", "", ""); err != nil || payload != nil || message == "" {
		t.Fatalf("CheckLogin = %v %q %v", payload, message, err)
	}
	if err := registry.SaveConfig(ctx, "plain", map[string]any{}); err != nil {
		t.Fatalf("无配置能力的存储 SaveConfig 应视为成功: %v", err)
	}
	if err := registry.ResetConfig(ctx, "plain"); err != nil {
		t.Fatalf("无配置能力的存储 ResetConfig 应视为成功: %v", err)
	}

	// 未注册的存储名必须报错（区别于「已注册但不支持」）。
	if _, _, err := registry.QRCode(ctx, "nope"); !errors.Is(err, ErrUnsupportedStorage) {
		t.Fatalf("未知存储 QRCode 应报错，实际 %v", err)
	}
	if err := registry.SaveConfig(ctx, "nope", nil); !errors.Is(err, ErrUnsupportedStorage) {
		t.Fatalf("未知存储 SaveConfig 应报错，实际 %v", err)
	}
	if err := registry.ResetConfig(ctx, "nope"); !errors.Is(err, ErrUnsupportedStorage) {
		t.Fatalf("未知存储 ResetConfig 应报错，实际 %v", err)
	}
}

func TestProviderRegistryForwardsAuthToManager(t *testing.T) {
	auth := &registryAuthProvider{}
	registry := NewProviderRegistry()
	registry.Register("alist", auth)
	ctx := context.Background()

	payload, message, err := registry.QRCode(ctx, "alist")
	if err != nil || message != "扫码" {
		t.Fatalf("QRCode = %v %q %v", payload, message, err)
	}
	if payload, _, err := registry.AuthURL(ctx, "alist"); err != nil || payload == nil {
		t.Fatalf("AuthURL 应透传实现结果: %v %v", payload, err)
	}
	payload, _, err = registry.CheckLogin(ctx, "alist", "ck-value", "token-value")
	if err != nil {
		t.Fatalf("CheckLogin 失败: %v", err)
	}
	if mapped, ok := payload.(map[string]any); !ok || mapped["ck"] != "ck-value" || mapped["token"] != "token-value" {
		t.Fatalf("CheckLogin 参数未透传: %#v", payload)
	}
	if err := registry.SaveConfig(ctx, "alist", nil); err != nil {
		t.Fatalf("SaveConfig 失败: %v", err)
	}
	if err := registry.ResetConfig(ctx, "alist"); err != nil {
		t.Fatalf("ResetConfig 失败: %v", err)
	}

	found := false
	for _, call := range auth.calls {
		if call == "SaveConfig:alist" || call == "ResetConfig:alist" {
			found = true
		}
	}
	if !found {
		t.Fatalf("配置操作未转发到实现: %v", auth.calls)
	}
}
