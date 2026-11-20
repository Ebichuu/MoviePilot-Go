package app

import (
	"context"
	"errors"
	"sync"
	"time"

	"github.com/moviepilot-go/moviepilot-go/internal/jobs"
	"github.com/moviepilot-go/moviepilot-go/internal/subscriptionflow"
)

var errRSSHandlerAlreadyBound = errors.New("RSS 订阅处理器已绑定")

// rssHandlerBinder breaks the River construction cycle: the manager owns the
// workers, while the concrete RSS handler needs the manager to enqueue one
// site poll from a sweep.
type rssHandlerBinder struct {
	mu      sync.RWMutex
	handler *subscriptionflow.RSSHandler
}

func (b *rssHandlerBinder) Bind(handler *subscriptionflow.RSSHandler) error {
	if b == nil || handler == nil {
		return jobs.ErrRSSSweepHandlerUnavailable
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.handler != nil {
		return errRSSHandlerAlreadyBound
	}
	b.handler = handler
	return nil
}

func (b *rssHandlerBinder) HandleRSSSweep(
	ctx context.Context,
	cutoff time.Time,
	state jobs.RSSSweepState,
) (jobs.RSSSweepState, error) {
	if b == nil {
		return state, jobs.ErrRSSSweepHandlerUnavailable
	}
	b.mu.RLock()
	handler := b.handler
	b.mu.RUnlock()
	if handler == nil {
		return state, jobs.ErrRSSSweepHandlerUnavailable
	}
	return handler.HandleRSSSweep(ctx, cutoff, state)
}

func (b *rssHandlerBinder) HandleRSSPoll(ctx context.Context, args jobs.RSSPollArgs) error {
	if b == nil {
		return jobs.ErrRSSPollHandlerUnavailable
	}
	b.mu.RLock()
	handler := b.handler
	b.mu.RUnlock()
	if handler == nil {
		return jobs.ErrRSSPollHandlerUnavailable
	}
	return handler.HandleRSSPoll(ctx, args)
}

func (b *rssHandlerBinder) HandleRSSMatch(ctx context.Context, args jobs.RSSMatchArgs) error {
	if b == nil {
		return jobs.ErrRSSMatchHandlerUnavailable
	}
	b.mu.RLock()
	handler := b.handler
	b.mu.RUnlock()
	if handler == nil {
		return jobs.ErrRSSMatchHandlerUnavailable
	}
	return handler.HandleRSSMatch(ctx, args)
}

var _ jobs.RSSSweepHandler = (*rssHandlerBinder)(nil)
var _ jobs.RSSPollHandler = (*rssHandlerBinder)(nil)
var _ jobs.RSSMatchHandler = (*rssHandlerBinder)(nil)
