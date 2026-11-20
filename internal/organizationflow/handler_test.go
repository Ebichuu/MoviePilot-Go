package organizationflow

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"go.uber.org/goleak"

	"github.com/moviepilot-go/moviepilot-go/internal/database"
	"github.com/moviepilot-go/moviepilot-go/internal/jobs"
	"github.com/moviepilot-go/moviepilot-go/internal/mediaindex"
	"github.com/moviepilot-go/moviepilot-go/internal/organizer"
)

func TestHandlerCompletesClaimedBatch(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 8, 12, 15, 0, 0, 0, time.UTC)
	batchID := uuid.New()
	token := uuid.New()
	store := &executionStoreFake{
		batch: database.OrganizationBatch{
			ID: batchID, State: database.OrganizationBatchOrganizing,
			LeaseToken: token, SourceRoot: "/downloads",
			Plan: organizer.Plan{ID: "plan", TargetRoot: "/library"},
		},
		claimed: true,
		filePages: [][]database.OrganizationFile{{
			{
				ID: 1, BatchID: batchID, State: database.OrganizationFileOrganizing,
				SourcePath: "/downloads/a.mkv", TargetPath: "/library/a.mkv", Mode: organizer.Copy,
				ExpectedSize: 101, ExpectedSHA256: handlerTestSHA256,
			},
			{
				ID: 2, BatchID: batchID, State: database.OrganizationFileOrganizing,
				SourcePath: "/downloads/b.srt", TargetPath: "/library/b.srt", Mode: organizer.Copy,
				ExpectedSize: 202, ExpectedSHA256: handlerTestSHA256,
			},
		}, nil},
	}
	handler := mustExecutionHandler(t, store, now)
	handler.execute = func(_ context.Context, sourceRoot string, plan organizer.Plan) error {
		store.mu.Lock()
		store.executedSourceRoots = append(store.executedSourceRoots, sourceRoot)
		store.executed = append(store.executed, plan.Operations[0])
		store.mu.Unlock()
		return nil
	}

	if err := handler.HandleOrganization(t.Context(), jobs.OrganizationArgs{BatchID: batchID}); err != nil {
		t.Fatal(err)
	}
	store.mu.Lock()
	defer store.mu.Unlock()
	if len(store.executed) != 2 || len(store.completedFiles) != 2 || store.completedBatch != batchID {
		t.Fatalf("execution = %#v, completed files=%v batch=%s", store.executed, store.completedFiles, store.completedBatch)
	}
	if len(store.executedSourceRoots) != 2 ||
		store.executedSourceRoots[0] != "/downloads" ||
		store.executedSourceRoots[1] != "/downloads" {
		t.Fatalf("executor source roots = %v, want /downloads for every file", store.executedSourceRoots)
	}
	if store.executed[0].ExpectedSize != 101 || store.executed[0].ExpectedSHA256 != handlerTestSHA256 ||
		store.executed[1].ExpectedSize != 202 || store.executed[1].ExpectedSHA256 != handlerTestSHA256 {
		t.Fatalf("单文件计划未传递指纹: %#v", store.executed)
	}
	if !store.claimAt.Equal(now) {
		t.Fatalf("claim time = %s, want %s", store.claimAt, now)
	}
	if store.failBatchCalls != 0 {
		t.Fatalf("successful batch release calls = %d, want 0", store.failBatchCalls)
	}
}

func TestHandlerReturnsWithoutWorkWhenBatchIsNotDue(t *testing.T) {
	t.Parallel()
	store := &executionStoreFake{}
	handler := mustExecutionHandler(t, store, time.Now().UTC())
	handler.execute = func(context.Context, string, organizer.Plan) error {
		t.Fatal("execute called for unclaimed batch")
		return nil
	}
	if err := handler.HandleOrganization(t.Context(), jobs.OrganizationArgs{BatchID: uuid.New()}); err != nil {
		t.Fatal(err)
	}
}

func TestHandlerPersistsAndEnqueuesRequiredMediaIndexRequest(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 8, 12, 16, 0, 0, 0, time.UTC)
	batchID := uuid.New()
	requestID := uuid.New()
	token := uuid.New()
	serverID := uuid.New().String()
	plan := organizer.Plan{
		ID:         "plan",
		TargetRoot: "/library",
		Operations: []organizer.Operation{{
			Source: "/downloads/a.mkv", Target: "/library/a.mkv", Mode: organizer.Copy,
			ExpectedSize: 101, ExpectedSHA256: handlerTestSHA256,
		}},
	}
	completionPlan := mediaindex.CompletionPlan{
		RequiredServer: &mediaindex.ServerRef{
			ID: serverID, Name: "home", Type: "emby", Fingerprint: handlerTestServerFingerprint,
		},
		Files: []mediaindex.TargetFile{{
			LocalPath: "/library/a.mkv", RemotePath: "/media/a.mkv", PathStyle: "posix",
			CaseSensitive: true, ExpectedSize: 101, ExpectedSHA256: handlerTestSHA256, MediaType: "video",
		}},
	}
	store := &executionStoreFake{
		batch: database.OrganizationBatch{
			ID: batchID, State: database.OrganizationBatchOrganizing,
			LeaseToken: token, SourceRoot: "/downloads", Plan: plan,
		},
		claimed: true,
		filePages: [][]database.OrganizationFile{{{
			ID: 1, BatchID: batchID, State: database.OrganizationFileOrganizing,
			SourcePath: "/downloads/a.mkv", TargetPath: "/library/a.mkv", Mode: organizer.Copy,
			ExpectedSize: 101, ExpectedSHA256: handlerTestSHA256,
		}}, nil},
		mediaIndexRequestID: requestID,
	}
	planner := &mediaIndexCompletionPlannerFake{plan: completionPlan}
	enqueuer := &mediaIndexEnqueuerFake{}
	handler, err := NewHandler(store, &organizationEnqueuerFake{}, planner, enqueuer)
	if err != nil {
		t.Fatal(err)
	}
	handler.now = func() time.Time { return now }
	handler.execute = func(context.Context, string, organizer.Plan) error { return nil }

	if err := handler.HandleOrganization(t.Context(), jobs.OrganizationArgs{BatchID: batchID}); err != nil {
		t.Fatal(err)
	}
	store.mu.Lock()
	storedPlan := store.completionPlan.Clone()
	store.mu.Unlock()
	if planner.gotPlan.ID != plan.ID || !storedPlan.HasRequiredServer() || len(storedPlan.Files) != 1 {
		t.Fatalf("completion plan planner=%#v stored=%#v", planner.gotPlan, storedPlan)
	}
	enqueuer.mu.Lock()
	defer enqueuer.mu.Unlock()
	if len(enqueuer.ids) != 1 || enqueuer.ids[0] != requestID {
		t.Fatalf("media index enqueue IDs = %v, want [%s]", enqueuer.ids, requestID)
	}
}

func TestHandlerLeavesDurableMediaIndexRequestForSweepWhenEnqueueFails(t *testing.T) {
	t.Parallel()
	batchID := uuid.New()
	requestID := uuid.New()
	token := uuid.New()
	store := &executionStoreFake{
		batch: database.OrganizationBatch{
			ID: batchID, State: database.OrganizationBatchOrganizing,
			LeaseToken: token, SourceRoot: "/downloads", Plan: organizer.Plan{ID: "plan"},
		},
		claimed:             true,
		filePages:           [][]database.OrganizationFile{nil},
		mediaIndexRequestID: requestID,
	}
	planner := &mediaIndexCompletionPlannerFake{plan: mediaindex.CompletionPlan{
		RequiredServer: &mediaindex.ServerRef{
			ID: uuid.NewString(), Name: "home", Type: "emby", Fingerprint: handlerTestServerFingerprint,
		},
		Files: []mediaindex.TargetFile{{
			LocalPath: "/library/a.mkv", RemotePath: "/media/a.mkv", PathStyle: "posix",
			ExpectedSHA256: handlerTestSHA256, MediaType: "video",
		}},
	}}
	handler, err := NewHandler(
		store,
		&organizationEnqueuerFake{},
		planner,
		&mediaIndexEnqueuerFake{err: errors.New("river unavailable")},
	)
	if err != nil {
		t.Fatal(err)
	}
	handler.execute = func(context.Context, string, organizer.Plan) error { return nil }

	if err := handler.HandleOrganization(t.Context(), jobs.OrganizationArgs{BatchID: batchID}); err != nil {
		t.Fatalf("HandleOrganization() error = %v, want durable sweep recovery", err)
	}
	store.mu.Lock()
	defer store.mu.Unlock()
	if store.completedBatch != batchID {
		t.Fatalf("completed batch = %s, want %s", store.completedBatch, batchID)
	}
}

func TestHandlerPersistsStableMediaIndexPlanningFailure(t *testing.T) {
	t.Parallel()
	batchID := uuid.New()
	token := uuid.New()
	store := &executionStoreFake{
		batch: database.OrganizationBatch{
			ID: batchID, State: database.OrganizationBatchOrganizing,
			LeaseToken: token, SourceRoot: "/downloads", Plan: organizer.Plan{ID: "plan"},
		},
		claimed:   true,
		filePages: [][]database.OrganizationFile{nil},
	}
	handler, err := NewHandler(
		store,
		&organizationEnqueuerFake{},
		&mediaIndexCompletionPlannerFake{err: errors.New("secret host/path")},
		&mediaIndexEnqueuerFake{},
	)
	if err != nil {
		t.Fatal(err)
	}
	handler.execute = func(context.Context, string, organizer.Plan) error { return nil }

	err = handler.HandleOrganization(t.Context(), jobs.OrganizationArgs{BatchID: batchID})
	if !errors.Is(err, ErrOrganizationIndexPlanning) {
		t.Fatalf("HandleOrganization() error = %v, want %v", err, ErrOrganizationIndexPlanning)
	}
	store.mu.Lock()
	defer store.mu.Unlock()
	if store.failedBatchError != organizationStableIndexError {
		t.Fatalf("stable failure = %q, want %q", store.failedBatchError, organizationStableIndexError)
	}
}

func TestHandlerPersistsStableOperationFailure(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 8, 12, 15, 0, 0, 0, time.UTC)
	batchID := uuid.New()
	token := uuid.New()
	store := &executionStoreFake{
		batch: database.OrganizationBatch{
			ID: batchID, State: database.OrganizationBatchOrganizing,
			LeaseToken: token, SourceRoot: "/downloads",
			Plan: organizer.Plan{ID: "plan", TargetRoot: "/library"},
		},
		claimed: true,
		filePages: [][]database.OrganizationFile{{{
			ID: 1, BatchID: batchID, State: database.OrganizationFileOrganizing,
			SourcePath: "/private/source.mkv", TargetPath: "/library/source.mkv", Mode: organizer.Copy,
			ExpectedSize: 5, ExpectedSHA256: handlerTestSHA256,
		}}},
	}
	handler := mustExecutionHandler(t, store, now)
	handler.execute = func(context.Context, string, organizer.Plan) error {
		return errors.New("copy failed: /private/source.mkv token=secret")
	}

	err := handler.HandleOrganization(t.Context(), jobs.OrganizationArgs{BatchID: batchID})
	if !errors.Is(err, ErrOrganizationFileOperation) {
		t.Fatalf("HandleOrganization() error = %v, want %v", err, ErrOrganizationFileOperation)
	}
	store.mu.Lock()
	defer store.mu.Unlock()
	if store.failedFileError != organizationStableFileError || store.failedBatchError != organizationStableFileError {
		t.Fatalf("persisted errors = (%q, %q)", store.failedFileError, store.failedBatchError)
	}
	if !store.retryAt.Equal(now) {
		t.Fatalf("retry time = %s, want %s", store.retryAt, now)
	}
	if store.failBatchCalls != 1 {
		t.Fatalf("explicit failure batch calls = %d, want 1", store.failBatchCalls)
	}
}

func TestHandlerSanitizesStorageFailureAndIgnoresLeaseLoss(t *testing.T) {
	t.Parallel()
	sensitive := "postgres://private"
	tests := []struct {
		name     string
		claimErr error
		wantErr  error
	}{
		{name: "storage error", claimErr: errors.New(sensitive), wantErr: ErrOrganizationExecutionStorage},
		{name: "lease conflict", claimErr: database.ErrOrganizationLeaseConflict},
		{name: "missing batch", claimErr: database.ErrOrganizationBatchNotFound},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			handler := mustExecutionHandler(t, &executionStoreFake{claimErr: tt.claimErr}, time.Now().UTC())
			err := handler.HandleOrganization(t.Context(), jobs.OrganizationArgs{BatchID: uuid.New()})
			if !errors.Is(err, tt.wantErr) {
				t.Fatalf("HandleOrganization() error = %v, want %v", err, tt.wantErr)
			}
			if err != nil && err.Error() == sensitive {
				t.Fatal("storage error leaked dependency detail")
			}
		})
	}
}

func TestHandlerRenewsLeaseDuringSlowOperation(t *testing.T) {
	t.Parallel()
	batchID := uuid.New()
	token := uuid.New()
	renewed := make(chan struct{})
	store := &executionStoreFake{
		batch: database.OrganizationBatch{
			ID: batchID, State: database.OrganizationBatchOrganizing,
			LeaseToken: token, SourceRoot: "/downloads",
			Plan: organizer.Plan{ID: "plan", TargetRoot: "/library"},
		},
		claimed: true,
		filePages: [][]database.OrganizationFile{{{
			ID: 1, BatchID: batchID, State: database.OrganizationFileOrganizing,
			SourcePath: "/downloads/source.mkv", TargetPath: "/library/source.mkv", Mode: organizer.Copy,
			ExpectedSize: 5, ExpectedSHA256: handlerTestSHA256,
		}}, nil},
		renewed: renewed,
	}
	handler := mustExecutionHandler(t, store, time.Now().UTC())
	handler.leaseDuration = 100 * time.Millisecond
	handler.heartbeatInterval = time.Millisecond
	handler.execute = func(ctx context.Context, _ string, _ organizer.Plan) error {
		select {
		case <-renewed:
			return nil
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	if err := handler.HandleOrganization(t.Context(), jobs.OrganizationArgs{BatchID: batchID}); err != nil {
		t.Fatal(err)
	}
	store.mu.Lock()
	defer store.mu.Unlock()
	if store.renewCalls == 0 {
		t.Fatal("lease was not renewed")
	}
}

func TestHandlerReleasesClaimAfterCancellation(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 8, 12, 15, 30, 0, 0, time.UTC)
	batchID := uuid.New()
	token := uuid.New()
	store := &executionStoreFake{
		batch: database.OrganizationBatch{
			ID: batchID, State: database.OrganizationBatchOrganizing,
			LeaseToken: token, SourceRoot: "/downloads",
			Plan: organizer.Plan{ID: "plan", TargetRoot: "/library"},
		},
		claimed: true,
		filePages: [][]database.OrganizationFile{{{
			ID: 1, BatchID: batchID, State: database.OrganizationFileOrganizing,
			SourcePath: "/downloads/source.mkv", TargetPath: "/library/source.mkv", Mode: organizer.Copy,
			ExpectedSize: 5, ExpectedSHA256: handlerTestSHA256,
		}}},
	}
	handler := mustExecutionHandler(t, store, now)
	ctx, cancel := context.WithCancel(t.Context())
	handler.execute = func(workCtx context.Context, _ string, _ organizer.Plan) error {
		cancel()
		<-workCtx.Done()
		return workCtx.Err()
	}

	err := handler.HandleOrganization(ctx, jobs.OrganizationArgs{BatchID: batchID})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("HandleOrganization() error = %v, want context cancellation", err)
	}
	store.mu.Lock()
	defer store.mu.Unlock()
	if store.failBatchCalls != 1 {
		t.Fatalf("claim release calls = %d, want 1", store.failBatchCalls)
	}
	if len(store.failBatchContextErrors) != 1 || store.failBatchContextErrors[0] != nil {
		t.Fatalf("claim release context errors = %v, want [nil]", store.failBatchContextErrors)
	}
	if store.failedBatchError != organizationStableFileError {
		t.Fatalf("claim release error = %q, want stable redacted error", store.failedBatchError)
	}
}

func TestHandlerClassifiesHeartbeatRenewalFailureAndStopsHeartbeat(t *testing.T) {
	defer goleak.VerifyNone(t, goleak.IgnoreCurrent())
	const sensitive = "postgres://admin:secret@private/organization"
	batchID := uuid.New()
	token := uuid.New()
	renewed := make(chan struct{})
	store := &executionStoreFake{
		batch: database.OrganizationBatch{
			ID: batchID, State: database.OrganizationBatchOrganizing,
			LeaseToken: token, SourceRoot: "/downloads",
			Plan: organizer.Plan{ID: "plan", TargetRoot: "/library"},
		},
		claimed: true,
		filePages: [][]database.OrganizationFile{{{
			ID: 1, BatchID: batchID, State: database.OrganizationFileOrganizing,
			SourcePath: "/downloads/source.mkv", TargetPath: "/library/source.mkv", Mode: organizer.Copy,
			ExpectedSize: 5, ExpectedSHA256: handlerTestSHA256,
		}}},
		renewed:  renewed,
		renewErr: errors.New(sensitive),
	}
	handler := mustExecutionHandler(t, store, time.Now().UTC())
	handler.leaseDuration = 100 * time.Millisecond
	handler.heartbeatInterval = time.Millisecond
	handler.execute = func(workCtx context.Context, _ string, _ organizer.Plan) error {
		<-workCtx.Done()
		return workCtx.Err()
	}

	err := handler.HandleOrganization(t.Context(), jobs.OrganizationArgs{BatchID: batchID})
	if !errors.Is(err, ErrOrganizationExecutionStorage) {
		t.Fatalf("HandleOrganization() error = %v, want %v", err, ErrOrganizationExecutionStorage)
	}
	if strings.Contains(err.Error(), sensitive) {
		t.Fatalf("heartbeat error leaked dependency detail: %v", err)
	}
	store.mu.Lock()
	defer store.mu.Unlock()
	if store.renewCalls != 1 {
		t.Fatalf("renew calls after handler return = %d, want 1", store.renewCalls)
	}
	if store.failBatchCalls != 1 {
		t.Fatalf("heartbeat failure release calls = %d, want 1", store.failBatchCalls)
	}
	if len(store.failBatchContextErrors) != 1 || store.failBatchContextErrors[0] != nil {
		t.Fatalf("heartbeat release context errors = %v, want [nil]", store.failBatchContextErrors)
	}
}

func TestHandlerReleasesInvalidClaimedPage(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name      string
		claimSize int
		files     func(uuid.UUID) []database.OrganizationFile
	}{
		{
			name:      "page exceeds claim size",
			claimSize: 1,
			files: func(batchID uuid.UUID) []database.OrganizationFile {
				return []database.OrganizationFile{
					{ID: 1, BatchID: batchID, State: database.OrganizationFileOrganizing},
					{ID: 2, BatchID: batchID, State: database.OrganizationFileOrganizing},
				}
			},
		},
		{
			name:      "file belongs to another batch",
			claimSize: organizationFileClaimSize,
			files: func(uuid.UUID) []database.OrganizationFile {
				return []database.OrganizationFile{{ID: 1, BatchID: uuid.New(), State: database.OrganizationFileOrganizing}}
			},
		},
		{
			name:      "file id is invalid",
			claimSize: organizationFileClaimSize,
			files: func(batchID uuid.UUID) []database.OrganizationFile {
				return []database.OrganizationFile{{ID: 0, BatchID: batchID, State: database.OrganizationFileOrganizing}}
			},
		},
		{
			name:      "file state is invalid",
			claimSize: organizationFileClaimSize,
			files: func(batchID uuid.UUID) []database.OrganizationFile {
				return []database.OrganizationFile{{ID: 1, BatchID: batchID, State: database.OrganizationFilePlanned}}
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			batchID := uuid.New()
			store := &executionStoreFake{
				batch: database.OrganizationBatch{
					ID: batchID, State: database.OrganizationBatchOrganizing,
					LeaseToken: uuid.New(), SourceRoot: "/downloads",
					Plan: organizer.Plan{ID: "plan", TargetRoot: "/library"},
				},
				claimed:   true,
				filePages: [][]database.OrganizationFile{tt.files(batchID)},
			}
			handler := mustExecutionHandler(t, store, time.Now().UTC())
			handler.fileClaimSize = tt.claimSize
			handler.execute = func(context.Context, string, organizer.Plan) error {
				t.Fatal("executor called for invalid claimed page")
				return nil
			}

			err := handler.HandleOrganization(t.Context(), jobs.OrganizationArgs{BatchID: batchID})
			if !errors.Is(err, ErrOrganizationExecutionStorage) {
				t.Fatalf("HandleOrganization() error = %v, want %v", err, ErrOrganizationExecutionStorage)
			}
			store.mu.Lock()
			defer store.mu.Unlock()
			if store.failBatchCalls != 1 {
				t.Fatalf("invalid page release calls = %d, want 1", store.failBatchCalls)
			}
			if len(store.failBatchContextErrors) != 1 || store.failBatchContextErrors[0] != nil {
				t.Fatalf("invalid page release context errors = %v, want [nil]", store.failBatchContextErrors)
			}
		})
	}
}

func TestHandlerRejectsEmptyClaimedSourceRootAndReleasesLease(t *testing.T) {
	t.Parallel()
	batchID := uuid.New()
	store := &executionStoreFake{
		batch: database.OrganizationBatch{
			ID: batchID, State: database.OrganizationBatchOrganizing,
			LeaseToken: uuid.New(), SourceRoot: "",
			Plan: organizer.Plan{ID: "plan", TargetRoot: "/library"},
		},
		claimed: true,
	}
	handler := mustExecutionHandler(t, store, time.Now().UTC())
	handler.execute = func(context.Context, string, organizer.Plan) error {
		t.Fatal("executor called with empty source root")
		return nil
	}

	err := handler.HandleOrganization(t.Context(), jobs.OrganizationArgs{BatchID: batchID})
	if !errors.Is(err, ErrOrganizationExecutionStorage) {
		t.Fatalf("HandleOrganization() error = %v, want %v", err, ErrOrganizationExecutionStorage)
	}
	store.mu.Lock()
	defer store.mu.Unlock()
	if store.failBatchCalls != 1 {
		t.Fatalf("empty source root release calls = %d, want 1", store.failBatchCalls)
	}
}

func TestHandlerSweepRecoversAndEnqueuesDueBatches(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 8, 12, 16, 0, 0, 0, time.UTC)
	first := uuid.New()
	second := uuid.New()
	store := &executionStoreFake{dueIDs: []uuid.UUID{first, second}}
	enqueuer := &organizationEnqueuerFake{}
	handler, err := NewHandler(store, enqueuer, &mediaIndexCompletionPlannerFake{}, &mediaIndexEnqueuerFake{})
	if err != nil {
		t.Fatal(err)
	}
	handler.now = func() time.Time { return now }

	if err := handler.HandleOrganizationSweep(t.Context()); err != nil {
		t.Fatal(err)
	}
	store.mu.Lock()
	if !store.recoverAt.Equal(now) || !store.listDueAt.Equal(now) {
		store.mu.Unlock()
		t.Fatalf("sweep times = recover %s list %s, want %s", store.recoverAt, store.listDueAt, now)
	}
	store.mu.Unlock()
	enqueuer.mu.Lock()
	defer enqueuer.mu.Unlock()
	if len(enqueuer.ids) != 2 || enqueuer.ids[0] != first || enqueuer.ids[1] != second {
		t.Fatalf("enqueued ids = %v", enqueuer.ids)
	}
}

func TestHandlerSweepRejectsInvalidDuePageAndSanitizesEnqueueFailure(t *testing.T) {
	t.Parallel()
	sensitive := "river database secret"
	duplicate := uuid.MustParse("11111111-2222-4333-8444-555555555555")
	tests := []struct {
		name       string
		dueIDs     []uuid.UUID
		enqueueErr error
		wantErr    error
	}{
		{name: "nil id", dueIDs: []uuid.UUID{uuid.Nil}, wantErr: ErrOrganizationExecutionStorage},
		{name: "duplicate id", dueIDs: []uuid.UUID{duplicate, duplicate}, wantErr: ErrOrganizationExecutionStorage},
		{name: "enqueue failure", dueIDs: []uuid.UUID{uuid.New()}, enqueueErr: errors.New(sensitive), wantErr: ErrOrganizationExecutionStorage},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			handler, err := NewHandler(
				&executionStoreFake{dueIDs: tt.dueIDs},
				&organizationEnqueuerFake{err: tt.enqueueErr},
				&mediaIndexCompletionPlannerFake{},
				&mediaIndexEnqueuerFake{},
			)
			if err != nil {
				t.Fatal(err)
			}
			err = handler.HandleOrganizationSweep(t.Context())
			if !errors.Is(err, tt.wantErr) {
				t.Fatalf("HandleOrganizationSweep() error = %v, want %v", err, tt.wantErr)
			}
			if err != nil && err.Error() == sensitive {
				t.Fatal("sweep leaked enqueue failure")
			}
		})
	}
}

func mustExecutionHandler(t *testing.T, store ExecutionStore, now time.Time) *Handler {
	t.Helper()
	handler, err := NewHandler(
		store,
		&organizationEnqueuerFake{},
		&mediaIndexCompletionPlannerFake{},
		&mediaIndexEnqueuerFake{},
	)
	if err != nil {
		t.Fatal(err)
	}
	handler.now = func() time.Time { return now }
	return handler
}

const (
	handlerTestSHA256            = "2d711642b726b04401627ca9fbac32f5c8530fb1903cc4db02258717921a4881"
	handlerTestServerFingerprint = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
)

type executionStoreFake struct {
	mu sync.Mutex

	batch    database.OrganizationBatch
	claimed  bool
	claimErr error
	claimAt  time.Time

	filePages [][]database.OrganizationFile
	page      int

	executedSourceRoots    []string
	executed               []organizer.Operation
	completedFiles         []int64
	completedBatch         uuid.UUID
	failedFileError        string
	failedBatchError       string
	failBatchCalls         int
	failBatchContextErrors []error
	retryAt                time.Time
	renewCalls             int
	renewed                chan struct{}
	renewErr               error
	recoverAt              time.Time
	listDueAt              time.Time
	dueIDs                 []uuid.UUID
	completionPlan         mediaindex.CompletionPlan
	mediaIndexRequestID    uuid.UUID
}

func (s *executionStoreFake) ClaimOrganizationBatch(
	_ context.Context,
	_ uuid.UUID,
	now time.Time,
	_ time.Duration,
) (database.OrganizationBatch, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.claimAt = now
	return s.batch, s.claimed, s.claimErr
}

func (s *executionStoreFake) RenewOrganizationBatchLease(
	_ context.Context,
	_ uuid.UUID,
	_ uuid.UUID,
	_ time.Time,
	_ time.Duration,
) error {
	s.mu.Lock()
	s.renewCalls++
	if s.renewed != nil && s.renewCalls == 1 {
		close(s.renewed)
	}
	err := s.renewErr
	s.mu.Unlock()
	return err
}

func (s *executionStoreFake) ClaimOrganizationFiles(
	_ context.Context,
	_ uuid.UUID,
	_ uuid.UUID,
	_ int,
) ([]database.OrganizationFile, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.page >= len(s.filePages) {
		return nil, nil
	}
	page := append([]database.OrganizationFile(nil), s.filePages[s.page]...)
	s.page++
	return page, nil
}

func (s *executionStoreFake) CompleteOrganizationFile(_ context.Context, _ uuid.UUID, _ uuid.UUID, fileID int64) error {
	s.mu.Lock()
	s.completedFiles = append(s.completedFiles, fileID)
	s.mu.Unlock()
	return nil
}

func (s *executionStoreFake) FailOrganizationFile(_ context.Context, _ uuid.UUID, _ uuid.UUID, _ int64, stableErr string) error {
	s.mu.Lock()
	s.failedFileError = stableErr
	s.mu.Unlock()
	return nil
}

func (s *executionStoreFake) FailOrganizationBatch(
	ctx context.Context,
	_ uuid.UUID,
	_ uuid.UUID,
	retryAt time.Time,
	stableErr string,
) error {
	s.mu.Lock()
	s.failBatchCalls++
	s.failBatchContextErrors = append(s.failBatchContextErrors, ctx.Err())
	s.retryAt = retryAt
	s.failedBatchError = stableErr
	s.mu.Unlock()
	return nil
}

func (s *executionStoreFake) CompleteOrganizationBatchForIndexing(
	_ context.Context,
	batchID uuid.UUID,
	_ uuid.UUID,
	plan mediaindex.CompletionPlan,
) (uuid.UUID, bool, error) {
	s.mu.Lock()
	s.completedBatch = batchID
	s.completionPlan = plan.Clone()
	requestID := s.mediaIndexRequestID
	s.mu.Unlock()
	return requestID, requestID != uuid.Nil, nil
}

func (s *executionStoreFake) RecoverExpiredOrganizationLeases(_ context.Context, now time.Time, _ int) (int64, error) {
	s.mu.Lock()
	s.recoverAt = now
	s.mu.Unlock()
	return 0, nil
}

func (s *executionStoreFake) ListDueOrganizationBatchIDs(_ context.Context, now time.Time, _ int) ([]uuid.UUID, error) {
	s.mu.Lock()
	s.listDueAt = now
	ids := append([]uuid.UUID(nil), s.dueIDs...)
	s.mu.Unlock()
	return ids, nil
}

type organizationEnqueuerFake struct {
	mu  sync.Mutex
	ids []uuid.UUID
	err error
}

type mediaIndexCompletionPlannerFake struct {
	plan    mediaindex.CompletionPlan
	err     error
	gotPlan organizer.Plan
}

func (p *mediaIndexCompletionPlannerFake) MediaIndexCompletionPlan(
	_ context.Context,
	plan organizer.Plan,
) (mediaindex.CompletionPlan, error) {
	p.gotPlan = plan
	return p.plan.Clone(), p.err
}

type mediaIndexEnqueuerFake struct {
	mu  sync.Mutex
	ids []uuid.UUID
	err error
}

func (e *mediaIndexEnqueuerFake) EnqueueMediaIndex(_ context.Context, args jobs.MediaIndexArgs) error {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.ids = append(e.ids, args.RequestID)
	return e.err
}

func (e *organizationEnqueuerFake) EnqueueOrganization(_ context.Context, args jobs.OrganizationArgs) error {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.err != nil {
		return e.err
	}
	e.ids = append(e.ids, args.BatchID)
	return nil
}
