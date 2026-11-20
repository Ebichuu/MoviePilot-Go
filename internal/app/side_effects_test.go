package app

import (
	"context"
	"errors"
	"testing"

	"github.com/moviepilot-go/moviepilot-go/internal/jobs"
	"github.com/moviepilot-go/moviepilot-go/internal/notification"
	"github.com/moviepilot-go/moviepilot-go/internal/notificationflow"
	"github.com/moviepilot-go/moviepilot-go/internal/scrapeflow"
)

// 这些 binder 是「任务队列 → 处理器」的接缝。启动顺序没对上时，
// 队列拿到的是「未绑定」错误，而不是 nil 指针崩溃。
type stubScrapeStore struct{ scrapeflow.Store }
type stubScraper struct{ scrapeflow.Scraper }
type stubNotificationStore struct{ notificationflow.Store }
type stubNotificationSender struct{ notification.Sender }
type stubSiteUserDataRefresh struct {
	calls int
	err   error
}

func (s *stubSiteUserDataRefresh) HandleSiteUserDataRefresh(context.Context) error {
	s.calls++
	return s.err
}

func TestScrapeHandlerBinderLifecycle(t *testing.T) {
	var nilBinder *scrapeHandlerBinder
	if err := nilBinder.Bind(nil); !errors.Is(err, jobs.ErrScrapeHandlerUnavailable) {
		t.Fatalf("nil binder 绑定失败的错误 = %v", err)
	}
	if err := nilBinder.HandleScrape(context.Background(), jobs.ScrapeArgs{}); !errors.Is(err, jobs.ErrScrapeHandlerUnavailable) {
		t.Fatalf("nil binder 处理错误 = %v", err)
	}

	binder := &scrapeHandlerBinder{}
	if err := binder.Bind(nil); !errors.Is(err, jobs.ErrScrapeHandlerUnavailable) {
		t.Fatalf("绑定 nil 处理器应报不可用，实际 %v", err)
	}
	// 未绑定就消费任务：必须是「不可用」，队列据此重试而不是丢任务。
	if err := binder.HandleScrape(context.Background(), jobs.ScrapeArgs{}); !errors.Is(err, jobs.ErrScrapeHandlerUnavailable) {
		t.Fatalf("未绑定时处理错误 = %v", err)
	}

	service, err := scrapeflow.NewService(&stubScrapeStore{}, &stubScraper{})
	if err != nil {
		t.Fatalf("构造处理器失败: %v", err)
	}
	if err := binder.Bind(service); err != nil {
		t.Fatalf("绑定失败: %v", err)
	}
	// 重复绑定要拒绝：静默覆盖会让先绑定的实例被悄悄丢弃。
	if err := binder.Bind(service); err == nil {
		t.Fatal("重复绑定应报错")
	}
	// 绑定成功后错误应来自处理器本身（nil 请求号 → ErrInvalidPlan），证明调用真的转发出去了。
	if err := binder.HandleScrape(context.Background(), jobs.ScrapeArgs{}); !errors.Is(err, scrapeflow.ErrInvalidPlan) {
		t.Fatalf("转发后的错误 = %v，期望 ErrInvalidPlan", err)
	}
}

func TestNotificationHandlerBinderLifecycle(t *testing.T) {
	var nilBinder *notificationHandlerBinder
	if err := nilBinder.Bind(nil); !errors.Is(err, jobs.ErrNotificationHandlerUnavailable) {
		t.Fatalf("nil binder 绑定错误 = %v", err)
	}
	if err := nilBinder.HandleNotification(context.Background(), jobs.NotificationArgs{}); !errors.Is(err, jobs.ErrNotificationHandlerUnavailable) {
		t.Fatalf("nil binder 处理错误 = %v", err)
	}

	binder := &notificationHandlerBinder{}
	if err := binder.Bind(nil); !errors.Is(err, jobs.ErrNotificationHandlerUnavailable) {
		t.Fatalf("绑定 nil 处理器应报不可用，实际 %v", err)
	}
	if err := binder.HandleNotification(context.Background(), jobs.NotificationArgs{}); !errors.Is(err, jobs.ErrNotificationHandlerUnavailable) {
		t.Fatalf("未绑定时处理错误 = %v", err)
	}

	service, err := notificationflow.NewService(&stubNotificationStore{}, &stubNotificationSender{})
	if err != nil {
		t.Fatalf("构造处理器失败: %v", err)
	}
	if err := binder.Bind(service); err != nil {
		t.Fatalf("绑定失败: %v", err)
	}
	if err := binder.Bind(service); err == nil {
		t.Fatal("重复绑定应报错")
	}
	if err := binder.HandleNotification(context.Background(), jobs.NotificationArgs{}); !errors.Is(err, notificationflow.ErrInvalidPlan) {
		t.Fatalf("转发后的错误 = %v，期望 ErrInvalidPlan", err)
	}
}

func TestScrapeSweepHandlerBinderLifecycle(t *testing.T) {
	var nilBinder *scrapeSweepHandlerBinder
	if err := nilBinder.Bind(nil); !errors.Is(err, jobs.ErrScrapeSweepHandlerUnavailable) {
		t.Fatalf("nil binder 绑定错误 = %v", err)
	}
	if err := nilBinder.HandleScrapeSweep(context.Background()); !errors.Is(err, jobs.ErrScrapeSweepHandlerUnavailable) {
		t.Fatalf("nil binder 处理错误 = %v", err)
	}

	binder := &scrapeSweepHandlerBinder{}
	if err := binder.Bind(nil); !errors.Is(err, jobs.ErrScrapeSweepHandlerUnavailable) {
		t.Fatalf("绑定 nil 处理器应报不可用，实际 %v", err)
	}
	if err := binder.HandleScrapeSweep(context.Background()); !errors.Is(err, jobs.ErrScrapeSweepHandlerUnavailable) {
		t.Fatalf("未绑定时处理错误 = %v", err)
	}

	// 依赖不全时构造器必须拒绝，而不是造出一个「一调用就崩」的处理器。
	sweep, err := scrapeflow.NewSweepHandler(nil, nil)
	if !errors.Is(err, scrapeflow.ErrInvalidPlan) {
		t.Fatalf("构造空处理器应返回 ErrInvalidPlan，实际 %v", err)
	}
	if sweep != nil {
		t.Fatal("nil 依赖不应构造出处理器")
	}
}

func TestNotificationSweepHandlerBinderLifecycle(t *testing.T) {
	var nilBinder *notificationSweepHandlerBinder
	if err := nilBinder.Bind(nil); !errors.Is(err, jobs.ErrNotificationSweepUnavailable) {
		t.Fatalf("nil binder 绑定错误 = %v", err)
	}
	if err := nilBinder.HandleNotificationSweep(context.Background()); !errors.Is(err, jobs.ErrNotificationSweepUnavailable) {
		t.Fatalf("nil binder 处理错误 = %v", err)
	}

	binder := &notificationSweepHandlerBinder{}
	if err := binder.Bind(nil); !errors.Is(err, jobs.ErrNotificationSweepUnavailable) {
		t.Fatalf("绑定 nil 处理器应报不可用，实际 %v", err)
	}
	if err := binder.HandleNotificationSweep(context.Background()); !errors.Is(err, jobs.ErrNotificationSweepUnavailable) {
		t.Fatalf("未绑定时处理错误 = %v", err)
	}
}

func TestSiteUserDataRefreshBinderLifecycle(t *testing.T) {
	var nilBinder *siteUserDataRefreshBinder
	if err := nilBinder.Bind(nil); !errors.Is(err, jobs.ErrSiteUserDataRefreshUnavailable) {
		t.Fatalf("nil binder 绑定错误 = %v", err)
	}
	if err := nilBinder.HandleSiteUserDataRefresh(context.Background()); !errors.Is(err, jobs.ErrSiteUserDataRefreshUnavailable) {
		t.Fatalf("nil binder 处理错误 = %v", err)
	}

	binder := &siteUserDataRefreshBinder{}
	if err := binder.Bind(nil); !errors.Is(err, jobs.ErrSiteUserDataRefreshUnavailable) {
		t.Fatalf("绑定 nil 处理器应报不可用，实际 %v", err)
	}
	if err := binder.HandleSiteUserDataRefresh(context.Background()); !errors.Is(err, jobs.ErrSiteUserDataRefreshUnavailable) {
		t.Fatalf("未绑定时处理错误 = %v", err)
	}

	handler := &stubSiteUserDataRefresh{}
	if err := binder.Bind(handler); err != nil {
		t.Fatalf("绑定失败: %v", err)
	}
	if err := binder.Bind(handler); err == nil {
		t.Fatal("重复绑定应报错")
	}
	if err := binder.HandleSiteUserDataRefresh(context.Background()); err != nil {
		t.Fatalf("转发失败: %v", err)
	}
	if handler.calls != 1 {
		t.Fatalf("处理器调用次数 = %d", handler.calls)
	}

	// 处理器的错误必须原样上抛，让队列按失败重试。
	handler.err = errors.New("刷新失败")
	if err := binder.HandleSiteUserDataRefresh(context.Background()); err == nil || err.Error() != "刷新失败" {
		t.Fatalf("处理器错误未上抛: %v", err)
	}
}
