package database

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/moviepilot-go/moviepilot-go/internal/subscription"
)

func TestRecordEmptyTVSubscriptionProgressRejectsInvalidCallsAndPreservesCancellation(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 10, 4, 0, 0, 0, 0, time.UTC)
	canceled, cancel := context.WithCancel(t.Context())
	cancel()
	deadline, deadlineCancel := context.WithDeadline(t.Context(), time.Now().Add(-time.Hour))
	defer deadlineCancel()
	var store *Store
	for _, tc := range []struct {
		name  string
		ctx   context.Context
		state subscription.State
		want  error
	}{
		{name: "nil context", state: subscription.PendingIdentification, want: ErrSubscriptionPreciseSearchPersistence},
		{name: "uninitialized store", ctx: t.Context(), state: subscription.PendingIdentification, want: ErrSubscriptionPreciseSearchPersistence},
		{name: "canceled context", ctx: canceled, state: subscription.PendingIdentification, want: context.Canceled},
		{name: "expired deadline", ctx: deadline, state: subscription.PendingIdentification, want: context.DeadlineExceeded},
		{name: "N state is not progress-only", ctx: t.Context(), state: subscription.PendingSearch, want: ErrInvalidSubscriptionPreciseSearch},
		{name: "R state is not progress-only", ctx: t.Context(), state: subscription.Active, want: ErrInvalidSubscriptionPreciseSearch},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			err := store.RecordEmptyTVSubscriptionProgress(tc.ctx, "sensitive-owner", 71, tc.state, now)
			if !errors.Is(err, tc.want) {
				t.Fatalf("RecordEmptyTVSubscriptionProgress() error=%v want=%v", err, tc.want)
			}
			for _, sensitive := range []string{"sensitive-owner", "71", "UPDATE", "subscriptions"} {
				if strings.Contains(err.Error(), sensitive) {
					t.Fatalf("persistence error leaks %q: %v", sensitive, err)
				}
			}
		})
	}
}

func TestBuildRecordEmptyTVSubscriptionProgress(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 10, 4, 8, 0, 0, 0, time.FixedZone("fixture", 8*60*60))
	for _, tc := range []struct {
		name    string
		owner   string
		id      int64
		state   subscription.State
		at      time.Time
		wantErr error
	}{
		{name: "P accepts trimmed owner and UTC time", owner: " owner ", id: 42, state: subscription.PendingIdentification, at: now},
		{name: "blank owner", owner: " \t ", id: 42, state: subscription.PendingIdentification, at: now, wantErr: ErrInvalidSubscriptionPreciseSearch},
		{name: "zero id", owner: "owner", state: subscription.PendingIdentification, at: now, wantErr: ErrInvalidSubscriptionPreciseSearch},
		{name: "negative id", owner: "owner", id: -1, state: subscription.PendingIdentification, at: now, wantErr: ErrInvalidSubscriptionPreciseSearch},
		{name: "zero version", owner: "owner", id: 42, state: subscription.PendingIdentification, wantErr: ErrInvalidSubscriptionPreciseSearch},
		{name: "N rejected", owner: "owner", id: 42, state: subscription.PendingSearch, at: now, wantErr: ErrInvalidSubscriptionPreciseSearch},
		{name: "R rejected", owner: "owner", id: 42, state: subscription.Active, at: now, wantErr: ErrInvalidSubscriptionPreciseSearch},
		{name: "paused rejected", owner: "owner", id: 42, state: subscription.Paused, at: now, wantErr: ErrInvalidSubscriptionPreciseSearch},
		{name: "completed rejected", owner: "owner", id: 42, state: subscription.Completed, at: now, wantErr: ErrInvalidSubscriptionPreciseSearch},
		{name: "unknown state rejected", owner: "owner", id: 42, state: subscription.State("unknown"), at: now, wantErr: ErrInvalidSubscriptionPreciseSearch},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			query, args, err := buildRecordEmptyTVSubscriptionProgress(tc.owner, tc.id, tc.state, tc.at)
			if !errors.Is(err, tc.wantErr) {
				t.Fatalf("buildRecordEmptyTVSubscriptionProgress() error=%v want=%v", err, tc.wantErr)
			}
			if tc.wantErr != nil {
				if query != "" || args != nil {
					t.Fatalf("invalid call generated query=%s args=%v", query, args)
				}
				return
			}
			wantArgs := []any{int64(42), "owner", subscription.PendingIdentification, "P", now.UTC(), `{"lack_episode":0}`, "电视剧", "tv"}
			if query != recordEmptyTVSubscriptionProgressSQL || !reflect.DeepEqual(args, wantArgs) {
				t.Fatalf("query=%s args=%v want=%v", query, args, wantArgs)
			}
			var patch map[string]any
			if err := json.Unmarshal([]byte(args[5].(string)), &patch); err != nil || len(patch) != 1 || patch["lack_episode"] != float64(0) {
				t.Fatalf("progress patch=%v error=%v", patch, err)
			}
		})
	}
}

func TestRecordEmptyTVSubscriptionProgressSQLKeepsCASAndState(t *testing.T) {
	t.Parallel()
	query := compactSubscriptionPreciseSearchSQL(recordEmptyTVSubscriptionProgressSQL)
	for _, fragment := range []string{
		"SET config=config || $6::jsonb, updated_at=now()",
		"id=$1 AND username=$2 AND completed_at IS NULL",
		"current_download_intent_id IS NULL",
		"jsonb_typeof(config)='object'",
		"internal_state=$3 AND v2_state=$4",
		"updated_at=$5",
		"LOWER(BTRIM(media_type)) IN ($7, $8)",
		"AND NOT CASE jsonb_typeof(config->'best_version')",
		"WHEN 'boolean' THEN (config->>'best_version')::boolean",
		"(config->>'best_version')::numeric <> 0",
	} {
		if !strings.Contains(query, fragment) {
			t.Fatalf("progress query misses %q: %s", fragment, query)
		}
	}
	setClause := strings.Split(strings.Split(query, " SET ")[1], " WHERE ")[0]
	for _, field := range []string{"internal_state", "v2_state", "completed_at", "current_download_intent_id", "season", "total_episode", "note", "current_priority", "episode_priority"} {
		if strings.Contains(setClause, field) {
			t.Fatalf("progress-only query changes %q: %s", field, setClause)
		}
	}
}
