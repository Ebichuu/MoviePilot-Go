package downloadflow

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/moviepilot-go/moviepilot-go/internal/database"
	"github.com/moviepilot-go/moviepilot-go/internal/downloader"
	"github.com/moviepilot-go/moviepilot-go/internal/jobs"
)

var _ DownloadSearchFinalizationStore = (*database.Store)(nil)

func TestHandlerAcceptanceFinalizationByExecutionMode(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name       string
		state      downloader.IntentState
		batch      bool
		metainfo   bool
		reconciled bool
	}{
		{"worker add", downloader.IntentPlanned, false, false, false},
		{"worker downloading", downloader.IntentDownloading, false, false, false},
		{"worker reconciles", downloader.IntentSubmitting, false, false, true},
		{"batch add", downloader.IntentPlanned, true, false, false},
		{"batch inspected metainfo", downloader.IntentPlanned, true, true, false},
		{"batch downloading", downloader.IntentDownloading, true, false, false},
		{"batch reconciles", downloader.IntentSubmitting, true, false, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			fixture := newHandlerFixture(t, tc.state, "async-acceptance")
			store := &finalizingHandlerStore{handlerAcceptanceStore: &handlerAcceptanceStore{handlerTestStore: fixture.store}}
			fixture.handler.store = store
			if tc.state == downloader.IntentDownloading {
				fixture.store.intent.TorrentHash = handlerTestHash
			}
			if tc.reconciled {
				fixture.submitter.reconcile = func(intent downloader.Intent) (downloader.Intent, bool, error) {
					return downloadingIntent(t, intent, handlerTestHash), true, nil
				}
			}
			if tc.batch {
				var got IntentExecutionResult
				var err error
				if tc.metainfo {
					got, err = fixture.handler.ExecuteIntentWithMetainfo(t.Context(), fixture.args.Username, fixture.args.IntentID, validHandlerMetainfo(t))
				} else {
					got, err = fixture.handler.ExecuteIntent(t.Context(), fixture.args.Username, fixture.args.IntentID)
				}
				if err != nil || got.Outcome != IntentExecutionAccepted {
					t.Fatalf("batch result=%+v err=%v", got, err)
				}
			} else {
				got, err := fixture.handler.HandleDownload(t.Context(), fixture.args)
				if err != nil || got.Action != jobs.DownloadActionComplete {
					t.Fatalf("worker result=%+v err=%v", got, err)
				}
			}
			wantFinal := 1
			if tc.batch {
				wantFinal = 0
			}
			if store.finalizeCalls != wantFinal || len(store.acceptanceSnapshot()) != 1 {
				t.Fatalf("finalize=%d acceptance=%v want=%d/1", store.finalizeCalls, store.acceptanceSnapshot(), wantFinal)
			}
			call := store.acceptanceSnapshot()[0]
			if call.username != "alice" || call.id != fixture.args.IntentID || call.torrentHash == "" {
				t.Fatalf("acceptance lost owner/intent/hash: %+v", call)
			}
			if tc.metainfo && fixture.resolver.calls.Load() != 0 {
				t.Fatal("inspected metainfo resolved again")
			}
			if tc.state != downloader.IntentPlanned && fixture.submitter.addCalls.Load() != 0 {
				t.Fatal("reconciled acceptance added again")
			}
		})
	}
}

func TestWorkerFinalizationKeepsUnknownAndSelectionFailurePending(t *testing.T) {
	t.Parallel()
	t.Run("unknown hash", func(t *testing.T) {
		fixture := newHandlerFixture(t, downloader.IntentSubmitting, "async-unknown")
		store := &finalizingHandlerStore{handlerAcceptanceStore: &handlerAcceptanceStore{handlerTestStore: fixture.store}}
		fixture.handler.store = store
		got, err := fixture.handler.HandleDownload(t.Context(), fixture.args)
		if err != nil || got.Action != jobs.DownloadActionSnooze || store.finalizeCalls != 0 || len(store.acceptanceSnapshot()) != 0 {
			t.Fatalf("unknown result=%+v err=%v final=%d", got, err, store.finalizeCalls)
		}
	})
	t.Run("selection not confirmed", func(t *testing.T) {
		fixture, base, submitter := newExecutionV2Fixture(t)
		store := &finalizingExecutionStore{executionV2Store: base}
		fixture.handler.store = store
		submitter.selectionErr = downloader.ErrEpisodeSelectionPending
		got, err := fixture.handler.HandleDownload(t.Context(), fixture.args)
		if err != nil || got.Action != jobs.DownloadActionSnooze || store.finalizeCalls != 0 || len(base.acceptanceSnapshot()) != 0 {
			t.Fatalf("selection result=%+v err=%v final=%d", got, err, store.finalizeCalls)
		}
	})
}

func TestWorkerFinalizationPreservesAcceptanceValidation(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name string
		err  error
		bad  bool
	}{
		{name: "storage error", err: errors.New("async-acceptance-sensitive-marker")},
		{name: "invalid accepted state", bad: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			fixture := newHandlerFixture(t, downloader.IntentPlanned, "async-validation")
			base := &handlerAcceptanceStore{handlerTestStore: fixture.store, acceptErr: tc.err}
			if tc.bad {
				base.accept = func(intent downloader.Intent, hash string) (downloader.Intent, error) {
					intent.State, intent.TorrentHash = downloader.IntentCompleted, hash
					return intent, nil
				}
			}
			store := &finalizingHandlerStore{handlerAcceptanceStore: base}
			fixture.handler.store = store
			got, err := fixture.handler.HandleDownload(t.Context(), fixture.args)
			if got.Action != jobs.DownloadActionRetry || !errors.Is(err, ErrDownloadStateUnavailable) || store.finalizeCalls != 1 {
				t.Fatalf("result=%+v err=%v final=%d", got, err, store.finalizeCalls)
			}
			if strings.Contains(err.Error(), "sensitive-marker") {
				t.Fatal("storage detail leaked")
			}
		})
	}
}

func TestPeriodicReconciliationUsesFinalAcceptance(t *testing.T) {
	t.Parallel()
	cutoff := time.Date(2026, 10, 4, 3, 0, 0, 0, time.UTC)
	for index, tc := range []struct {
		name  string
		state downloader.IntentState
		task  downloader.TaskObservationState
		err   error
	}{
		{"submitting active", downloader.IntentSubmitting, downloader.TaskObservationActive, nil},
		{"submitting completed", downloader.IntentSubmitting, downloader.TaskObservationCompleted, nil},
		{"downloading active", downloader.IntentDownloading, downloader.TaskObservationActive, nil},
		{"downloading completed", downloader.IntentDownloading, downloader.TaskObservationCompleted, nil},
		{"downloading missing replay", downloader.IntentDownloading, downloader.TaskObservationMissing, nil},
		{"storage failure", downloader.IntentSubmitting, downloader.TaskObservationActive, errors.New("async-reconcile-sensitive-marker")},
		{"state conflict", downloader.IntentSubmitting, downloader.TaskObservationActive, database.ErrDownloadIntentStateConflict},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			target := reconciliationTarget(byte(150+index), handlerTestDownloaderID, tc.state, cutoff.Add(-time.Minute), "a")
			base := &reconciliationAcceptanceStore{
				reconciliationTestStore: &reconciliationTestStore{pages: [][]downloader.ReconciliationTarget{{target}}},
				consumed:                make(map[uuid.UUID]bool), acceptErr: tc.err,
			}
			store := &finalizingReconciliationStore{reconciliationAcceptanceStore: base}
			observer := &reconciliationTestSubmitter{states: map[string]downloader.TaskObservationState{target.TorrentHash: tc.task}}
			handler := newReconciliationTestHandler(t, store, map[string]TorrentSubmitter{handlerTestDownloaderID: observer}, &reconciliationCompletionHookFake{})
			err := handler.HandleDownloadReconciliation(t.Context(), cutoff)
			if tc.err != nil && !errors.Is(tc.err, database.ErrDownloadIntentStateConflict) {
				// 单目标的存储失败被隔离：轮次不失败，但必须进报告且不泄露底层细节。
				if err != nil {
					t.Fatalf("隔离后的单目标失败不应让整轮失败: %v", err)
				}
				failure := requireReconciliationFailure(t, handler, target.ID, reconcileStageAcceptance)
				if strings.Contains(failure.Err.Error(), "sensitive-marker") {
					t.Fatalf("reconciliation error leaked: %v", failure.Err)
				}
			} else if err != nil {
				t.Fatal(err)
			}
			calls := base.acceptanceSnapshot()
			if store.finalizeCalls != 1 || len(calls) != 1 || calls[0].username != target.Username || calls[0].id != target.ID || calls[0].torrentHash != target.TorrentHash {
				t.Fatalf("finalize=%d acceptance=%+v target=%+v", store.finalizeCalls, calls, target)
			}
		})
	}
}

type finalizingHandlerStore struct {
	*handlerAcceptanceStore
	finalizeCalls int
}

func (s *finalizingHandlerStore) AcceptDownloadIntentAndFinishSearch(ctx context.Context, username string, id uuid.UUID, hash string) (downloader.Intent, bool, error) {
	s.finalizeCalls++
	return s.AcceptDownloadIntent(ctx, username, id, hash)
}

type finalizingExecutionStore struct {
	*executionV2Store
	finalizeCalls int
}

func (s *finalizingExecutionStore) AcceptDownloadIntentAndFinishSearch(ctx context.Context, username string, id uuid.UUID, hash string) (downloader.Intent, bool, error) {
	s.finalizeCalls++
	return s.AcceptDownloadIntent(ctx, username, id, hash)
}

type finalizingReconciliationStore struct {
	*reconciliationAcceptanceStore
	finalizeCalls int
}

func (s *finalizingReconciliationStore) AcceptDownloadIntentAndFinishSearch(ctx context.Context, username string, id uuid.UUID, hash string) (downloader.Intent, bool, error) {
	s.finalizeCalls++
	return s.AcceptDownloadIntent(ctx, username, id, hash)
}
