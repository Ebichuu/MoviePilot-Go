package database

import (
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"testing"
)

func TestSubscriptionV2DistinguishesNullAndEmptyMediaType(t *testing.T) {
	t.Parallel()

	if got := (Subscription{MediaType: "", MediaTypeNull: true}).V2()["type"]; got != nil {
		t.Fatalf("NULL media type projection = %#v, want nil", got)
	}
	if got := (Subscription{MediaType: ""}).V2()["type"]; got != "" {
		t.Fatalf("empty media type projection = %#v, want empty string", got)
	}
	if got := (Subscription{MediaType: "电视剧"}).V2()["type"]; got != "电视剧" {
		t.Fatalf("TV media type projection = %#v", got)
	}
}

func TestSubscriptionV2ProjectsNullablePublicColumns(t *testing.T) {
	t.Parallel()
	got := (Subscription{
		YearNull: true, MediaSourceNull: true, MediaIDNull: true,
	}).V2()
	for _, key := range []string{"year", "media_source", "media_id"} {
		if got[key] != nil {
			t.Fatalf("%s projection = %#v, want nil", key, got[key])
		}
	}
	got = (Subscription{}).V2()
	for _, key := range []string{"year", "media_source", "media_id"} {
		if got[key] != "" {
			t.Fatalf("zero-value %s projection = %#v, want empty string", key, got[key])
		}
	}
}

func TestResetSubscriptionTVTargetClearsRuntimeFacts(t *testing.T) {
	t.Parallel()
	season := 2
	target, err := resetSubscriptionTVTarget(Subscription{
		Season: &season,
		Config: map[string]any{"start_episode": float64(2), "total_episode": float64(4), "lack_episode": float64(1), "note": []any{2.0}},
	})
	if err != nil {
		t.Fatal(err)
	}
	got := target.Snapshot
	if got.Season != 2 || got.StartEpisode != 2 || got.TotalEpisode != 4 || got.LackEpisode != 4 || got.ManualTotalEpisode ||
		len(got.Note) != 0 || got.CurrentPriority != nil || !reflect.DeepEqual(got.EpisodePriority, map[int]int{}) {
		t.Fatalf("reset target = %#v", got)
	}
}

func TestPublicMutationSQLPreservesCurrentIntentDuringReset(t *testing.T) {
	t.Parallel()
	query := strings.Join(strings.Fields(applySubscriptionPublicMutationSQL), " ")
	for _, fragment := range []string{
		"WHEN $11 AND current_download_intent_id IS NULL THEN $12",
		"v2_state=CASE WHEN $11 THEN 'R' ELSE v2_state END",
		"updated_at=$3",
	} {
		if !strings.Contains(query, fragment) {
			t.Fatalf("public mutation SQL missing %q: %s", fragment, query)
		}
	}
	if strings.Contains(query, "current_download_intent_id=NULL") {
		t.Fatalf("reset clears current intent: %s", query)
	}
}

func TestPublicMutationConfigRejectsRuntimeAndIdentityBypass(t *testing.T) {
	t.Parallel()
	for _, key := range []string{"username", "state", "note", "lack_episode", "current_priority", "episode_priority", "manual_total_episode"} {
		if validSubscriptionPublicMutationConfig(SubscriptionPublicMutation{Config: map[string]any{key: "malicious"}}) {
			t.Fatalf("public mutation accepted protected key %q", key)
		}
	}
	if !validSubscriptionPublicMutationConfig(SubscriptionPublicMutation{Config: map[string]any{"keyword": nil, "sites": []int{}}}) {
		t.Fatal("public mutation rejected public fields")
	}
	if !validSubscriptionPublicMutationConfig(SubscriptionPublicMutation{Reset: true, Config: map[string]any{
		"note": []int{}, "lack_episode": 3, "current_priority": nil,
		"episode_priority": map[string]int{}, "manual_total_episode": 0,
	}}) {
		t.Fatal("reset mutation rejected protected reset payload")
	}
}

func TestResolveSubscriptionTVPublicTargetDiscardsInvalidEpisodePriority(t *testing.T) {
	t.Parallel()
	for name, test := range map[string]struct {
		value any
		want  map[int]int
	}{
		"scalar":        {"bad", map[int]int{}},
		"array":         {[]any{100}, map[int]int{}},
		"invalid key":   {map[string]any{"episode": float64(100)}, map[int]int{}},
		"invalid value": {map[string]any{"1": "bad"}, map[int]int{}},
		"mixed":         {map[string]any{"1": float64(100), "2": "bad"}, map[int]int{1: 100}},
		"string value":  {map[string]any{"1": "100", "2": nil}, map[int]int{1: 100}},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			observed := Subscription{Config: map[string]any{
				"total_episode": 2, "start_episode": 1, "lack_episode": 2,
				"episode_priority": test.value,
			}}
			got, err := ResolveSubscriptionTVPublicTarget(observed, 1, 1, 2)
			if err != nil || !reflect.DeepEqual(got.Snapshot.EpisodePriority, test.want) {
				t.Fatalf("priorities=%v want=%v err=%v", got.Snapshot.EpisodePriority, test.want, err)
			}
		})
	}
}

func TestResolveSubscriptionTVPublicTargetRejectsConflictingPriorityAliases(t *testing.T) {
	t.Parallel()
	observed := Subscription{Config: map[string]any{
		"total_episode": 2, "episode_priority": map[string]any{"1": 90, "01": 100},
	}}
	if _, err := ResolveSubscriptionTVPublicTarget(observed, 1, 1, 2); !errors.Is(err, ErrInvalidSubscriptionPublicMutation) {
		t.Fatalf("error=%v", err)
	}
}

func TestResolveSubscriptionTVPublicTargetAcceptsValidEpisodePriority(t *testing.T) {
	t.Parallel()
	observed := Subscription{Config: map[string]any{
		"total_episode": 2, "start_episode": 1, "lack_episode": 1,
		"episode_priority": map[string]any{"1": float64(100), "2": json.Number("90")},
	}}
	target, err := ResolveSubscriptionTVPublicTarget(observed, 1, 1, 2)
	if err != nil {
		t.Fatal(err)
	}
	if target.Snapshot.EpisodePriority[1] != 100 || target.Snapshot.EpisodePriority[2] != 90 {
		t.Fatalf("episode priority=%v", target.Snapshot.EpisodePriority)
	}
}

func TestSubscriptionSearchQueriesExcludeNullMediaType(t *testing.T) {
	t.Parallel()

	for name, query := range map[string]string{
		"precise first page": listSubscriptionsForPreciseSearchSQL,
		"precise next page":  listSubscriptionsForPreciseSearchAfterSQL,
		"rss":                subscriptionRSSBaseSQL,
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			if !strings.Contains(strings.Join(strings.Fields(query), " "), "media_type IS NOT NULL") {
				t.Fatalf("query does not quarantine NULL media type: %s", query)
			}
		})
	}
}

func TestNullableSubscriptionMediaTypeMigrationHasExplicitRollbackGuard(t *testing.T) {
	t.Parallel()

	content, err := migrationFS.ReadFile("migrations/036_subscription_nullable_media_type.sql")
	if err != nil {
		t.Fatal(err)
	}
	migration := strings.Join(strings.Fields(string(content)), " ")
	for _, fragment := range []string{
		"ALTER COLUMN media_type DROP NOT NULL",
		"-- +goose Down",
		"ALTER COLUMN media_type SET NOT NULL",
	} {
		if !strings.Contains(migration, fragment) {
			t.Fatalf("nullable media type migration missing %q: %s", fragment, migration)
		}
	}
	for _, forbidden := range []string{"SET media_type='movie'", "SET media_type='tv'", "SET media_type='电影'", "SET media_type='电视剧'"} {
		if strings.Contains(migration, forbidden) {
			t.Fatalf("rollback guesses a media type through %q", forbidden)
		}
	}
}
