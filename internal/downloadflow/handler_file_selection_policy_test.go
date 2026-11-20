package downloadflow

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/moviepilot-go/moviepilot-go/internal/database"
	"github.com/moviepilot-go/moviepilot-go/internal/downloader"
	"github.com/moviepilot-go/moviepilot-go/internal/jobs"
	"github.com/moviepilot-go/moviepilot-go/internal/organization"
)

func TestHandlerUsesPersistedFileSelectionAcrossSubmissionAndReplay(t *testing.T) {
	t.Parallel()

	policies := []struct {
		name     string
		episodes []downloader.Episode
	}{
		{name: "整包保留覆盖但不选文件"},
		{name: "标题覆盖保留覆盖但不选文件"},
		{
			name: "拆包只选择持久化目标",
			episodes: []downloader.Episode{
				{Season: 2, Episode: 3}, {Season: 1, Episode: 7}, {Season: 2, Episode: 3},
			},
		},
	}
	paths := []struct {
		name  string
		state downloader.IntentState
	}{
		{name: "首次提交", state: downloader.IntentPlanned},
		{name: "hash对账重放", state: downloader.IntentSubmitting},
		{name: "downloading补偿", state: downloader.IntentDownloading},
	}
	for _, policy := range policies {
		for _, path := range paths {
			for _, synchronous := range []bool{false, true} {
				mode := "River"
				if synchronous {
					mode = "同步V2"
				}
				t.Run(policy.name+"/"+path.name+"/"+mode, func(t *testing.T) {
					t.Parallel()

					fixture, store := newHandlerFileSelectionPolicyFixture(t, path.state)
					store.episodes = slices.Clone(policy.episodes)
					coverage := slices.Clone(store.input.Episodes)
					var (
						submitter TorrentSubmitter
						selector  *handlerEpisodeSelectingSubmitter
						recorder  *handlerFileSelectionRecorder
					)
					if len(policy.episodes) > 0 {
						selector = &handlerEpisodeSelectingSubmitter{base: fixture.submitter}
						submitter = selector
					} else {
						recorder = &handlerFileSelectionRecorder{handlerTestSubmitter: fixture.submitter}
						submitter = recorder
						if _, supported := submitter.(EpisodeFileSelectionSubmitter); supported {
							t.Fatal("无选集测试的下载器不能提供文件选择能力")
						}
					}
					fixture.handler = newHandlerTestHandler(t, store, fixture.resolver, submitter)
					if synchronous {
						result, err := fixture.handler.ExecuteIntent(t.Context(), fixture.args.Username, fixture.args.IntentID)
						if err != nil || result.Outcome != IntentExecutionAccepted {
							t.Fatalf("ExecuteIntent() = %#v, %v", result, err)
						}
					} else {
						result, err := fixture.handler.HandleDownload(t.Context(), fixture.args)
						if err != nil || result.Action != jobs.DownloadActionComplete {
							t.Fatalf("HandleDownload() = %#v, %v", result, err)
						}
					}
					if store.selectionReads != 1 || store.successInputReads != 0 ||
						store.selectionUsername != "alice" || store.selectionIntentID != fixture.args.IntentID {
						t.Fatalf("policy reads = %d, success input reads = %d, owner/id = %q/%s",
							store.selectionReads, store.successInputReads, store.selectionUsername, store.selectionIntentID)
					}
					if !slices.Equal(store.input.Episodes, coverage) {
						t.Fatalf("成功记录的质量/通知覆盖被改写: %#v", store.input.Episodes)
					}
					wantEpisodes := []downloader.Episode(nil)
					if len(policy.episodes) > 0 {
						wantEpisodes = []downloader.Episode{{Season: 1, Episode: 7}, {Season: 2, Episode: 3}}
						if selector.selectionCalls != 1 || !slices.Equal(selector.selectedEpisodes, wantEpisodes) {
							t.Fatalf("selection = %d/%#v, want 1/%#v", selector.selectionCalls, selector.selectedEpisodes, wantEpisodes)
						}
					}
					if path.state == downloader.IntentPlanned {
						candidate := downloader.Candidate{}
						if selector != nil {
							candidate = selector.addedCandidate
						} else {
							candidate = recorder.addedCandidate
						}
						if !slices.Equal(candidate.TargetEpisodes, wantEpisodes) || fixture.submitter.addCalls.Load() != 1 {
							t.Fatalf("AddOnce target episodes = %#v, calls = %d", candidate.TargetEpisodes, fixture.submitter.addCalls.Load())
						}
					} else if fixture.submitter.addCalls.Load() != 0 || fixture.resolver.calls.Load() != 0 {
						t.Fatal("重放重新解析了种子或调用 AddOnce")
					}
					if calls := store.acceptanceSnapshot(); len(calls) != 1 || !store.consumed {
						t.Fatalf("acceptance = %#v, consumed = %t", calls, store.consumed)
					}
					if selector != nil {
						events := fixture.store.eventsSnapshot()
						if slices.Index(events, "select") >= slices.Index(events, "accept") {
							t.Fatalf("acceptance 必须发生在文件选择之后: %#v", events)
						}
					}
				})
			}
		}
	}
}

func TestHandlerStagedMetainfoUsesFileSelectionPolicy(t *testing.T) {
	t.Parallel()

	fixture, store := newHandlerFileSelectionPolicyFixture(t, downloader.IntentPlanned)
	submitter := &handlerFileSelectionRecorder{handlerTestSubmitter: fixture.submitter}
	fixture.handler = newHandlerTestHandler(t, store, fixture.resolver, submitter)
	result, err := fixture.handler.ExecuteIntentWithMetainfo(t.Context(), fixture.args.Username, fixture.args.IntentID, validHandlerMetainfo(t))
	if err != nil || result.Outcome != IntentExecutionAccepted {
		t.Fatalf("ExecuteIntentWithMetainfo() = %#v, %v", result, err)
	}
	if len(submitter.addedCandidate.TargetEpisodes) != 0 || fixture.resolver.calls.Load() != 0 ||
		fixture.submitter.addCalls.Load() != 1 || len(store.acceptanceSnapshot()) != 1 {
		t.Fatalf("staged target/resolver/add/accept = %#v/%d/%d/%d", submitter.addedCandidate.TargetEpisodes,
			fixture.resolver.calls.Load(), fixture.submitter.addCalls.Load(), len(store.acceptanceSnapshot()))
	}
}

func TestHandlerFileSelectionLookupCannotSilentlyFallBack(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name       string
		episodes   []downloader.Episode
		lookupErr  error
		wantErr    error
		wantAccept bool
	}{
		{name: "历史成功输入缺失", lookupErr: database.ErrDownloadIntentSuccessInputNotFound, wantAccept: true},
		{name: "计划缺失", lookupErr: database.ErrDownloadIntentPlanNotFound, wantErr: ErrDownloadStateUnavailable},
		{name: "计划查询失败", lookupErr: errors.Join(database.ErrDownloadIntentPlanPersistence, errors.New("private-policy-marker")), wantErr: ErrDownloadStateUnavailable},
		{name: "非法坐标", episodes: []downloader.Episode{{Season: 1, Episode: 0}}, wantErr: ErrDownloadStateUnavailable},
		{name: "取消传播", lookupErr: errors.Join(context.Canceled, errors.New("private-policy-marker")), wantErr: context.Canceled},
		{name: "超时传播", lookupErr: context.DeadlineExceeded, wantErr: context.DeadlineExceeded},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			fixture, store := newHandlerFileSelectionPolicyFixture(t, downloader.IntentPlanned)
			store.episodes, store.lookupErr = slices.Clone(tt.episodes), tt.lookupErr
			submitter := &handlerFileSelectionRecorder{handlerTestSubmitter: fixture.submitter}
			fixture.handler = newHandlerTestHandler(t, store, fixture.resolver, submitter)
			result, err := fixture.handler.HandleDownload(t.Context(), fixture.args)
			if !errors.Is(err, tt.wantErr) ||
				(tt.wantAccept && result.Action != jobs.DownloadActionComplete) ||
				(!tt.wantAccept && result.Action != jobs.DownloadActionRetry) {
				t.Fatalf("HandleDownload() = %#v, %v", result, err)
			}
			if err != nil && strings.Contains(err.Error(), "private-policy-marker") {
				t.Fatalf("policy lookup leaked storage error: %v", err)
			}
			if store.successInputReads != 0 {
				t.Fatal("读取策略失败后回退到了成功覆盖数据")
			}
			if !tt.wantAccept && (fixture.resolver.calls.Load() != 0 || submitter.reconcileCalls != 0 ||
				fixture.submitter.addCalls.Load() != 0 || len(store.acceptanceSnapshot()) != 0 || len(fixture.store.transitions()) != 0) {
				t.Fatal("读取策略失败产生了下载器调用或状态推进")
			}
		})
	}
}

func TestHandlerUnpackFileSelectionFailureKeepsV2HashAcceptance(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name      string
		selection error
		wantErr   error
	}{
		{name: "文件尚未就绪", selection: downloader.ErrEpisodeSelectionPending},
		{name: "目标没有匹配", selection: downloader.ErrEpisodeSelectionNoMatch},
		{name: "下载器选集失败", selection: downloader.ErrEpisodeSelectionFailed},
		{name: "取消必须停止", selection: context.Canceled, wantErr: context.Canceled},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			fixture, store := newHandlerFileSelectionPolicyFixture(t, downloader.IntentPlanned)
			store.episodes = []downloader.Episode{{Season: 1, Episode: 7}}
			submitter := &handlerEpisodeSelectingSubmitter{base: fixture.submitter, selectionErr: tt.selection}
			fixture.handler = newHandlerTestHandler(t, store, fixture.resolver, submitter)
			result, err := fixture.handler.ExecuteIntent(t.Context(), fixture.args.Username, fixture.args.IntentID)
			if !errors.Is(err, tt.wantErr) || (tt.wantErr == nil && result.Outcome != IntentExecutionAccepted) ||
				(tt.wantErr != nil && result.Outcome != IntentExecutionRetryableFailure) {
				t.Fatalf("ExecuteIntent() = %#v, %v", result, err)
			}
			wantAccepts := 1
			if tt.wantErr != nil {
				wantAccepts = 0
			}
			if submitter.selectionCalls != 1 || len(store.acceptanceSnapshot()) != wantAccepts {
				t.Fatalf("selection/accept calls = %d/%d", submitter.selectionCalls, len(store.acceptanceSnapshot()))
			}
		})
	}
}

type handlerFileSelectionPolicyStore struct {
	*handlerAcceptanceStore
	input             organization.SuccessInput
	episodes          []downloader.Episode
	lookupErr         error
	selectionReads    int
	successInputReads int
	selectionUsername string
	selectionIntentID uuid.UUID
}

func (s *handlerFileSelectionPolicyStore) GetDownloadIntentFileSelection(
	_ context.Context,
	username string,
	intentID uuid.UUID,
) ([]downloader.Episode, error) {
	s.selectionReads++
	s.selectionUsername, s.selectionIntentID = username, intentID
	return slices.Clone(s.episodes), s.lookupErr
}

func (s *handlerFileSelectionPolicyStore) GetDownloadIntentSuccessInput(
	_ context.Context,
	_ string,
	_ uuid.UUID,
) (organization.SuccessInput, error) {
	s.successInputReads++
	return s.input.Clone(), nil
}

type handlerFileSelectionRecorder struct {
	*handlerTestSubmitter
	addedCandidate downloader.Candidate
	reconcileCalls int
}

func (s *handlerFileSelectionRecorder) Reconcile(
	ctx context.Context,
	intent downloader.Intent,
) (downloader.Intent, bool, error) {
	s.reconcileCalls++
	return s.handlerTestSubmitter.Reconcile(ctx, intent)
}

func (s *handlerFileSelectionRecorder) AddOnce(
	ctx context.Context,
	intent downloader.Intent,
	candidate downloader.Candidate,
	metainfo []byte,
) (downloader.Intent, error) {
	s.addedCandidate = candidate
	return s.handlerTestSubmitter.AddOnce(ctx, intent, candidate, metainfo)
}

func newHandlerFileSelectionPolicyFixture(
	t *testing.T,
	state downloader.IntentState,
) (*handlerFixture, *handlerFileSelectionPolicyStore) {
	t.Helper()
	fixture := newHandlerFixture(t, state, "file-selection-policy")
	if state != downloader.IntentPlanned {
		fixture.store.intent.TorrentHash = handlerTestHash
	}
	if state == downloader.IntentSubmitting {
		fixture.submitter.reconcile = func(intent downloader.Intent) (downloader.Intent, bool, error) {
			return downloadingIntent(t, intent, handlerTestHash), true, nil
		}
	}
	store := &handlerFileSelectionPolicyStore{
		handlerAcceptanceStore: &handlerAcceptanceStore{handlerTestStore: fixture.store},
		input: organization.SuccessInput{
			IntentID: fixture.args.IntentID,
			Username: "alice",
			Episodes: []organization.Episode{{Season: 1, Episode: 1}, {Season: 1, Episode: 7}, {Season: 2, Episode: 3}},
		},
	}
	return fixture, store
}
