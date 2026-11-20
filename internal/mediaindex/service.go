package mediaindex

import (
	"context"
	"errors"
	"strings"
	"time"

	"github.com/google/uuid"
)

const (
	leaseDuration     = 2 * time.Minute
	leaseRenewEvery   = 30 * time.Second
	pageSize          = maxFilePage
	refreshWait       = 30 * time.Second
	noMatchWait       = 45 * time.Second
	serverBackoff     = 2 * time.Minute
	invalidConfigWait = 15 * time.Minute
	maxSweepPage      = 100

	stableAwaitingFiles           = "awaiting_remaining_files"
	stableNotVisible              = "not_visible_yet"
	stableAmbiguousMatch          = "ambiguous_match"
	stablePathMismatch            = "path_mismatch"
	stableProviderMismatch        = "provider_mismatch"
	stableLocalFileUnavailable    = "local_file_unavailable"
	stableLocalFingerprintChanged = "local_fingerprint_mismatch"
)

// Service performs exactly one bounded confirmation action per call.
type Service struct {
	leases     LeaseStore
	files      FileStore
	completion CompletionStore
	servers    Registry
	verifier   LocalVerifier
	now        func() time.Time
	renewEvery time.Duration
}

func NewService(
	leases LeaseStore,
	files FileStore,
	completion CompletionStore,
	servers Registry,
	verifier LocalVerifier,
) (*Service, error) {
	if leases == nil || files == nil || completion == nil || servers == nil || verifier == nil {
		return nil, ErrInvalidRequest
	}
	return &Service{
		leases: leases, files: files, completion: completion, servers: servers, verifier: verifier,
		now: time.Now, renewEvery: leaseRenewEvery,
	}, nil
}

// Handle executes one request. Delayed/normal upstream states are persisted
// and returned as nil so River's finite retry budget is not consumed.
func (s *Service) Handle(ctx context.Context, requestID uuid.UUID) error {
	if ctx == nil || s == nil || requestID == uuid.Nil || s.now == nil || s.renewEvery <= 0 {
		return ErrInvalidRequest
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	now := s.now().UTC()
	lease, claimed, err := s.leases.Claim(ctx, requestID, now, leaseDuration)
	if err != nil {
		return ErrPersistence
	}
	if !claimed {
		return nil
	}
	if !lease.Valid(requestID) {
		return ErrPersistence
	}
	return s.handleLease(ctx, lease, now)
}

func (s *Service) handleLease(parent context.Context, lease Lease, now time.Time) error {
	ctx, cancel := context.WithCancelCause(parent)
	heartbeatDone := make(chan error, 1)
	go s.keepLeaseAlive(ctx, cancel, lease, heartbeatDone)

	handleErr := s.handleClaimed(ctx, lease, now)
	cancel(nil)
	heartbeatErr := <-heartbeatDone
	if handleErr == nil {
		return nil
	}
	if heartbeatErr != nil {
		return heartbeatErr
	}
	if err := parent.Err(); err != nil {
		return err
	}
	return handleErr
}

func (s *Service) keepLeaseAlive(
	ctx context.Context,
	cancel context.CancelCauseFunc,
	lease Lease,
	done chan<- error,
) {
	ticker := time.NewTicker(s.renewEvery)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			done <- nil
			return
		case <-ticker.C:
			now := s.now().UTC()
			if err := s.leases.Renew(
				ctx,
				lease.Request.ID,
				lease.LeaseToken,
				now,
				now.Add(leaseDuration),
			); err != nil {
				if ctx.Err() != nil {
					done <- nil
					return
				}
				cancel(ErrLeaseLost)
				done <- ErrLeaseLost
				return
			}
		}
	}
}

func (s *Service) handleClaimed(ctx context.Context, lease Lease, now time.Time) error {
	server, ok := s.servers.Resolve(lease.Request.Server)
	if !ok || server == nil {
		return s.deferRequest(ctx, lease, now.Add(invalidConfigWait), "server_unavailable")
	}
	if lease.Request.RefreshRequestedAt == nil {
		return s.requestRefresh(ctx, lease, server, now)
	}
	return s.confirmPage(ctx, lease, server, now)
}

func (s *Service) requestRefresh(ctx context.Context, lease Lease, server Server, now time.Time) error {
	if err := server.RefreshAll(ctx); err != nil && !errors.Is(err, ErrOutcomeUnknown) {
		if contextErr := mediaIndexContextError(ctx, err); contextErr != nil {
			return contextErr
		}
		return s.deferRequest(ctx, lease, now.Add(serverBackoff), stableServerError(err))
	}
	if err := s.leases.RecordRefresh(
		ctx,
		lease.Request.ID,
		lease.LeaseToken,
		now,
		now.Add(refreshWait),
	); err != nil {
		return ErrPersistence
	}
	return nil
}

func (s *Service) confirmPage(ctx context.Context, lease Lease, server Server, now time.Time) error {
	requestID := lease.Request.ID
	files, err := s.files.ListPendingFiles(ctx, requestID, lease.LeaseToken, pageSize)
	if err != nil {
		return ErrPersistence
	}
	if len(files) > pageSize {
		return ErrPersistence
	}
	matchedAny := false
	lastMismatch := ""
	for _, file := range files {
		if err := ctx.Err(); err != nil {
			return err
		}
		if !file.Valid(requestID) {
			return ErrPersistence
		}
		verified, verifyErr := s.verifier.Verify(
			ctx,
			file.LocalPath,
			file.ExpectedSize,
			file.ExpectedSHA256,
		)
		if verifyErr != nil {
			if contextErr := mediaIndexContextError(ctx, verifyErr); contextErr != nil {
				return contextErr
			}
			return s.deferRequest(ctx, lease, now.Add(serverBackoff), stableLocalFileUnavailable)
		}
		if !verified {
			return s.deferRequest(ctx, lease, now.Add(invalidConfigWait), stableLocalFingerprintChanged)
		}
		items, findErr := server.FindItems(ctx, Query{Path: file.RemotePath, ProviderID: file.ProviderID})
		if findErr != nil {
			if contextErr := mediaIndexContextError(ctx, findErr); contextErr != nil {
				return contextErr
			}
			return s.deferRequest(ctx, lease, now.Add(serverBackoff), stableServerError(findErr))
		}
		itemID, mismatch := uniquePathMatch(items, file)
		if mismatch != "" {
			lastMismatch = preferMatchMismatch(lastMismatch, mismatch)
			continue
		}
		if err := s.files.ConfirmFile(ctx, requestID, lease.LeaseToken, file.ID, itemID); err != nil {
			return ErrPersistence
		}
		matchedAny = true
	}

	confirmed, err := s.completion.ConfirmRequest(ctx, requestID, lease.LeaseToken, now)
	if err != nil {
		return ErrPersistence
	}
	if confirmed {
		return nil
	}
	if lastMismatch != "" && lastMismatch != stableNotVisible {
		return s.deferRequest(ctx, lease, now.Add(noMatchWait), lastMismatch)
	}
	if matchedAny {
		return s.deferRequest(ctx, lease, now.Add(refreshWait), stableAwaitingFiles)
	}
	return s.deferRequest(ctx, lease, now.Add(noMatchWait), stableNotVisible)
}

func (s *Service) deferRequest(ctx context.Context, lease Lease, next time.Time, stableErr string) error {
	err := s.leases.Reschedule(
		ctx,
		lease.Request.ID,
		lease.LeaseToken,
		next.UTC(),
		trimStableError(stableErr),
	)
	if err != nil {
		return ErrPersistence
	}
	return nil
}

func uniquePathMatch(items []Item, file File) (string, string) {
	if len(items) == 0 {
		return "", stableNotVisible
	}
	if len(items) > 1 {
		return "", stableAmbiguousMatch
	}
	item := items[0]
	if strings.TrimSpace(item.ID) == "" {
		return "", stableNotVisible
	}
	if !EqualRemotePath(item.Path, file.RemotePath, file.PathStyle, file.CaseSensitive) {
		return "", stablePathMismatch
	}
	if !providerIDMatches(item.ProviderIDs, file.ProviderID) {
		return "", stableProviderMismatch
	}
	return item.ID, ""
}

func preferMatchMismatch(current, candidate string) string {
	if matchMismatchPriority(candidate) > matchMismatchPriority(current) {
		return candidate
	}
	return current
}

func matchMismatchPriority(value string) int {
	switch value {
	case stableAmbiguousMatch:
		return 4
	case stablePathMismatch:
		return 3
	case stableProviderMismatch:
		return 2
	case stableNotVisible:
		return 1
	default:
		return 0
	}
}

func providerIDMatches(actual map[string]string, expected string) bool {
	expected = strings.TrimSpace(expected)
	if expected == "" {
		return true
	}
	provider, value, found := strings.Cut(expected, ".")
	provider = strings.TrimSpace(provider)
	value = strings.TrimSpace(value)
	if !found || provider == "" || value == "" {
		return false
	}
	for key, candidate := range actual {
		if strings.EqualFold(strings.TrimSpace(key), provider) && strings.TrimSpace(candidate) == value {
			return true
		}
	}
	return false
}

func stableServerError(err error) string {
	if errors.Is(err, ErrOutcomeUnknown) {
		return "outcome_unknown"
	}
	if errors.Is(err, ErrAmbiguousMatch) {
		return stableAmbiguousMatch
	}
	if errors.Is(err, ErrPathMismatch) {
		return stablePathMismatch
	}
	if errors.Is(err, ErrProviderMismatch) {
		return stableProviderMismatch
	}
	return "server_unavailable"
}

func trimStableError(value string) string {
	value = strings.TrimSpace(value)
	if len(value) > 128 {
		return value[:128]
	}
	return value
}

func mediaIndexContextError(ctx context.Context, err error) error {
	if ctx != nil {
		if contextErr := ctx.Err(); contextErr != nil {
			return contextErr
		}
	}
	if errors.Is(err, context.Canceled) {
		return context.Canceled
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return context.DeadlineExceeded
	}
	return nil
}

// SweepHandler recovers leases and enqueues only a bounded page of due IDs.
type SweepHandler struct {
	store    SweepStore
	enqueuer Enqueuer
	now      func() time.Time
}

func NewSweepHandler(store SweepStore, enqueuer Enqueuer) (*SweepHandler, error) {
	if store == nil || enqueuer == nil {
		return nil, ErrInvalidRequest
	}
	return &SweepHandler{store: store, enqueuer: enqueuer, now: time.Now}, nil
}

func (h *SweepHandler) Handle(ctx context.Context) error {
	if ctx == nil || h == nil || h.now == nil {
		return ErrInvalidRequest
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	now := h.now().UTC()
	if _, err := h.store.RecoverExpired(ctx, now, maxSweepPage); err != nil {
		return ErrPersistence
	}
	ids, err := h.store.ListDue(ctx, now, maxSweepPage)
	if err != nil {
		return ErrPersistence
	}
	if len(ids) > maxSweepPage {
		return ErrPersistence
	}
	for _, id := range ids {
		if id == uuid.Nil {
			return ErrPersistence
		}
		if err := h.enqueuer.Enqueue(ctx, id); err != nil {
			return ErrPersistence
		}
	}
	return nil
}
