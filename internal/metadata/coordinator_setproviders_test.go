package metadata

import (
	"context"
	"testing"
	"time"
)

// stubSwapProvider 是只用于「换源」测试的假数据源：拿指针就能分辨是谁。
type stubSwapProvider struct {
	name string
}

func (p *stubSwapProvider) Name() string { return p.name }

func (p *stubSwapProvider) Search(context.Context, string) ([]Media, error) {
	return []Media{{ID: "1", Source: p.name, Title: p.name}}, nil
}

func (p *stubSwapProvider) Detail(_ context.Context, id string) (Media, error) {
	return Media{ID: id, Source: p.name}, nil
}

// TestCoordinatorSetProvidersSwapsSources 锁住运行期热替换数据源的行为：
// 系统设置里填了 TMDB 密钥后不用重启，识别层立刻能用上新客户端（对应 V2 CONFIG_WATCH）。
func TestCoordinatorSetProvidersSwapsSources(t *testing.T) {
	first := &stubSwapProvider{name: "themoviedb"}
	coordinator, err := NewCoordinator([]Provider{first}, Options{
		RequestTimeout: time.Second, CacheTTL: time.Minute, MaxResults: 10,
	})
	if err != nil {
		t.Fatalf("NewCoordinator() error = %v", err)
	}
	if _, ok := coordinator.Provider("themoviedb"); !ok {
		t.Fatal("初始数据源缺失")
	}

	// 换一个新实例：名字相同、指针不同，必须取到新的那个。
	second := &stubSwapProvider{name: "themoviedb"}
	if err := coordinator.SetProviders([]Provider{second}); err != nil {
		t.Fatalf("SetProviders() error = %v", err)
	}
	got, ok := coordinator.Provider("themoviedb")
	if !ok {
		t.Fatal("替换后数据源消失")
	}
	if got.(*stubSwapProvider) != second {
		t.Fatal("Provider() 仍返回旧实例，热替换没生效")
	}

	// 再加一个来源：原先不存在的名字现在能查到。
	douban := &stubSwapProvider{name: "douban"}
	if err := coordinator.SetProviders([]Provider{second, douban}); err != nil {
		t.Fatalf("SetProviders() error = %v", err)
	}
	if _, ok := coordinator.Provider("douban"); !ok {
		t.Fatal("新增数据源不可见")
	}

	// 清空：TMDB 密钥被删掉后，识别层不该再声称有这个来源。
	if err := coordinator.SetProviders(nil); err != nil {
		t.Fatalf("SetProviders(nil) error = %v", err)
	}
	if _, ok := coordinator.Provider("themoviedb"); ok {
		t.Fatal("清空后仍能取到数据源")
	}
	if _, err := coordinator.Detail(context.Background(), "themoviedb", "1"); err == nil {
		t.Fatal("未配置的数据源应当报错，而不是返回空结果")
	}
}

// TestCoordinatorSetProvidersRejectsBadInput 确认坏输入不会把已有数据源弄坏。
func TestCoordinatorSetProvidersRejectsBadInput(t *testing.T) {
	first := &stubSwapProvider{name: "themoviedb"}
	coordinator, err := NewCoordinator([]Provider{first}, Options{
		RequestTimeout: time.Second, CacheTTL: time.Minute, MaxResults: 10,
	})
	if err != nil {
		t.Fatalf("NewCoordinator() error = %v", err)
	}
	if err := coordinator.SetProviders([]Provider{nil}); err == nil {
		t.Fatal("nil 数据源应当报错")
	}
	if err := coordinator.SetProviders([]Provider{
		&stubSwapProvider{name: "douban"}, &stubSwapProvider{name: "douban"},
	}); err == nil {
		t.Fatal("重名数据源应当报错")
	}
	if got, ok := coordinator.Provider("themoviedb"); !ok || got.(*stubSwapProvider) != first {
		t.Fatal("替换失败后应保留原数据源")
	}
}
