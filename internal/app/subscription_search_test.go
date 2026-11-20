package app

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/moviepilot-go/moviepilot-go/internal/jobs"
)

func TestSubscriptionSearchBinderRequiresOneBinding(t *testing.T) {
	t.Parallel()
	binder := &subscriptionSearchBinder{}
	if err := binder.HandleSubscriptionSearch(t.Context(), time.Now()); !errors.Is(err, jobs.ErrSubscriptionSearchUnavailable) {
		t.Fatalf("unbound error = %v", err)
	}
	var calls atomic.Int32
	handler := appSubscriptionSearchHandlerFunc(func(context.Context, time.Time) error {
		calls.Add(1)
		return nil
	})
	if err := binder.Bind(handler); err != nil {
		t.Fatal(err)
	}
	if err := binder.Bind(handler); !errors.Is(err, errSubscriptionSearchHandlerAlreadyBound) {
		t.Fatalf("second bind error = %v", err)
	}
	if err := binder.HandleSubscriptionSearch(t.Context(), time.Now()); err != nil || calls.Load() != 1 {
		t.Fatalf("HandleSubscriptionSearch() error=%v calls=%d", err, calls.Load())
	}
}

func TestSubscriptionSearchBinderConcurrentReadsAfterBinding(t *testing.T) {
	t.Parallel()
	binder := &subscriptionSearchBinder{}
	var calls atomic.Int32
	if err := binder.Bind(appSubscriptionSearchHandlerFunc(func(context.Context, time.Time) error {
		calls.Add(1)
		return nil
	})); err != nil {
		t.Fatal(err)
	}
	var group sync.WaitGroup
	for range 32 {
		group.Add(1)
		go func() {
			defer group.Done()
			if err := binder.HandleSubscriptionSearch(t.Context(), time.Now()); err != nil {
				t.Error(err)
			}
		}()
	}
	group.Wait()
	if calls.Load() != 32 {
		t.Fatalf("calls = %d", calls.Load())
	}
}

type appSubscriptionSearchHandlerFunc func(context.Context, time.Time) error

func (f appSubscriptionSearchHandlerFunc) HandleSubscriptionSearch(ctx context.Context, cutoff time.Time) error {
	return f(ctx, cutoff)
}
