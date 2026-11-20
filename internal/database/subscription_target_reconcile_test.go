package database

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/moviepilot-go/moviepilot-go/internal/subscription"
)

func TestBuildReconcileSubscriptionTargetLocksAndPreservesBoundHistory(t *testing.T) {
	now := time.Date(2026, 9, 13, 12, 0, 0, 123000000, time.FixedZone("fixture", 8*60*60))
	target, err := subscription.DescribeTVTarget(subscription.TVTargetSnapshot{
		Season: 2, StartEpisode: 1, TotalEpisode: 3, LackEpisode: 2,
	})
	if err != nil {
		t.Fatal(err)
	}
	query, args, err := buildReconcileSubscriptionTarget(SubscriptionTargetReconcile{
		Username: " owner ", SubscriptionID: 42, ExpectedUpdatedAt: now,
		Target: target,
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, fragment := range []string{
		"FROM subscriptions", "updated_at=$3", "FOR UPDATE",
		"ORDER BY se.id", "FOR UPDATE OF se", "season=$6", "config=s.config || $7::jsonb",
		"count(*) <= $8", "download_intent_id IS NULL", "changes.required", "USING authorized",
		"NOT EXISTS", "ON CONFLICT (subscription_id, season, episode) DO NOTHING",
	} {
		if !strings.Contains(query, fragment) {
			t.Fatalf("query missing %q: %s", fragment, query)
		}
	}
	for _, forbidden := range []string{"UPDATE download_intents", "DELETE FROM download_intents", "payload="} {
		if strings.Contains(query, forbidden) {
			t.Fatalf("query changes immutable intent through %q: %s", forbidden, query)
		}
	}
	if args[1] != "owner" || args[2] != now.UTC() {
		t.Fatalf("identity/version args=%#v", args[:3])
	}
	if got := args[3].([]int32); len(got) != 3 || got[0] != 2 || got[1] != 2 || got[2] != 2 {
		t.Fatalf("seasons=%#v", got)
	}
	if got := args[4].([]int32); len(got) != 3 || got[0] != 1 || got[1] != 2 || got[2] != 3 {
		t.Fatalf("episodes=%#v", got)
	}
	var patch map[string]any
	if err := json.Unmarshal(args[6].([]byte), &patch); err != nil || patch["total_episode"] != float64(3) {
		t.Fatalf("config patch=%#v err=%v", patch, err)
	}
	if args[5] != 2 || args[7] != maxSubscriptionEpisodes {
		t.Fatalf("season/capacity args=%#v", args[5:])
	}
}

func TestBuildReconcileSubscriptionTargetAllowsExplicitUnknownTarget(t *testing.T) {
	target, err := subscription.DescribeTVTarget(subscription.TVTargetSnapshot{})
	if err != nil {
		t.Fatal(err)
	}
	_, args, err := buildReconcileSubscriptionTarget(SubscriptionTargetReconcile{
		Username: "owner", SubscriptionID: 1, ExpectedUpdatedAt: time.Now(),
		Target: target,
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(args[3].([]int32)) != 0 || len(args[4].([]int32)) != 0 {
		t.Fatalf("unknown target coordinates=%#v %#v", args[3], args[4])
	}
}

func TestBuildReconcileSubscriptionTargetRejectsInvalidInput(t *testing.T) {
	target, err := subscription.DescribeTVTarget(subscription.TVTargetSnapshot{Season: 1, TotalEpisode: 1})
	if err != nil {
		t.Fatal(err)
	}
	valid := SubscriptionTargetReconcile{
		Username: "owner", SubscriptionID: 1, ExpectedUpdatedAt: time.Now(),
		Target: target,
	}
	tests := []struct {
		name   string
		mutate func(*SubscriptionTargetReconcile)
	}{
		{"empty owner", func(input *SubscriptionTargetReconcile) { input.Username = "" }},
		{"zero id", func(input *SubscriptionTargetReconcile) { input.SubscriptionID = 0 }},
		{"zero version", func(input *SubscriptionTargetReconcile) { input.ExpectedUpdatedAt = time.Time{} }},
		{"invalid snapshot", func(input *SubscriptionTargetReconcile) { input.Target.Snapshot.Season = 1000 }},
		{"mismatched coordinates", func(input *SubscriptionTargetReconcile) {
			input.Target.TargetEpisodes = []subscription.Episode{{Season: 1, Episode: 2}}
		}},
		{"mismatched knowledge", func(input *SubscriptionTargetReconcile) {
			input.Target.Knowledge = subscription.TVTargetUnknown
		}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			input := valid
			tt.mutate(&input)
			if _, _, err := buildReconcileSubscriptionTarget(input); !errors.Is(err, ErrInvalidSubscriptionTarget) {
				t.Fatalf("error=%v", err)
			}
		})
	}
}

func TestListSubscriptionEpisodesForTargetSQLUsesExplicitMembership(t *testing.T) {
	for _, fragment := range []string{
		"JOIN requested ON requested.season=se.season AND requested.episode=se.episode",
		"completed_at IS NULL", "se.state=ANY($5::text[])", "ORDER BY se.season, se.episode", "LIMIT $6",
	} {
		if !strings.Contains(listSubscriptionEpisodesForTargetSQL, fragment) {
			t.Fatalf("target query missing %q: %s", fragment, listSubscriptionEpisodesForTargetSQL)
		}
	}
}

func TestVerifySubscriptionTargetVersionSQLLocksCASRow(t *testing.T) {
	for _, fragment := range []string{
		"id=$1", "username=$2", "completed_at IS NULL", "updated_at=$3", "FOR UPDATE",
	} {
		if !strings.Contains(verifySubscriptionTargetVersionSQL, fragment) {
			t.Fatalf("version query missing %q: %s", fragment, verifySubscriptionTargetVersionSQL)
		}
	}
}

func TestBuildReconcileSubscriptionTargetGuardsRetainedHistoryCapacity(t *testing.T) {
	for _, fragment := range []string{
		"SELECT season, episode FROM locked_episodes WHERE download_intent_id IS NOT NULL",
		"UNION\n\t\tSELECT season, episode FROM requested",
		"count(*) <= $8 AS allowed",
		"FROM target, capacity",
		"CROSS JOIN authorized",
	} {
		if !strings.Contains(reconcileSubscriptionTargetSQL, fragment) {
			t.Fatalf("capacity guard missing %q: %s", fragment, reconcileSubscriptionTargetSQL)
		}
	}
}
