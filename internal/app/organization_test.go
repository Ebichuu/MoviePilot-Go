package app

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/google/uuid"

	"github.com/moviepilot-go/moviepilot-go/internal/downloader"
	"github.com/moviepilot-go/moviepilot-go/internal/downloadflow"
	"github.com/moviepilot-go/moviepilot-go/internal/jobs"
)

func TestOrganizationHandlerBinderRequiresOneBinding(t *testing.T) {
	t.Parallel()
	binder := &organizationHandlerBinder{}
	args := jobs.OrganizationArgs{BatchID: uuid.New()}
	if err := binder.HandleOrganization(t.Context(), args); !errors.Is(err, jobs.ErrOrganizationHandlerUnavailable) {
		t.Fatalf("unbound organization error = %v", err)
	}
	if err := binder.HandleOrganizationSweep(t.Context()); !errors.Is(err, jobs.ErrOrganizationSweepHandlerUnavailable) {
		t.Fatalf("unbound sweep error = %v", err)
	}
	var calls atomic.Int32
	handler := appOrganizationHandlerFunc(func(context.Context, *jobs.OrganizationArgs) error {
		calls.Add(1)
		return nil
	})
	if err := binder.Bind(handler); err != nil {
		t.Fatal(err)
	}
	if err := binder.Bind(handler); !errors.Is(err, errOrganizationHandlerAlreadyBound) {
		t.Fatalf("second bind error = %v", err)
	}
	if err := binder.HandleOrganization(t.Context(), args); err != nil {
		t.Fatal(err)
	}
	if err := binder.HandleOrganizationSweep(t.Context()); err != nil {
		t.Fatal(err)
	}
	if calls.Load() != 2 {
		t.Fatalf("calls = %d", calls.Load())
	}
}

func TestConfigureOrganizationAppDomainPreservesV2Value(t *testing.T) {
	tests := []struct {
		name   string
		domain string
	}{
		{name: "empty"},
		{name: "subpath", domain: "https://mp.example.com/moviepilot"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			setter := &appOrganizationDomainSetter{}
			configureOrganizationAppDomain(setter, tt.domain)
			if setter.calls != 1 || setter.domain != tt.domain {
				t.Fatalf("SetAppDomain calls=%d domain=%q, want 1/%q", setter.calls, setter.domain, tt.domain)
			}
		})
	}
}

func TestDownloadCompletionBinderConcurrentReadsAfterBinding(t *testing.T) {
	t.Parallel()
	binder := &downloadCompletionBinder{}
	target := downloader.ReconciliationTarget{ID: uuid.New(), State: downloader.IntentCompleted}
	if err := binder.HandleDownloadCompleted(t.Context(), target); !errors.Is(err, downloadflow.ErrDownloadReconciliationUnavailable) {
		t.Fatalf("unbound completion error = %v", err)
	}
	var calls atomic.Int32
	handler := appDownloadCompletionHookFunc(func(context.Context, downloader.ReconciliationTarget) error {
		calls.Add(1)
		return nil
	})
	if err := binder.Bind(handler); err != nil {
		t.Fatal(err)
	}
	if err := binder.Bind(handler); !errors.Is(err, errDownloadCompletionAlreadyBound) {
		t.Fatalf("second bind error = %v", err)
	}
	var group sync.WaitGroup
	for range 32 {
		group.Add(1)
		go func() {
			defer group.Done()
			if err := binder.HandleDownloadCompleted(t.Context(), target); err != nil {
				t.Error(err)
			}
		}()
	}
	group.Wait()
	if calls.Load() != 32 {
		t.Fatalf("calls = %d", calls.Load())
	}
}

type appOrganizationHandlerFunc func(context.Context, *jobs.OrganizationArgs) error

type appOrganizationDomainSetter struct {
	calls  int
	domain string
}

func (s *appOrganizationDomainSetter) SetAppDomain(domain string) {
	s.calls++
	s.domain = domain
}

func (f appOrganizationHandlerFunc) HandleOrganization(ctx context.Context, args jobs.OrganizationArgs) error {
	return f(ctx, &args)
}

func (f appOrganizationHandlerFunc) HandleOrganizationSweep(ctx context.Context) error {
	return f(ctx, nil)
}

type appDownloadCompletionHookFunc func(context.Context, downloader.ReconciliationTarget) error

func (f appDownloadCompletionHookFunc) HandleDownloadCompleted(
	ctx context.Context,
	target downloader.ReconciliationTarget,
) error {
	return f(ctx, target)
}
