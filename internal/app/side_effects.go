package app

import (
	"context"
	"errors"
	"sync"

	"github.com/moviepilot-go/moviepilot-go/internal/jobs"
	"github.com/moviepilot-go/moviepilot-go/internal/notificationflow"
	"github.com/moviepilot-go/moviepilot-go/internal/scrapeflow"
)

type scrapeHandlerBinder struct {
	mu      sync.RWMutex
	handler *scrapeflow.Service
}

func (b *scrapeHandlerBinder) Bind(handler *scrapeflow.Service) error {
	if b == nil || handler == nil {
		return jobs.ErrScrapeHandlerUnavailable
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.handler != nil {
		return errors.New("NFO 任务处理器已绑定")
	}
	b.handler = handler
	return nil
}

func (b *scrapeHandlerBinder) HandleScrape(ctx context.Context, args jobs.ScrapeArgs) error {
	if b == nil {
		return jobs.ErrScrapeHandlerUnavailable
	}
	b.mu.RLock()
	handler := b.handler
	b.mu.RUnlock()
	if handler == nil {
		return jobs.ErrScrapeHandlerUnavailable
	}
	return handler.Handle(ctx, args.RequestID)
}

type scrapeSweepHandlerBinder struct {
	mu      sync.RWMutex
	handler *scrapeflow.SweepHandler
}

func (b *scrapeSweepHandlerBinder) Bind(handler *scrapeflow.SweepHandler) error {
	if b == nil || handler == nil {
		return jobs.ErrScrapeSweepHandlerUnavailable
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.handler != nil {
		return errors.New("NFO 恢复处理器已绑定")
	}
	b.handler = handler
	return nil
}

func (b *scrapeSweepHandlerBinder) HandleScrapeSweep(ctx context.Context) error {
	if b == nil {
		return jobs.ErrScrapeSweepHandlerUnavailable
	}
	b.mu.RLock()
	handler := b.handler
	b.mu.RUnlock()
	if handler == nil {
		return jobs.ErrScrapeSweepHandlerUnavailable
	}
	return handler.Handle(ctx)
}

type notificationHandlerBinder struct {
	mu      sync.RWMutex
	handler *notificationflow.Service
}

func (b *notificationHandlerBinder) Bind(handler *notificationflow.Service) error {
	if b == nil || handler == nil {
		return jobs.ErrNotificationHandlerUnavailable
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.handler != nil {
		return errors.New("通知任务处理器已绑定")
	}
	b.handler = handler
	return nil
}

func (b *notificationHandlerBinder) HandleNotification(ctx context.Context, args jobs.NotificationArgs) error {
	if b == nil {
		return jobs.ErrNotificationHandlerUnavailable
	}
	b.mu.RLock()
	handler := b.handler
	b.mu.RUnlock()
	if handler == nil {
		return jobs.ErrNotificationHandlerUnavailable
	}
	return handler.Handle(ctx, args.RequestID)
}

type notificationSweepHandlerBinder struct {
	mu      sync.RWMutex
	handler *notificationflow.SweepHandler
}

func (b *notificationSweepHandlerBinder) Bind(handler *notificationflow.SweepHandler) error {
	if b == nil || handler == nil {
		return jobs.ErrNotificationSweepUnavailable
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.handler != nil {
		return errors.New("通知恢复处理器已绑定")
	}
	b.handler = handler
	return nil
}

func (b *notificationSweepHandlerBinder) HandleNotificationSweep(ctx context.Context) error {
	if b == nil {
		return jobs.ErrNotificationSweepUnavailable
	}
	b.mu.RLock()
	handler := b.handler
	b.mu.RUnlock()
	if handler == nil {
		return jobs.ErrNotificationSweepUnavailable
	}
	return handler.Handle(ctx)
}

type siteUserDataRefreshBinder struct {
	mu      sync.RWMutex
	handler jobs.SiteUserDataRefreshHandler
}

func (b *siteUserDataRefreshBinder) Bind(handler jobs.SiteUserDataRefreshHandler) error {
	if b == nil || handler == nil {
		return jobs.ErrSiteUserDataRefreshUnavailable
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.handler != nil {
		return errors.New("站点用户数据批量刷新处理器已绑定")
	}
	b.handler = handler
	return nil
}

func (b *siteUserDataRefreshBinder) HandleSiteUserDataRefresh(ctx context.Context) error {
	if b == nil {
		return jobs.ErrSiteUserDataRefreshUnavailable
	}
	b.mu.RLock()
	handler := b.handler
	b.mu.RUnlock()
	if handler == nil {
		return jobs.ErrSiteUserDataRefreshUnavailable
	}
	return handler.HandleSiteUserDataRefresh(ctx)
}

var _ jobs.ScrapeHandler = (*scrapeHandlerBinder)(nil)
var _ jobs.ScrapeSweepHandler = (*scrapeSweepHandlerBinder)(nil)
var _ jobs.NotificationHandler = (*notificationHandlerBinder)(nil)
var _ jobs.NotificationSweepHandler = (*notificationSweepHandlerBinder)(nil)
var _ jobs.SiteUserDataRefreshHandler = (*siteUserDataRefreshBinder)(nil)
