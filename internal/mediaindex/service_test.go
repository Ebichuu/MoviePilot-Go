package mediaindex

import (
	"context"
	"errors"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"go.uber.org/goleak"
)

func TestEqualRemotePathUsesConfiguredSyntax(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name  string
		left  string
		right string
		style string
		fold  bool
		want  bool
	}{
		{name: "posix separators", left: "/media/show/a.mkv", right: `/media/show//a.mkv`, style: "posix", want: true},
		{name: "windows case insensitive", left: `D:\Media\Show\a.mkv`, right: `d:/media/show/a.mkv`, style: "windows", fold: true, want: true},
		{name: "windows case sensitive", left: `D:\Media\Show\a.mkv`, right: `D:\media\Show\a.mkv`, style: "windows", fold: false, want: false},
		{name: "component boundary", left: "/media/show/a.mkv", right: "/media/show2/a.mkv", style: "posix", want: false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := EqualRemotePath(tt.left, tt.right, tt.style, !tt.fold); got != tt.want {
				t.Fatalf("EqualRemotePath() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestServiceRefreshThenConfirmAndFinalize(t *testing.T) {
	t.Parallel()
	requestID := uuid.New()
	serverID := "server-1"
	now := time.Date(2026, 8, 12, 12, 0, 0, 0, time.UTC)
	store := newMediaIndexStoreFake(requestID, serverID, now)
	server := &mediaIndexServerFake{}
	registry := &registryFake{ref: store.request.Server, server: server}
	verifier := &localVerifierFake{valid: true}
	service, err := NewService(store, store, store, registry, verifier)
	if err != nil {
		t.Fatal(err)
	}
	service.now = func() time.Time { return now }
	if err := service.Handle(context.Background(), requestID); err != nil {
		t.Fatalf("first Handle() error = %v", err)
	}
	if server.refreshes != 1 || !store.refreshRecorded {
		t.Fatalf("refresh state = %d/%v", server.refreshes, store.refreshRecorded)
	}
	if registry.resolved != store.request.Server {
		t.Fatalf("Resolve() ref = %#v, want %#v", registry.resolved, store.request.Server)
	}
	store.nextClaimAt = now.Add(31 * time.Second)
	now = now.Add(time.Minute)
	server.items = []Item{{ID: "item-1", Path: "/emby/show/episode.mkv"}}
	if err := service.Handle(context.Background(), requestID); err != nil {
		t.Fatalf("second Handle() error = %v", err)
	}
	if !store.fileConfirmed || !store.requestConfirmed {
		t.Fatalf("confirmation = file:%v request:%v", store.fileConfirmed, store.requestConfirmed)
	}
}

func TestNewServiceRequiresLocalVerifier(t *testing.T) {
	t.Parallel()
	requestID := uuid.New()
	now := time.Now().UTC()
	store := newMediaIndexStoreFake(requestID, "server", now)
	_, err := NewService(
		store,
		store,
		store,
		&registryFake{ref: store.request.Server, server: &mediaIndexServerFake{}},
		nil,
	)
	if !errors.Is(err, ErrInvalidRequest) {
		t.Fatalf("NewService(nil verifier) error = %v, want ErrInvalidRequest", err)
	}
}

func TestServiceVerifiesLocalFingerprintBeforeRemoteLookup(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name      string
		valid     bool
		verifyErr error
		wantError string
	}{
		{name: "fingerprint changed", wantError: stableLocalFingerprintChanged},
		{
			name: "local file unavailable", valid: true,
			verifyErr: errors.New("private path C:/secret token=hidden"),
			wantError: stableLocalFileUnavailable,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			requestID := uuid.New()
			now := time.Now().UTC()
			store := newMediaIndexStoreFake(requestID, "server", now)
			store.refreshRecorded = true
			store.request.RefreshRequestedAt = &now
			server := &mediaIndexServerFake{items: []Item{{
				ID: "item", Path: "/emby/show/episode.mkv",
			}}}
			verifier := &localVerifierFake{valid: tt.valid, err: tt.verifyErr}
			service, err := NewService(
				store,
				store,
				store,
				&registryFake{ref: store.request.Server, server: server},
				verifier,
			)
			if err != nil {
				t.Fatal(err)
			}
			if err := service.Handle(context.Background(), requestID); err != nil {
				t.Fatalf("Handle() error = %v", err)
			}
			if store.fileConfirmed || store.requestConfirmed || store.lastError != tt.wantError {
				t.Fatalf("unexpected state: %#v", store)
			}
			if server.finds != 0 {
				t.Fatalf("FindItems() calls = %d, want 0", server.finds)
			}
			verifier.mu.Lock()
			calls := verifier.calls
			paths := append([]string(nil), verifier.paths...)
			sizes := append([]int64(nil), verifier.sizes...)
			digests := append([]string(nil), verifier.digests...)
			verifier.mu.Unlock()
			if calls != 1 || len(paths) != 1 || paths[0] != "C:/library/episode.mkv" ||
				len(sizes) != 1 || sizes[0] != 1 || len(digests) != 1 || digests[0] != strings.Repeat("a", 64) {
				t.Fatalf("Verify() calls = %d paths %#v sizes %#v digests %#v", calls, paths, sizes, digests)
			}
		})
	}
}

func TestServiceContinuesBeyondThirtyTwoAttempts(t *testing.T) {
	t.Parallel()
	requestID := uuid.New()
	now := time.Now().UTC()
	store := newMediaIndexStoreFake(requestID, "server", now)
	store.request.AttemptCount = 32
	store.refreshRecorded = true
	store.request.RefreshRequestedAt = &now
	server := &mediaIndexServerFake{items: []Item{{ID: "item", Path: "/emby/show/episode.mkv"}}}
	service, err := NewService(
		store,
		store,
		store,
		&registryFake{ref: store.request.Server, server: server},
		&localVerifierFake{valid: true},
	)
	if err != nil {
		t.Fatal(err)
	}
	if err := service.Handle(context.Background(), requestID); err != nil {
		t.Fatalf("Handle() error = %v", err)
	}
	if store.request.AttemptCount != 33 || !store.requestConfirmed {
		t.Fatalf("attempt/state = %d/%v, want 33/confirmed", store.request.AttemptCount, store.requestConfirmed)
	}
}

func TestServiceRenewsLeaseAndStopsHeartbeat(t *testing.T) {
	defer goleak.VerifyNone(t, goleak.IgnoreCurrent())
	requestID := uuid.New()
	now := time.Date(2026, 8, 12, 13, 0, 0, 0, time.UTC)
	store := newMediaIndexStoreFake(requestID, "server", now)
	store.refreshRecorded = true
	store.request.RefreshRequestedAt = &now
	store.renewed = make(chan struct{}, 1)
	server := newBlockingMediaIndexServer([]Item{{ID: "item", Path: "/emby/show/episode.mkv"}})
	service, err := NewService(
		store,
		store,
		store,
		&registryFake{ref: store.request.Server, server: server},
		&localVerifierFake{valid: true},
	)
	if err != nil {
		t.Fatal(err)
	}
	service.now = func() time.Time { return now }
	service.renewEvery = 5 * time.Millisecond

	result := make(chan error, 1)
	go func() { result <- service.Handle(context.Background(), requestID) }()
	waitMediaIndexSignal(t, server.started, "FindItems start")
	waitMediaIndexSignal(t, store.renewed, "lease renewal")
	close(server.release)
	if err := waitMediaIndexError(t, result); err != nil {
		t.Fatalf("Handle() error = %v", err)
	}

	store.mu.Lock()
	renewCalls := store.renewCalls
	renewNow := store.renewNow
	renewUntil := store.renewUntil
	store.mu.Unlock()
	if renewCalls < 1 || renewUntil.Sub(renewNow) != leaseDuration {
		t.Fatalf("renewal = calls %d, now %s, until %s", renewCalls, renewNow, renewUntil)
	}
	time.Sleep(4 * service.renewEvery)
	store.mu.Lock()
	afterStop := store.renewCalls
	store.mu.Unlock()
	if afterStop != renewCalls {
		t.Fatalf("heartbeat continued after Handle(): before %d after %d", renewCalls, afterStop)
	}
}

func TestServiceRenewFailureCancelsWork(t *testing.T) {
	defer goleak.VerifyNone(t, goleak.IgnoreCurrent())
	requestID := uuid.New()
	now := time.Now().UTC()
	store := newMediaIndexStoreFake(requestID, "server", now)
	store.refreshRecorded = true
	store.request.RefreshRequestedAt = &now
	store.renewErr = errors.New("lease conflict token=secret")
	server := newBlockingMediaIndexServer(nil)
	service, err := NewService(
		store,
		store,
		store,
		&registryFake{ref: store.request.Server, server: server},
		&localVerifierFake{valid: true},
	)
	if err != nil {
		t.Fatal(err)
	}
	service.renewEvery = 5 * time.Millisecond

	result := make(chan error, 1)
	go func() { result <- service.Handle(context.Background(), requestID) }()
	waitMediaIndexSignal(t, server.started, "FindItems start")
	handleErr := waitMediaIndexError(t, result)
	if !errors.Is(handleErr, ErrLeaseLost) {
		t.Fatalf("Handle() error = %v, want ErrLeaseLost", handleErr)
	}
	waitMediaIndexSignal(t, server.canceled, "FindItems cancellation")
	if store.fileConfirmed || store.requestConfirmed || store.lastError != "" {
		t.Fatalf("lost lease mutated durable state: %#v", store)
	}
}

func TestServiceContextCancellationStopsHeartbeat(t *testing.T) {
	defer goleak.VerifyNone(t, goleak.IgnoreCurrent())
	requestID := uuid.New()
	now := time.Now().UTC()
	store := newMediaIndexStoreFake(requestID, "server", now)
	store.refreshRecorded = true
	store.request.RefreshRequestedAt = &now
	server := newBlockingMediaIndexServer(nil)
	service, err := NewService(
		store,
		store,
		store,
		&registryFake{ref: store.request.Server, server: server},
		&localVerifierFake{valid: true},
	)
	if err != nil {
		t.Fatal(err)
	}
	service.renewEvery = time.Hour
	ctx, cancel := context.WithCancel(context.Background())
	result := make(chan error, 1)
	go func() { result <- service.Handle(ctx, requestID) }()
	waitMediaIndexSignal(t, server.started, "FindItems start")
	cancel()
	if err := waitMediaIndexError(t, result); !errors.Is(err, context.Canceled) {
		t.Fatalf("Handle() error = %v, want context.Canceled", err)
	}
	waitMediaIndexSignal(t, server.canceled, "FindItems cancellation")
	store.mu.Lock()
	renewCalls := store.renewCalls
	store.mu.Unlock()
	if renewCalls != 0 {
		t.Fatalf("Renew() calls = %d, want 0", renewCalls)
	}
}

func TestServiceDefersZeroAndAmbiguousMatches(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name       string
		providerID string
		items      []Item
		findErr    error
		wantError  string
	}{
		{name: "zero matches", wantError: stableNotVisible},
		{name: "Emby unavailable", findErr: errors.New("connection refused"), wantError: "server_unavailable"},
		{
			name: "ambiguous matches",
			items: []Item{
				{ID: "one", Path: "/emby/show/episode.mkv"},
				{ID: "two", Path: "/emby/show/episode.mkv"},
			},
			wantError: stableAmbiguousMatch,
		},
		{
			name:      "path mismatch",
			items:     []Item{{ID: "one", Path: "/emby/show/other.mkv"}},
			wantError: stablePathMismatch,
		},
		{
			name:       "provider mismatch",
			providerID: "tmdb.123",
			items: []Item{{
				ID: "one", Path: "/emby/show/episode.mkv", ProviderIDs: map[string]string{"Tmdb": "999"},
			}},
			wantError: stableProviderMismatch,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			requestID := uuid.New()
			now := time.Now().UTC()
			store := newMediaIndexStoreFake(requestID, "server", now)
			store.refreshRecorded = true
			store.request.RefreshRequestedAt = &now
			store.providerID = tt.providerID
			server := &mediaIndexServerFake{items: tt.items, findErr: tt.findErr}
			service, err := NewService(
				store,
				store,
				store,
				&registryFake{ref: store.request.Server, server: server},
				&localVerifierFake{valid: true},
			)
			if err != nil {
				t.Fatal(err)
			}
			if err := service.Handle(context.Background(), requestID); err != nil {
				t.Fatalf("Handle() error = %v", err)
			}
			if store.requestConfirmed || store.fileConfirmed || store.lastError != tt.wantError {
				t.Fatalf("unexpected settled state: %#v", store)
			}
		})
	}
}

func TestServiceDefersWhenEmbyRefreshIsUnavailable(t *testing.T) {
	t.Parallel()
	requestID := uuid.New()
	now := time.Date(2026, 8, 13, 10, 0, 0, 0, time.UTC)
	store := newMediaIndexStoreFake(requestID, "server", now)
	server := &mediaIndexServerFake{refreshErr: errors.New("connection refused: emby.internal")}
	service, err := NewService(
		store,
		store,
		store,
		&registryFake{ref: store.request.Server, server: server},
		&localVerifierFake{valid: true},
	)
	if err != nil {
		t.Fatal(err)
	}
	service.now = func() time.Time { return now }

	if err := service.Handle(t.Context(), requestID); err != nil {
		t.Fatalf("Handle() error = %v, want durable deferral", err)
	}
	store.mu.Lock()
	deferred := store.request
	store.mu.Unlock()
	if deferred.State != RequestFailed || deferred.LastError != "server_unavailable" {
		t.Fatalf("deferred request = %#v, want failed/server_unavailable", deferred)
	}
	if !deferred.NextAttemptAt.Equal(now.Add(serverBackoff)) {
		t.Fatalf("next attempt = %s, want %s", deferred.NextAttemptAt, now.Add(serverBackoff))
	}
	if store.refreshRecorded {
		t.Fatal("refresh success was recorded after Emby refresh failure")
	}
	if server.refreshes != 1 || server.finds != 0 {
		t.Fatalf("server calls = refresh %d/find %d, want 1/0", server.refreshes, server.finds)
	}
}

func TestUniquePathMatchRequiresProviderConsistencyWhenAvailable(t *testing.T) {
	t.Parallel()
	file := File{
		RemotePath: "/media/movie.mkv", PathStyle: "posix", CaseSensitive: true,
		ProviderID: "tmdb.123",
	}
	tests := []struct {
		name         string
		items        []Item
		wantMismatch string
	}{
		{
			name:  "matching provider",
			items: []Item{{ID: "item", Path: file.RemotePath, ProviderIDs: map[string]string{"Tmdb": "123"}}},
		},
		{
			name:         "different provider value",
			items:        []Item{{ID: "item", Path: file.RemotePath, ProviderIDs: map[string]string{"Tmdb": "999"}}},
			wantMismatch: stableProviderMismatch,
		},
		{
			name:         "missing provider",
			items:        []Item{{ID: "item", Path: file.RemotePath}},
			wantMismatch: stableProviderMismatch,
		},
		{
			name:         "path mismatch precedes provider check",
			items:        []Item{{ID: "item", Path: "/media/other.mkv", ProviderIDs: map[string]string{"Tmdb": "999"}}},
			wantMismatch: stablePathMismatch,
		},
		{
			name: "ambiguous precedes item checks",
			items: []Item{
				{ID: "one", Path: "/media/other.mkv"},
				{ID: "two", Path: file.RemotePath},
			},
			wantMismatch: stableAmbiguousMatch,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, gotMismatch := uniquePathMatch(tt.items, file)
			if gotMismatch != tt.wantMismatch {
				t.Fatalf("uniquePathMatch() mismatch = %q, want %q", gotMismatch, tt.wantMismatch)
			}
		})
	}
}

func TestServiceDoesNotLeakServerErrors(t *testing.T) {
	t.Parallel()
	requestID := uuid.New()
	now := time.Now().UTC()
	store := newMediaIndexStoreFake(requestID, "server", now)
	store.refreshRecorded = true
	store.request.RefreshRequestedAt = &now
	server := &mediaIndexServerFake{findErr: errors.New("token=secret path=/private")}
	service, err := NewService(
		store,
		store,
		store,
		&registryFake{ref: store.request.Server, server: server},
		&localVerifierFake{valid: true},
	)
	if err != nil {
		t.Fatal(err)
	}
	if err := service.Handle(context.Background(), requestID); err != nil {
		t.Fatalf("Handle() error = %v", err)
	}
	if slices.Contains([]string{store.lastError}, "secret") || slices.Contains([]string{store.lastError}, "private") {
		t.Fatalf("sensitive error persisted: %q", store.lastError)
	}
}

func TestSweepHandlerBoundsAndEnqueues(t *testing.T) {
	t.Parallel()
	ids := []uuid.UUID{uuid.New(), uuid.New()}
	store := &sweepStoreFake{ids: ids}
	enqueuer := &enqueuerFake{}
	handler, err := NewSweepHandler(store, enqueuer)
	if err != nil {
		t.Fatal(err)
	}
	if err := handler.Handle(context.Background()); err != nil {
		t.Fatalf("Handle() error = %v", err)
	}
	if store.recovered != 1 || !slices.Equal(enqueuer.ids, ids) {
		t.Fatalf("sweep = recovered %d ids %#v", store.recovered, enqueuer.ids)
	}
}

type registryFake struct {
	ref      ServerRef
	server   Server
	resolved ServerRef
}

func (r *registryFake) Resolve(ref ServerRef) (Server, bool) {
	r.resolved = ref
	if ref.ID != r.ref.ID || !strings.EqualFold(ref.Type, r.ref.Type) || ref.Fingerprint != r.ref.Fingerprint {
		return nil, false
	}
	return r.server, r.server != nil
}

type localVerifierFake struct {
	mu      sync.Mutex
	valid   bool
	err     error
	calls   int
	paths   []string
	sizes   []int64
	digests []string
}

func (v *localVerifierFake) Verify(_ context.Context, path string, size int64, digest string) (bool, error) {
	v.mu.Lock()
	defer v.mu.Unlock()
	v.calls++
	v.paths = append(v.paths, path)
	v.sizes = append(v.sizes, size)
	v.digests = append(v.digests, digest)
	return v.valid, v.err
}

type mediaIndexServerFake struct {
	refreshes  int
	refreshErr error
	finds      int
	items      []Item
	findErr    error
}

func (s *mediaIndexServerFake) RefreshAll(context.Context) error {
	s.refreshes++
	return s.refreshErr
}
func (s *mediaIndexServerFake) FindItems(context.Context, Query) ([]Item, error) {
	s.finds++
	return append([]Item(nil), s.items...), s.findErr
}

type blockingMediaIndexServer struct {
	started    chan struct{}
	release    chan struct{}
	canceled   chan struct{}
	startOnce  sync.Once
	cancelOnce sync.Once
	items      []Item
}

func newBlockingMediaIndexServer(items []Item) *blockingMediaIndexServer {
	return &blockingMediaIndexServer{
		started: make(chan struct{}), release: make(chan struct{}), canceled: make(chan struct{}),
		items: append([]Item(nil), items...),
	}
}

func (*blockingMediaIndexServer) RefreshAll(context.Context) error { return nil }

func (s *blockingMediaIndexServer) FindItems(ctx context.Context, _ Query) ([]Item, error) {
	s.startOnce.Do(func() { close(s.started) })
	select {
	case <-s.release:
		return append([]Item(nil), s.items...), nil
	case <-ctx.Done():
		s.cancelOnce.Do(func() { close(s.canceled) })
		return nil, ctx.Err()
	}
}

type mediaIndexStoreFake struct {
	mu               sync.Mutex
	request          Request
	now              time.Time
	nextClaimAt      time.Time
	refreshRecorded  bool
	fileConfirmed    bool
	requestConfirmed bool
	lastError        string
	providerID       string
	renewErr         error
	renewCalls       int
	renewed          chan struct{}
	renewNow         time.Time
	renewUntil       time.Time
}

func newMediaIndexStoreFake(id uuid.UUID, serverID string, now time.Time) *mediaIndexStoreFake {
	return &mediaIndexStoreFake{
		request: Request{ID: id, Server: ServerRef{
			ID: serverID, Name: "test", Type: "emby", Fingerprint: strings.Repeat("b", 64),
		}, State: RequestPending},
		now: now,
	}
}

func (s *mediaIndexStoreFake) Claim(_ context.Context, id uuid.UUID, now time.Time, _ time.Duration) (Lease, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.request.ID != id || (!s.nextClaimAt.IsZero() && now.Before(s.nextClaimAt)) {
		return Lease{}, false, nil
	}
	s.request.State = RequestWorking
	s.request.AttemptCount++
	return Lease{Request: s.request, LeaseToken: uuid.MustParse("aaaaaaaa-bbbb-4ccc-8ddd-eeeeeeeeeeee")}, true, nil
}

func (s *mediaIndexStoreFake) Renew(
	_ context.Context,
	_ uuid.UUID,
	_ uuid.UUID,
	now time.Time,
	leaseUntil time.Time,
) error {
	s.mu.Lock()
	s.renewCalls++
	s.renewNow = now
	s.renewUntil = leaseUntil
	err := s.renewErr
	renewed := s.renewed
	s.mu.Unlock()
	if renewed != nil {
		select {
		case renewed <- struct{}{}:
		default:
		}
	}
	return err
}

func (s *mediaIndexStoreFake) Reschedule(_ context.Context, _ uuid.UUID, _ uuid.UUID, next time.Time, stableErr string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.request.State = RequestFailed
	s.request.NextAttemptAt = next
	s.request.LastError = stableErr
	s.nextClaimAt = next
	s.lastError = stableErr
	return nil
}

func (s *mediaIndexStoreFake) RecordRefresh(_ context.Context, _ uuid.UUID, _ uuid.UUID, at, next time.Time) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.refreshRecorded = true
	s.request.RefreshRequestedAt = &at
	s.nextClaimAt = next
	return nil
}

func (s *mediaIndexStoreFake) ListPendingFiles(context.Context, uuid.UUID, uuid.UUID, int) ([]File, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.fileConfirmed {
		return nil, nil
	}
	return []File{{
		ID: 1, RequestID: s.request.ID, LocalPath: "C:/library/episode.mkv",
		RemotePath: "/emby/show/episode.mkv", PathStyle: "posix", CaseSensitive: true,
		ExpectedSize: 1, ExpectedSHA256: strings.Repeat("a", 64), ProviderID: s.providerID,
	}}, nil
}

func (s *mediaIndexStoreFake) ConfirmFile(context.Context, uuid.UUID, uuid.UUID, int64, string) error {
	s.mu.Lock()
	s.fileConfirmed = true
	s.mu.Unlock()
	return nil
}

func (s *mediaIndexStoreFake) ConfirmRequest(context.Context, uuid.UUID, uuid.UUID, time.Time) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.fileConfirmed {
		return false, nil
	}
	s.requestConfirmed = true
	s.request.State = RequestConfirmed
	return true, nil
}

type sweepStoreFake struct {
	ids       []uuid.UUID
	recovered int
}

func (s *sweepStoreFake) RecoverExpired(context.Context, time.Time, int) (int64, error) {
	s.recovered++
	return 1, nil
}
func (s *sweepStoreFake) ListDue(context.Context, time.Time, int) ([]uuid.UUID, error) {
	return s.ids, nil
}

type enqueuerFake struct{ ids []uuid.UUID }

func (e *enqueuerFake) Enqueue(_ context.Context, id uuid.UUID) error {
	e.ids = append(e.ids, id)
	return nil
}

func waitMediaIndexSignal(t *testing.T, signal <-chan struct{}, name string) {
	t.Helper()
	select {
	case <-signal:
	case <-time.After(time.Second):
		t.Fatalf("timed out waiting for %s", name)
	}
}

func waitMediaIndexError(t *testing.T, result <-chan error) error {
	t.Helper()
	select {
	case err := <-result:
		return err
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for Handle()")
		return nil
	}
}
