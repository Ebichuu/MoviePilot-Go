package app

import (
	"context"
	"errors"
	"sync"
	"time"

	"github.com/moviepilot-go/moviepilot-go/internal/jobs"
)

var errSubscriptionSearchHandlerAlreadyBound = errors.New("订阅精准搜索处理器已绑定")

type subscriptionSearchBinder struct {
	mu      sync.RWMutex
	handler jobs.SubscriptionSearchHandler
}

func (b *subscriptionSearchBinder) Bind(handler jobs.SubscriptionSearchHandler) error {
	if b == nil || handler == nil {
		return jobs.ErrSubscriptionSearchUnavailable
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.handler != nil {
		return errSubscriptionSearchHandlerAlreadyBound
	}
	b.handler = handler
	return nil
}

func (b *subscriptionSearchBinder) HandleSubscriptionSearch(ctx context.Context, cutoff time.Time) error {
	if b == nil {
		return jobs.ErrSubscriptionSearchUnavailable
	}
	b.mu.RLock()
	handler := b.handler
	b.mu.RUnlock()
	if handler == nil {
		return jobs.ErrSubscriptionSearchUnavailable
	}
	return handler.HandleSubscriptionSearch(ctx, cutoff)
}

var _ jobs.SubscriptionSearchHandler = (*subscriptionSearchBinder)(nil)
