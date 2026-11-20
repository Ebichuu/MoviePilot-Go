package downloadflow

import (
	"context"
	"errors"
	"testing"

	"github.com/google/uuid"

	"github.com/moviepilot-go/moviepilot-go/internal/downloader"
	"github.com/moviepilot-go/moviepilot-go/internal/organization"
)

func TestHandlerExecuteIntentAcceptsHashDespiteEpisodeSelectionFailure(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name         string
		selectionErr error
	}{
		{name: "torrent files pending", selectionErr: downloader.ErrEpisodeSelectionPending},
		{name: "target files not found", selectionErr: downloader.ErrEpisodeSelectionNoMatch},
		{name: "file selection failed", selectionErr: downloader.ErrEpisodeSelectionFailed},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			fixture, store, submitter := newExecutionV2Fixture(t)
			submitter.selectionErr = tt.selectionErr

			got, err := fixture.handler.ExecuteIntent(t.Context(), fixture.args.Username, fixture.args.IntentID)
			if err != nil || got.Outcome != IntentExecutionAccepted {
				t.Fatalf("ExecuteIntent() = %#v, %v; want accepted after qB Hash", got, err)
			}
			if fixture.submitter.addCalls.Load() != 1 || submitter.selectionCalls != 1 {
				t.Fatalf(
					"add/selection calls = %d/%d, want 1/1",
					fixture.submitter.addCalls.Load(), submitter.selectionCalls,
				)
			}
			if len(store.acceptCalls) != 1 || !store.consumed {
				t.Fatalf("accept calls/consumed = %d/%t, want 1/true", len(store.acceptCalls), store.consumed)
			}
			intent := fixture.store.snapshot()
			if intent.State != downloader.IntentDownloading || intent.TorrentHash == "" {
				t.Fatalf("accepted intent = %#v", intent)
			}
		})
	}
}

func TestHandlerExecuteIntentStopsWhenSubscriptionWasNotReleased(t *testing.T) {
	t.Parallel()

	fixture, store, _ := newExecutionV2Fixture(t)
	store.notReleased = true

	got, err := fixture.handler.ExecuteIntent(t.Context(), fixture.args.Username, fixture.args.IntentID)
	if got.Outcome != IntentExecutionRetryableFailure || !errors.Is(err, ErrDownloadStateUnavailable) {
		t.Fatalf("ExecuteIntent() = %#v, %v; want retryable stop", got, err)
	}
	if fixture.submitter.addCalls.Load() != 1 || len(store.acceptCalls) != 1 {
		t.Fatalf("add/accept calls = %d/%d, want 1/1", fixture.submitter.addCalls.Load(), len(store.acceptCalls))
	}
	if len(fixture.store.recoverySnapshot()) != 0 {
		t.Fatalf("unreleased subscription was recovered: %#v", fixture.store.recoverySnapshot())
	}
}

func TestHandlerExecuteIntentDoesNotAcceptHashAfterContextCancellation(t *testing.T) {
	t.Parallel()

	fixture, store, submitter := newExecutionV2Fixture(t)
	submitter.selectionErr = context.Canceled

	got, err := fixture.handler.ExecuteIntent(t.Context(), fixture.args.Username, fixture.args.IntentID)
	if got.Outcome != IntentExecutionRetryableFailure || !errors.Is(err, context.Canceled) {
		t.Fatalf("ExecuteIntent() = %#v, %v; want canceled retryable stop", got, err)
	}
	if len(store.acceptCalls) != 0 || store.consumed {
		t.Fatalf("canceled selection reached acceptance: calls=%d consumed=%t", len(store.acceptCalls), store.consumed)
	}
}

func TestHandlerExecuteIntentWithMetainfoReusesInspectedTorrent(t *testing.T) {
	t.Parallel()

	fixture := newHandlerFixture(t, downloader.IntentPlanned, "resource-v2-projected")
	got, err := fixture.handler.ExecuteIntentWithMetainfo(
		t.Context(),
		fixture.args.Username,
		fixture.args.IntentID,
		validHandlerMetainfo(t),
	)
	if err != nil || got.Outcome != IntentExecutionAccepted {
		t.Fatalf("ExecuteIntentWithMetainfo() = %#v, %v", got, err)
	}
	if fixture.resolver.calls.Load() != 0 || fixture.submitter.addCalls.Load() != 1 {
		t.Fatalf(
			"resolver/add calls = %d/%d, want 0/1",
			fixture.resolver.calls.Load(),
			fixture.submitter.addCalls.Load(),
		)
	}

	invalid, invalidErr := fixture.handler.ExecuteIntentWithMetainfo(
		t.Context(), fixture.args.Username, fixture.args.IntentID, nil,
	)
	if invalid.Outcome != IntentExecutionRetryableFailure || !errors.Is(invalidErr, ErrDownloadMetainfoInvalid) {
		t.Fatalf("empty metainfo result = %#v, %v", invalid, invalidErr)
	}
}

type executionV2Store struct {
	*handlerAcceptanceStore
	input organization.SuccessInput
}

func (s *executionV2Store) GetDownloadIntentSuccessInput(
	_ context.Context,
	username string,
	intentID uuid.UUID,
) (organization.SuccessInput, error) {
	input := s.input.Clone()
	input.Username = username
	input.IntentID = intentID
	return input, nil
}

func newExecutionV2Fixture(
	t *testing.T,
) (*handlerFixture, *executionV2Store, *handlerEpisodeSelectingSubmitter) {
	t.Helper()

	fixture := newHandlerFixture(t, downloader.IntentPlanned, "resource-v2-selection")
	store := &executionV2Store{
		handlerAcceptanceStore: &handlerAcceptanceStore{handlerTestStore: fixture.store},
		input: organization.SuccessInput{
			Episodes: []organization.Episode{{Season: 1, Episode: 1}},
		},
	}
	submitter := &handlerEpisodeSelectingSubmitter{base: fixture.submitter}
	fixture.handler = newHandlerTestHandler(t, store, fixture.resolver, submitter)
	return fixture, store, submitter
}
