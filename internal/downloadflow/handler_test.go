package downloadflow

import (
	"bytes"
	"context"
	"errors"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/zeebo/bencode"

	"github.com/moviepilot-go/moviepilot-go/internal/database"
	"github.com/moviepilot-go/moviepilot-go/internal/downloader"
	"github.com/moviepilot-go/moviepilot-go/internal/indexer"
	"github.com/moviepilot-go/moviepilot-go/internal/jobs"
	"github.com/moviepilot-go/moviepilot-go/internal/subscription"
)

func TestHandlerPlannedAndFailedIntentsUseCASBeforeSingleAdd(t *testing.T) {
	t.Parallel()

	for _, state := range []downloader.IntentState{downloader.IntentPlanned, downloader.IntentFailed} {
		state := state
		t.Run(string(state), func(t *testing.T) {
			t.Parallel()

			fixture := newHandlerFixture(t, state, "42")
			if _, supported := any(fixture.store).(DownloadAcceptanceStore); supported {
				t.Fatal("legacy handler fake unexpectedly implements download acceptance")
			}
			result, err := fixture.handler.HandleDownload(t.Context(), fixture.args)
			if err != nil {
				t.Fatalf("HandleDownload() error = %v", err)
			}
			if result.Action != jobs.DownloadActionComplete {
				t.Fatalf("HandleDownload() action = %v", result.Action)
			}
			if got := fixture.store.snapshot(); got.State != downloader.IntentDownloading || got.TorrentHash == "" {
				t.Fatalf("持久意图未进入 downloading: %#v", got)
			}
			if fixture.submitter.addCalls.Load() != 1 || fixture.resolver.calls.Load() != 1 {
				t.Fatalf("resolver/add calls = %d/%d, want 1/1", fixture.resolver.calls.Load(), fixture.submitter.addCalls.Load())
			}
			transitions := fixture.store.transitions()
			wantFirst := string(state) + "->submitting"
			if len(transitions) != 2 || transitions[0] != wantFirst || transitions[1] != "submitting->downloading" {
				t.Fatalf("CAS transitions = %#v, want [%q %q]", transitions, wantFirst, "submitting->downloading")
			}
			events := fixture.store.eventsSnapshot()
			wantEvents := []string{"cas:" + wantFirst, "add", "cas:submitting->downloading"}
			if !slices.Equal(events, wantEvents) {
				t.Fatalf("执行事件 = %#v, want %#v", events, wantEvents)
			}
		})
	}
}

func TestHandlerSubmittingReplayOnlyReconciles(t *testing.T) {
	t.Parallel()

	fixture := newHandlerFixture(t, downloader.IntentSubmitting, "42")
	result, err := fixture.handler.HandleDownload(t.Context(), fixture.args)
	if err != nil {
		t.Fatalf("HandleDownload() error = %v", err)
	}
	if result.Action != jobs.DownloadActionSnooze || result.SnoozeFor != reconciliationSnooze {
		t.Fatalf("HandleDownload() result = %#v", result)
	}
	if fixture.submitter.addCalls.Load() != 0 || fixture.resolver.calls.Load() != 0 {
		t.Fatal("submitting 重放触发了资源解析或重复 Add")
	}
}

func TestHandlerTerminalIntentReplayRepairsSubscriptionEpisodes(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name         string
		state        downloader.IntentState
		wantEpisodes []string
		wantParent   []string
	}{
		{
			name: "downloading", state: downloader.IntentDownloading,
			wantEpisodes: []string{"acquired->downloading"},
			wantParent:   []string{"acquired->downloading"},
		},
		{
			name: "completed", state: downloader.IntentCompleted,
			wantEpisodes: []string{"acquired->downloading", "downloading->pending_organization"},
			wantParent:   []string{"acquired->downloading", "downloading->pending_organization"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			fixture := newHandlerFixture(t, tt.state, "42")
			result, err := fixture.handler.HandleDownload(t.Context(), fixture.args)
			if err != nil || result.Action != jobs.DownloadActionComplete {
				t.Fatalf("HandleDownload() = %#v, %v", result, err)
			}
			fixture.store.mu.Lock()
			got := append([]string(nil), fixture.store.episodeLog...)
			gotParent := append([]string(nil), fixture.store.parentLog...)
			fixture.store.mu.Unlock()
			if !slices.Equal(got, tt.wantEpisodes) {
				t.Fatalf("episode transitions = %v, want %v", got, tt.wantEpisodes)
			}
			if !slices.Equal(gotParent, tt.wantParent) {
				t.Fatalf("parent transitions = %v, want %v", gotParent, tt.wantParent)
			}
		})
	}
}

func TestHandlerTerminalIntentReplayAdvancesMovieSubscriptionWithoutEpisodes(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name       string
		state      downloader.IntentState
		wantParent subscription.State
	}{
		{
			name:       "downloading",
			state:      downloader.IntentDownloading,
			wantParent: subscription.Downloading,
		},
		{
			name:       "completed",
			state:      downloader.IntentCompleted,
			wantParent: subscription.PendingOrganization,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			zeroRows := int64(0)
			fixture := newHandlerFixture(t, tt.state, "movie-resource")
			fixture.store.episodeRows = &zeroRows
			fixture.store.parentState = subscription.Acquired
			fixture.store.parentStateSet = true

			result, err := fixture.handler.HandleDownload(t.Context(), fixture.args)
			if err != nil || result.Action != jobs.DownloadActionComplete {
				t.Fatalf("HandleDownload() = %#v, %v", result, err)
			}
			if got := fixture.store.parentSubscriptionState(); got != tt.wantParent {
				t.Fatalf("parent subscription state = %s, want %s", got, tt.wantParent)
			}
		})
	}
}

func TestHandlerCompletedReplayInvokesCompletionHook(t *testing.T) {
	t.Parallel()
	fixture := newHandlerFixture(t, downloader.IntentCompleted, "42")
	fixture.store.intent.TorrentHash = handlerTestHash
	hook := &reconciliationCompletionHookFake{}
	provider, err := NewSubmitterRegistry(map[string]TorrentSubmitter{
		handlerTestDownloaderID: fixture.submitter,
	})
	if err != nil {
		t.Fatal(err)
	}
	handler, err := NewHandler(
		fixture.store,
		handlerResolverProvider{resolver: fixture.resolver},
		provider,
		hook,
	)
	if err != nil {
		t.Fatal(err)
	}

	result, err := handler.HandleDownload(t.Context(), fixture.args)
	if err != nil || result.Action != jobs.DownloadActionComplete {
		t.Fatalf("HandleDownload() = %#v, %v", result, err)
	}
	calls := hook.snapshot()
	if len(calls) != 1 || calls[0].ID != fixture.args.IntentID ||
		calls[0].Username != "alice" || calls[0].DownloaderID != handlerTestDownloaderID ||
		calls[0].State != downloader.IntentCompleted || calls[0].TorrentHash != handlerTestHash {
		t.Fatalf("completion calls = %#v", calls)
	}
}

func TestHandlerRetriesSanitizedCompletionHookFailure(t *testing.T) {
	t.Parallel()
	const sensitiveMarker = "organization-planning-secret"
	fixture := newHandlerFixture(t, downloader.IntentCompleted, "42")
	fixture.store.intent.TorrentHash = handlerTestHash
	hook := &reconciliationCompletionHookFake{err: errors.New(sensitiveMarker)}
	provider, err := NewSubmitterRegistry(map[string]TorrentSubmitter{
		handlerTestDownloaderID: fixture.submitter,
	})
	if err != nil {
		t.Fatal(err)
	}
	handler, err := NewHandler(
		fixture.store,
		handlerResolverProvider{resolver: fixture.resolver},
		provider,
		hook,
	)
	if err != nil {
		t.Fatal(err)
	}

	result, err := handler.HandleDownload(t.Context(), fixture.args)
	if result.Action != jobs.DownloadActionRetry || !errors.Is(err, ErrDownloadStateUnavailable) {
		t.Fatalf("HandleDownload() = %#v, %v", result, err)
	}
	if strings.Contains(err.Error(), sensitiveMarker) {
		t.Fatalf("completion hook error leaked: %v", err)
	}
}

func TestHandlerRetriesSanitizedEpisodePersistenceFailure(t *testing.T) {
	t.Parallel()
	fixture := newHandlerFixture(t, downloader.IntentDownloading, "42")
	fixture.store.episodeErr = errors.New("postgres failed for subscription secret-owner")
	result, err := fixture.handler.HandleDownload(t.Context(), fixture.args)
	if result.Action != jobs.DownloadActionRetry || !errors.Is(err, ErrDownloadStateUnavailable) {
		t.Fatalf("HandleDownload() = %#v, %v", result, err)
	}
	if strings.Contains(err.Error(), "secret-owner") {
		t.Fatalf("episode persistence error leaked: %v", err)
	}
}

func TestHandlerDoesNotAdvanceParentAfterEpisodeStateConflict(t *testing.T) {
	t.Parallel()

	fixture := newHandlerFixture(t, downloader.IntentDownloading, "42")
	fixture.store.episodeErr = database.ErrSubscriptionEpisodeStateConflict
	result, err := fixture.handler.HandleDownload(t.Context(), fixture.args)
	if result.Action != jobs.DownloadActionRetry || !errors.Is(err, ErrDownloadStateUnavailable) {
		t.Fatalf("HandleDownload() = %#v, %v", result, err)
	}
	fixture.store.mu.Lock()
	parentTransitions := append([]string(nil), fixture.store.parentLog...)
	fixture.store.mu.Unlock()
	if len(parentTransitions) != 0 {
		t.Fatalf("episode state conflict advanced parent: %v", parentTransitions)
	}
}

func TestHandlerRecoversExistingDownloaderTask(t *testing.T) {
	t.Parallel()

	for _, state := range []downloader.IntentState{downloader.IntentPlanned, downloader.IntentSubmitting, downloader.IntentFailed} {
		state := state
		t.Run(string(state), func(t *testing.T) {
			t.Parallel()

			fixture := newHandlerFixture(t, state, "42")
			fixture.submitter.reconcile = func(intent downloader.Intent) (downloader.Intent, bool, error) {
				return downloadingIntent(t, intent, handlerTestHash), true, nil
			}
			result, err := fixture.handler.HandleDownload(t.Context(), fixture.args)
			if err != nil || result.Action != jobs.DownloadActionComplete {
				t.Fatalf("HandleDownload() = %#v, %v", result, err)
			}
			if got := fixture.store.snapshot(); got.State != downloader.IntentDownloading || got.TorrentHash != handlerTestHash {
				t.Fatalf("对账状态未持久化: %#v", got)
			}
			if fixture.submitter.addCalls.Load() != 0 || fixture.resolver.calls.Load() != 0 {
				t.Fatal("已有下载任务仍触发了解析或 Add")
			}
		})
	}
}

func TestHandlerUsesDownloadAcceptanceCapability(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name          string
		state         downloader.IntentState
		configure     func(*testing.T, *handlerFixture, *handlerAcceptanceStore)
		wantAction    jobs.DownloadAction
		wantSnooze    time.Duration
		wantErr       error
		wantAccepts   int
		wantAddCalls  int32
		wantCAS       []string
		wantEvents    []string
		wantFinal     downloader.IntentState
		wantSensitive string
	}{
		{
			name:         "explicit add result is accepted atomically",
			state:        downloader.IntentPlanned,
			wantAction:   jobs.DownloadActionComplete,
			wantAccepts:  1,
			wantAddCalls: 1,
			wantCAS:      []string{"planned->submitting"},
			wantEvents:   []string{"cas:planned->submitting", "add", "accept"},
			wantFinal:    downloader.IntentDownloading,
		},
		{
			name:  "downloading replay compensates acceptance",
			state: downloader.IntentDownloading,
			configure: func(_ *testing.T, fixture *handlerFixture, _ *handlerAcceptanceStore) {
				fixture.store.intent.TorrentHash = handlerTestHash
			},
			wantAction:  jobs.DownloadActionComplete,
			wantAccepts: 1,
			wantEvents:  []string{"accept"},
			wantFinal:   downloader.IntentDownloading,
		},
		{
			name:  "reconciled task compensates acceptance",
			state: downloader.IntentPlanned,
			configure: func(t *testing.T, fixture *handlerFixture, _ *handlerAcceptanceStore) {
				fixture.submitter.reconcile = func(intent downloader.Intent) (downloader.Intent, bool, error) {
					return downloadingIntent(t, intent, handlerTestHash), true, nil
				}
			},
			wantAction:  jobs.DownloadActionComplete,
			wantAccepts: 1,
			wantCAS:     []string{"planned->submitting"},
			wantEvents:  []string{"cas:planned->submitting", "accept"},
			wantFinal:   downloader.IntentDownloading,
		},
		{
			name:  "unknown add result is not accepted",
			state: downloader.IntentPlanned,
			configure: func(_ *testing.T, fixture *handlerFixture, _ *handlerAcceptanceStore) {
				fixture.submitter.addErr = downloader.ErrReconciliationPending
			},
			wantAction:    jobs.DownloadActionSnooze,
			wantSnooze:    reconciliationSnooze,
			wantAddCalls:  1,
			wantCAS:       []string{"planned->submitting"},
			wantEvents:    []string{"cas:planned->submitting", "add"},
			wantFinal:     downloader.IntentSubmitting,
			wantAccepts:   0,
			wantSensitive: "",
		},
		{
			name:  "acceptance persistence error retries",
			state: downloader.IntentPlanned,
			configure: func(_ *testing.T, _ *handlerFixture, store *handlerAcceptanceStore) {
				store.acceptErr = errors.New("acceptance-sensitive-marker")
			},
			wantAction:    jobs.DownloadActionRetry,
			wantErr:       ErrDownloadStateUnavailable,
			wantAccepts:   1,
			wantAddCalls:  1,
			wantCAS:       []string{"planned->submitting"},
			wantEvents:    []string{"cas:planned->submitting", "add", "accept"},
			wantFinal:     downloader.IntentSubmitting,
			wantSensitive: "acceptance-sensitive-marker",
		},
		{
			name:  "invalid acceptance state retries",
			state: downloader.IntentPlanned,
			configure: func(_ *testing.T, _ *handlerFixture, store *handlerAcceptanceStore) {
				store.accept = func(intent downloader.Intent, _ string) (downloader.Intent, error) {
					intent.State = downloader.IntentCompleted
					return intent, nil
				}
			},
			wantAction:   jobs.DownloadActionRetry,
			wantErr:      ErrDownloadStateUnavailable,
			wantAccepts:  1,
			wantAddCalls: 1,
			wantCAS:      []string{"planned->submitting"},
			wantEvents:   []string{"cas:planned->submitting", "add", "accept"},
			wantFinal:    downloader.IntentSubmitting,
		},
		{
			name:  "acceptance state conflict falls back to reconciliation",
			state: downloader.IntentPlanned,
			configure: func(_ *testing.T, _ *handlerFixture, store *handlerAcceptanceStore) {
				store.acceptErr = database.ErrDownloadIntentStateConflict
			},
			wantAction:    jobs.DownloadActionSnooze,
			wantSnooze:    reconciliationSnooze,
			wantAccepts:   1,
			wantAddCalls:  1,
			wantCAS:       []string{"planned->submitting"},
			wantEvents:    []string{"cas:planned->submitting", "add", "accept"},
			wantFinal:     downloader.IntentSubmitting,
			wantSensitive: "",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			fixture := newHandlerFixture(t, tt.state, "42")
			store := &handlerAcceptanceStore{handlerTestStore: fixture.store}
			if tt.configure != nil {
				tt.configure(t, fixture, store)
			}
			fixture.handler = newHandlerTestHandler(t, store, fixture.resolver, fixture.submitter)

			result, err := fixture.handler.HandleDownload(t.Context(), fixture.args)
			if result.Action != tt.wantAction || result.SnoozeFor != tt.wantSnooze ||
				!errors.Is(err, tt.wantErr) {
				t.Fatalf("HandleDownload() = %#v, %v; want action=%v snooze=%s error=%v", result, err, tt.wantAction, tt.wantSnooze, tt.wantErr)
			}
			if tt.wantSensitive != "" && err != nil && strings.Contains(err.Error(), tt.wantSensitive) {
				t.Fatalf("acceptance error leaked: %v", err)
			}
			acceptanceCalls := store.acceptanceSnapshot()
			if got := len(acceptanceCalls); got != tt.wantAccepts {
				t.Fatalf("acceptance calls = %d, want %d", got, tt.wantAccepts)
			}
			for _, call := range acceptanceCalls {
				if call.username != "alice" || call.id != fixture.args.IntentID || call.torrentHash == "" {
					t.Fatalf("acceptance call = %#v", call)
				}
			}
			if got := fixture.submitter.addCalls.Load(); got != tt.wantAddCalls {
				t.Fatalf("AddOnce() calls = %d, want %d", got, tt.wantAddCalls)
			}
			if got := fixture.store.transitions(); !slices.Equal(got, tt.wantCAS) {
				t.Fatalf("CAS transitions = %v, want %v", got, tt.wantCAS)
			}
			if got := fixture.store.eventsSnapshot(); !slices.Equal(got, tt.wantEvents) {
				t.Fatalf("events = %v, want %v", got, tt.wantEvents)
			}
			if got := fixture.store.snapshot().State; got != tt.wantFinal {
				t.Fatalf("final intent state = %s, want %s", got, tt.wantFinal)
			}
			fixture.store.mu.Lock()
			episodeLog := append([]string(nil), fixture.store.episodeLog...)
			parentLog := append([]string(nil), fixture.store.parentLog...)
			fixture.store.mu.Unlock()
			if len(episodeLog) != 0 || len(parentLog) != 0 {
				t.Fatalf("capability path used legacy subscription transitions: episodes=%v parent=%v", episodeLog, parentLog)
			}
		})
	}
}

func TestHandlerCompletedReplayAfterAcceptancePlansWithoutSecondConsumption(t *testing.T) {
	t.Parallel()

	fixture := newHandlerFixture(t, downloader.IntentCompleted, "42")
	fixture.store.intent.TorrentHash = handlerTestHash
	store := &handlerAcceptanceStore{handlerTestStore: fixture.store, consumed: true}
	hook := &reconciliationCompletionHookFake{}
	fixture.handler = newHandlerTestHandler(t, store, fixture.resolver, fixture.submitter, hook)

	result, err := fixture.handler.HandleDownload(t.Context(), fixture.args)
	if err != nil || result.Action != jobs.DownloadActionComplete {
		t.Fatalf("HandleDownload() = %#v, %v", result, err)
	}
	if calls := store.acceptanceSnapshot(); len(calls) != 0 {
		t.Fatalf("completed replay consumed twice: %#v", calls)
	}
	fixture.store.mu.Lock()
	episodeLog := append([]string(nil), fixture.store.episodeLog...)
	parentLog := append([]string(nil), fixture.store.parentLog...)
	fixture.store.mu.Unlock()
	if len(episodeLog) != 0 || len(parentLog) != 0 {
		t.Fatalf("already consumed subscription was advanced by legacy path: episodes=%v parent=%v", episodeLog, parentLog)
	}
	if calls := hook.snapshot(); len(calls) != 1 || calls[0].ID != fixture.args.IntentID {
		t.Fatalf("completion hook calls = %#v", calls)
	}
}

func TestHandlerConcurrentCASAllowsOneAdd(t *testing.T) {
	fixture := newHandlerFixture(t, downloader.IntentPlanned, "42")
	provider, err := NewSubmitterRegistry(map[string]TorrentSubmitter{handlerTestDownloaderID: fixture.submitter})
	if err != nil {
		t.Fatal(err)
	}
	secondHandler, err := NewHandler(fixture.store, handlerResolverProvider{resolver: fixture.resolver}, provider)
	if err != nil {
		t.Fatal(err)
	}
	release := make(chan struct{})
	fixture.resolver.beforeReturn = func() {
		if fixture.resolver.calls.Add(1) == 2 {
			close(release)
		}
		<-release
	}
	fixture.resolver.countInResolve = false

	results := make(chan jobs.DownloadResult, 2)
	errorsCh := make(chan error, 2)
	var wait sync.WaitGroup
	for _, handler := range []*Handler{fixture.handler, secondHandler} {
		wait.Add(1)
		go func(handler *Handler) {
			defer wait.Done()
			result, err := handler.HandleDownload(t.Context(), fixture.args)
			results <- result
			errorsCh <- err
		}(handler)
	}
	wait.Wait()
	close(results)
	close(errorsCh)
	for err := range errorsCh {
		if err != nil {
			t.Fatalf("并发 HandleDownload() error = %v", err)
		}
	}
	for result := range results {
		if result.Action != jobs.DownloadActionComplete && result.Action != jobs.DownloadActionSnooze {
			t.Fatalf("并发结果 = %#v", result)
		}
	}
	if fixture.submitter.addCalls.Load() != 1 {
		t.Fatalf("并发 Add 次数 = %d, want 1", fixture.submitter.addCalls.Load())
	}
	transitions := fixture.store.transitions()
	if len(transitions) != 2 || transitions[0] != "planned->submitting" || transitions[1] != "submitting->downloading" {
		t.Fatalf("并发 CAS transitions = %#v", transitions)
	}
	events := fixture.store.eventsSnapshot()
	wantEvents := []string{"cas:planned->submitting", "add", "cas:submitting->downloading"}
	if !slices.Equal(events, wantEvents) {
		t.Fatalf("并发执行事件 = %#v, want %#v", events, wantEvents)
	}
}

func TestHandlerUnknownAddResultSnoozesWithoutRetryBudget(t *testing.T) {
	t.Parallel()

	fixture := newHandlerFixture(t, downloader.IntentPlanned, "42")
	fixture.submitter.addErr = downloader.ErrReconciliationPending
	result, err := fixture.handler.HandleDownload(t.Context(), fixture.args)
	if err != nil {
		t.Fatalf("HandleDownload() error = %v", err)
	}
	if result.Action != jobs.DownloadActionSnooze || result.SnoozeFor != reconciliationSnooze {
		t.Fatalf("HandleDownload() result = %#v", result)
	}
	if got := fixture.store.snapshot(); got.State != downloader.IntentSubmitting {
		t.Fatalf("未知 Add 结果状态 = %s, want submitting", got.State)
	}
}

func TestHandlerDefinitiveRejectionRecoversSubscription(t *testing.T) {
	t.Parallel()

	fixture := newHandlerFixture(t, downloader.IntentPlanned, "42")
	fixture.submitter.addErr = downloader.ErrDefinitiveAddFailure
	result, err := fixture.handler.HandleDownload(t.Context(), fixture.args)
	if err != nil || result.Action != jobs.DownloadActionCancel {
		t.Fatalf("HandleDownload() = %#v, %v", result, err)
	}
	if got := fixture.store.snapshot(); got.State != downloader.IntentFailed {
		t.Fatalf("明确拒绝状态 = %s, want failed", got.State)
	}
	fixture.store.mu.Lock()
	recoveries := append([]string(nil), fixture.store.recoveryLog...)
	fixture.store.mu.Unlock()
	if !slices.Equal(recoveries, []string{"submitting"}) {
		t.Fatalf("recovery calls = %v, want [submitting]", recoveries)
	}
}

func TestHandlerDefinitiveRejectionSanitizesRecoveryFailure(t *testing.T) {
	t.Parallel()
	const sensitiveMarker = "recovery-database-sensitive-marker"
	fixture := newHandlerFixture(t, downloader.IntentPlanned, "42")
	fixture.submitter.addErr = downloader.ErrDefinitiveAddFailure
	fixture.store.recoveryErr = errors.New(sensitiveMarker)

	result, err := fixture.handler.HandleDownload(t.Context(), fixture.args)
	if result.Action != jobs.DownloadActionRetry || !errors.Is(err, ErrDownloadStateUnavailable) {
		t.Fatalf("HandleDownload() = %#v, %v", result, err)
	}
	if strings.Contains(err.Error(), sensitiveMarker) {
		t.Fatalf("recovery error leaked: %v", err)
	}
}

func TestHandlerRetryExhaustedRecoversOnlyPreSubmissionIntents(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name        string
		state       downloader.IntentState
		wantRecover bool
	}{
		{name: "planned", state: downloader.IntentPlanned, wantRecover: true},
		{name: "failed", state: downloader.IntentFailed, wantRecover: true},
		{name: "submitting", state: downloader.IntentSubmitting},
		{name: "downloading", state: downloader.IntentDownloading},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			fixture := newHandlerFixture(t, tt.state, "42")
			if err := fixture.handler.HandleDownloadRetryExhausted(t.Context(), fixture.args); err != nil {
				t.Fatalf("HandleDownloadRetryExhausted() error = %v", err)
			}

			calls := fixture.store.recoverySnapshot()
			wantCalls := 0
			if tt.wantRecover {
				wantCalls = 1
			}
			if len(calls) != wantCalls {
				t.Fatalf("recovery calls = %#v, want %d", calls, wantCalls)
			}
			if tt.wantRecover {
				call := calls[0]
				wantStates := []downloader.IntentState{downloader.IntentPlanned, downloader.IntentFailed}
				if call.username != "alice" || call.id != fixture.args.IntentID ||
					!slices.Equal(call.fromStates, wantStates) || call.expectedUpdatedAt != nil {
					t.Fatalf("recovery call = %#v", call)
				}
			}

			wantState := tt.state
			if tt.wantRecover {
				wantState = downloader.IntentFailed
			}
			if got := fixture.store.snapshot().State; got != wantState {
				t.Fatalf("intent state = %s, want %s", got, wantState)
			}
		})
	}
}

func TestHandlerRetryExhaustedSanitizesStorageFailures(t *testing.T) {
	t.Parallel()

	const sensitiveMarker = "retry-exhausted-storage-sensitive-marker"
	tests := []struct {
		name        string
		configure   func(*handlerTestStore)
		wantErr     error
		wantRecover int
	}{
		{
			name: "intent lookup failure",
			configure: func(store *handlerTestStore) {
				store.intentErr = errors.New(sensitiveMarker)
			},
			wantErr: ErrDownloadStateUnavailable,
		},
		{
			name: "recovery failure",
			configure: func(store *handlerTestStore) {
				store.recoveryErr = errors.New(sensitiveMarker)
			},
			wantErr:     ErrDownloadStateUnavailable,
			wantRecover: 1,
		},
		{
			name: "intent disappeared",
			configure: func(store *handlerTestStore) {
				store.intentErr = database.ErrDownloadIntentNotFound
			},
		},
		{
			name: "recovery lost state race",
			configure: func(store *handlerTestStore) {
				store.recoveryErr = database.ErrDownloadIntentStateConflict
			},
			wantRecover: 1,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			fixture := newHandlerFixture(t, downloader.IntentPlanned, "42")
			tt.configure(fixture.store)
			err := fixture.handler.HandleDownloadRetryExhausted(t.Context(), fixture.args)
			if !errors.Is(err, tt.wantErr) {
				t.Fatalf("HandleDownloadRetryExhausted() error = %v, want %v", err, tt.wantErr)
			}
			if err != nil && strings.Contains(err.Error(), sensitiveMarker) {
				t.Fatalf("HandleDownloadRetryExhausted() leaked storage error: %v", err)
			}
			if got := len(fixture.store.recoverySnapshot()); got != tt.wantRecover {
				t.Fatalf("recovery calls = %d, want %d", got, tt.wantRecover)
			}
		})
	}
}

func TestHandlerRejectsInvalidPersistentInputsWithoutSideEffects(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name   string
		mutate func(*handlerFixture)
	}{
		{
			name: "资源秘密摘要不匹配",
			mutate: func(f *handlerFixture) {
				f.store.plan.ResourceSecret += "tampered"
			},
		},
		{
			name: "候选关联键不匹配",
			mutate: func(f *handlerFixture) {
				f.store.intent.CandidateKey = "sha256:" + strings.Repeat("0", 64)
			},
		},
		{
			name: "执行计划不存在",
			mutate: func(f *handlerFixture) {
				f.store.planErr = database.ErrDownloadIntentPlanNotFound
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			fixture := newHandlerFixture(t, downloader.IntentPlanned, "opaque-sensitive-locator")
			tt.mutate(fixture)
			result, err := fixture.handler.HandleDownload(t.Context(), fixture.args)
			if err != nil || result.Action != jobs.DownloadActionCancel {
				t.Fatalf("HandleDownload() = %#v, %v", result, err)
			}
			if fixture.resolver.calls.Load() != 0 || fixture.submitter.addCalls.Load() != 0 || len(fixture.store.transitions()) != 0 {
				t.Fatal("无效持久输入产生了外部或状态副作用")
			}
		})
	}
}

func TestHandlerErrorsDoNotLeakSensitiveInputs(t *testing.T) {
	t.Parallel()

	const (
		locatorSecret  = "https://tracker.invalid/download?passkey=locator-secret"
		metainfoSecret = "not-bencode-metainfo-secret"
		savePathSecret = "D:\\Private\\save-path-secret"
	)
	tests := []struct {
		name         string
		locator      string
		sensitive    string
		configure    func(*handlerFixture)
		wantErr      error
		wantAddCalls int32
		wantCASCalls int
	}{
		{
			name:      "locator",
			locator:   locatorSecret,
			sensitive: locatorSecret,
			configure: func(f *handlerFixture) {
				f.resolver.err = errors.New("resolver failed for " + locatorSecret)
			},
			wantErr: ErrDownloadSourceUnavailable,
		},
		{
			name:      "metainfo",
			locator:   "42",
			sensitive: metainfoSecret,
			configure: func(f *handlerFixture) {
				f.resolver.body = []byte(metainfoSecret)
			},
			wantErr: ErrDownloadMetainfoInvalid,
		},
		{
			name:      "save path",
			locator:   "42",
			sensitive: savePathSecret,
			configure: func(f *handlerFixture) {
				f.store.plan.SavePath = savePathSecret
				f.submitter.addErr = errors.New("downloader failed for " + savePathSecret)
			},
			wantErr:      ErrDownloadStateUnavailable,
			wantAddCalls: 1,
			wantCASCalls: 1,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			fixture := newHandlerFixture(t, downloader.IntentPlanned, tt.locator)
			tt.configure(fixture)
			result, err := fixture.handler.HandleDownload(t.Context(), fixture.args)
			if !errors.Is(err, tt.wantErr) || result.Action != jobs.DownloadActionRetry {
				t.Fatalf("HandleDownload() = %#v, %v, want retry with %v", result, err, tt.wantErr)
			}
			if strings.Contains(err.Error(), tt.sensitive) {
				t.Fatalf("HandleDownload() error 泄露 %s: %q", tt.name, err)
			}
			if got := fixture.submitter.addCalls.Load(); got != tt.wantAddCalls {
				t.Fatalf("Add calls = %d, want %d", got, tt.wantAddCalls)
			}
			if got := len(fixture.store.transitions()); got != tt.wantCASCalls {
				t.Fatalf("CAS calls = %d, want %d", got, tt.wantCASCalls)
			}
		})
	}
}

const (
	handlerTestDownloaderID = "12e2fe2b-2ed7-4c6a-b5ba-1c1c0f7a3ad8"
	handlerTestHash         = "0123456789ABCDEF0123456789ABCDEF01234567"
)

type handlerFixture struct {
	handler   *Handler
	store     *handlerTestStore
	resolver  *handlerTestResolver
	submitter *handlerTestSubmitter
	args      jobs.DownloadArgs
}

func newHandlerFixture(t *testing.T, state downloader.IntentState, locator string) *handlerFixture {
	t.Helper()
	resourceID, resourceSecret, err := ProtectResourceLocator(locator)
	if err != nil {
		t.Fatal(err)
	}
	plan := database.DownloadIntentPlan{
		SiteID: 7, DownloaderID: handlerTestDownloaderID, ResolverKind: "fixture",
		ResolverVersion: 1, ResourceID: resourceID, ResourceSecret: resourceSecret,
		SavePath: "/downloads", Paused: true,
	}
	candidate, err := (SourceReference{
		SiteID: plan.SiteID, ResolverKind: plan.ResolverKind, ResolverVersion: plan.ResolverVersion,
		ResourceID: plan.ResourceID, DownloaderID: plan.DownloaderID,
	}).Candidate(plan.SavePath, plan.Paused)
	if err != nil {
		t.Fatal(err)
	}
	intent, err := downloader.NewIntent("alice", candidate)
	if err != nil {
		t.Fatal(err)
	}
	intent.State = state
	plan.IntentID = intent.ID
	store := &handlerTestStore{intent: intent, plan: plan}
	resolver := &handlerTestResolver{body: validHandlerMetainfo(t), countInResolve: true}
	submitter := &handlerTestSubmitter{eventStore: store}
	provider, err := NewSubmitterRegistry(map[string]TorrentSubmitter{handlerTestDownloaderID: submitter})
	if err != nil {
		t.Fatal(err)
	}
	handler, err := NewHandler(store, handlerResolverProvider{resolver: resolver}, provider)
	if err != nil {
		t.Fatal(err)
	}
	return &handlerFixture{
		handler: handler, store: store, resolver: resolver, submitter: submitter,
		args: jobs.DownloadArgs{Username: " alice ", IntentID: intent.ID},
	}
}

func newHandlerTestHandler(
	t *testing.T,
	store IntentStore,
	resolver indexer.VersionedDownloadResolver,
	submitter TorrentSubmitter,
	completionHooks ...DownloadCompletionHook,
) *Handler {
	t.Helper()
	provider, err := NewSubmitterRegistry(map[string]TorrentSubmitter{handlerTestDownloaderID: submitter})
	if err != nil {
		t.Fatal(err)
	}
	handler, err := NewHandler(store, handlerResolverProvider{resolver: resolver}, provider, completionHooks...)
	if err != nil {
		t.Fatal(err)
	}
	return handler
}

type handlerTestStore struct {
	mu             sync.Mutex
	intent         downloader.Intent
	plan           database.DownloadIntentPlan
	intentErr      error
	planErr        error
	casLog         []string
	events         []string
	episodeErr     error
	episodeRows    *int64
	episodeLog     []string
	parentErr      error
	parentLog      []string
	parentState    subscription.State
	parentStateSet bool
	recoveryErr    error
	recoveryLog    []string
	recoveryCalls  []handlerRecoveryCall
}

type handlerRecoveryCall struct {
	username          string
	id                uuid.UUID
	fromStates        []downloader.IntentState
	expectedUpdatedAt *time.Time
}

type handlerAcceptanceCall struct {
	username    string
	id          uuid.UUID
	torrentHash string
}

type handlerAcceptanceStore struct {
	*handlerTestStore
	accept      func(downloader.Intent, string) (downloader.Intent, error)
	acceptErr   error
	acceptCalls []handlerAcceptanceCall
	consumed    bool
	notReleased bool
}

func (s *handlerAcceptanceStore) AcceptDownloadIntent(
	_ context.Context,
	username string,
	id uuid.UUID,
	torrentHash string,
) (downloader.Intent, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.acceptCalls = append(s.acceptCalls, handlerAcceptanceCall{
		username: username, id: id, torrentHash: torrentHash,
	})
	s.events = append(s.events, "accept")
	if s.acceptErr != nil {
		return downloader.Intent{}, false, s.acceptErr
	}
	if s.accept != nil {
		accepted, err := s.accept(s.intent, torrentHash)
		return accepted, err == nil && !s.notReleased, err
	}
	if username != "alice" || id != s.intent.ID ||
		(s.intent.State != downloader.IntentSubmitting && s.intent.State != downloader.IntentDownloading) {
		return downloader.Intent{}, false, database.ErrDownloadIntentStateConflict
	}
	if s.intent.TorrentHash != "" {
		storedHash, storedErr := downloader.NormalizeTorrentHash(s.intent.TorrentHash)
		acceptedHash, acceptedErr := downloader.NormalizeTorrentHash(torrentHash)
		if storedErr != nil || acceptedErr != nil || storedHash != acceptedHash {
			return downloader.Intent{}, false, database.ErrDownloadIntentStateConflict
		}
	}
	updated, err := s.intent.WithTorrentHash(torrentHash)
	if err != nil {
		return downloader.Intent{}, false, err
	}
	updated, err = updated.Transition(downloader.IntentDownloading)
	if err != nil {
		return downloader.Intent{}, false, err
	}
	s.intent = updated
	s.consumed = true
	return updated, !s.notReleased, nil
}

func (s *handlerAcceptanceStore) TransitionSubscriptionEpisodesByIntent(
	ctx context.Context,
	username string,
	id uuid.UUID,
	from []subscription.SubscriptionEpisodeState,
	next subscription.SubscriptionEpisodeState,
) (int64, error) {
	s.mu.Lock()
	consumed := s.consumed
	s.mu.Unlock()
	if consumed {
		return 0, database.ErrSubscriptionEpisodeNotFound
	}
	return s.handlerTestStore.TransitionSubscriptionEpisodesByIntent(ctx, username, id, from, next)
}

func (s *handlerAcceptanceStore) CompareAndSwapSubscriptionStateByIntent(
	ctx context.Context,
	username string,
	id uuid.UUID,
	current subscription.State,
	next subscription.State,
) error {
	s.mu.Lock()
	consumed := s.consumed
	s.mu.Unlock()
	if consumed {
		return database.ErrSubscriptionDownloadStateNotFound
	}
	return s.handlerTestStore.CompareAndSwapSubscriptionStateByIntent(ctx, username, id, current, next)
}

func (s *handlerAcceptanceStore) acceptanceSnapshot() []handlerAcceptanceCall {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]handlerAcceptanceCall(nil), s.acceptCalls...)
}

func (s *handlerTestStore) GetDownloadIntent(context.Context, string, uuid.UUID) (downloader.Intent, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.intent, s.intentErr
}

func (s *handlerTestStore) GetDownloadIntentPlan(context.Context, string, uuid.UUID) (database.DownloadIntentPlan, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.plan, s.planErr
}

func (s *handlerTestStore) CompareAndSwapDownloadIntent(
	_ context.Context,
	_ string,
	_ uuid.UUID,
	from []downloader.IntentState,
	next downloader.IntentState,
	hash string,
) (downloader.Intent, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	matched := false
	for _, state := range from {
		if s.intent.State == state {
			matched = true
			break
		}
	}
	if !matched {
		return downloader.Intent{}, database.ErrDownloadIntentStateConflict
	}
	previous := s.intent.State
	updated := s.intent
	var err error
	if hash != "" {
		updated, err = updated.WithTorrentHash(hash)
		if err != nil {
			return downloader.Intent{}, err
		}
	}
	updated, err = updated.Transition(next)
	if err != nil {
		return downloader.Intent{}, err
	}
	s.intent = updated
	s.casLog = append(s.casLog, string(previous)+"->"+string(next))
	s.events = append(s.events, "cas:"+string(previous)+"->"+string(next))
	return updated, nil
}

func (s *handlerTestStore) TransitionSubscriptionEpisodesByIntent(
	_ context.Context,
	_ string,
	_ uuid.UUID,
	fromStates []subscription.SubscriptionEpisodeState,
	next subscription.SubscriptionEpisodeState,
) (int64, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.episodeErr != nil {
		return 0, s.episodeErr
	}
	from := make([]string, 0, len(fromStates))
	for _, state := range fromStates {
		from = append(from, string(state))
	}
	s.episodeLog = append(s.episodeLog, strings.Join(from, "|")+"->"+string(next))
	if s.episodeRows != nil {
		return *s.episodeRows, nil
	}
	return 1, nil
}

func (s *handlerTestStore) CompareAndSwapSubscriptionStateByIntent(
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
	s.parentLog = append(s.parentLog, string(current)+"->"+string(next))
	return nil
}

func (s *handlerTestStore) RecoverSubscriptionDownloadIntent(
	_ context.Context,
	username string,
	id uuid.UUID,
	fromStates []downloader.IntentState,
	expectedUpdatedAt *time.Time,
) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	var expectedCopy *time.Time
	if expectedUpdatedAt != nil {
		value := expectedUpdatedAt.UTC()
		expectedCopy = &value
	}
	s.recoveryCalls = append(s.recoveryCalls, handlerRecoveryCall{
		username:          username,
		id:                id,
		fromStates:        append([]downloader.IntentState(nil), fromStates...),
		expectedUpdatedAt: expectedCopy,
	})
	if s.recoveryErr != nil {
		return s.recoveryErr
	}
	if expectedUpdatedAt != nil {
		return database.ErrInvalidDownloadIntent
	}
	for _, state := range fromStates {
		if s.intent.State == state {
			s.intent.State = downloader.IntentFailed
			s.recoveryLog = append(s.recoveryLog, string(state))
			return nil
		}
	}
	return database.ErrDownloadIntentStateConflict
}

func (s *handlerTestStore) recoverySnapshot() []handlerRecoveryCall {
	s.mu.Lock()
	defer s.mu.Unlock()
	result := make([]handlerRecoveryCall, len(s.recoveryCalls))
	for index, call := range s.recoveryCalls {
		result[index] = call
		result[index].fromStates = append([]downloader.IntentState(nil), call.fromStates...)
		if call.expectedUpdatedAt != nil {
			value := *call.expectedUpdatedAt
			result[index].expectedUpdatedAt = &value
		}
	}
	return result
}

func (s *handlerTestStore) snapshot() downloader.Intent {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.intent
}

func (s *handlerTestStore) transitions() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.casLog...)
}

func (s *handlerTestStore) recordEvent(event string) {
	s.mu.Lock()
	s.events = append(s.events, event)
	s.mu.Unlock()
}

func (s *handlerTestStore) eventsSnapshot() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.events...)
}

func (s *handlerTestStore) parentSubscriptionState() subscription.State {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.parentState
}

type handlerResolverProvider struct {
	resolver indexer.VersionedDownloadResolver
	err      error
}

func (p handlerResolverProvider) ResolveDownloadResolver(int, string, int16) (indexer.VersionedDownloadResolver, error) {
	return p.resolver, p.err
}

type handlerTestResolver struct {
	body           []byte
	err            error
	calls          atomic.Int32
	beforeReturn   func()
	countInResolve bool
}

func (*handlerTestResolver) Name() string { return "fixture" }
func (*handlerTestResolver) Search(context.Context, indexer.Query) ([]indexer.Resource, error) {
	return nil, nil
}
func (*handlerTestResolver) SiteID() int            { return 7 }
func (*handlerTestResolver) ResolverKind() string   { return "fixture" }
func (*handlerTestResolver) ResolverVersion() int16 { return 1 }
func (r *handlerTestResolver) ResolveDownload(context.Context, string) (indexer.ResolvedDownload, error) {
	if r.countInResolve {
		r.calls.Add(1)
	}
	if r.beforeReturn != nil {
		r.beforeReturn()
	}
	return indexer.ResolvedDownload{Body: r.body}, r.err
}

type handlerTestSubmitter struct {
	addCalls   atomic.Int32
	addErr     error
	reconcile  func(downloader.Intent) (downloader.Intent, bool, error)
	eventStore *handlerTestStore
}

func (s *handlerTestSubmitter) ObserveTasks(context.Context, []string) (map[string]downloader.TaskObservation, error) {
	return map[string]downloader.TaskObservation{}, nil
}

func (s *handlerTestSubmitter) Reconcile(_ context.Context, intent downloader.Intent) (downloader.Intent, bool, error) {
	if s.reconcile != nil {
		return s.reconcile(intent)
	}
	return intent, false, nil
}

func (s *handlerTestSubmitter) AddOnce(
	_ context.Context,
	intent downloader.Intent,
	_ downloader.Candidate,
	_ []byte,
) (downloader.Intent, error) {
	s.addCalls.Add(1)
	if s.eventStore != nil {
		s.eventStore.recordEvent("add")
	}
	if errors.Is(s.addErr, downloader.ErrDefinitiveAddFailure) {
		failed, err := intent.Transition(downloader.IntentFailed)
		return failed, errors.Join(s.addErr, err)
	}
	if s.addErr != nil {
		return intent, s.addErr
	}
	return intent.Transition(downloader.IntentDownloading)
}

func downloadingIntent(t *testing.T, intent downloader.Intent, hash string) downloader.Intent {
	t.Helper()
	updated, err := intent.WithTorrentHash(hash)
	if err != nil {
		t.Fatal(err)
	}
	if updated.State == downloader.IntentPlanned || updated.State == downloader.IntentFailed {
		updated, err = updated.Transition(downloader.IntentSubmitting)
		if err != nil {
			t.Fatal(err)
		}
	}
	updated, err = updated.Transition(downloader.IntentDownloading)
	if err != nil {
		t.Fatal(err)
	}
	return updated
}

func validHandlerMetainfo(t *testing.T) []byte {
	t.Helper()
	var buffer bytes.Buffer
	err := bencode.NewEncoder(&buffer).Encode(map[string]any{
		"info": map[string]any{
			"length": int64(1), "name": "fixture.mkv", "piece length": int64(16 << 10),
			"pieces": strings.Repeat("a", 20),
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	return buffer.Bytes()
}
