package indexer

import (
	"context"
	"errors"
	"net/url"
	"testing"
)

type siteUserDataTestAdapter struct {
	id       int
	started  chan struct{}
	release  chan struct{}
	messages chan bool
}

func (a *siteUserDataTestAdapter) Name() string { return "userdata" }
func (a *siteUserDataTestAdapter) SiteID() int  { return a.id }
func (a *siteUserDataTestAdapter) Search(context.Context, Query) ([]Resource, error) {
	return nil, nil
}
func (a *siteUserDataTestAdapter) FetchSiteUserData(ctx context.Context) (SiteUserData, error) {
	if a.messages != nil {
		a.messages <- siteUserDataMessagesEnabled(ctx)
	}
	close(a.started)
	select {
	case <-a.release:
		return SiteUserData{Domain: "example.com"}, nil
	case <-ctx.Done():
		return SiteUserData{}, ctx.Err()
	}
}

func TestCoordinatorFetchSiteUserDataPassesV2MessageOption(t *testing.T) {
	tests := []struct {
		name    string
		options []SiteUserDataOptions
		want    bool
	}{
		{name: "default enabled", want: true},
		{name: "explicitly disabled", options: []SiteUserDataOptions{{FetchMessages: false}}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			coordinator, err := NewCoordinator(1)
			if err != nil {
				t.Fatal(err)
			}
			defer coordinator.Close()
			release := make(chan struct{})
			close(release)
			adapter := &siteUserDataTestAdapter{
				id: 1, started: make(chan struct{}), release: release, messages: make(chan bool, 1),
			}
			if _, err := coordinator.FetchSiteUserData(t.Context(), adapter, tt.options...); err != nil {
				t.Fatal(err)
			}
			if got := <-adapter.messages; got != tt.want {
				t.Fatalf("FetchMessages = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestCoordinatorFetchSiteUserDataUsesPerSiteGate(t *testing.T) {
	coordinator, err := NewCoordinator(2)
	if err != nil {
		t.Fatalf("NewCoordinator() error = %v", err)
	}
	defer coordinator.Close()
	adapter := &siteUserDataTestAdapter{id: 7, started: make(chan struct{}), release: make(chan struct{})}
	firstDone := make(chan error, 1)
	go func() {
		_, fetchErr := coordinator.FetchSiteUserData(t.Context(), adapter)
		firstDone <- fetchErr
	}()
	<-adapter.started

	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if _, err := coordinator.FetchSiteUserData(ctx, adapter); !errors.Is(err, context.Canceled) {
		t.Fatalf("second FetchSiteUserData() error = %v, want context.Canceled", err)
	}
	close(adapter.release)
	if err := <-firstDone; err != nil {
		t.Fatalf("first FetchSiteUserData() error = %v", err)
	}
}

func TestSiteUserDataDomainMatchesV2Normalization(t *testing.T) {
	tests := []struct {
		name string
		raw  string
		want string
	}{
		{name: "www host", raw: "https://www.example.com/path", want: "example.com"},
		{name: "four labels stay intact", raw: "https://a.b.example.com", want: "a.b.example.com"},
		{name: "special domain", raw: "https://mirror.u2.dmhy.org", want: "u2.dmhy.org"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			parsed, err := url.Parse(tt.raw)
			if err != nil {
				t.Fatal(err)
			}
			if got := siteUserDataDomain(parsed); got != tt.want {
				t.Fatalf("siteUserDataDomain(%q) = %q, want %q", tt.raw, got, tt.want)
			}
		})
	}
}
