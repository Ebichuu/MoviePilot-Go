package app

import (
	"context"
	"errors"
	"sync"

	"github.com/moviepilot-go/moviepilot-go/internal/downloader"
	"github.com/moviepilot-go/moviepilot-go/internal/downloadflow"
	"github.com/moviepilot-go/moviepilot-go/internal/jobs"
)

var (
	errOrganizationHandlerAlreadyBound = errors.New("整理任务处理器已绑定")
	errDownloadCompletionAlreadyBound  = errors.New("下载完成处理器已绑定")
)

type organizationJobHandler interface {
	jobs.OrganizationHandler
	jobs.OrganizationSweepHandler
}

type organizationAppDomainSetter interface {
	SetAppDomain(string)
}

func configureOrganizationAppDomain(setter organizationAppDomainSetter, appDomain string) {
	setter.SetAppDomain(appDomain)
}

// organizationHandlerBinder breaks the River construction cycle: the manager
// owns the workers, while the concrete handler needs the manager to enqueue
// recovered and newly planned batches.
type organizationHandlerBinder struct {
	mu      sync.RWMutex
	handler organizationJobHandler
}

func (b *organizationHandlerBinder) Bind(handler organizationJobHandler) error {
	if b == nil || handler == nil {
		return jobs.ErrOrganizationHandlerUnavailable
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.handler != nil {
		return errOrganizationHandlerAlreadyBound
	}
	b.handler = handler
	return nil
}

func (b *organizationHandlerBinder) HandleOrganization(ctx context.Context, args jobs.OrganizationArgs) error {
	if b == nil {
		return jobs.ErrOrganizationHandlerUnavailable
	}
	b.mu.RLock()
	handler := b.handler
	b.mu.RUnlock()
	if handler == nil {
		return jobs.ErrOrganizationHandlerUnavailable
	}
	return handler.HandleOrganization(ctx, args)
}

func (b *organizationHandlerBinder) HandleOrganizationSweep(ctx context.Context) error {
	if b == nil {
		return jobs.ErrOrganizationSweepHandlerUnavailable
	}
	b.mu.RLock()
	handler := b.handler
	b.mu.RUnlock()
	if handler == nil {
		return jobs.ErrOrganizationSweepHandlerUnavailable
	}
	return handler.HandleOrganizationSweep(ctx)
}

// downloadCompletionBinder defers completed-download planning until the River
// manager exists and can be injected as the durable batch enqueuer.
type downloadCompletionBinder struct {
	mu      sync.RWMutex
	handler downloadflow.DownloadCompletionHook
}

func (b *downloadCompletionBinder) Bind(handler downloadflow.DownloadCompletionHook) error {
	if b == nil || handler == nil {
		return downloadflow.ErrDownloadReconciliationUnavailable
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.handler != nil {
		return errDownloadCompletionAlreadyBound
	}
	b.handler = handler
	return nil
}

func (b *downloadCompletionBinder) HandleDownloadCompleted(
	ctx context.Context,
	target downloader.ReconciliationTarget,
) error {
	if b == nil {
		return downloadflow.ErrDownloadReconciliationUnavailable
	}
	b.mu.RLock()
	handler := b.handler
	b.mu.RUnlock()
	if handler == nil {
		return downloadflow.ErrDownloadReconciliationUnavailable
	}
	return handler.HandleDownloadCompleted(ctx, target)
}

var _ jobs.OrganizationHandler = (*organizationHandlerBinder)(nil)
var _ jobs.OrganizationSweepHandler = (*organizationHandlerBinder)(nil)
var _ downloadflow.DownloadCompletionHook = (*downloadCompletionBinder)(nil)
