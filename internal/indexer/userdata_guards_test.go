package indexer

import (
	"context"
	"encoding/json"
	"net/url"
	"testing"
	"time"
)

// 本文件覆盖站点用户数据协调器的闸门与辅助函数：
// nil 参数、不支持用户数据的适配器、站点闸门占用时取消、站点域名回退、
// 数值与时间字段的边界解析。

type userDataGuardAdapter struct{}

func (userDataGuardAdapter) Name() string { return "userdata-guard" }

func (userDataGuardAdapter) Search(context.Context, Query) ([]Resource, error) { return nil, nil }

type userDataBlockingFetcher struct {
	entered chan struct{}
	release chan struct{}
}

func (f *userDataBlockingFetcher) Name() string { return "userdata-blocking" }

func (f *userDataBlockingFetcher) Search(context.Context, Query) ([]Resource, error) { return nil, nil }

func (f *userDataBlockingFetcher) FetchSiteUserData(context.Context) (SiteUserData, error) {
	close(f.entered)
	<-f.release
	return SiteUserData{}, nil
}

func TestCoordinatorFetchSiteUserDataGuards(t *testing.T) {
	coordinator, err := NewCoordinator(1)
	if err != nil {
		t.Fatalf("构造协调器: %v", err)
	}
	defer coordinator.Close()
	fetcher := &userDataBlockingFetcher{entered: make(chan struct{}), release: make(chan struct{})}
	close(fetcher.release)

	t.Run("nil 上下文", func(t *testing.T) {
		if _, err := coordinator.FetchSiteUserData(nil, fetcher); err == nil {
			t.Fatal("nil 上下文应返回错误")
		}
	})

	t.Run("nil 适配器", func(t *testing.T) {
		if _, err := coordinator.FetchSiteUserData(context.Background(), nil); err == nil {
			t.Fatal("nil 适配器应返回错误")
		}
	})

	t.Run("nil 协调器", func(t *testing.T) {
		var nilCoordinator *Coordinator
		if _, err := nilCoordinator.FetchSiteUserData(context.Background(), fetcher); err == nil {
			t.Fatal("nil 协调器应返回错误")
		}
	})

	t.Run("适配器不支持用户数据", func(t *testing.T) {
		if _, err := coordinator.FetchSiteUserData(context.Background(), userDataGuardAdapter{}); err == nil {
			t.Fatal("不支持用户数据的适配器应返回错误")
		}
	})
}

func TestCoordinatorFetchSiteUserDataGateCanceled(t *testing.T) {
	coordinator, err := NewCoordinator(1)
	if err != nil {
		t.Fatalf("构造协调器: %v", err)
	}
	defer coordinator.Close()

	fetcher := &userDataBlockingFetcher{entered: make(chan struct{}), release: make(chan struct{})}
	firstDone := make(chan error, 1)
	go func() {
		_, fetchErr := coordinator.FetchSiteUserData(context.Background(), fetcher)
		firstDone <- fetchErr
	}()
	<-fetcher.entered

	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(20 * time.Millisecond)
		cancel()
	}()
	start := time.Now()
	if _, err := coordinator.FetchSiteUserData(ctx, fetcher); err == nil {
		t.Fatal("站点闸门占用且上下文取消时应返回错误")
	}
	if elapsed := time.Since(start); elapsed > 2*time.Second {
		t.Fatalf("闸门等待未被取消，耗时 %v", elapsed)
	}
	close(fetcher.release)
	if err := <-firstDone; err != nil {
		t.Fatalf("首次抓取 error = %v", err)
	}
}

func TestSiteUserDataHelpers(t *testing.T) {
	if !siteUserDataMessagesEnabled(nil) {
		t.Fatal("nil 上下文应默认拉取站内信")
	}

	if got := siteUserDataDomain(nil); got != "" {
		t.Fatalf("nil base 域名 = %q", got)
	}
	singleLabel, err := url.Parse("http://localhost/path")
	if err != nil {
		t.Fatalf("解析 URL: %v", err)
	}
	if got := siteUserDataDomain(singleLabel); got != "localhost" {
		t.Fatalf("单标签域名 = %q", got)
	}
	deep, err := url.Parse("https://a.b.c.example.com/")
	if err != nil {
		t.Fatalf("解析 URL: %v", err)
	}
	if got := siteUserDataDomain(deep); got != "a.b.c.example.com" {
		t.Fatalf("多级域名 = %q", got)
	}

	if got := userDataInt64("12.5"); got != 12 {
		t.Fatalf("小数文本 = %d", got)
	}
	if got := userDataInt64("1,024"); got != 1024 {
		t.Fatalf("千分位文本 = %d", got)
	}
	if got := userDataID(json.Number("1.5")); got != json.Number("1.5") {
		t.Fatalf("非整数 json.Number = %v", got)
	}
	if got := userDataID(float64(42)); got != int64(42) {
		t.Fatalf("整数值 float64 = %v", got)
	}
	if got := userDataID(float64(1.5)); got != float64(1.5) {
		t.Fatalf("非整数值 float64 = %v", got)
	}
}
