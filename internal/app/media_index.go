package app

import (
	"context"
	"errors"
	"sync"

	"github.com/google/uuid"

	"github.com/moviepilot-go/moviepilot-go/internal/jobs"
)

var (
	errMediaIndexHandlerAlreadyBound      = errors.New("媒体入库确认处理器已绑定")
	errMediaIndexSweepHandlerAlreadyBound = errors.New("媒体入库确认恢复处理器已绑定")
)

type mediaIndexService interface {
	Handle(context.Context, uuid.UUID) error
}

type mediaIndexSweepService interface {
	Handle(context.Context) error
}

// mediaIndexHandlerBinder breaks the construction cycle between River and the
// mediaindex service without exposing River to the business package.
type mediaIndexHandlerBinder struct {
	mu      sync.RWMutex
	handler mediaIndexService
}

func (b *mediaIndexHandlerBinder) Bind(handler mediaIndexService) error {
	if b == nil || handler == nil {
		return jobs.ErrMediaIndexHandlerUnavailable
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.handler != nil {
		return errMediaIndexHandlerAlreadyBound
	}
	b.handler = handler
	return nil
}

func (b *mediaIndexHandlerBinder) HandleMediaIndex(ctx context.Context, args jobs.MediaIndexArgs) error {
	if b == nil {
		return jobs.ErrMediaIndexHandlerUnavailable
	}
	b.mu.RLock()
	handler := b.handler
	b.mu.RUnlock()
	if handler == nil {
		return jobs.ErrMediaIndexHandlerUnavailable
	}
	return handler.Handle(ctx, args.RequestID)
}

type mediaIndexSweepHandlerBinder struct {
	mu      sync.RWMutex
	handler mediaIndexSweepService
}

func (b *mediaIndexSweepHandlerBinder) Bind(handler mediaIndexSweepService) error {
	if b == nil || handler == nil {
		return jobs.ErrMediaIndexSweepHandlerUnavailable
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.handler != nil {
		return errMediaIndexSweepHandlerAlreadyBound
	}
	b.handler = handler
	return nil
}

func (b *mediaIndexSweepHandlerBinder) HandleMediaIndexSweep(ctx context.Context) error {
	if b == nil {
		return jobs.ErrMediaIndexSweepHandlerUnavailable
	}
	b.mu.RLock()
	handler := b.handler
	b.mu.RUnlock()
	if handler == nil {
		return jobs.ErrMediaIndexSweepHandlerUnavailable
	}
	return handler.Handle(ctx)
}

type mediaIndexEnqueuer struct{ manager *jobs.Manager }

func (e mediaIndexEnqueuer) Enqueue(ctx context.Context, requestID uuid.UUID) error {
	if e.manager == nil {
		return jobs.ErrMediaIndexHandlerUnavailable
	}
	return e.manager.EnqueueMediaIndex(ctx, jobs.MediaIndexArgs{RequestID: requestID})
}

var _ jobs.MediaIndexHandler = (*mediaIndexHandlerBinder)(nil)
var _ jobs.MediaIndexSweepHandler = (*mediaIndexSweepHandlerBinder)(nil)
