package database

import (
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/moviepilot-go/moviepilot-go/internal/downloader"
	"github.com/moviepilot-go/moviepilot-go/internal/subscription"
)

func TestBuildRecoverSubscriptionDownloadIntent(t *testing.T) {
	t.Parallel()
	intentID := uuid.MustParse("77777777-8888-4999-8aaa-bbbbbbbbbbbb")
	expectedAt := time.Date(2026, 8, 11, 18, 0, 0, 123000, time.FixedZone("test", 8*60*60))
	query, args, err := buildRecoverSubscriptionDownloadIntent(
		" alice ",
		intentID,
		[]downloader.IntentState{
			downloader.IntentPlanned,
			downloader.IntentFailed,
			downloader.IntentSubmitting,
			downloader.IntentPlanned,
		},
		&expectedAt,
	)
	if err != nil {
		t.Fatal(err)
	}
	want := []any{
		intentID,
		"alice",
		[]string{"planned", "failed", "submitting"},
		expectedAt.UTC(),
		subscription.PendingSearch,
		"N",
		[]string{"acquired", "downloading", "pending_identification"},
		subscription.PendingIdentification,
		"P",
	}
	if !reflect.DeepEqual(args, want) {
		t.Fatalf("args = %#v, want %#v", args, want)
	}
	compact := strings.Join(strings.Fields(query), " ")
	for _, required := range []string{
		"state='failed', updated_at=now()",
		"$4::timestamptz IS NULL OR updated_at=$4",
		"active_subscription.current_download_intent_id=download_intents.id",
		"active_subscription.internal_state=ANY($7::text[])",
		"recovered_subscription.current_download_intent_id IS NULL",
		"recovered_subscription.internal_state IN ($5, $8)",
		"SET state='missing', download_intent_id=NULL, updated_at=now()",
		"owner.id=se.subscription_id AND owner.username=$2 AND owner.completed_at IS NULL",
		"SET internal_state=CASE WHEN s.internal_state=$8 THEN $8 ELSE $5 END",
		"current_download_intent_id=NULL",
		"s.current_download_intent_id=$1",
		"s.internal_state=ANY($7::text[])",
	} {
		if !strings.Contains(compact, required) {
			t.Fatalf("recovery SQL missing %q: %s", required, compact)
		}
	}
}

func TestBuildRecoverSubscriptionDownloadIntentRejectsInvalidInput(t *testing.T) {
	t.Parallel()
	intentID := uuid.MustParse("88888888-9999-4aaa-8bbb-cccccccccccc")
	zero := time.Time{}
	tests := []struct {
		name       string
		username   string
		intentID   uuid.UUID
		states     []downloader.IntentState
		expectedAt *time.Time
		wantErr    error
	}{
		{name: "empty owner", intentID: intentID, states: []downloader.IntentState{downloader.IntentFailed}, wantErr: ErrInvalidDownloadIntent},
		{name: "empty intent", username: "alice", states: []downloader.IntentState{downloader.IntentFailed}, wantErr: ErrInvalidDownloadIntent},
		{name: "empty states", username: "alice", intentID: intentID, wantErr: ErrInvalidDownloadIntent},
		{name: "unsafe state", username: "alice", intentID: intentID, states: []downloader.IntentState{downloader.IntentCompleted}, wantErr: ErrInvalidDownloadIntentState},
		{name: "zero lease version", username: "alice", intentID: intentID, states: []downloader.IntentState{downloader.IntentSubmitting}, expectedAt: &zero, wantErr: ErrInvalidDownloadIntent},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			_, _, err := buildRecoverSubscriptionDownloadIntent(
				tt.username, tt.intentID, tt.states, tt.expectedAt,
			)
			if !errors.Is(err, tt.wantErr) {
				t.Fatalf("error = %v, want %v", err, tt.wantErr)
			}
		})
	}
}

func TestBuildRecoverSubscriptionDownloadIntentAcceptsDownloading(t *testing.T) {
	t.Parallel()
	intentID := uuid.MustParse("88888888-9999-4aaa-8bbb-cccccccccccc")
	query, args, err := buildRecoverSubscriptionDownloadIntent(
		"alice", intentID, []downloader.IntentState{downloader.IntentDownloading}, nil,
	)
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	if !strings.Contains(query, "state='failed'") {
		t.Fatalf("recovery SQL 应把意图置为 failed: %s", query)
	}
	if len(args) < 3 {
		t.Fatalf("args = %v", args)
	}
	states, ok := args[2].([]string)
	if !ok || len(states) != 1 || states[0] != string(downloader.IntentDownloading) {
		t.Fatalf("from states = %v", args[2])
	}
}
