//go:build integration

package database

import (
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"testing"

	"github.com/moviepilot-go/moviepilot-go/internal/downloader"
	"github.com/moviepilot-go/moviepilot-go/internal/subscription"
	"github.com/pressly/goose/v3"
)

func TestTVQualityAcceptancePostgres(t *testing.T) {
	store := newDownloadIntentPlanIntegrationStore(t)
	const owner = "tv-quality-owner"
	insertDownloadIntentIntegrationUsers(t, store, owner, "tv-quality-other")
	site := insertDownloadIntentPlanIntegrationSite(t, store, "tv-quality-site")
	episodes := []subscription.Episode{{Season: 1, Episode: 1}, {Season: 1, Episode: 2}}
	for index, tc := range []struct {
		name                 string
		best, full           bool
		quality              *int
		coverage             []subscription.Episode
		confirmed            bool
		state, want          subscription.State
		current, wantCurrent *int
		wantLack             int
	}{
		{"per90 continues", true, false, r08MoviePriority(90), episodes, true, subscription.Acquired, subscription.Active, nil, r08MoviePriority(90), 0},
		{"per100 completes", true, false, r08MoviePriority(100), episodes, true, subscription.Downloading, subscription.Completed, nil, r08MoviePriority(100), 0},
		{"per101 continues", true, false, r08MoviePriority(101), episodes, true, subscription.Acquired, subscription.Active, nil, r08MoviePriority(101), 0},
		{"per partial still missing", true, false, r08MoviePriority(100), episodes[:1], false, subscription.Acquired, subscription.Active, nil, r08MoviePriority(0), 1},
		{"full100 completes", true, true, r08MoviePriority(100), episodes, true, subscription.Acquired, subscription.Completed, r08MoviePriority(90), r08MoviePriority(100), 0},
		{"V2 full confirmed partial completes", true, true, r08MoviePriority(100), episodes[:1], true, subscription.Acquired, subscription.Completed, r08MoviePriority(90), r08MoviePriority(100), 1},
		{"full requires confirmation", true, true, r08MoviePriority(100), episodes, false, subscription.Acquired, subscription.Active, r08MoviePriority(90), r08MoviePriority(90), 0},
		{"P100 stays P", true, false, r08MoviePriority(100), episodes, true, subscription.PendingIdentification, subscription.PendingIdentification, nil, r08MoviePriority(100), 0},
		{"legacy never invents quality", true, false, nil, nil, false, subscription.Acquired, subscription.Active, nil, r08MoviePriority(0), 2},
		{"known empty never claims note", true, false, r08MoviePriority(100), []subscription.Episode{}, false, subscription.Acquired, subscription.Active, nil, r08MoviePriority(0), 2},
		{"ordinary row missing stays N", false, false, r08MoviePriority(90), episodes[:1], false, subscription.Acquired, subscription.PendingSearch, r08MoviePriority(30), r08MoviePriority(30), 1},
		{"ordinary no overall derivation", false, false, r08MoviePriority(90), episodes, true, subscription.Acquired, subscription.Completed, r08MoviePriority(30), r08MoviePriority(30), 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			key := fmt.Sprintf("tv-quality-%d", index)
			// Best upgrades own independent plan facts, without rebinding old episode rows.
			var bound []subscription.Episode
			if !tc.best {
				bound = tc.coverage
			}
			fixture := prepareDownloadIntentAcceptanceFixture(t, store, owner, key, "电视剧", episodes, bound, "")
			config := map[string]any{"best_version": tc.best, "best_version_full": tc.full, "total_episode": 2, "start_episode": 1, "lack_episode": 2, "note": []int{}}
			if tc.current != nil {
				config["current_priority"] = *tc.current
			}
			encoded, _ := json.Marshal(config)
			v2, _ := subscription.V2State(tc.state)
			if _, err := store.db.Pool.Exec(t.Context(), `UPDATE subscriptions SET season=1,config=$2::jsonb,internal_state=$3,v2_state=$4 WHERE id=$1`, fixture.subscriptionID, encoded, tc.state, v2); err != nil {
				t.Fatal(err)
			}
			plan := downloadIntentPlanIntegrationFixture(fixture.intent.ID, site, key)
			plan.TVPriority, plan.TVCoverage, plan.TVFullCoverage = tc.quality, tc.coverage, tc.confirmed
			if _, err := store.CreateDownloadIntentPlan(t.Context(), owner, plan); err != nil {
				t.Fatal(err)
			}
			before := readTVQualityConfig(t, store, fixture.subscriptionID)
			if _, ok := before["episode_priority"]; ok {
				t.Fatal("plan published quality before acceptance")
			}
			if _, _, err := store.AcceptDownloadIntent(t.Context(), "tv-quality-other", fixture.intent.ID, databaseIntentV1Hash); !errors.Is(err, ErrDownloadIntentNotFound) {
				t.Fatalf("owner err=%v", err)
			}
			intent, released, err := store.AcceptDownloadIntent(t.Context(), owner, fixture.intent.ID, databaseIntentV1Hash)
			if err != nil || !released || intent.State != downloader.IntentDownloading {
				t.Fatalf("accept=%+v released=%v err=%v", intent, released, err)
			}
			parent := readDownloadIntentAcceptanceParent(t, store, fixture.subscriptionID)
			if parent.state != tc.want || parent.currentIntent != nil || parent.completed != (tc.want == subscription.Completed) {
				t.Fatalf("parent=%+v", parent)
			}
			after := readTVQualityConfig(t, store, fixture.subscriptionID)
			if after["lack_episode"] != float64(tc.wantLack) || after["current_priority"] != float64(*tc.wantCurrent) {
				t.Fatalf("config=%+v", after)
			}
			if tc.quality == nil {
				if _, ok := after["episode_priority"]; ok {
					t.Fatal("legacy quality inferred")
				}
			}
			if _, replayed, err := store.AcceptDownloadIntent(t.Context(), owner, fixture.intent.ID, databaseIntentV1Hash); err != nil || !replayed {
				t.Fatalf("replay=%v err=%v", replayed, err)
			}
			replay := readDownloadIntentAcceptanceParent(t, store, fixture.subscriptionID)
			if !replay.updatedAt.Equal(parent.updatedAt) || !reflect.DeepEqual(after, readTVQualityConfig(t, store, fixture.subscriptionID)) {
				t.Fatal("replay changed settled quality")
			}
			if tc.best {
				for _, state := range readDownloadIntentAcceptanceEpisodeStates(t, store, fixture.subscriptionID) {
					if state != subscription.SubscriptionEpisodeMissing {
						t.Fatal("independent best facts overwrote old rows")
					}
				}
			}
		})
	}
}

func TestTVQualityAcceptancePostgresTargetAndParentProtection(t *testing.T) {
	store := newDownloadIntentPlanIntegrationStore(t)
	const owner = "tv-quality-guard"
	insertDownloadIntentIntegrationUsers(t, store, owner)
	site := insertDownloadIntentPlanIntegrationSite(t, store, "tv-quality-guard-site")
	for _, mode := range []string{"expanded", "season-changed", "paused", "new-intent", "null-type", "movie-type"} {
		t.Run(mode, func(t *testing.T) {
			fixture := prepareDownloadIntentAcceptanceFixture(t, store, owner, mode, "电视剧", nil, nil, "")
			plan := downloadIntentPlanIntegrationFixture(fixture.intent.ID, site, mode)
			plan.TVPriority = r08MoviePriority(100)
			plan.TVCoverage = []subscription.Episode{{Season: 1, Episode: 1}, {Season: 1, Episode: 2}}
			plan.TVFullCoverage = true
			if _, err := store.CreateDownloadIntentPlan(t.Context(), owner, plan); err != nil {
				t.Fatal(err)
			}
			if _, err := store.db.Pool.Exec(t.Context(), `UPDATE subscriptions SET season=1,config='{"best_version":true,"best_version_full":true,"total_episode":2,"current_priority":90}'::jsonb WHERE id=$1`, fixture.subscriptionID); err != nil {
				t.Fatal(err)
			}
			var err error
			switch mode {
			case "expanded":
				_, err = store.db.Pool.Exec(t.Context(), `UPDATE subscriptions SET config=jsonb_set(config,'{total_episode}','3') WHERE id=$1`, fixture.subscriptionID)
			case "season-changed":
				_, err = store.db.Pool.Exec(t.Context(), `UPDATE subscriptions SET season=2 WHERE id=$1`, fixture.subscriptionID)
			case "paused":
				_, err = store.db.Pool.Exec(t.Context(), `UPDATE subscriptions SET internal_state='paused',v2_state='S' WHERE id=$1`, fixture.subscriptionID)
			case "new-intent":
				other := createDownloadIntentAcceptanceIntent(t, store, owner, "guard-new")
				_, err = store.db.Pool.Exec(t.Context(), `UPDATE subscriptions SET current_download_intent_id=$2 WHERE id=$1`, fixture.subscriptionID, other.ID)
			case "null-type":
				_, err = store.db.Pool.Exec(t.Context(), `UPDATE subscriptions SET media_type=NULL WHERE id=$1`, fixture.subscriptionID)
			case "movie-type":
				_, err = store.db.Pool.Exec(t.Context(), `UPDATE subscriptions SET media_type='电影' WHERE id=$1`, fixture.subscriptionID)
			}
			if err != nil {
				t.Fatal(err)
			}
			before := readTVQualityConfig(t, store, fixture.subscriptionID)
			parentBefore := readDownloadIntentAcceptanceParent(t, store, fixture.subscriptionID)
			if accepted, _, err := store.AcceptDownloadIntent(t.Context(), owner, fixture.intent.ID, databaseIntentV1Hash); err != nil || accepted.State != downloader.IntentDownloading {
				t.Fatalf("accepted=%+v err=%v", accepted, err)
			}
			after := readTVQualityConfig(t, store, fixture.subscriptionID)
			wantPriority := float64(90)
			if mode == "expanded" || mode == "season-changed" {
				wantPriority = 100
			}
			if after["current_priority"] != wantPriority {
				t.Fatalf("V2 confirmed coverage priority=%v want=%v", after["current_priority"], wantPriority)
			}
			if mode == "paused" || mode == "new-intent" || mode == "null-type" {
				if !reflect.DeepEqual(before, after) || !reflect.DeepEqual(parentBefore, readDownloadIntentAcceptanceParent(t, store, fixture.subscriptionID)) {
					t.Fatal("protected parent changed")
				}
			}
			if mode == "movie-type" {
				if _, ok := after["episode_priority"]; ok {
					t.Fatal("TV quality crossed media boundary")
				}
			}
		})
	}
}

func TestDownloadIntentTVQualityMigrationPostgres(t *testing.T) {
	store := newDownloadIntentPlanIntegrationStore(t)
	var schema string
	if err := store.db.Pool.QueryRow(t.Context(), `SELECT current_schema()`).Scan(&schema); err != nil {
		t.Fatal(err)
	}
	migrationDB, _ := openSubscriptionV2StateMigrationDB(t, schema)
	goose.SetBaseFS(migrationFS)
	if err := goose.DownToContext(t.Context(), migrationDB, "migrations", 38); err != nil {
		t.Fatal(err)
	}
	const owner = "tv-quality-migration"
	insertDownloadIntentIntegrationUsers(t, store, owner)
	site := insertDownloadIntentPlanIntegrationSite(t, store, "tv-quality-migration-site")
	intent := createDownloadIntentAcceptanceIntent(t, store, owner, "legacy-tv-plan")
	if _, err := store.db.Pool.Exec(t.Context(), `INSERT INTO download_intent_plans(intent_id,site_id,downloader_id,resolver_kind,resolver_version,resource_id) VALUES($1,$2,$3,'haidan',1,'legacy')`, intent.ID, site, downloadIntentPlanTestDownloaderID); err != nil {
		t.Fatal(err)
	}
	if err := goose.UpToContext(t.Context(), migrationDB, "migrations", 40); err != nil {
		t.Fatal(err)
	}
	if plan, err := store.GetDownloadIntentPlan(t.Context(), owner, intent.ID); err != nil || plan.TVPriority != nil || plan.TVCoverage != nil || plan.TVFullCoverage {
		t.Fatalf("legacy=%+v err=%v", plan, err)
	}
	knownIntent := createDownloadIntentAcceptanceIntent(t, store, owner, "knownempty-tv-plan")
	known := downloadIntentPlanIntegrationFixture(knownIntent.ID, site, "empty")
	known.TVCoverage = []subscription.Episode{}
	known.TVPriority = r08MoviePriority(0)
	if _, err := store.CreateDownloadIntentPlan(t.Context(), owner, known); err != nil {
		t.Fatal(err)
	}
	if err := goose.DownToContext(t.Context(), migrationDB, "migrations", 38); err == nil {
		t.Fatal("down dropped known empty/zero quality")
	}
	if err := goose.UpToContext(t.Context(), migrationDB, "migrations", 40); err != nil {
		t.Fatal(err)
	}
	if plan, err := store.GetDownloadIntentPlan(t.Context(), owner, knownIntent.ID); err != nil || plan.TVPriority == nil || *plan.TVPriority != 0 || plan.TVCoverage == nil {
		t.Fatalf("guard lost facts %+v err=%v", plan, err)
	}
	if _, err := store.db.Pool.Exec(t.Context(), `DELETE FROM download_intents WHERE id=$1`, knownIntent.ID); err != nil {
		t.Fatal(err)
	}
	if err := goose.DownToContext(t.Context(), migrationDB, "migrations", 38); err != nil {
		t.Fatal(err)
	}
	if err := goose.UpToContext(t.Context(), migrationDB, "migrations", 40); err != nil {
		t.Fatal(err)
	}
}

func readTVQualityConfig(t *testing.T, store *Store, id int64) map[string]any {
	t.Helper()
	var encoded []byte
	if err := store.db.Pool.QueryRow(t.Context(), `SELECT config FROM subscriptions WHERE id=$1`, id).Scan(&encoded); err != nil {
		t.Fatal(err)
	}
	var config map[string]any
	if err := json.Unmarshal(encoded, &config); err != nil {
		t.Fatal(err)
	}
	return config
}
