//go:build integration

package database

import (
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"testing"
	"time"

	"github.com/pressly/goose/v3"

	"github.com/moviepilot-go/moviepilot-go/internal/downloader"
	"github.com/moviepilot-go/moviepilot-go/internal/subscription"
)

func TestMovieQualityAcceptancePostgres(t *testing.T) {
	store := newDownloadIntentPlanIntegrationStore(t)
	const owner = "movie-quality-owner"
	insertDownloadIntentIntegrationUsers(t, store, owner, "movie-quality-other")
	siteID := insertDownloadIntentPlanIntegrationSite(t, store, "movie-quality-site")
	for index, tc := range []struct {
		name              string
		best              bool
		current, recorded *int
		state             subscription.State
		want              subscription.State
		wantPriority      *int
	}{
		{"normal records 90", false, nil, r08MoviePriority(90), subscription.Acquired, subscription.Completed, r08MoviePriority(90)},
		{"best records 90 and stays R", true, nil, r08MoviePriority(90), subscription.Acquired, subscription.Active, r08MoviePriority(90)},
		{"best records 100 and completes", true, r08MoviePriority(90), r08MoviePriority(100), subscription.Downloading, subscription.Completed, r08MoviePriority(100)},
		{"best 101 remains R", true, r08MoviePriority(90), r08MoviePriority(101), subscription.Acquired, subscription.Active, r08MoviePriority(101)},
		{"best negative known quality", true, r08MoviePriority(0), r08MoviePriority(-1), subscription.Acquired, subscription.Active, r08MoviePriority(-1)},
		{"P records 100 and stays P", true, nil, r08MoviePriority(100), subscription.PendingIdentification, subscription.PendingIdentification, r08MoviePriority(100)},
		{"ordinary P also stays P", false, nil, r08MoviePriority(90), subscription.PendingIdentification, subscription.PendingIdentification, r08MoviePriority(90)},
		{"legacy absent plan preserves 90", true, r08MoviePriority(90), nil, subscription.Acquired, subscription.Active, r08MoviePriority(90)},
		{"legacy absent plan preserves 100", true, r08MoviePriority(100), nil, subscription.Acquired, subscription.Completed, r08MoviePriority(100)},
		{"legacy absent plan stays unknown", true, nil, nil, subscription.Acquired, subscription.Active, nil},
		{"legacy null plan preserves 90", true, r08MoviePriority(90), nil, subscription.Acquired, subscription.Active, r08MoviePriority(90)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			key := fmt.Sprintf("movie-quality-%d", index)
			fixture := prepareDownloadIntentAcceptanceFixture(t, store, owner, key, "电影", nil, nil, "")
			config := map[string]any{"best_version": tc.best, "note": []int{2}, "lack_episode": 3}
			if tc.current != nil {
				config["current_priority"] = *tc.current
			}
			encoded, err := json.Marshal(config)
			if err != nil {
				t.Fatal(err)
			}
			v2, _ := subscription.V2State(tc.state)
			if _, err := store.db.Pool.Exec(t.Context(), `UPDATE subscriptions SET config=$2::jsonb, internal_state=$3, v2_state=$4 WHERE id=$1`, fixture.subscriptionID, encoded, tc.state, v2); err != nil {
				t.Fatal(err)
			}
			if tc.recorded != nil || tc.name == "legacy null plan preserves 90" {
				plan := downloadIntentPlanIntegrationFixture(fixture.intent.ID, siteID, key)
				plan.MoviePriority = tc.recorded
				if _, err := store.CreateDownloadIntentPlan(t.Context(), owner, plan); err != nil {
					t.Fatal(err)
				}
			}
			if _, _, err := store.AcceptDownloadIntent(t.Context(), "movie-quality-other", fixture.intent.ID, databaseIntentV1Hash); err != ErrDownloadIntentNotFound {
				t.Fatalf("other owner: %v", err)
			}
			accepted, released, err := store.AcceptDownloadIntent(t.Context(), owner, fixture.intent.ID, databaseIntentV1Hash)
			if err != nil || !released || accepted.State != downloader.IntentDownloading {
				t.Fatalf("accept=%+v released=%v err=%v", accepted, released, err)
			}
			parent := readDownloadIntentAcceptanceParent(t, store, fixture.subscriptionID)
			wantV2, _ := subscription.V2State(tc.want)
			if parent.state != tc.want || parent.v2State != wantV2 || parent.currentIntent != nil || parent.completed != (tc.want == subscription.Completed) {
				t.Fatalf("parent=%+v want=%s", parent, tc.want)
			}
			priority := readR08MovieQuality(t, store, fixture.subscriptionID)
			if !reflect.DeepEqual(priority, tc.wantPriority) {
				t.Fatalf("priority=%v want=%v", priority, tc.wantPriority)
			}
			note, lack := readDownloadIntentAcceptanceProgress(t, store, fixture.subscriptionID)
			wantNote, wantLack := []int{2}, 3
			if !tc.best {
				wantNote, wantLack = []int{1, 2}, 0
			}
			if !reflect.DeepEqual(note, wantNote) || lack != wantLack {
				t.Fatalf("note=%v lack=%d", note, lack)
			}
			if _, released, err := store.AcceptDownloadIntent(t.Context(), owner, fixture.intent.ID, databaseIntentV1Hash); err != nil || !released {
				t.Fatalf("replay released=%v err=%v", released, err)
			}
			replay := readDownloadIntentAcceptanceParent(t, store, fixture.subscriptionID)
			if !replay.updatedAt.Equal(parent.updatedAt) || !sameDownloadIntentAcceptanceTime(replay.completedAt, parent.completedAt) || !reflect.DeepEqual(readR08MovieQuality(t, store, fixture.subscriptionID), priority) {
				t.Fatal("replay changed settled parent")
			}
		})
	}
}

func TestMovieQualityAcceptancePostgresParentProtection(t *testing.T) {
	store := newDownloadIntentPlanIntegrationStore(t)
	const owner = "movie-quality-guard"
	insertDownloadIntentIntegrationUsers(t, store, owner)
	siteID := insertDownloadIntentPlanIntegrationSite(t, store, "movie-quality-guard-site")
	for _, mode := range []string{"paused", "new-intent", "released", "null-type", "tv-type"} {
		t.Run(mode, func(t *testing.T) {
			fixture := prepareDownloadIntentAcceptanceFixture(t, store, owner, mode, "电影", nil, nil, "")
			plan := downloadIntentPlanIntegrationFixture(fixture.intent.ID, siteID, mode)
			plan.MoviePriority = r08MoviePriority(100)
			if _, err := store.CreateDownloadIntentPlan(t.Context(), owner, plan); err != nil {
				t.Fatal(err)
			}
			if _, err := store.db.Pool.Exec(t.Context(), `UPDATE subscriptions SET config='{"best_version":true,"current_priority":90}'::jsonb WHERE id=$1`, fixture.subscriptionID); err != nil {
				t.Fatal(err)
			}
			switch mode {
			case "paused":
				_, err := store.db.Pool.Exec(t.Context(), `UPDATE subscriptions SET internal_state='paused',v2_state='S' WHERE id=$1`, fixture.subscriptionID)
				if err != nil {
					t.Fatal(err)
				}
			case "new-intent":
				other := createDownloadIntentAcceptanceIntent(t, store, owner, "new-parent-intent")
				_, err := store.db.Pool.Exec(t.Context(), `UPDATE subscriptions SET current_download_intent_id=$2 WHERE id=$1`, fixture.subscriptionID, other.ID)
				if err != nil {
					t.Fatal(err)
				}
			case "released":
				_, err := store.db.Pool.Exec(t.Context(), `UPDATE subscriptions SET current_download_intent_id=NULL, internal_state='active',v2_state='R' WHERE id=$1`, fixture.subscriptionID)
				if err != nil {
					t.Fatal(err)
				}
			case "null-type":
				_, err := store.db.Pool.Exec(t.Context(), `UPDATE subscriptions SET media_type=NULL WHERE id=$1`, fixture.subscriptionID)
				if err != nil {
					t.Fatal(err)
				}
			case "tv-type":
				_, err := store.db.Pool.Exec(t.Context(), `UPDATE subscriptions SET media_type='电视剧' WHERE id=$1`, fixture.subscriptionID)
				if err != nil {
					t.Fatal(err)
				}
			}
			before := readDownloadIntentAcceptanceParent(t, store, fixture.subscriptionID)
			accepted, _, err := store.AcceptDownloadIntent(t.Context(), owner, fixture.intent.ID, databaseIntentV1Hash)
			if err != nil || accepted.State != downloader.IntentDownloading {
				t.Fatalf("accept=%+v err=%v", accepted, err)
			}
			after := readDownloadIntentAcceptanceParent(t, store, fixture.subscriptionID)
			if mode != "tv-type" && !reflect.DeepEqual(after, before) {
				t.Fatalf("protected parent changed: before=%+v after=%+v", before, after)
			}
			quality := readR08MovieQuality(t, store, fixture.subscriptionID)
			if quality == nil || *quality != 90 {
				t.Fatalf("old movie fact changed current quality: %v", quality)
			}
		})
	}
}

func TestDownloadIntentMoviePriorityMigrationPostgres(t *testing.T) {
	store := newDownloadIntentPlanIntegrationStore(t)
	var schema string
	if err := store.db.Pool.QueryRow(t.Context(), `SELECT current_schema()`).Scan(&schema); err != nil {
		t.Fatal(err)
	}
	migrationDB, _ := openSubscriptionV2StateMigrationDB(t, schema)
	goose.SetBaseFS(migrationFS)
	if err := goose.DownToContext(t.Context(), migrationDB, "migrations", 37); err != nil {
		t.Fatal(err)
	}
	const owner = "movie-quality-migration"
	insertDownloadIntentIntegrationUsers(t, store, owner)
	siteID := insertDownloadIntentPlanIntegrationSite(t, store, "movie-quality-migration-site")
	intent := createDownloadIntentAcceptanceIntent(t, store, owner, "legacy-migration-plan")
	if _, err := store.db.Pool.Exec(t.Context(), `INSERT INTO download_intent_plans(intent_id,site_id,downloader_id,resolver_kind,resolver_version,resource_id) VALUES($1,$2,$3,'haidan',1,'legacy')`, intent.ID, siteID, downloadIntentPlanTestDownloaderID); err != nil {
		t.Fatal(err)
	}
	if err := goose.UpToContext(t.Context(), migrationDB, "migrations", 40); err != nil {
		t.Fatal(err)
	}
	plan, err := store.GetDownloadIntentPlan(t.Context(), owner, intent.ID)
	if err != nil || plan.MoviePriority != nil {
		t.Fatalf("legacy plan=%+v err=%v", plan, err)
	}
	newIntent := createDownloadIntentAcceptanceIntent(t, store, owner, "known-migration-plan")
	known := downloadIntentPlanIntegrationFixture(newIntent.ID, siteID, "known")
	known.MoviePriority = r08MoviePriority(0)
	if _, err := store.CreateDownloadIntentPlan(t.Context(), owner, known); err != nil {
		t.Fatal(err)
	}
	if err := goose.DownToContext(t.Context(), migrationDB, "migrations", 37); err == nil {
		t.Fatal("rollback dropped recorded zero quality")
	}
	// A later empty TV-facts migration can have rolled back before the movie
	// guard rejected migration 038; restore the schema required by this binary.
	if err := goose.UpToContext(t.Context(), migrationDB, "migrations", 40); err != nil {
		t.Fatal(err)
	}
	if persisted, err := store.GetDownloadIntentPlan(t.Context(), owner, newIntent.ID); err != nil || persisted.MoviePriority == nil || *persisted.MoviePriority != 0 {
		t.Fatalf("guard lost snapshot: %+v err=%v", persisted, err)
	}
	// Explicitly remove the disposable fixture intent and its cascading plan.
	if _, err := store.db.Pool.Exec(t.Context(), `DELETE FROM download_intents WHERE id=$1`, newIntent.ID); err != nil {
		t.Fatal(err)
	}
	if err := goose.DownToContext(t.Context(), migrationDB, "migrations", 37); err != nil {
		t.Fatal(err)
	}
	if err := goose.UpToContext(t.Context(), migrationDB, "migrations", 40); err != nil {
		t.Fatal(err)
	}
	if persisted, err := store.GetDownloadIntentPlan(t.Context(), owner, intent.ID); err != nil || persisted.MoviePriority != nil {
		t.Fatalf("legacy restore: %+v err=%v", persisted, err)
	}
}

func TestMovieQualityAcceptancePostgresUpgradeAndOldReplay(t *testing.T) {
	store := newDownloadIntentPlanIntegrationStore(t)
	const owner = "movie-quality-upgrade"
	insertDownloadIntentIntegrationUsers(t, store, owner)
	siteID := insertDownloadIntentPlanIntegrationSite(t, store, "movie-quality-upgrade-site")
	fixture := prepareDownloadIntentAcceptanceFixture(t, store, owner, "first-90", "电影", nil, nil, "")
	if _, err := store.db.Pool.Exec(t.Context(), `UPDATE subscriptions SET config='{"best_version":true}'::jsonb WHERE id=$1`, fixture.subscriptionID); err != nil {
		t.Fatal(err)
	}
	first := downloadIntentPlanIntegrationFixture(fixture.intent.ID, siteID, "first-90")
	first.MoviePriority = r08MoviePriority(90)
	if _, err := store.CreateDownloadIntentPlan(t.Context(), owner, first); err != nil {
		t.Fatal(err)
	}
	// Persisting a plan for an unknown Add result must not publish its quality.
	if readR08MovieQuality(t, store, fixture.subscriptionID) != nil {
		t.Fatal("plan creation published quality before acceptance")
	}
	if _, released, err := store.AcceptDownloadIntent(t.Context(), owner, fixture.intent.ID, databaseIntentV1Hash); err != nil || !released {
		t.Fatalf("first accepted: %v %v", released, err)
	}
	if parent := readDownloadIntentAcceptanceParent(t, store, fixture.subscriptionID); parent.state != subscription.Active || parent.completed {
		t.Fatalf("90 prematurely completed: %+v", parent)
	}
	secondIntent := createDownloadIntentAcceptanceIntent(t, store, owner, "second-100")
	second := downloadIntentPlanIntegrationFixture(secondIntent.ID, siteID, "second-100")
	second.MoviePriority = r08MoviePriority(100)
	if _, err := store.CreateDownloadIntentPlan(t.Context(), owner, second); err != nil {
		t.Fatal(err)
	}
	if _, err := store.db.Pool.Exec(t.Context(), `UPDATE download_intents SET subscription_id=$2, state='submitting' WHERE id=$1`, secondIntent.ID, fixture.subscriptionID); err != nil {
		t.Fatal(err)
	}
	if _, err := store.db.Pool.Exec(t.Context(), `UPDATE subscriptions SET current_download_intent_id=$2, internal_state='acquired', v2_state='R' WHERE id=$1`, fixture.subscriptionID, secondIntent.ID); err != nil {
		t.Fatal(err)
	}
	before := readDownloadIntentAcceptanceParent(t, store, fixture.subscriptionID)
	if _, _, err := store.AcceptDownloadIntent(t.Context(), owner, fixture.intent.ID, databaseIntentV1Hash); err != nil {
		t.Fatal(err)
	}
	if after := readDownloadIntentAcceptanceParent(t, store, fixture.subscriptionID); !reflect.DeepEqual(after, before) {
		t.Fatal("old acceptance changed new current intent")
	}
	if _, released, err := store.AcceptDownloadIntent(t.Context(), owner, secondIntent.ID, databaseIntentDifferentV1Hash); err != nil || !released {
		t.Fatalf("second accepted: %v %v", released, err)
	}
	completed := readDownloadIntentAcceptanceParent(t, store, fixture.subscriptionID)
	if completed.state != subscription.Completed || !completed.completed {
		t.Fatalf("100 not complete: %+v", completed)
	}
	if _, released, err := store.AcceptDownloadIntent(t.Context(), owner, fixture.intent.ID, databaseIntentV1Hash); err != nil || !released {
		t.Fatalf("old completed replay: %v %v", released, err)
	}
	if quality := readR08MovieQuality(t, store, fixture.subscriptionID); quality == nil || *quality != 100 {
		t.Fatalf("old 90 overwrote 100: %v", quality)
	}
	if replay := readDownloadIntentAcceptanceParent(t, store, fixture.subscriptionID); !replay.updatedAt.Equal(completed.updatedAt) {
		t.Fatal("old acceptance updated completed parent")
	}
}

func readR08MovieQuality(t *testing.T, store *Store, subscriptionID int64) *int {
	t.Helper()
	var raw []byte
	if err := store.db.Pool.QueryRow(t.Context(), `SELECT config FROM subscriptions WHERE id=$1`, subscriptionID).Scan(&raw); err != nil {
		t.Fatal(err)
	}
	var config struct {
		Current *int `json:"current_priority"`
	}
	if err := json.Unmarshal(raw, &config); err != nil {
		t.Fatal(err)
	}
	return config.Current
}

func TestMovieTerminalQualityCompletionPostgresCAS(t *testing.T) {
	store := newDownloadIntentPlanIntegrationStore(t)
	const owner = "movie-terminal-owner"
	insertDownloadIntentIntegrationUsers(t, store, owner, "movie-terminal-other")
	for index, tc := range []struct {
		name                string
		state               subscription.State
		config              map[string]any
		mediaType           string
		bound, stale, other bool
		want                error
	}{
		{"N terminal", subscription.PendingSearch, map[string]any{"best_version": true, "current_priority": 100}, "电影", false, false, false, nil},
		{"R terminal", subscription.Active, map[string]any{"best_version": 1, "current_priority": 100}, "电影", false, false, false, nil},
		{"90 is not terminal", subscription.Active, map[string]any{"best_version": true, "current_priority": 90}, "电影", false, false, false, ErrSubscriptionPreciseSearchStateConflict},
		{"101 is not terminal", subscription.Active, map[string]any{"best_version": true, "current_priority": 101}, "电影", false, false, false, ErrSubscriptionPreciseSearchStateConflict},
		{"string 100 is not a quality fact", subscription.Active, map[string]any{"best_version": true, "current_priority": "100"}, "电影", false, false, false, ErrSubscriptionPreciseSearchStateConflict},
		{"TV remains outside movie completion", subscription.Active, map[string]any{"best_version": true, "current_priority": 100}, "电视剧", false, false, false, ErrSubscriptionPreciseSearchStateConflict},
		{"P cannot complete", subscription.PendingIdentification, map[string]any{"best_version": true, "current_priority": 100}, "电影", false, false, false, ErrInvalidSubscriptionPreciseSearch},
		{"S cannot complete", subscription.Paused, map[string]any{"best_version": true, "current_priority": 100}, "电影", false, false, false, ErrInvalidSubscriptionPreciseSearch},
		{"current intent protects parent", subscription.Active, map[string]any{"best_version": true, "current_priority": 100}, "电影", true, false, false, ErrSubscriptionPreciseSearchStateConflict},
		{"stale version", subscription.Active, map[string]any{"best_version": true, "current_priority": 100}, "电影", false, true, false, ErrSubscriptionPreciseSearchStateConflict},
		{"other owner", subscription.Active, map[string]any{"best_version": true, "current_priority": 100}, "电影", false, false, true, ErrSubscriptionNotFound},
	} {
		t.Run(tc.name, func(t *testing.T) {
			// CreateSubscription 只接受初始状态（待识别/待搜索/暂停），
			// 这里先以 PendingSearch 建单，再按用例需要把状态推进到目标态。
			id, err := store.CreateSubscription(t.Context(), NewSubscription{Username: owner, Name: fmt.Sprintf("terminal-%d", index), MediaType: tc.mediaType, MediaSource: "themoviedb", MediaID: fmt.Sprintf("terminal-%d", index), State: subscription.PendingSearch, Config: tc.config})
			if err != nil {
				t.Fatal(err)
			}
			if tc.state != subscription.PendingSearch {
				if _, err := store.db.Pool.Exec(t.Context(),
					`UPDATE subscriptions SET internal_state=$2, v2_state=$3 WHERE id=$1`,
					id, tc.state, v2State(tc.state)); err != nil {
					t.Fatalf("推进订阅状态到 %q: %v", tc.state, err)
				}
			}
			if tc.bound {
				intent := createDownloadIntentAcceptanceIntent(t, store, owner, fmt.Sprintf("terminal-bound-%d", index))
				if _, err := store.db.Pool.Exec(t.Context(), `UPDATE subscriptions SET current_download_intent_id=$2 WHERE id=$1`, id, intent.ID); err != nil {
					t.Fatal(err)
				}
			}
			before := readDownloadIntentAcceptanceParent(t, store, id)
			version := before.updatedAt
			if tc.stale {
				version = version.Add(-time.Second)
			}
			username := owner
			if tc.other {
				username = "movie-terminal-other"
			}
			err = store.CompleteSatisfiedMovieSubscription(t.Context(), username, id, tc.state, version)
			if !errors.Is(err, tc.want) {
				t.Fatalf("completion error=%v want=%v", err, tc.want)
			}
			after := readDownloadIntentAcceptanceParent(t, store, id)
			if tc.want != nil {
				if !reflect.DeepEqual(before, after) {
					t.Fatal("rejected completion changed parent")
				}
				return
			}
			if !after.completed || after.state != subscription.Completed || after.currentIntent != nil {
				t.Fatalf("completed=%+v", after)
			}
			if quality := readR08MovieQuality(t, store, id); quality == nil || *quality != 100 {
				t.Fatal("completion changed quality")
			}
		})
	}
}
