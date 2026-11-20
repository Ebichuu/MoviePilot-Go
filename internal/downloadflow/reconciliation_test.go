package downloadflow

import (
	"context"
	"errors"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/moviepilot-go/moviepilot-go/internal/database"
	"github.com/moviepilot-go/moviepilot-go/internal/downloader"
	"github.com/moviepilot-go/moviepilot-go/internal/organization"
	"github.com/moviepilot-go/moviepilot-go/internal/subscription"
)

func TestReconciliationHandlerAdvancesOnlyConfirmedStates(t *testing.T) {
	t.Parallel()

	cutoff := time.Date(2026, 8, 11, 12, 0, 0, 0, time.UTC)
	downloaderID := "9b21ac72-198d-44a0-a766-39196b27d5c8"
	targets := []downloader.ReconciliationTarget{
		reconciliationTarget(1, downloaderID, downloader.IntentSubmitting, cutoff.Add(-7*time.Minute), "1"),
		reconciliationTarget(2, downloaderID, downloader.IntentSubmitting, cutoff.Add(-6*time.Minute), "2"),
		reconciliationTarget(3, downloaderID, downloader.IntentDownloading, cutoff.Add(-5*time.Minute), "3"),
		reconciliationTarget(4, downloaderID, downloader.IntentDownloading, cutoff.Add(-4*time.Minute), "4"),
		reconciliationTarget(5, downloaderID, downloader.IntentDownloading, cutoff.Add(-3*time.Minute), "5"),
		reconciliationTarget(6, downloaderID, downloader.IntentDownloading, cutoff.Add(-2*time.Minute), "6"),
		reconciliationTarget(7, downloaderID, downloader.IntentDownloading, cutoff.Add(-time.Minute), "7"),
	}
	observer := &reconciliationTestSubmitter{states: map[string]downloader.TaskObservationState{
		targets[0].TorrentHash: downloader.TaskObservationActive,
		targets[1].TorrentHash: downloader.TaskObservationCompleted,
		targets[2].TorrentHash: downloader.TaskObservationCompleted,
		targets[3].TorrentHash: downloader.TaskObservationActive,
		targets[4].TorrentHash: downloader.TaskObservationMissing,
		targets[5].TorrentHash: downloader.TaskObservationError,
		targets[6].TorrentHash: downloader.TaskObservationUnknown,
	}}
	store := &reconciliationTestStore{pages: [][]downloader.ReconciliationTarget{targets}}
	if _, supported := any(store).(DownloadAcceptanceStore); supported {
		t.Fatal("legacy reconciliation fake unexpectedly implements download acceptance")
	}
	handler := newReconciliationTestHandler(t, store, map[string]TorrentSubmitter{downloaderID: observer})
	if err := handler.HandleDownloadReconciliation(t.Context(), cutoff); err != nil {
		t.Fatal(err)
	}

	got := store.transitionSnapshot()
	slices.Sort(got)
	want := []string{
		targets[0].ID.String() + ":submitting->downloading",
		targets[1].ID.String() + ":downloading->completed",
		targets[1].ID.String() + ":submitting->downloading",
		targets[2].ID.String() + ":downloading->completed",
	}
	slices.Sort(want)
	if !slices.Equal(got, want) {
		t.Fatalf("CAS transitions = %#v, want %#v", got, want)
	}
	if observer.calls != 1 || len(observer.batches) != 1 || len(observer.batches[0]) != len(targets) {
		t.Fatalf("observer calls/batches = %d/%#v", observer.calls, observer.batches)
	}
}

func TestReconciliationHandlerBoundsBatchesAndPaginates(t *testing.T) {
	t.Parallel()

	cutoff := time.Date(2026, 8, 11, 13, 0, 0, 0, time.UTC)
	firstDownloader := "64601015-c428-4e18-a8be-b05a416f59c5"
	secondDownloader := "c3d1c497-a450-46cc-8637-dbad844673f1"
	targets := []downloader.ReconciliationTarget{
		reconciliationTarget(10, firstDownloader, downloader.IntentDownloading, cutoff.Add(-5*time.Minute), "a"),
		reconciliationTarget(11, firstDownloader, downloader.IntentDownloading, cutoff.Add(-4*time.Minute), "b"),
		reconciliationTarget(12, firstDownloader, downloader.IntentDownloading, cutoff.Add(-3*time.Minute), "c"),
		reconciliationTarget(13, secondDownloader, downloader.IntentDownloading, cutoff.Add(-2*time.Minute), "d"),
		reconciliationTarget(14, secondDownloader, downloader.IntentDownloading, cutoff.Add(-time.Minute), "e"),
	}
	first := &reconciliationTestSubmitter{}
	second := &reconciliationTestSubmitter{}
	store := &reconciliationTestStore{pages: [][]downloader.ReconciliationTarget{
		targets[:2], targets[2:4], targets[4:],
	}}
	handler := newReconciliationTestHandler(t, store, map[string]TorrentSubmitter{
		firstDownloader: first, secondDownloader: second,
	})
	handler.pageSize = 2
	handler.observeBatch = 2
	if err := handler.HandleDownloadReconciliation(t.Context(), cutoff); err != nil {
		t.Fatal(err)
	}
	if store.listCalls != 3 {
		t.Fatalf("page calls = %d, want 3", store.listCalls)
	}
	if first.calls != 2 || second.calls != 2 {
		t.Fatalf("observer calls = %d/%d, want 2/2", first.calls, second.calls)
	}
	for _, observer := range []*reconciliationTestSubmitter{first, second} {
		for _, batch := range observer.batches {
			if len(batch) > 2 {
				t.Fatalf("batch exceeded bound: %#v", batch)
			}
		}
	}
	if !store.cutoffsStable(cutoff) || !store.cursorsProgress(targets[1], targets[3]) {
		t.Fatalf("page calls = %#v", store.listRequests)
	}
}

func TestReconciliationHandlerIsolatesDownloaderFailures(t *testing.T) {
	t.Parallel()

	cutoff := time.Now().UTC()
	missingDownloader := "32e25af1-a0bd-47d8-b1f8-9822a547a7bb"
	failingDownloader := "65d13f57-5504-4f0b-9198-13290da9d9ea"
	healthyDownloader := "c3c6c302-71e2-46d9-99b3-59348fab0bcd"
	targets := []downloader.ReconciliationTarget{
		reconciliationTarget(20, missingDownloader, downloader.IntentDownloading, cutoff.Add(-3*time.Minute), "1"),
		reconciliationTarget(21, failingDownloader, downloader.IntentDownloading, cutoff.Add(-2*time.Minute), "2"),
		reconciliationTarget(22, healthyDownloader, downloader.IntentDownloading, cutoff.Add(-time.Minute), "3"),
	}
	failing := &reconciliationTestSubmitter{err: errors.New("upstream-sensitive-marker")}
	healthy := &reconciliationTestSubmitter{states: map[string]downloader.TaskObservationState{
		targets[2].TorrentHash: downloader.TaskObservationCompleted,
	}}
	store := &reconciliationTestStore{pages: [][]downloader.ReconciliationTarget{targets}}
	handler := newReconciliationTestHandler(t, store, map[string]TorrentSubmitter{
		failingDownloader: failing,
		healthyDownloader: healthy,
	})
	if err := handler.HandleDownloadReconciliation(t.Context(), cutoff); err != nil {
		t.Fatalf("isolated downloader failure returned %v", err)
	}
	transitions := store.transitionSnapshot()
	if len(transitions) != 1 || !strings.HasPrefix(transitions[0], targets[2].ID.String()+":") {
		t.Fatalf("healthy downloader was not reconciled: %#v", transitions)
	}
}

func TestReconciliationHandlerRejectsInvalidPagesAndSanitizesStorageErrors(t *testing.T) {
	t.Parallel()

	cutoff := time.Now().UTC()
	downloaderID := "852a32c3-1964-4cd5-99ce-ff974b02c3ec"
	target := reconciliationTarget(30, downloaderID, downloader.IntentDownloading, cutoff.Add(-time.Minute), "1")
	tests := []struct {
		name      string
		store     *reconciliationTestStore
		wantErr   error
		sensitive string
	}{
		{name: "查询错误脱敏", store: &reconciliationTestStore{listErr: errors.New("database-sensitive-marker")}, wantErr: ErrDownloadReconciliationUnavailable, sensitive: "database-sensitive-marker"},
		{name: "超过 cutoff", store: &reconciliationTestStore{pages: [][]downloader.ReconciliationTarget{{func() downloader.ReconciliationTarget {
			invalid := target
			invalid.UpdatedAt = cutoff.Add(time.Second)
			return invalid
		}()}}}, wantErr: ErrInvalidDownloadReconciliationPage},
		{name: "游标不前进", store: &reconciliationTestStore{pages: [][]downloader.ReconciliationTarget{{target, target}}}, wantErr: ErrInvalidDownloadReconciliationPage},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			handler := newReconciliationTestHandler(t, tt.store, map[string]TorrentSubmitter{
				downloaderID: &reconciliationTestSubmitter{},
			})
			err := handler.HandleDownloadReconciliation(t.Context(), cutoff)
			if !errors.Is(err, tt.wantErr) {
				t.Fatalf("HandleDownloadReconciliation() error = %v, want %v", err, tt.wantErr)
			}
			if err != nil && tt.sensitive != "" && strings.Contains(err.Error(), tt.sensitive) {
				t.Fatal("对账错误泄露存储连接信息")
			}
		})
	}
}

func TestReconciliationHandlerIgnoresCASConflicts(t *testing.T) {
	t.Parallel()

	cutoff := time.Now().UTC()
	downloaderID := "03158472-b848-4211-b72a-b3ca83d7b8ae"
	target := reconciliationTarget(40, downloaderID, downloader.IntentDownloading, cutoff.Add(-time.Minute), "1")
	store := &reconciliationTestStore{
		pages:  [][]downloader.ReconciliationTarget{{target}},
		casErr: database.ErrDownloadIntentStateConflict,
	}
	handler := newReconciliationTestHandler(t, store, map[string]TorrentSubmitter{
		downloaderID: &reconciliationTestSubmitter{states: map[string]downloader.TaskObservationState{
			target.TorrentHash: downloader.TaskObservationCompleted,
		}},
	})
	if err := handler.HandleDownloadReconciliation(t.Context(), cutoff); err != nil {
		t.Fatalf("CAS conflict should be benign: %v", err)
	}
}

func TestReconciliationHandlerRepairsCompletedIntentEpisodesWithoutDownloader(t *testing.T) {
	t.Parallel()
	cutoff := time.Now().UTC()
	downloaderID := "03158472-b848-4211-b72a-b3ca83d7b8ae"
	target := reconciliationTarget(41, downloaderID, downloader.IntentCompleted, cutoff.Add(-time.Minute), "2")
	store := &reconciliationTestStore{pages: [][]downloader.ReconciliationTarget{{target}}}
	observer := &reconciliationTestSubmitter{}
	handler := newReconciliationTestHandler(t, store, map[string]TorrentSubmitter{downloaderID: observer})

	if err := handler.HandleDownloadReconciliation(t.Context(), cutoff); err != nil {
		t.Fatal(err)
	}
	if observer.calls != 0 {
		t.Fatalf("completed recovery queried downloader %d times", observer.calls)
	}
	store.mu.Lock()
	got := append([]string(nil), store.episodeTransitions...)
	gotParent := append([]string(nil), store.parentTransitions...)
	store.mu.Unlock()
	wantEpisodes := []string{
		target.ID.String() + ":acquired->downloading",
		target.ID.String() + ":downloading->pending_organization",
	}
	if !slices.Equal(got, wantEpisodes) {
		t.Fatalf("episode transitions = %v, want %v", got, wantEpisodes)
	}
	wantParent := []string{"acquired->downloading", "downloading->pending_organization"}
	if !slices.Equal(gotParent, wantParent) {
		t.Fatalf("parent transitions = %v, want %v", gotParent, wantParent)
	}
}

func TestReconciliationHandlerDoesNotAdvanceParentAfterEpisodeStateConflict(t *testing.T) {
	t.Parallel()

	cutoff := time.Now().UTC()
	const downloaderID = "03158472-b848-4211-b72a-b3ca83d7b8ae"
	target := reconciliationTarget(42, downloaderID, downloader.IntentCompleted, cutoff.Add(-time.Minute), "3")
	store := &reconciliationTestStore{
		pages:      [][]downloader.ReconciliationTarget{{target}},
		episodeErr: database.ErrSubscriptionEpisodeStateConflict,
	}
	handler := newReconciliationTestHandler(t, store, map[string]TorrentSubmitter{
		downloaderID: &reconciliationTestSubmitter{},
	})

	err := handler.HandleDownloadReconciliation(t.Context(), cutoff)
	if err != nil {
		t.Fatalf("单目标失败被隔离，轮次不应失败: %v", err)
	}
	failure := requireReconciliationFailure(t, handler, target.ID, reconcileStageEpisodeSync)
	if !errors.Is(failure.Err, ErrDownloadReconciliationUnavailable) {
		t.Fatalf("失败原因 = %v, want ErrDownloadReconciliationUnavailable", failure.Err)
	}
	store.mu.Lock()
	parentTransitions := append([]string(nil), store.parentTransitions...)
	store.mu.Unlock()
	if len(parentTransitions) != 0 {
		t.Fatalf("episode state conflict advanced parent: %v", parentTransitions)
	}
}

func TestReconciliationHandlerAdvancesMovieSubscriptionWithoutEpisodes(t *testing.T) {
	t.Parallel()

	cutoff := time.Now().UTC()
	const downloaderID = "03158472-b848-4211-b72a-b3ca83d7b8ae"
	tests := []struct {
		name          string
		state         downloader.IntentState
		observedState downloader.TaskObservationState
		wantParent    subscription.State
	}{
		{
			name:          "active download",
			state:         downloader.IntentSubmitting,
			observedState: downloader.TaskObservationActive,
			wantParent:    subscription.Downloading,
		},
		{
			name:       "completed replay",
			state:      downloader.IntentCompleted,
			wantParent: subscription.PendingOrganization,
		},
	}

	for index, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			zeroRows := int64(0)
			target := reconciliationTarget(byte(50+index), downloaderID, tt.state, cutoff.Add(-time.Minute), "7")
			observer := &reconciliationTestSubmitter{states: map[string]downloader.TaskObservationState{
				target.TorrentHash: tt.observedState,
			}}
			store := &reconciliationTestStore{
				pages:          [][]downloader.ReconciliationTarget{{target}},
				episodeRows:    &zeroRows,
				parentState:    subscription.Acquired,
				parentStateSet: true,
			}
			handler := newReconciliationTestHandler(t, store, map[string]TorrentSubmitter{
				downloaderID: observer,
			})

			if err := handler.HandleDownloadReconciliation(t.Context(), cutoff); err != nil {
				t.Fatal(err)
			}
			if got := store.parentSubscriptionState(); got != tt.wantParent {
				t.Fatalf("parent subscription state = %s, want %s", got, tt.wantParent)
			}
		})
	}
}

func TestReconciliationHandlerInvokesCompletionHookForTransitionAndReplay(t *testing.T) {
	t.Parallel()
	cutoff := time.Now().UTC()
	downloaderID := "03158472-b848-4211-b72a-b3ca83d7b8ae"

	tests := []struct {
		name          string
		state         downloader.IntentState
		observerState downloader.TaskObservationState
		wantObserved  int
	}{
		{name: "newly confirmed completion", state: downloader.IntentDownloading, observerState: downloader.TaskObservationCompleted, wantObserved: 1},
		{name: "completed recovery replay", state: downloader.IntentCompleted, wantObserved: 0},
	}
	for index, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			target := reconciliationTarget(byte(60+index), downloaderID, tt.state, cutoff.Add(-time.Minute), "8")
			observer := &reconciliationTestSubmitter{states: map[string]downloader.TaskObservationState{
				target.TorrentHash: tt.observerState,
			}}
			store := &reconciliationTestStore{pages: [][]downloader.ReconciliationTarget{{target}}}
			hook := &reconciliationCompletionHookFake{}
			handler := newReconciliationTestHandler(
				t,
				store,
				map[string]TorrentSubmitter{downloaderID: observer},
				hook,
			)

			if err := handler.HandleDownloadReconciliation(t.Context(), cutoff); err != nil {
				t.Fatal(err)
			}
			if observer.calls != tt.wantObserved {
				t.Fatalf("observer calls = %d, want %d", observer.calls, tt.wantObserved)
			}
			calls := hook.snapshot()
			if len(calls) != 1 || calls[0].ID != target.ID || calls[0].State != downloader.IntentCompleted {
				t.Fatalf("completion hook calls = %#v", calls)
			}
		})
	}
}

func TestReconciliationHandlerSanitizesCompletionHookFailure(t *testing.T) {
	t.Parallel()
	const sensitiveMarker = "organization-sensitive-marker"
	cutoff := time.Now().UTC()
	downloaderID := "03158472-b848-4211-b72a-b3ca83d7b8ae"
	target := reconciliationTarget(70, downloaderID, downloader.IntentCompleted, cutoff.Add(-time.Minute), "9")
	store := &reconciliationTestStore{pages: [][]downloader.ReconciliationTarget{{target}}}
	hook := &reconciliationCompletionHookFake{err: errors.New(sensitiveMarker)}
	handler := newReconciliationTestHandler(
		t,
		store,
		map[string]TorrentSubmitter{downloaderID: &reconciliationTestSubmitter{}},
		hook,
	)

	err := handler.HandleDownloadReconciliation(t.Context(), cutoff)
	if err != nil {
		t.Fatalf("单目标失败被隔离，轮次不应失败: %v", err)
	}
	failure := requireReconciliationFailure(t, handler, target.ID, reconcileStageCompletionHook)
	if strings.Contains(failure.Err.Error(), sensitiveMarker) {
		t.Fatalf("completion hook error leaked into report: %v", failure.Err)
	}
	if failure.Cause == nil || !strings.Contains(failure.Cause.Error(), sensitiveMarker) {
		t.Fatalf("底层原因应保留给日志: %+v", failure)
	}
}

func TestReconciliationHandlerContinuesSweepAfterCompletionHookFailure(t *testing.T) {
	t.Parallel()
	const sensitiveMarker = "organization-sensitive-marker"
	cutoff := time.Date(2026, 8, 15, 9, 0, 0, 0, time.UTC)
	const downloaderID = "03158472-b848-4211-b72a-b3ca83d7b8ae"
	targets := []downloader.ReconciliationTarget{
		reconciliationTarget(71, downloaderID, downloader.IntentCompleted, cutoff.Add(-3*time.Minute), "a"),
		reconciliationTarget(72, downloaderID, downloader.IntentCompleted, cutoff.Add(-2*time.Minute), "b"),
		reconciliationTarget(73, downloaderID, downloader.IntentCompleted, cutoff.Add(-time.Minute), "c"),
	}
	store := &reconciliationTestStore{pages: [][]downloader.ReconciliationTarget{
		targets[:2], targets[2:],
	}}
	hook := &reconciliationCompletionHookFake{errs: map[uuid.UUID]error{
		targets[0].ID: errors.New(sensitiveMarker),
	}}
	handler := newReconciliationTestHandler(
		t,
		store,
		map[string]TorrentSubmitter{downloaderID: &reconciliationTestSubmitter{}},
		hook,
	)
	handler.pageSize = 2

	err := handler.HandleDownloadReconciliation(t.Context(), cutoff)
	if err != nil {
		t.Fatalf("HandleDownloadReconciliation() error = %v, want isolated target failure", err)
	}
	failure := requireReconciliationFailure(t, handler, targets[0].ID, reconcileStageCompletionHook)
	if strings.Contains(failure.Err.Error(), sensitiveMarker) {
		t.Fatalf("completion hook error leaked: %v", failure.Err)
	}
	if report := handler.LastSweepReport(); report.Scanned != len(targets) || report.Succeeded != len(targets)-1 {
		t.Fatalf("sweep report = %+v, want %d scanned / %d succeeded", report, len(targets), len(targets)-1)
	}
	calls := hook.snapshot()
	if len(calls) != len(targets) {
		t.Fatalf("completion hook calls = %#v, want all targets", calls)
	}
	for index, target := range targets {
		if calls[index].ID != target.ID {
			t.Fatalf("completion hook call %d = %s, want %s", index, calls[index].ID, target.ID)
		}
	}
	store.mu.Lock()
	requests := append([]reconciliationListRequest(nil), store.listRequests...)
	store.mu.Unlock()
	if len(requests) != 2 || !requests[1].afterTime.Equal(targets[1].UpdatedAt) ||
		requests[1].afterID != targets[1].ID {
		t.Fatalf("page requests = %#v, want cursor after first full page", requests)
	}
}

func TestReconciliationHandlerStopsOnCompletionHookContextError(t *testing.T) {
	t.Parallel()
	cutoff := time.Date(2026, 8, 15, 10, 0, 0, 0, time.UTC)
	const downloaderID = "03158472-b848-4211-b72a-b3ca83d7b8ae"

	tests := []struct {
		name string
		err  error
	}{
		{name: "canceled", err: context.Canceled},
		{name: "deadline exceeded", err: context.DeadlineExceeded},
	}
	for index, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			targets := []downloader.ReconciliationTarget{
				reconciliationTarget(byte(74+index*2), downloaderID, downloader.IntentCompleted, cutoff.Add(-2*time.Minute), "d"),
				reconciliationTarget(byte(75+index*2), downloaderID, downloader.IntentCompleted, cutoff.Add(-time.Minute), "e"),
			}
			store := &reconciliationTestStore{pages: [][]downloader.ReconciliationTarget{targets}}
			hook := &reconciliationCompletionHookFake{errs: map[uuid.UUID]error{targets[0].ID: tt.err}}
			handler := newReconciliationTestHandler(
				t,
				store,
				map[string]TorrentSubmitter{downloaderID: &reconciliationTestSubmitter{}},
				hook,
			)

			err := handler.HandleDownloadReconciliation(t.Context(), cutoff)
			if !errors.Is(err, tt.err) {
				t.Fatalf("HandleDownloadReconciliation() error = %v, want %v", err, tt.err)
			}
			if calls := hook.snapshot(); len(calls) != 1 || calls[0].ID != targets[0].ID {
				t.Fatalf("completion hook calls after context error = %#v", calls)
			}
		})
	}
}

func TestReconciliationHandlerUsesDownloadAcceptanceCapability(t *testing.T) {
	t.Parallel()

	cutoff := time.Date(2026, 8, 15, 8, 0, 0, 0, time.UTC)
	const downloaderID = "03158472-b848-4211-b72a-b3ca83d7b8ae"
	tests := []struct {
		name          string
		state         downloader.IntentState
		observedState downloader.TaskObservationState
		consumed      bool
		wantAccepts   int
		wantObserved  int
		wantCompleted bool
	}{
		{
			name:          "submitting active is accepted without legacy CAS",
			state:         downloader.IntentSubmitting,
			observedState: downloader.TaskObservationActive,
			wantAccepts:   1,
			wantObserved:  1,
		},
		{
			name:          "submitting completed accepts before completion",
			state:         downloader.IntentSubmitting,
			observedState: downloader.TaskObservationCompleted,
			wantAccepts:   1,
			wantObserved:  1,
			wantCompleted: true,
		},
		{
			name:          "downloading active replay compensates acceptance",
			state:         downloader.IntentDownloading,
			observedState: downloader.TaskObservationActive,
			wantAccepts:   1,
			wantObserved:  1,
		},
		{
			name:          "downloading paused replay compensates acceptance",
			state:         downloader.IntentDownloading,
			observedState: downloader.TaskObservationPaused,
			wantAccepts:   1,
			wantObserved:  1,
		},
		{
			name:          "downloading missing replay compensates acceptance",
			state:         downloader.IntentDownloading,
			observedState: downloader.TaskObservationMissing,
			wantAccepts:   1,
			wantObserved:  1,
		},
		{
			name:          "downloading completed replay compensates then completes",
			state:         downloader.IntentDownloading,
			observedState: downloader.TaskObservationCompleted,
			wantAccepts:   1,
			wantObserved:  1,
			wantCompleted: true,
		},
		{
			name:          "completed replay plans without second consumption",
			state:         downloader.IntentCompleted,
			consumed:      true,
			wantCompleted: true,
		},
	}
	hashDigits := []string{"a", "b", "c", "d", "e", "f", "1"}

	for index, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			target := reconciliationTarget(
				byte(80+index), downloaderID, tt.state, cutoff.Add(-time.Minute), hashDigits[index],
			)
			baseStore := &reconciliationTestStore{pages: [][]downloader.ReconciliationTarget{{target}}}
			store := &reconciliationAcceptanceStore{
				reconciliationTestStore: baseStore,
				consumed:                make(map[uuid.UUID]bool),
			}
			if tt.consumed {
				store.consumed[target.ID] = true
			}
			observer := &reconciliationTestSubmitter{states: map[string]downloader.TaskObservationState{
				target.TorrentHash: tt.observedState,
			}}
			hook := &reconciliationCompletionHookFake{}
			handler := newReconciliationTestHandler(
				t, store, map[string]TorrentSubmitter{downloaderID: observer}, hook,
			)

			if err := handler.HandleDownloadReconciliation(t.Context(), cutoff); err != nil {
				t.Fatal(err)
			}
			acceptanceCalls := store.acceptanceSnapshot()
			if len(acceptanceCalls) != tt.wantAccepts {
				t.Fatalf("acceptance calls = %#v, want %d", acceptanceCalls, tt.wantAccepts)
			}
			for _, call := range acceptanceCalls {
				if call.username != target.Username || call.id != target.ID || call.torrentHash != target.TorrentHash {
					t.Fatalf("acceptance call = %#v, target = %#v", call, target)
				}
			}
			if observer.calls != tt.wantObserved {
				t.Fatalf("observer calls = %d, want %d", observer.calls, tt.wantObserved)
			}
			transitions := baseStore.transitionSnapshot()
			if tt.wantCompleted {
				if tt.state == downloader.IntentCompleted {
					if len(transitions) != 0 {
						t.Fatalf("completed replay changed intent: %v", transitions)
					}
				} else {
					want := []string{target.ID.String() + ":downloading->completed"}
					if !slices.Equal(transitions, want) {
						t.Fatalf("intent transitions = %v, want %v", transitions, want)
					}
				}
			} else if len(transitions) != 0 {
				t.Fatalf("active acceptance used legacy intent CAS: %v", transitions)
			}
			baseStore.mu.Lock()
			episodeTransitions := append([]string(nil), baseStore.episodeTransitions...)
			parentTransitions := append([]string(nil), baseStore.parentTransitions...)
			baseStore.mu.Unlock()
			if len(episodeTransitions) != 0 || len(parentTransitions) != 0 {
				t.Fatalf("consumed subscription used legacy transitions: episodes=%v parent=%v", episodeTransitions, parentTransitions)
			}
			completionCalls := hook.snapshot()
			if len(completionCalls) != boolCount(tt.wantCompleted) {
				t.Fatalf("completion hook calls = %#v", completionCalls)
			}
			if tt.wantCompleted && (completionCalls[0].ID != target.ID || completionCalls[0].State != downloader.IntentCompleted) {
				t.Fatalf("completion hook target = %#v", completionCalls[0])
			}
		})
	}
}

func TestReconciliationHandlerDownloadAcceptanceFailures(t *testing.T) {
	t.Parallel()

	cutoff := time.Date(2026, 8, 15, 9, 0, 0, 0, time.UTC)
	const (
		downloaderID   = "03158472-b848-4211-b72a-b3ca83d7b8ae"
		sensitiveError = "acceptance-storage-sensitive-marker"
	)
	tests := []struct {
		name       string
		configure  func(*reconciliationAcceptanceStore)
		wantErr    error
		sensitive  string
		wantAccept int
	}{
		{
			name: "persistence error retries sweep",
			configure: func(store *reconciliationAcceptanceStore) {
				store.acceptErr = errors.New(sensitiveError)
			},
			wantErr:    ErrDownloadReconciliationUnavailable,
			sensitive:  sensitiveError,
			wantAccept: 1,
		},
		{
			name: "invalid returned state retries sweep",
			configure: func(store *reconciliationAcceptanceStore) {
				store.accept = func(id uuid.UUID, hash string) (downloader.Intent, error) {
					return downloader.Intent{ID: id, State: downloader.IntentCompleted, TorrentHash: hash}, nil
				}
			},
			wantErr:    ErrDownloadReconciliationUnavailable,
			wantAccept: 1,
		},
		{
			name: "state conflict is benign",
			configure: func(store *reconciliationAcceptanceStore) {
				store.acceptErr = database.ErrDownloadIntentStateConflict
			},
			wantAccept: 1,
		},
	}

	for index, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			target := reconciliationTarget(
				byte(90+index), downloaderID, downloader.IntentDownloading,
				cutoff.Add(-time.Minute), "f",
			)
			baseStore := &reconciliationTestStore{pages: [][]downloader.ReconciliationTarget{{target}}}
			store := &reconciliationAcceptanceStore{
				reconciliationTestStore: baseStore,
				consumed:                make(map[uuid.UUID]bool),
			}
			tt.configure(store)
			observer := &reconciliationTestSubmitter{states: map[string]downloader.TaskObservationState{
				target.TorrentHash: downloader.TaskObservationActive,
			}}
			handler := newReconciliationTestHandler(t, store, map[string]TorrentSubmitter{
				downloaderID: observer,
			})

			err := handler.HandleDownloadReconciliation(t.Context(), cutoff)
			failure := requireIsolatedReconciliationFailure(
				t, handler, err, tt.wantErr, target.ID, reconcileStageAcceptance,
			)
			if tt.sensitive != "" && failure.Err != nil && strings.Contains(failure.Err.Error(), tt.sensitive) {
				t.Fatalf("acceptance error leaked: %v", failure.Err)
			}
			if got := len(store.acceptanceSnapshot()); got != tt.wantAccept {
				t.Fatalf("acceptance calls = %d, want %d", got, tt.wantAccept)
			}
			if observer.calls != 1 {
				t.Fatalf("observer calls = %d, want 1 before acceptance", observer.calls)
			}
			if transitions := baseStore.transitionSnapshot(); len(transitions) != 0 {
				t.Fatalf("failed acceptance used legacy CAS: %v", transitions)
			}
		})
	}
}

func TestReconciliationHandlerSelectsEpisodesBeforeAcceptanceOrCAS(t *testing.T) {
	t.Parallel()

	cutoff := time.Date(2026, 8, 15, 11, 0, 0, 0, time.UTC)
	const downloaderID = "03158472-b848-4211-b72a-b3ca83d7b8ae"
	tests := []struct {
		name          string
		state         downloader.IntentState
		observedState downloader.TaskObservationState
		acceptance    bool
		wantEvents    []string
		wantSelection int
		wantAccepts   int
		wantObserved  int
	}{
		{
			name:  "submitting active selects before acceptance",
			state: downloader.IntentSubmitting, observedState: downloader.TaskObservationActive,
			acceptance: true, wantEvents: []string{"select", "accept"},
			wantSelection: 1, wantAccepts: 1, wantObserved: 1,
		},
		{
			name:  "submitting paused selects before acceptance",
			state: downloader.IntentSubmitting, observedState: downloader.TaskObservationPaused,
			acceptance: true, wantEvents: []string{"select", "accept"},
			wantSelection: 1, wantAccepts: 1, wantObserved: 1,
		},
		{
			name:  "submitting completed selects before acceptance and completion",
			state: downloader.IntentSubmitting, observedState: downloader.TaskObservationCompleted,
			acceptance:    true,
			wantEvents:    []string{"select", "accept", "cas:downloading->completed"},
			wantSelection: 1, wantAccepts: 1, wantObserved: 1,
		},
		{
			name:  "downloading active selects before compensating acceptance",
			state: downloader.IntentDownloading, observedState: downloader.TaskObservationActive,
			acceptance: true, wantEvents: []string{"select", "accept"},
			wantSelection: 1, wantAccepts: 1, wantObserved: 1,
		},
		{
			name:  "downloading paused selects before compensating acceptance",
			state: downloader.IntentDownloading, observedState: downloader.TaskObservationPaused,
			acceptance: true, wantEvents: []string{"select", "accept"},
			wantSelection: 1, wantAccepts: 1, wantObserved: 1,
		},
		{
			name:  "downloading completed selects before acceptance and completion",
			state: downloader.IntentDownloading, observedState: downloader.TaskObservationCompleted,
			acceptance:    true,
			wantEvents:    []string{"select", "accept", "cas:downloading->completed"},
			wantSelection: 1, wantAccepts: 1, wantObserved: 1,
		},
		{
			name:  "downloading missing keeps compatibility",
			state: downloader.IntentDownloading, observedState: downloader.TaskObservationMissing,
			acceptance: true, wantEvents: []string{"accept"},
			wantAccepts: 1, wantObserved: 1,
		},
		{
			name:  "downloading unknown does not accept",
			state: downloader.IntentDownloading, observedState: downloader.TaskObservationUnknown,
			acceptance: true, wantObserved: 1,
		},
		{
			name:  "downloading observation error does not accept",
			state: downloader.IntentDownloading, observedState: downloader.TaskObservationError,
			acceptance: true, wantObserved: 1,
		},
		{
			name:  "submitting active selects before legacy CAS",
			state: downloader.IntentSubmitting, observedState: downloader.TaskObservationActive,
			wantEvents:    []string{"select", "cas:submitting->downloading"},
			wantSelection: 1, wantObserved: 1,
		},
		{
			name:  "downloading completed selects before legacy completion CAS",
			state: downloader.IntentDownloading, observedState: downloader.TaskObservationCompleted,
			wantEvents:    []string{"select", "cas:downloading->completed"},
			wantSelection: 1, wantObserved: 1,
		},
		{
			name:       "completed compensation does not query or select downloader",
			state:      downloader.IntentCompleted,
			acceptance: true,
		},
	}

	for index, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			target := reconciliationTarget(
				byte(110+index), downloaderID, tt.state, cutoff.Add(-time.Minute), "a",
			)
			baseStore := &reconciliationTestStore{pages: [][]downloader.ReconciliationTarget{{target}}}
			input := organization.SuccessInput{
				IntentID: target.ID,
				Username: target.Username,
				Episodes: []organization.Episode{
					{Season: 2, Episode: 3},
					{Season: 1, Episode: 2},
					{Season: 2, Episode: 3},
				},
			}
			var (
				store       ReconciliationStore
				acceptStore *reconciliationAcceptanceStore
			)
			if tt.acceptance {
				acceptStore = &reconciliationAcceptanceStore{
					reconciliationTestStore: baseStore,
					consumed:                make(map[uuid.UUID]bool),
				}
				store = &reconciliationSuccessInputAcceptanceStore{
					reconciliationAcceptanceStore: acceptStore,
					input:                         input,
				}
			} else {
				store = &reconciliationSuccessInputStore{
					reconciliationTestStore: baseStore,
					input:                   input,
				}
			}
			observer := &reconciliationTestSubmitter{states: map[string]downloader.TaskObservationState{
				target.TorrentHash: tt.observedState,
			}}
			selector := &reconciliationEpisodeSelectingSubmitter{
				reconciliationTestSubmitter: observer,
				eventStore:                  baseStore,
			}
			handler := newReconciliationTestHandler(t, store, map[string]TorrentSubmitter{
				downloaderID: selector,
			})

			if err := handler.HandleDownloadReconciliation(t.Context(), cutoff); err != nil {
				t.Fatal(err)
			}
			if got := baseStore.eventSnapshot(); !slices.Equal(got, tt.wantEvents) {
				t.Fatalf("events = %#v, want %#v", got, tt.wantEvents)
			}
			if selector.selectionCalls != tt.wantSelection {
				t.Fatalf("selection calls = %d, want %d", selector.selectionCalls, tt.wantSelection)
			}
			if selector.selectionCalls > 0 {
				wantEpisodes := []downloader.Episode{{Season: 1, Episode: 2}, {Season: 2, Episode: 3}}
				if selector.selectedIntent.ID != target.ID ||
					selector.selectedIntent.State != downloader.IntentDownloading ||
					selector.selectedIntent.TorrentHash != target.TorrentHash ||
					!slices.Equal(selector.selectedEpisodes, wantEpisodes) {
					t.Fatalf(
						"selected intent/episodes = %#v/%#v",
						selector.selectedIntent, selector.selectedEpisodes,
					)
				}
			}
			if observer.calls != tt.wantObserved {
				t.Fatalf("observer calls = %d, want %d", observer.calls, tt.wantObserved)
			}
			if acceptStore != nil && len(acceptStore.acceptanceSnapshot()) != tt.wantAccepts {
				t.Fatalf("acceptance calls = %d, want %d", len(acceptStore.acceptanceSnapshot()), tt.wantAccepts)
			}
		})
	}
}

func TestReconciliationHandlerEpisodeSelectionFailuresDoNotAdvanceIntent(t *testing.T) {
	t.Parallel()

	cutoff := time.Date(2026, 8, 15, 12, 0, 0, 0, time.UTC)
	const downloaderID = "03158472-b848-4211-b72a-b3ca83d7b8ae"
	tests := []struct {
		name         string
		err          error
		noCapability bool
	}{
		{name: "metadata pending", err: downloader.ErrEpisodeSelectionPending},
		{name: "no matching target", err: downloader.ErrEpisodeSelectionNoMatch},
		{name: "selector unavailable", err: downloader.ErrEpisodeSelectionUnavailable},
		{name: "selection failed", err: downloader.ErrEpisodeSelectionFailed},
		{name: "selector capability missing", noCapability: true},
	}

	for index, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			target := reconciliationTarget(
				byte(130+index), downloaderID, downloader.IntentSubmitting,
				cutoff.Add(-time.Minute), "b",
			)
			baseStore := &reconciliationTestStore{pages: [][]downloader.ReconciliationTarget{{target}}}
			acceptStore := &reconciliationAcceptanceStore{
				reconciliationTestStore: baseStore,
				consumed:                make(map[uuid.UUID]bool),
			}
			store := &reconciliationSuccessInputAcceptanceStore{
				reconciliationAcceptanceStore: acceptStore,
				input: organization.SuccessInput{
					IntentID: target.ID, Username: target.Username,
					Episodes: []organization.Episode{{Season: 1, Episode: 1}},
				},
			}
			observer := &reconciliationTestSubmitter{
				states: map[string]downloader.TaskObservationState{
					target.TorrentHash: downloader.TaskObservationActive,
				},
			}
			var submitter TorrentSubmitter = observer
			if !tt.noCapability {
				submitter = &reconciliationEpisodeSelectingSubmitter{
					reconciliationTestSubmitter: observer,
					eventStore:                  baseStore,
					selectionErr:                tt.err,
				}
			}
			handler := newReconciliationTestHandler(t, store, map[string]TorrentSubmitter{
				downloaderID: submitter,
			})

			err := handler.HandleDownloadReconciliation(t.Context(), cutoff)
			if err != nil {
				t.Fatalf("单目标失败被隔离，轮次不应失败: %v", err)
			}
			failure := requireReconciliationFailure(t, handler, target.ID, reconcileStageEpisodeSelection)
			if !errors.Is(failure.Err, ErrDownloadReconciliationUnavailable) {
				t.Fatalf("失败原因 = %v, want ErrDownloadReconciliationUnavailable", failure.Err)
			}
			wantEvents := []string{"select"}
			if tt.noCapability {
				wantEvents = nil
			}
			if got := baseStore.eventSnapshot(); !slices.Equal(got, wantEvents) {
				t.Fatalf("events = %#v, want %#v", got, wantEvents)
			}
			if len(acceptStore.acceptanceSnapshot()) != 0 || len(baseStore.transitionSnapshot()) != 0 {
				t.Fatal("selection failure advanced acceptance or intent state")
			}
		})
	}
}

func TestReconciliationHandlerRecoversOnlyConfirmedStaleSubmissionsAndFailedIntents(t *testing.T) {
	t.Parallel()
	cutoff := time.Date(2026, 8, 11, 18, 0, 0, 0, time.UTC)
	downloaderID := "03158472-b848-4211-b72a-b3ca83d7b8ae"
	recent := reconciliationTarget(42, downloaderID, downloader.IntentSubmitting, cutoff.Add(-staleSubmissionGrace).Add(time.Nanosecond), "3")
	stale := reconciliationTarget(43, downloaderID, downloader.IntentSubmitting, cutoff.Add(-staleSubmissionGrace), "4")
	failed := reconciliationTarget(44, downloaderID, downloader.IntentFailed, cutoff.Add(-time.Minute), "5")
	store := &reconciliationTestStore{pages: [][]downloader.ReconciliationTarget{{stale, recent, failed}}}
	observer := &reconciliationTestSubmitter{}
	handler := newReconciliationTestHandler(t, store, map[string]TorrentSubmitter{downloaderID: observer})

	if err := handler.HandleDownloadReconciliation(t.Context(), cutoff); err != nil {
		t.Fatal(err)
	}
	store.mu.Lock()
	recoveries := append([]reconciliationRecoveryCall(nil), store.recoveries...)
	store.mu.Unlock()
	if len(recoveries) != 2 {
		t.Fatalf("recovery calls = %#v, want 2", recoveries)
	}
	if recoveries[0].id != failed.ID || recoveries[0].state != downloader.IntentFailed ||
		!recoveries[0].updatedAt.Equal(failed.UpdatedAt) {
		t.Fatalf("failed recovery = %#v", recoveries[0])
	}
	if recoveries[1].id != stale.ID || recoveries[1].state != downloader.IntentSubmitting ||
		!recoveries[1].updatedAt.Equal(stale.UpdatedAt) {
		t.Fatalf("stale recovery = %#v", recoveries[1])
	}
	if observer.calls != 1 || len(observer.batches) != 1 || len(observer.batches[0]) != 2 {
		t.Fatalf("observer calls/batches = %d/%#v", observer.calls, observer.batches)
	}
}

func TestReconciliationHandlerRecoversPlannedIntentAtGraceBoundary(t *testing.T) {
	t.Parallel()

	cutoff := time.Date(2026, 8, 16, 12, 0, 0, 0, time.UTC)
	const downloaderID = "03158472-b848-4211-b72a-b3ca83d7b8ae"
	tests := []struct {
		name        string
		updatedAt   time.Time
		wantRecover bool
	}{
		{
			name:      "inside grace is retained",
			updatedAt: cutoff.Add(-stalePlannedGrace).Add(time.Nanosecond),
		},
		{
			name:        "grace boundary is recovered",
			updatedAt:   cutoff.Add(-stalePlannedGrace),
			wantRecover: true,
		},
		{
			name:        "beyond grace is recovered",
			updatedAt:   cutoff.Add(-stalePlannedGrace).Add(-time.Nanosecond),
			wantRecover: true,
		},
	}

	for index, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			target := reconciliationTarget(
				byte(46+index), downloaderID, downloader.IntentPlanned, tt.updatedAt, "7",
			)
			store := &reconciliationTestStore{pages: [][]downloader.ReconciliationTarget{{target}}}
			observer := &reconciliationTestSubmitter{}
			handler := newReconciliationTestHandler(t, store, map[string]TorrentSubmitter{
				downloaderID: observer,
			})

			if err := handler.HandleDownloadReconciliation(t.Context(), cutoff); err != nil {
				t.Fatal(err)
			}
			calls := store.recoverySnapshot()
			wantCalls := 0
			if tt.wantRecover {
				wantCalls = 1
			}
			if len(calls) != wantCalls {
				t.Fatalf("recovery calls = %#v, want %d", calls, wantCalls)
			}
			if tt.wantRecover {
				call := calls[0]
				if call.id != target.ID || call.state != downloader.IntentPlanned ||
					!call.updatedAt.Equal(target.UpdatedAt) {
					t.Fatalf("planned recovery = %#v", call)
				}
			}
			if observer.calls != 0 {
				t.Fatalf("planned intent queried downloader %d times", observer.calls)
			}
		})
	}
}

func TestReconciliationHandlerSanitizesRecoveryFailure(t *testing.T) {
	t.Parallel()

	const (
		sensitiveMarker = "recovery-storage-sensitive-marker"
		downloaderID    = "03158472-b848-4211-b72a-b3ca83d7b8ae"
	)
	cutoff := time.Date(2026, 8, 16, 12, 0, 0, 0, time.UTC)
	tests := []struct {
		name      string
		state     downloader.IntentState
		updatedAt time.Time
	}{
		{name: "failed", state: downloader.IntentFailed, updatedAt: cutoff.Add(-time.Minute)},
		{name: "stale planned", state: downloader.IntentPlanned, updatedAt: cutoff.Add(-stalePlannedGrace)},
	}

	for index, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			target := reconciliationTarget(byte(50+index), downloaderID, tt.state, tt.updatedAt, "8")
			store := &reconciliationTestStore{
				pages:       [][]downloader.ReconciliationTarget{{target}},
				recoveryErr: errors.New(sensitiveMarker),
			}
			observer := &reconciliationTestSubmitter{}
			handler := newReconciliationTestHandler(t, store, map[string]TorrentSubmitter{
				downloaderID: observer,
			})

			err := handler.HandleDownloadReconciliation(t.Context(), cutoff)
			if err != nil {
				t.Fatalf("单目标恢复失败被隔离，轮次不应失败: %v", err)
			}
			failure := requireReconciliationFailure(t, handler, target.ID, reconcileStageRecoverIntent)
			if strings.Contains(failure.Err.Error(), sensitiveMarker) {
				t.Fatalf("recovery error leaked: %v", failure.Err)
			}
			calls := store.recoverySnapshot()
			if len(calls) != 1 || calls[0].id != target.ID || calls[0].state != tt.state ||
				!calls[0].updatedAt.Equal(target.UpdatedAt) {
				t.Fatalf("recovery calls = %#v", calls)
			}
			if observer.calls != 0 {
				t.Fatalf("direct recovery queried downloader %d times", observer.calls)
			}
		})
	}
}

type reconciliationListRequest struct {
	cutoff    time.Time
	afterTime time.Time
	afterID   uuid.UUID
	limit     int
}

type reconciliationRecoveryCall struct {
	id        uuid.UUID
	state     downloader.IntentState
	updatedAt time.Time
}

type reconciliationTestStore struct {
	mu                 sync.Mutex
	pages              [][]downloader.ReconciliationTarget
	listErr            error
	casErr             error
	listCalls          int
	listRequests       []reconciliationListRequest
	transitions        []string
	events             []string
	episodeErr         error
	episodeRows        *int64
	episodeTransitions []string
	parentErr          error
	parentTransitions  []string
	parentState        subscription.State
	parentStateSet     bool
	recoveryErr        error
	recoveries         []reconciliationRecoveryCall
}

type reconciliationAcceptanceCall struct {
	username    string
	id          uuid.UUID
	torrentHash string
}

type reconciliationAcceptanceStore struct {
	*reconciliationTestStore
	accept      func(uuid.UUID, string) (downloader.Intent, error)
	acceptErr   error
	acceptCalls []reconciliationAcceptanceCall
	consumed    map[uuid.UUID]bool
}

type reconciliationSuccessInputStore struct {
	*reconciliationTestStore
	input organization.SuccessInput
	err   error
}

func (s *reconciliationSuccessInputStore) GetDownloadIntentSuccessInput(
	_ context.Context,
	_ string,
	_ uuid.UUID,
) (organization.SuccessInput, error) {
	if s.err != nil {
		return organization.SuccessInput{}, s.err
	}
	return s.input.Clone(), nil
}

type reconciliationSuccessInputAcceptanceStore struct {
	*reconciliationAcceptanceStore
	input organization.SuccessInput
	err   error
}

func (s *reconciliationSuccessInputAcceptanceStore) GetDownloadIntentSuccessInput(
	_ context.Context,
	_ string,
	_ uuid.UUID,
) (organization.SuccessInput, error) {
	if s.err != nil {
		return organization.SuccessInput{}, s.err
	}
	return s.input.Clone(), nil
}

func (s *reconciliationAcceptanceStore) AcceptDownloadIntent(
	_ context.Context,
	username string,
	id uuid.UUID,
	torrentHash string,
) (downloader.Intent, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.acceptCalls = append(s.acceptCalls, reconciliationAcceptanceCall{
		username: username, id: id, torrentHash: torrentHash,
	})
	s.events = append(s.events, "accept")
	if s.acceptErr != nil {
		return downloader.Intent{}, false, s.acceptErr
	}
	var (
		intent downloader.Intent
		err    error
	)
	if s.accept != nil {
		intent, err = s.accept(id, torrentHash)
	} else {
		intent = downloader.Intent{ID: id, State: downloader.IntentDownloading, TorrentHash: torrentHash}
	}
	if err == nil && intent.State == downloader.IntentDownloading {
		s.consumed[id] = true
	}
	return intent, err == nil, err
}

func (s *reconciliationAcceptanceStore) TransitionSubscriptionEpisodesByIntent(
	ctx context.Context,
	username string,
	id uuid.UUID,
	from []subscription.SubscriptionEpisodeState,
	next subscription.SubscriptionEpisodeState,
) (int64, error) {
	s.mu.Lock()
	consumed := s.consumed[id]
	s.mu.Unlock()
	if consumed {
		return 0, database.ErrSubscriptionEpisodeNotFound
	}
	return s.reconciliationTestStore.TransitionSubscriptionEpisodesByIntent(ctx, username, id, from, next)
}

func (s *reconciliationAcceptanceStore) CompareAndSwapSubscriptionStateByIntent(
	ctx context.Context,
	username string,
	id uuid.UUID,
	current subscription.State,
	next subscription.State,
) error {
	s.mu.Lock()
	consumed := s.consumed[id]
	s.mu.Unlock()
	if consumed {
		return database.ErrSubscriptionDownloadStateNotFound
	}
	return s.reconciliationTestStore.CompareAndSwapSubscriptionStateByIntent(ctx, username, id, current, next)
}

func (s *reconciliationAcceptanceStore) acceptanceSnapshot() []reconciliationAcceptanceCall {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]reconciliationAcceptanceCall(nil), s.acceptCalls...)
}

func boolCount(value bool) int {
	if value {
		return 1
	}
	return 0
}

func (s *reconciliationTestStore) ListDownloadReconciliationTargets(
	_ context.Context,
	cutoff time.Time,
	afterTime time.Time,
	afterID uuid.UUID,
	limit int,
) ([]downloader.ReconciliationTarget, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.listRequests = append(s.listRequests, reconciliationListRequest{cutoff: cutoff, afterTime: afterTime, afterID: afterID, limit: limit})
	index := s.listCalls
	s.listCalls++
	if s.listErr != nil {
		return nil, s.listErr
	}
	if index >= len(s.pages) {
		return nil, nil
	}
	return append([]downloader.ReconciliationTarget(nil), s.pages[index]...), nil
}

func (s *reconciliationTestStore) CompareAndSwapDownloadIntent(
	_ context.Context,
	_ string,
	id uuid.UUID,
	from []downloader.IntentState,
	next downloader.IntentState,
	_ string,
) (downloader.Intent, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.casErr != nil {
		return downloader.Intent{}, s.casErr
	}
	s.transitions = append(s.transitions, id.String()+":"+string(from[0])+"->"+string(next))
	s.events = append(s.events, "cas:"+string(from[0])+"->"+string(next))
	return downloader.Intent{ID: id, State: next}, nil
}

func (s *reconciliationTestStore) TransitionSubscriptionEpisodesByIntent(
	_ context.Context,
	_ string,
	id uuid.UUID,
	from []subscription.SubscriptionEpisodeState,
	next subscription.SubscriptionEpisodeState,
) (int64, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.episodeErr != nil {
		return 0, s.episodeErr
	}
	fromText := make([]string, 0, len(from))
	for _, state := range from {
		fromText = append(fromText, string(state))
	}
	s.episodeTransitions = append(
		s.episodeTransitions,
		id.String()+":"+strings.Join(fromText, "|")+"->"+string(next),
	)
	if s.episodeRows != nil {
		return *s.episodeRows, nil
	}
	return 1, nil
}

func (s *reconciliationTestStore) CompareAndSwapSubscriptionStateByIntent(
	_ context.Context,
	_ string,
	_ uuid.UUID,
	current subscription.State,
	next subscription.State,
) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.parentErr != nil {
		return s.parentErr
	}
	if s.parentStateSet {
		if s.parentState != current {
			return database.ErrSubscriptionDownloadStateConflict
		}
		s.parentState = next
	}
	s.parentTransitions = append(s.parentTransitions, string(current)+"->"+string(next))
	return nil
}

func (s *reconciliationTestStore) RecoverSubscriptionDownloadIntent(
	_ context.Context,
	_ string,
	id uuid.UUID,
	fromStates []downloader.IntentState,
	expectedUpdatedAt *time.Time,
) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(fromStates) != 1 || expectedUpdatedAt == nil {
		return database.ErrInvalidDownloadIntent
	}
	s.recoveries = append(s.recoveries, reconciliationRecoveryCall{
		id: id, state: fromStates[0], updatedAt: expectedUpdatedAt.UTC(),
	})
	if s.recoveryErr != nil {
		return s.recoveryErr
	}
	return nil
}

func (s *reconciliationTestStore) recoverySnapshot() []reconciliationRecoveryCall {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]reconciliationRecoveryCall(nil), s.recoveries...)
}

func (s *reconciliationTestStore) transitionSnapshot() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.transitions...)
}

func (s *reconciliationTestStore) recordEvent(event string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.events = append(s.events, event)
}

func (s *reconciliationTestStore) eventSnapshot() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.events...)
}

func (s *reconciliationTestStore) parentSubscriptionState() subscription.State {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.parentState
}

func (s *reconciliationTestStore) cutoffsStable(cutoff time.Time) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, request := range s.listRequests {
		if !request.cutoff.Equal(cutoff) {
			return false
		}
	}
	return true
}

func (s *reconciliationTestStore) cursorsProgress(first, second downloader.ReconciliationTarget) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.listRequests) == 3 &&
		s.listRequests[1].afterTime.Equal(first.UpdatedAt) && s.listRequests[1].afterID == first.ID &&
		s.listRequests[2].afterTime.Equal(second.UpdatedAt) && s.listRequests[2].afterID == second.ID
}

type reconciliationTestSubmitter struct {
	states  map[string]downloader.TaskObservationState
	err     error
	calls   int
	batches [][]string
}

type reconciliationEpisodeSelectingSubmitter struct {
	*reconciliationTestSubmitter
	eventStore       *reconciliationTestStore
	selectionErr     error
	selectionCalls   int
	selectedIntent   downloader.Intent
	selectedEpisodes []downloader.Episode
}

func (s *reconciliationEpisodeSelectingSubmitter) SelectEpisodeFiles(
	_ context.Context,
	intent downloader.Intent,
	episodes []downloader.Episode,
) error {
	s.selectionCalls++
	s.selectedIntent = intent
	s.selectedEpisodes = append([]downloader.Episode(nil), episodes...)
	if s.eventStore != nil {
		s.eventStore.recordEvent("select")
	}
	return s.selectionErr
}

func (s *reconciliationTestSubmitter) ObserveTasks(_ context.Context, hashes []string) (map[string]downloader.TaskObservation, error) {
	s.calls++
	s.batches = append(s.batches, append([]string(nil), hashes...))
	if s.err != nil {
		return nil, s.err
	}
	result := make(map[string]downloader.TaskObservation, len(hashes))
	for _, hash := range hashes {
		state := s.states[hash]
		if state == "" {
			state = downloader.TaskObservationMissing
		}
		result[hash] = downloader.TaskObservation{Hash: hash, State: state}
	}
	return result, nil
}

func (*reconciliationTestSubmitter) Reconcile(_ context.Context, intent downloader.Intent) (downloader.Intent, bool, error) {
	return intent, false, nil
}

func (*reconciliationTestSubmitter) AddOnce(_ context.Context, intent downloader.Intent, _ downloader.Candidate, _ []byte) (downloader.Intent, error) {
	return intent, nil
}

func newReconciliationTestHandler(
	t *testing.T,
	store ReconciliationStore,
	submitters map[string]TorrentSubmitter,
	completionHooks ...DownloadCompletionHook,
) *ReconciliationHandler {
	t.Helper()
	registry, err := NewSubmitterRegistry(submitters)
	if err != nil {
		t.Fatal(err)
	}
	handler, err := NewReconciliationHandler(store, registry, completionHooks...)
	if err != nil {
		t.Fatal(err)
	}
	return handler
}

// reconciliationFailureFor 返回最近一轮对账报告里指定目标的失败记录。
// 单目标失败被隔离在报告与日志里，不再让整轮返回错误。
func reconciliationFailureFor(
	t *testing.T,
	handler *ReconciliationHandler,
	intentID uuid.UUID,
) (ReconciliationTargetFailure, bool) {
	t.Helper()
	for _, failure := range handler.LastSweepReport().Failures {
		if failure.IntentID == intentID {
			return failure, true
		}
	}
	return ReconciliationTargetFailure{}, false
}

// requireReconciliationFailure 断言指定目标进了报告，且阶段符合预期。
func requireReconciliationFailure(
	t *testing.T,
	handler *ReconciliationHandler,
	intentID uuid.UUID,
	wantStage string,
) ReconciliationTargetFailure {
	t.Helper()
	failure, ok := reconciliationFailureFor(t, handler, intentID)
	if !ok {
		t.Fatalf("目标 %s 未出现在对账失败报告里: %+v", intentID, handler.LastSweepReport())
	}
	if wantStage != "" && failure.Stage != wantStage {
		t.Fatalf("失败阶段 = %q, want %q (failure=%+v)", failure.Stage, wantStage, failure)
	}
	return failure
}

// requireIsolatedReconciliationFailure 断言上下文错误直接透传，
// 其余失败被隔离进报告而不再让整轮返回错误。
func requireIsolatedReconciliationFailure(
	t *testing.T,
	handler *ReconciliationHandler,
	gotErr error,
	wantErr error,
	intentID uuid.UUID,
	wantStage string,
) ReconciliationTargetFailure {
	t.Helper()
	if errors.Is(wantErr, context.Canceled) || errors.Is(wantErr, context.DeadlineExceeded) {
		if !errors.Is(gotErr, wantErr) {
			t.Fatalf("HandleDownloadReconciliation() = %v, want %v", gotErr, wantErr)
		}
		return ReconciliationTargetFailure{}
	}
	if gotErr != nil {
		t.Fatalf("单目标失败被隔离，轮次不应失败: %v", gotErr)
	}
	if wantErr == nil {
		if _, ok := reconciliationFailureFor(t, handler, intentID); ok {
			t.Fatalf("目标 %s 不应出现在失败报告里", intentID)
		}
		return ReconciliationTargetFailure{}
	}
	failure := requireReconciliationFailure(t, handler, intentID, wantStage)
	if !errors.Is(failure.Err, wantErr) {
		t.Fatalf("失败原因 = %v, want %v", failure.Err, wantErr)
	}
	return failure
}

type reconciliationCompletionHookFake struct {
	mu    sync.Mutex
	calls []downloader.ReconciliationTarget
	err   error
	errs  map[uuid.UUID]error
}

func (h *reconciliationCompletionHookFake) HandleDownloadCompleted(
	_ context.Context,
	target downloader.ReconciliationTarget,
) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.calls = append(h.calls, target)
	if err, ok := h.errs[target.ID]; ok {
		return err
	}
	return h.err
}

func (h *reconciliationCompletionHookFake) snapshot() []downloader.ReconciliationTarget {
	h.mu.Lock()
	defer h.mu.Unlock()
	return append([]downloader.ReconciliationTarget(nil), h.calls...)
}

func reconciliationTarget(
	seed byte,
	downloaderID string,
	state downloader.IntentState,
	updatedAt time.Time,
	hashDigit string,
) downloader.ReconciliationTarget {
	idBytes := [16]byte{}
	idBytes[15] = seed
	return downloader.ReconciliationTarget{
		ID: uuid.UUID(idBytes), Username: "alice", DownloaderID: downloaderID,
		State: state, TorrentHash: strings.Repeat(hashDigit, 40), UpdatedAt: updatedAt,
	}
}
