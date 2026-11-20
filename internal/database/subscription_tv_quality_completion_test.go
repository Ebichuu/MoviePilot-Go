package database

import (
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/moviepilot-go/moviepilot-go/internal/media"
	"github.com/moviepilot-go/moviepilot-go/internal/subscription"
)

func tvQualityCompletionSnapshot() Subscription {
	season := 1
	return Subscription{
		ID: 42, Username: "owner", MediaType: string(media.TypeTV), Season: &season,
		InternalState: subscription.PendingSearch, V2State: "N",
		UpdatedAt: time.Date(2026, 10, 4, 0, 0, 0, 0, time.UTC),
		Config: map[string]any{
			"best_version": true, "total_episode": 3, "lack_episode": 3,
			"episode_priority": map[string]any{"1": 100, "2": "100", "3": 100},
		},
	}
}

func TestPrepareBestVersionTVCompletion(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name     string
		change   func(*Subscription)
		wantErr  error
		wantLack int
	}{
		{name: "per episode terminal quality"},
		{name: "full terminal quality", change: func(s *Subscription) { s.Config["best_version_full"], s.Config["current_priority"] = true, 100 }},
		{name: "V2 full confirmed partial preserves any version lack", wantLack: 2, change: func(s *Subscription) {
			s.Config["best_version_full"], s.Config["current_priority"] = true, 100
			s.Config["episode_priority"] = map[int]int{}
			s.Config["note"] = []int{1}
		}},
		{name: "legacy overall expands to terminal per episode", change: func(s *Subscription) { delete(s.Config, "episode_priority"); s.Config["current_priority"] = 100 }},
		{name: "integer truthy best version", change: func(s *Subscription) { s.Config["best_version"] = -1 }},
		{name: "TV alias", change: func(s *Subscription) { s.MediaType = " TV " }},
		{name: "per episode low quality cannot finish", change: func(s *Subscription) { s.Config["episode_priority"] = map[int]int{1: 100, 2: 90, 3: 100} }, wantErr: ErrSubscriptionPreciseSearchStateConflict},
		{name: "note does not prove terminal quality", change: func(s *Subscription) { s.Config["episode_priority"], s.Config["note"] = map[int]int{}, []int{1, 2, 3} }, wantErr: ErrSubscriptionPreciseSearchStateConflict},
		{name: "full per episode100 does not prove full quality", change: func(s *Subscription) { s.Config["best_version_full"] = true }, wantErr: ErrSubscriptionPreciseSearchStateConflict},
		{name: "full101 is not terminal", change: func(s *Subscription) { s.Config["best_version_full"], s.Config["current_priority"] = true, 101 }, wantErr: ErrSubscriptionPreciseSearchStateConflict},
		{name: "ordinary quality does not use best completion", change: func(s *Subscription) { s.Config["best_version"] = false }, wantErr: ErrSubscriptionPreciseSearchStateConflict},
		{name: "P projection cannot complete", change: func(s *Subscription) { s.V2State = "P" }, wantErr: ErrSubscriptionPreciseSearchStateConflict},
		{name: "foreign owner not found", change: func(s *Subscription) { s.Username = "other" }, wantErr: ErrSubscriptionNotFound},
		{name: "wrong id not found", change: func(s *Subscription) { s.ID++ }, wantErr: ErrSubscriptionNotFound},
		{name: "completed snapshot not found", change: func(s *Subscription) { now := s.UpdatedAt; s.CompletedAt = &now }, wantErr: ErrSubscriptionNotFound},
		{name: "movie cannot enter TV completion", change: func(s *Subscription) { s.MediaType = string(media.TypeMovie) }, wantErr: ErrSubscriptionPreciseSearchStateConflict},
		{name: "state changed", change: func(s *Subscription) { s.InternalState = subscription.Active }, wantErr: ErrSubscriptionPreciseSearchStateConflict},
		{name: "version changed", change: func(s *Subscription) { s.UpdatedAt = s.UpdatedAt.Add(time.Nanosecond) }, wantErr: ErrSubscriptionPreciseSearchStateConflict},
		{name: "V2 empty target legacy100 completes", change: func(s *Subscription) { s.Config["total_episode"], s.Config["current_priority"] = 0, 100 }},
		{name: "V2 empty target full100 completes", change: func(s *Subscription) {
			s.Config["start_episode"], s.Config["best_version_full"], s.Config["current_priority"] = 4, true, 100
		}},
		{name: "malformed current quality rejects", change: func(s *Subscription) { s.Config["current_priority"] = "bad" }, wantErr: ErrSubscriptionPreciseSearchPersistence},
		{name: "unbounded target rejects", change: func(s *Subscription) { s.Config["total_episode"] = 5001 }, wantErr: ErrSubscriptionPreciseSearchPersistence},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			observed := tvQualityCompletionSnapshot()
			expectedAt := observed.UpdatedAt
			if test.change != nil {
				test.change(&observed)
			}
			query, args, err := prepareBestVersionTVCompletion(observed, " owner ", 42, subscription.PendingSearch, expectedAt)
			if !errors.Is(err, test.wantErr) {
				t.Fatalf("query=%s error=%v want=%v", query, err, test.wantErr)
			}
			if test.wantErr != nil {
				if query != "" || args != nil {
					t.Fatal("completion generated query for rejected facts")
				}
				return
			}
			if query != completeSatisfiedBestVersionTVSubscriptionSQL || len(args) != 12 || args[1] != "owner" || args[11] != "N" {
				t.Fatalf("query=%s args=%v", query, args)
			}
			var patch, comparedConfig map[string]any
			if err := json.Unmarshal([]byte(args[6].(string)), &patch); err != nil {
				t.Fatal(err)
			}
			if err := json.Unmarshal([]byte(args[9].(string)), &comparedConfig); err != nil {
				t.Fatal(err)
			}
			total, _ := subscriptionInteger(observed.Config["total_episode"])
			if patch["lack_episode"] != float64(test.wantLack) || comparedConfig["total_episode"] != float64(total) {
				t.Fatalf("patch=%v comparedConfig=%v", patch, comparedConfig)
			}
			if full, _ := observed.Config["best_version_full"].(bool); full {
				if _, exists := patch["current_priority"]; exists {
					t.Fatal("full completion must preserve its accepted overall quality")
				}
			} else if patch["current_priority"] != float64(100) {
				t.Fatalf("per episode completion must derive overall quality100: %v", patch)
			}
		})
	}
}

func TestTVQualityCompletionKeepsIndependentCAS(t *testing.T) {
	t.Parallel()
	query := completeSatisfiedBestVersionTVSubscriptionSQL
	for _, required := range []string{
		"id=$1 AND username=$2", "completed_at IS NULL", "current_download_intent_id IS NULL",
		"jsonb_typeof(config)='object'", "internal_state=$5", "v2_state=$12", "v2_state IN ('N', 'R')",
		"updated_at=$6", "LOWER(BTRIM(media_type)) IN ($8, $9)", "config=$10::jsonb", "season IS NOT DISTINCT FROM $11",
		"jsonb_typeof(config->'best_version')",
	} {
		if !strings.Contains(query, required) {
			t.Fatalf("best TV completion missing guard %q", required)
		}
	}
	ordinary, _, err := buildCompleteSatisfiedTVSubscription("owner", 42, subscription.PendingSearch, time.Now())
	if err != nil || ordinary != completeSatisfiedSubscriptionSQL || strings.Contains(ordinary, "config=$10") || !strings.Contains(ordinary, "AND NOT CASE") {
		t.Fatal("best TV completion widened ordinary completion predicate")
	}
	observed := tvQualityCompletionSnapshot()
	if _, _, err := prepareBestVersionTVCompletion(observed, "owner", 42, subscription.PendingIdentification, observed.UpdatedAt); !errors.Is(err, ErrInvalidSubscriptionPreciseSearch) {
		t.Fatalf("P expected state completion error=%v", err)
	}
}

func TestBuildGetOwnedSubscription(t *testing.T) {
	t.Parallel()
	query, args, err := buildGetOwnedSubscription(" owner ", 42)
	if err != nil || query != getOwnedSubscriptionSQL || !reflect.DeepEqual(args, []any{int64(42), "owner"}) {
		t.Fatalf("query=%s args=%v error=%v", query, args, err)
	}
	if strings.Contains(query, "completed_at IS NULL") || !strings.Contains(query, "id=$1 AND username=$2") {
		t.Fatal("owned read must include owner history without widening owner guard")
	}
	if _, _, err := buildGetOwnedSubscription(" ", 42); !errors.Is(err, ErrInvalidSubscriptionPreciseSearch) {
		t.Fatalf("empty owner error=%v", err)
	}
}
