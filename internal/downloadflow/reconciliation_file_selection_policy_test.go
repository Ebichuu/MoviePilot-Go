package downloadflow

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/moviepilot-go/moviepilot-go/internal/database"
	"github.com/moviepilot-go/moviepilot-go/internal/downloader"
	"github.com/moviepilot-go/moviepilot-go/internal/organization"
)

func TestReconciliationUsesPersistedFileSelectionBeforeAcceptance(t *testing.T) {
	t.Parallel()

	policies := []struct {
		name     string
		episodes []downloader.Episode
	}{
		{name: "整包不选文件"},
		{name: "标题覆盖不选文件"},
		{name: "拆包选择目标", episodes: []downloader.Episode{
			{Season: 2, Episode: 3}, {Season: 1, Episode: 7}, {Season: 2, Episode: 3},
		}},
	}
	observations := []downloader.TaskObservationState{
		downloader.TaskObservationActive,
		downloader.TaskObservationPaused,
		downloader.TaskObservationCompleted,
	}
	for _, policy := range policies {
		for _, state := range []downloader.IntentState{downloader.IntentSubmitting, downloader.IntentDownloading} {
			for _, observation := range observations {
				t.Run(policy.name+"/"+string(state)+"/"+string(observation), func(t *testing.T) {
					t.Parallel()

					store, target, cutoff := newReconciliationFileSelectionPolicyFixture(state)
					store.episodes = slices.Clone(policy.episodes)
					coverage := slices.Clone(store.input.Episodes)
					observer := &reconciliationTestSubmitter{states: map[string]downloader.TaskObservationState{
						target.TorrentHash: observation,
					}}
					var (
						submitter TorrentSubmitter = observer
						selector  *reconciliationEpisodeSelectingSubmitter
					)
					if len(policy.episodes) > 0 {
						selector = &reconciliationEpisodeSelectingSubmitter{
							reconciliationTestSubmitter: observer,
							eventStore:                  store.reconciliationTestStore,
						}
						submitter = selector
					} else if _, supported := submitter.(EpisodeFileSelectionSubmitter); supported {
						t.Fatal("无选集测试的下载器不能提供文件选择能力")
					}
					handler := newReconciliationTestHandler(t, store, map[string]TorrentSubmitter{target.DownloaderID: submitter})
					if err := handler.HandleDownloadReconciliation(t.Context(), cutoff); err != nil {
						t.Fatalf("HandleDownloadReconciliation() = %v", err)
					}
					if store.selectionReads != 1 || store.successInputReads != 0 ||
						store.selectionUsername != target.Username || store.selectionIntentID != target.ID {
						t.Fatalf("policy reads = %d, success input reads = %d, owner/id = %q/%s",
							store.selectionReads, store.successInputReads, store.selectionUsername, store.selectionIntentID)
					}
					if !slices.Equal(store.input.Episodes, coverage) {
						t.Fatalf("成功记录的质量/通知覆盖被改写: %#v", store.input.Episodes)
					}
					wantEvents := []string{"accept"}
					if selector != nil {
						want := []downloader.Episode{{Season: 1, Episode: 7}, {Season: 2, Episode: 3}}
						if selector.selectionCalls != 1 || !slices.Equal(selector.selectedEpisodes, want) ||
							selector.selectedIntent.TorrentHash != target.TorrentHash {
							t.Fatalf("selection = %d/%#v, intent = %#v", selector.selectionCalls, selector.selectedEpisodes, selector.selectedIntent)
						}
						wantEvents = []string{"select", "accept"}
					}
					if observation == downloader.TaskObservationCompleted {
						wantEvents = append(wantEvents, "cas:downloading->completed")
					}
					if events := store.eventSnapshot(); !slices.Equal(events, wantEvents) {
						t.Fatalf("events = %#v, want %#v", events, wantEvents)
					}
					if calls := store.acceptanceSnapshot(); len(calls) != 1 || !store.consumed[target.ID] {
						t.Fatalf("acceptance = %#v, consumed = %t", calls, store.consumed[target.ID])
					}
				})
			}
		}
	}
}

func TestReconciliationFileSelectionLookupCannotSilentlyFallBack(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name       string
		episodes   []downloader.Episode
		lookupErr  error
		wantErr    error
		wantAccept bool
	}{
		{name: "历史成功输入缺失", lookupErr: database.ErrDownloadIntentSuccessInputNotFound, wantAccept: true},
		{name: "计划缺失", lookupErr: database.ErrDownloadIntentPlanNotFound, wantErr: ErrDownloadReconciliationUnavailable},
		{name: "计划查询失败", lookupErr: errors.Join(database.ErrDownloadIntentPlanPersistence, errors.New("private-policy-marker")), wantErr: ErrDownloadReconciliationUnavailable},
		{name: "非法坐标", episodes: []downloader.Episode{{Season: 1, Episode: 0}}, wantErr: ErrDownloadReconciliationUnavailable},
		{name: "取消传播", lookupErr: errors.Join(context.Canceled, errors.New("private-policy-marker")), wantErr: context.Canceled},
		{name: "超时传播", lookupErr: context.DeadlineExceeded, wantErr: context.DeadlineExceeded},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			store, target, cutoff := newReconciliationFileSelectionPolicyFixture(downloader.IntentSubmitting)
			store.episodes, store.lookupErr = slices.Clone(tt.episodes), tt.lookupErr
			observer := &reconciliationTestSubmitter{states: map[string]downloader.TaskObservationState{
				target.TorrentHash: downloader.TaskObservationActive,
			}}
			handler := newReconciliationTestHandler(t, store, map[string]TorrentSubmitter{target.DownloaderID: observer})
			err := handler.HandleDownloadReconciliation(t.Context(), cutoff)
			if errors.Is(tt.wantErr, context.Canceled) || errors.Is(tt.wantErr, context.DeadlineExceeded) {
				if !errors.Is(err, tt.wantErr) {
					t.Fatalf("HandleDownloadReconciliation() = %v, want %v", err, tt.wantErr)
				}
			} else {
				if err != nil {
					t.Fatalf("单目标失败被隔离，轮次不应失败: %v", err)
				}
				if tt.wantErr != nil {
					failure := requireReconciliationFailure(t, handler, target.ID, reconcileStageEpisodeSelection)
					if !errors.Is(failure.Err, tt.wantErr) ||
						strings.Contains(failure.Err.Error(), "private-policy-marker") {
						t.Fatalf("failure = %+v, want stable %v without leak", failure, tt.wantErr)
					}
				}
			}
			if store.successInputReads != 0 {
				t.Fatal("读取策略失败后回退到了成功覆盖数据")
			}
			if !tt.wantAccept && (len(store.acceptanceSnapshot()) != 0 || len(store.transitionSnapshot()) != 0) {
				t.Fatal("读取策略失败推进了 acceptance 或 intent 状态")
			}
			if tt.wantAccept && len(store.acceptanceSnapshot()) != 1 {
				t.Fatalf("acceptance calls = %#v", store.acceptanceSnapshot())
			}
		})
	}
}

func TestReconciliationUnpackFileSelectionFailureKeepsAcceptancePending(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name      string
		selection error
		wantErr   error
	}{
		{name: "文件尚未就绪", selection: downloader.ErrEpisodeSelectionPending, wantErr: ErrDownloadReconciliationUnavailable},
		{name: "目标没有匹配", selection: downloader.ErrEpisodeSelectionNoMatch, wantErr: ErrDownloadReconciliationUnavailable},
		{name: "下载器选集失败", selection: downloader.ErrEpisodeSelectionFailed, wantErr: ErrDownloadReconciliationUnavailable},
		{name: "取消传播", selection: context.Canceled, wantErr: context.Canceled},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			store, target, cutoff := newReconciliationFileSelectionPolicyFixture(downloader.IntentSubmitting)
			store.episodes = []downloader.Episode{{Season: 1, Episode: 7}}
			submitter := &reconciliationEpisodeSelectingSubmitter{
				reconciliationTestSubmitter: &reconciliationTestSubmitter{states: map[string]downloader.TaskObservationState{
					target.TorrentHash: downloader.TaskObservationActive,
				}},
				selectionErr: tt.selection,
			}
			handler := newReconciliationTestHandler(t, store, map[string]TorrentSubmitter{target.DownloaderID: submitter})
			err := handler.HandleDownloadReconciliation(t.Context(), cutoff)
			requireIsolatedReconciliationFailure(
				t, handler, err, tt.wantErr, target.ID, reconcileStageEpisodeSelection,
			)
			if submitter.selectionCalls != 1 || len(store.acceptanceSnapshot()) != 0 || len(store.transitionSnapshot()) != 0 {
				t.Fatalf("selection/accept/CAS = %d/%d/%#v", submitter.selectionCalls, len(store.acceptanceSnapshot()), store.transitionSnapshot())
			}
		})
	}
}

type reconciliationFileSelectionPolicyStore struct {
	*reconciliationAcceptanceStore
	input             organization.SuccessInput
	episodes          []downloader.Episode
	lookupErr         error
	selectionReads    int
	successInputReads int
	selectionUsername string
	selectionIntentID uuid.UUID
}

func (s *reconciliationFileSelectionPolicyStore) GetDownloadIntentFileSelection(
	_ context.Context,
	username string,
	intentID uuid.UUID,
) ([]downloader.Episode, error) {
	s.selectionReads++
	s.selectionUsername, s.selectionIntentID = username, intentID
	return slices.Clone(s.episodes), s.lookupErr
}

func (s *reconciliationFileSelectionPolicyStore) GetDownloadIntentSuccessInput(
	_ context.Context,
	_ string,
	_ uuid.UUID,
) (organization.SuccessInput, error) {
	s.successInputReads++
	return s.input.Clone(), nil
}

func newReconciliationFileSelectionPolicyFixture(
	state downloader.IntentState,
) (*reconciliationFileSelectionPolicyStore, downloader.ReconciliationTarget, time.Time) {
	cutoff := time.Date(2026, 10, 4, 0, 0, 0, 0, time.UTC)
	target := reconciliationTarget(1, handlerTestDownloaderID, state, cutoff.Add(-time.Minute), "a")
	store := &reconciliationFileSelectionPolicyStore{
		reconciliationAcceptanceStore: &reconciliationAcceptanceStore{
			reconciliationTestStore: &reconciliationTestStore{pages: [][]downloader.ReconciliationTarget{{target}}},
			consumed:                make(map[uuid.UUID]bool),
		},
		input: organization.SuccessInput{
			IntentID: target.ID,
			Username: target.Username,
			Episodes: []organization.Episode{{Season: 1, Episode: 1}, {Season: 1, Episode: 7}, {Season: 2, Episode: 3}},
		},
	}
	return store, target, cutoff
}
