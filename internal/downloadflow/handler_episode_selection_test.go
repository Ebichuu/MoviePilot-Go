package downloadflow

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/moviepilot-go/moviepilot-go/internal/downloader"
	"github.com/moviepilot-go/moviepilot-go/internal/jobs"
	"github.com/moviepilot-go/moviepilot-go/internal/organization"
)

func TestHandlerSelectsTargetEpisodesBeforePersistingAcceptance(t *testing.T) {
	t.Parallel()

	fixture, store, submitter := newHandlerEpisodeSelectionFixture(t, downloader.IntentPlanned)
	store.input.Episodes = []organization.Episode{
		{Season: 2, Episode: 3},
		{Season: 1, Episode: 2},
		{Season: 2, Episode: 3},
	}

	result, err := fixture.handler.HandleDownload(t.Context(), fixture.args)
	if err != nil || result.Action != jobs.DownloadActionComplete {
		t.Fatalf("HandleDownload() = %#v, %v", result, err)
	}
	wantEpisodes := []downloader.Episode{{Season: 1, Episode: 2}, {Season: 2, Episode: 3}}
	if !slices.Equal(submitter.selectedEpisodes, wantEpisodes) {
		t.Fatalf("selected episodes = %#v, want %#v", submitter.selectedEpisodes, wantEpisodes)
	}
	if submitter.selectedIntent.State != downloader.IntentDownloading ||
		submitter.selectedIntent.TorrentHash == "" {
		t.Fatalf("selected intent = %#v", submitter.selectedIntent)
	}
	if !slices.Equal(submitter.addedCandidate.TargetEpisodes, wantEpisodes) {
		t.Fatalf("candidate target episodes = %#v, want %#v", submitter.addedCandidate.TargetEpisodes, wantEpisodes)
	}
	wantEvents := []string{
		"cas:planned->submitting",
		"add",
		"select",
		"cas:submitting->downloading",
	}
	if events := fixture.store.eventsSnapshot(); !slices.Equal(events, wantEvents) {
		t.Fatalf("events = %#v, want %#v", events, wantEvents)
	}
}

func TestHandlerReplaysEpisodeSelectionBeforeAcceptance(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name      string
		state     downloader.IntentState
		reconcile bool
		wantCAS   []string
	}{
		{
			name:      "submitting 对账重放",
			state:     downloader.IntentSubmitting,
			reconcile: true,
			wantCAS:   []string{"submitting->downloading"},
		},
		{
			name:  "downloading 补偿重放",
			state: downloader.IntentDownloading,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			fixture, store, submitter := newHandlerEpisodeSelectionFixture(t, tt.state)
			store.input.Episodes = []organization.Episode{{Season: 1, Episode: 7}}
			fixture.store.intent.TorrentHash = handlerTestHash
			if tt.reconcile {
				submitter.base.reconcile = func(intent downloader.Intent) (downloader.Intent, bool, error) {
					return downloadingIntent(t, intent, handlerTestHash), true, nil
				}
			}

			result, err := fixture.handler.HandleDownload(t.Context(), fixture.args)
			if err != nil || result.Action != jobs.DownloadActionComplete {
				t.Fatalf("HandleDownload() = %#v, %v", result, err)
			}
			if submitter.selectionCalls != 1 || submitter.selectedIntent.TorrentHash != handlerTestHash {
				t.Fatalf("selection calls/intent = %d/%#v", submitter.selectionCalls, submitter.selectedIntent)
			}
			if submitter.base.addCalls.Load() != 0 || fixture.resolver.calls.Load() != 0 {
				t.Fatal("重放路径触发了资源解析或重复 Add")
			}
			if got := fixture.store.transitions(); !slices.Equal(got, tt.wantCAS) {
				t.Fatalf("CAS transitions = %#v, want %#v", got, tt.wantCAS)
			}
		})
	}
}

func TestHandlerDoesNotAcceptWhenEpisodeSelectionIsUncertain(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name       string
		selection  error
		wantAction jobs.DownloadAction
		wantErr    error
	}{
		{
			name:       "元数据未就绪时延后",
			selection:  downloader.ErrEpisodeSelectionPending,
			wantAction: jobs.DownloadActionSnooze,
		},
		{
			name:       "没有目标文件时重试",
			selection:  downloader.ErrEpisodeSelectionNoMatch,
			wantAction: jobs.DownloadActionRetry,
			wantErr:    downloader.ErrEpisodeSelectionNoMatch,
		},
		{
			name:       "下载器不支持时重试",
			selection:  downloader.ErrEpisodeSelectionUnavailable,
			wantAction: jobs.DownloadActionRetry,
			wantErr:    downloader.ErrEpisodeSelectionUnavailable,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			fixture, store, submitter := newHandlerEpisodeSelectionFixture(t, downloader.IntentPlanned)
			store.input.Episodes = []organization.Episode{{Season: 1, Episode: 1}}
			submitter.selectionErr = tt.selection

			result, err := fixture.handler.HandleDownload(t.Context(), fixture.args)
			if result.Action != tt.wantAction || !errors.Is(err, tt.wantErr) {
				t.Fatalf("HandleDownload() = %#v, %v; want action=%v error=%v", result, err, tt.wantAction, tt.wantErr)
			}
			if tt.wantAction == jobs.DownloadActionRetry && !errors.Is(err, ErrDownloadStateUnavailable) {
				t.Fatalf("HandleDownload() error = %v, want ErrDownloadStateUnavailable", err)
			}
			if got := fixture.store.snapshot(); got.State != downloader.IntentSubmitting {
				t.Fatalf("intent state = %s, want submitting", got.State)
			}
			if transitions := fixture.store.transitions(); !slices.Equal(transitions, []string{"planned->submitting"}) {
				t.Fatalf("CAS transitions = %#v", transitions)
			}
		})
	}
}

func TestHandlerSuccessInputFailureIsSanitizedBeforeExternalWork(t *testing.T) {
	t.Parallel()

	fixture, store, submitter := newHandlerEpisodeSelectionFixture(t, downloader.IntentPlanned)
	store.err = errors.New("postgres failed for secret-success-input")

	result, err := fixture.handler.HandleDownload(t.Context(), fixture.args)
	if result.Action != jobs.DownloadActionRetry || !errors.Is(err, ErrDownloadStateUnavailable) {
		t.Fatalf("HandleDownload() = %#v, %v", result, err)
	}
	if strings.Contains(err.Error(), "secret-success-input") {
		t.Fatalf("HandleDownload() leaked success input error: %v", err)
	}
	if fixture.resolver.calls.Load() != 0 || submitter.base.addCalls.Load() != 0 ||
		submitter.selectionCalls != 0 || len(fixture.store.transitions()) != 0 {
		t.Fatal("success input failure produced an external or state side effect")
	}
}

type handlerSuccessInputStore struct {
	*handlerTestStore
	input organization.SuccessInput
	err   error
}

func (s *handlerSuccessInputStore) GetDownloadIntentSuccessInput(
	_ context.Context,
	username string,
	intentID uuid.UUID,
) (organization.SuccessInput, error) {
	if s.err != nil {
		return organization.SuccessInput{}, s.err
	}
	input := s.input.Clone()
	input.Username = username
	input.IntentID = intentID
	return input, nil
}

type handlerEpisodeSelectingSubmitter struct {
	base             *handlerTestSubmitter
	selectionErr     error
	selectionCalls   int
	selectedIntent   downloader.Intent
	selectedEpisodes []downloader.Episode
	addedCandidate   downloader.Candidate
}

func (s *handlerEpisodeSelectingSubmitter) ObserveTasks(
	ctx context.Context,
	hashes []string,
) (map[string]downloader.TaskObservation, error) {
	return s.base.ObserveTasks(ctx, hashes)
}

func (s *handlerEpisodeSelectingSubmitter) Reconcile(
	ctx context.Context,
	intent downloader.Intent,
) (downloader.Intent, bool, error) {
	return s.base.Reconcile(ctx, intent)
}

func (s *handlerEpisodeSelectingSubmitter) AddOnce(
	ctx context.Context,
	intent downloader.Intent,
	candidate downloader.Candidate,
	metainfo []byte,
) (downloader.Intent, error) {
	s.addedCandidate = candidate
	return s.base.AddOnce(ctx, intent, candidate, metainfo)
}

func (s *handlerEpisodeSelectingSubmitter) SelectEpisodeFiles(
	_ context.Context,
	intent downloader.Intent,
	episodes []downloader.Episode,
) error {
	s.selectionCalls++
	s.selectedIntent = intent
	s.selectedEpisodes = append([]downloader.Episode(nil), episodes...)
	s.base.eventStore.recordEvent("select")
	return s.selectionErr
}

func newHandlerEpisodeSelectionFixture(
	t *testing.T,
	state downloader.IntentState,
) (*handlerFixture, *handlerSuccessInputStore, *handlerEpisodeSelectingSubmitter) {
	t.Helper()
	fixture := newHandlerFixture(t, state, "42")
	store := &handlerSuccessInputStore{handlerTestStore: fixture.store}
	submitter := &handlerEpisodeSelectingSubmitter{base: fixture.submitter}
	fixture.handler = newHandlerTestHandler(t, store, fixture.resolver, submitter)
	return fixture, store, submitter
}
