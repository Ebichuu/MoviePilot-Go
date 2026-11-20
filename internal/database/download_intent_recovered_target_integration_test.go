//go:build integration

package database

import (
	"errors"
	"fmt"
	"reflect"
	"testing"

	"github.com/moviepilot-go/moviepilot-go/internal/downloader"
	"github.com/moviepilot-go/moviepilot-go/internal/organization"
	"github.com/moviepilot-go/moviepilot-go/internal/subscription"
	"github.com/pressly/goose/v3"
)

func TestRecoveredTVProgressPostgresStateAndFacts(t *testing.T) {
	store := newDownloadIntentPlanIntegrationStore(t)
	const owner = "recovered-progress"
	insertDownloadIntentIntegrationUsers(t, store, owner, "recovered-progress-other")
	for index, tc := range []struct {
		name  string
		state subscription.State
		lack  int
		want  subscription.State
	}{
		{"N partial", subscription.PendingSearch, 2, subscription.Active},
		{"R partial", subscription.Active, 2, subscription.Active},
		{"P partial", subscription.PendingIdentification, 2, subscription.PendingIdentification},
		{"N complete", subscription.PendingSearch, 0, subscription.Completed},
		{"R complete", subscription.Active, 0, subscription.Completed},
		{"P zero", subscription.PendingIdentification, 0, subscription.PendingIdentification},
	} {
		t.Run(tc.name, func(t *testing.T) {
			id := createTVSubmissionIntegrationParent(t, store, owner, fmt.Sprintf("recovered-progress-%d", index), tc.state, false)
			setTVQualityIntegrationConfig(t, store, id, map[string]any{"total_episode": 0, "lack_episode": 9, "note": []int{9}, "current_priority": 70, "episode_priority": map[string]int{"9": 90}, "fixture": tc.name})
			before := mustActiveSubscription(t, store, owner, id)
			if err := store.RecordRecoveredTVSubscriptionProgress(t.Context(), owner, id, tc.state, before.UpdatedAt, tc.lack); err != nil {
				t.Fatal(err)
			}
			parent := readDownloadIntentAcceptanceParent(t, store, id)
			after := readTVQualityConfig(t, store, id)
			wantConfig := before.Config
			wantConfig["lack_episode"] = float64(tc.lack)
			if parent.state != tc.want || parent.completed != (tc.want == subscription.Completed) || parent.currentIntent != nil ||
				!reflect.DeepEqual(after, wantConfig) || !parent.updatedAt.After(before.UpdatedAt) {
				t.Fatalf("parent=%+v config=%v", parent, after)
			}
			if err := store.RecordRecoveredTVSubscriptionProgress(t.Context(), owner, id, tc.state, before.UpdatedAt, tc.lack); err == nil {
				t.Fatal("stale progress replay succeeded")
			}
		})
	}
	for index, tc := range []struct {
		name, mutation, callOwner string
		want                      error
	}{
		{"foreign owner", "", "recovered-progress-other", ErrSubscriptionNotFound},
		{"stale version", "UPDATE subscriptions SET updated_at=updated_at + interval '1 second' WHERE id=$1", "", ErrSubscriptionPreciseSearchStateConflict},
		{"positive configured total", "UPDATE subscriptions SET config=jsonb_set(config,'{total_episode}','2') WHERE id=$1", "", ErrSubscriptionPreciseSearchStateConflict},
		{"best version", "UPDATE subscriptions SET config=jsonb_set(config,'{best_version}','true') WHERE id=$1", "", ErrSubscriptionPreciseSearchStateConflict},
		{"movie type", "UPDATE subscriptions SET media_type='movie' WHERE id=$1", "", ErrSubscriptionPreciseSearchStateConflict},
	} {
		t.Run(tc.name, func(t *testing.T) {
			id := createTVSubmissionIntegrationParent(t, store, owner, fmt.Sprintf("recovered-guard-%d", index), subscription.Active, false)
			setTVQualityIntegrationConfig(t, store, id, map[string]any{"total_episode": 0, "lack_episode": 9})
			before := mustActiveSubscription(t, store, owner, id)
			if tc.mutation != "" {
				if _, err := store.db.Pool.Exec(t.Context(), tc.mutation, id); err != nil {
					t.Fatal(err)
				}
			}
			callOwner := owner
			if tc.callOwner != "" {
				callOwner = tc.callOwner
			}
			configBefore := readTVQualityConfig(t, store, id)
			parentBefore := readDownloadIntentAcceptanceParent(t, store, id)
			if err := store.RecordRecoveredTVSubscriptionProgress(t.Context(), callOwner, id, subscription.Active, before.UpdatedAt, 0); !errors.Is(err, tc.want) {
				t.Fatalf("error=%v want=%v", err, tc.want)
			}
			if !reflect.DeepEqual(configBefore, readTVQualityConfig(t, store, id)) || !reflect.DeepEqual(parentBefore, readDownloadIntentAcceptanceParent(t, store, id)) {
				t.Fatal("guard failure modified parent")
			}
		})
	}
}

func TestRecoveredTVSubmissionAndAcceptancePostgresPreserveHistoryAndReplay(t *testing.T) {
	store := newDownloadIntentPlanIntegrationStore(t)
	const owner = "recovered-acceptance"
	insertDownloadIntentIntegrationUsers(t, store, owner, "recovered-acceptance-other")
	site := insertDownloadIntentPlanIntegrationSite(t, store, "recovered-acceptance-site")
	for index, tc := range []struct {
		name     string
		state    subscription.State
		expected []int
		want     subscription.State
		lack     int
	}{
		{"N partial", subscription.PendingSearch, []int{1}, subscription.PendingSearch, 2},
		{"N full count-only", subscription.PendingSearch, []int{1, 2, 3}, subscription.Completed, 0},
		{"R partial", subscription.Active, []int{1}, subscription.PendingSearch, 2},
		{"R full", subscription.Active, []int{1, 2, 3}, subscription.Completed, 0},
		{"P partial", subscription.PendingIdentification, []int{1}, subscription.PendingIdentification, 2},
		{"P full", subscription.PendingIdentification, []int{1, 2, 3}, subscription.PendingIdentification, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			id := createTVSubmissionIntegrationParent(t, store, owner, fmt.Sprintf("recovered-acceptance-%d", index), tc.state, false)
			setTVQualityIntegrationConfig(t, store, id, map[string]any{"total_episode": 0, "lack_episode": 9, "note": []int{9}, "current_priority": 70, "episode_priority": map[string]int{"9": 90}})
			old := createDownloadIntentAcceptanceIntent(t, store, owner, fmt.Sprintf("recovered-old-%d", index))
			if _, err := store.db.Pool.Exec(t.Context(), `UPDATE download_intents SET state='downloading',torrent_hash=$2,subscription_id=$3 WHERE id=$1`, old.ID, databaseIntentV1Hash, id); err != nil {
				t.Fatal(err)
			}
			if err := store.UpsertSubscriptionEpisodes(t.Context(), owner, id, recoveredTargetFixture(1).MissingEpisodes); err != nil {
				t.Fatal(err)
			}
			if _, err := store.db.Pool.Exec(t.Context(), `UPDATE subscription_episodes SET state='indexed',download_intent_id=$2 WHERE subscription_id=$1`, id, old.ID); err != nil {
				t.Fatal(err)
			}
			before := mustActiveSubscription(t, store, owner, id)
			intent := mustDatabaseDownloadIntent(t, owner, fmt.Sprintf("recovered-new-%d", index))
			plan := downloadIntentPlanIntegrationFixture(intent.ID, site, fmt.Sprintf("recovered-plan-%d", index))
			plan.TVPriority = r08MoviePriority(90)
			plan.TVCoverage = []subscription.Episode{{Season: 1, Episode: 101}}
			plan.TVRecoveredTarget = recoveredTargetFixture(tc.expected...)
			input := organization.SuccessInput{IntentID: intent.ID, Version: organization.CurrentSuccessVersion, Username: owner,
				Media:    organization.MediaContext{Source: "tmdb", MediaID: before.MediaID, TitleYear: "Recovered (2026)", MediaType: "电视剧"},
				Episodes: []organization.Episode{{Season: 1, Episode: 101}}}
			tx := beginTVSubmissionIntegrationTx(t, store)
			if err := store.VerifySubscriptionTargetVersionTx(t.Context(), tx, owner, id, before.UpdatedAt); err != nil {
				t.Fatal(err)
			}
			if _, err := store.CreateDownloadIntentTx(t.Context(), tx, owner, intent); err != nil {
				t.Fatal(err)
			}
			if _, err := store.CreateDownloadIntentPlanTx(t.Context(), tx, owner, plan); err != nil {
				t.Fatal(err)
			}
			if _, err := store.CreateDownloadIntentSuccessInputTx(t.Context(), tx, owner, input); err != nil {
				t.Fatal(err)
			}
			if err := store.BindDownloadIntentSubscriptionTx(t.Context(), tx, owner, intent.ID, id); err != nil {
				t.Fatal(err)
			}
			if independent, err := store.VerifyTVBestVersionSubmissionTx(t.Context(), tx, owner, id, intent.ID, plan.TVRecoveredTarget.ExpectedCoverage); err != nil || !independent {
				t.Fatalf("independent=%v err=%v", independent, err)
			}
			if tc.state != subscription.PendingSearch {
				if err := store.CompareAndSwapSubscriptionRSSStateTx(t.Context(), tx, owner, id, tc.state); err != nil {
					t.Fatal(err)
				}
			} else if err := store.CompareAndSwapSubscriptionPreciseSearchStateTx(t.Context(), tx, owner, id, subscription.Acquired); err != nil {
				t.Fatal(err)
			}
			if err := store.VerifyTVBestVersionSubmissionReplayTx(t.Context(), tx, owner, id, intent.ID, plan.TVRecoveredTarget.ExpectedCoverage, tc.state); err != nil {
				t.Fatal(err)
			}
			if err := tx.Commit(t.Context()); err != nil {
				t.Fatal(err)
			}
			assertTVSubmissionIntegrationOldRows(t, store, id, old.ID, 3)
			storedPlan, err := store.GetDownloadIntentPlan(t.Context(), owner, intent.ID)
			if err != nil || !reflect.DeepEqual(storedPlan.TVRecoveredTarget, plan.TVRecoveredTarget) {
				t.Fatalf("roundtrip=%+v err=%v", storedPlan, err)
			}
			if _, err := store.CompareAndSwapDownloadIntent(t.Context(), owner, intent.ID, []downloader.IntentState{downloader.IntentPlanned}, downloader.IntentSubmitting, ""); err != nil {
				t.Fatal(err)
			}
			if _, _, err := store.AcceptDownloadIntent(t.Context(), "recovered-acceptance-other", intent.ID, databaseIntentV1Hash); !errors.Is(err, ErrDownloadIntentNotFound) {
				t.Fatalf("owner=%v", err)
			}
			accepted, released, err := store.AcceptDownloadIntent(t.Context(), owner, intent.ID, databaseIntentV1Hash)
			if err != nil || !released || accepted.State != downloader.IntentDownloading {
				t.Fatalf("accepted=%+v released=%v err=%v", accepted, released, err)
			}
			parent := readDownloadIntentAcceptanceParent(t, store, id)
			wantConfig := before.Config
			wantConfig["lack_episode"] = float64(tc.lack)
			if parent.state != tc.want || parent.completed != (tc.want == subscription.Completed) || parent.currentIntent != nil || !reflect.DeepEqual(readTVQualityConfig(t, store, id), wantConfig) {
				t.Fatalf("parent=%+v config=%v", parent, readTVQualityConfig(t, store, id))
			}
			assertTVSubmissionIntegrationOldRows(t, store, id, old.ID, 3)
			if _, released, err := store.AcceptDownloadIntent(t.Context(), owner, intent.ID, databaseIntentV1Hash); err != nil || !released {
				t.Fatalf("replay released=%v err=%v", released, err)
			}
			if !reflect.DeepEqual(parent, readDownloadIntentAcceptanceParent(t, store, id)) {
				t.Fatal("acceptance replay moved parent")
			}
			other := plan
			other.TVRecoveredTarget = recoveredTargetFixture(2)
			if _, err := store.CreateDownloadIntentPlan(t.Context(), owner, other); !errors.Is(err, ErrDownloadIntentPlanConflict) {
				t.Fatalf("immutable recovered replay=%v", err)
			}
			if tc.want == subscription.PendingSearch {
				if err := store.RecordRecoveredTVSubscriptionProgress(t.Context(), owner, id, tc.want, parent.updatedAt, tc.lack); err != nil {
					t.Fatal(err)
				}
				if current := readDownloadIntentAcceptanceParent(t, store, id); current.state != subscription.Active {
					t.Fatalf("outer final state=%+v", current)
				}
			}
		})
	}
}

func TestRecoveredTVPlanMigrationPostgresNullableAndRollbackGuard(t *testing.T) {
	store := newDownloadIntentPlanIntegrationStore(t)
	var schema string
	if err := store.db.Pool.QueryRow(t.Context(), `SELECT current_schema()`).Scan(&schema); err != nil {
		t.Fatal(err)
	}
	migrationDB, _ := openSubscriptionV2StateMigrationDB(t, schema)
	goose.SetBaseFS(migrationFS)
	if err := goose.DownToContext(t.Context(), migrationDB, "migrations", 39); err != nil {
		t.Fatal(err)
	}
	const owner = "recovered-migration"
	insertDownloadIntentIntegrationUsers(t, store, owner)
	site := insertDownloadIntentPlanIntegrationSite(t, store, "recovered-migration-site")
	legacy := createDownloadIntentAcceptanceIntent(t, store, owner, "recovered-migration-legacy")
	if _, err := store.db.Pool.Exec(t.Context(), `INSERT INTO download_intent_plans(intent_id,site_id,downloader_id,resolver_kind,resolver_version,resource_id) VALUES($1,$2,$3,'haidan',1,'legacy')`, legacy.ID, site, downloadIntentPlanTestDownloaderID); err != nil {
		t.Fatal(err)
	}
	if err := goose.UpToContext(t.Context(), migrationDB, "migrations", 40); err != nil {
		t.Fatal(err)
	}
	if plan, err := store.GetDownloadIntentPlan(t.Context(), owner, legacy.ID); err != nil || plan.TVRecoveredTarget != nil {
		t.Fatalf("legacy=%+v err=%v", plan, err)
	}
	intent := createDownloadIntentAcceptanceIntent(t, store, owner, "recovered-migration-known")
	plan := downloadIntentPlanIntegrationFixture(intent.ID, site, "recovered-known")
	plan.TVPriority = r08MoviePriority(90)
	plan.TVCoverage = []subscription.Episode{}
	plan.TVRecoveredTarget = recoveredTargetFixture(1)
	if _, err := store.CreateDownloadIntentPlan(t.Context(), owner, plan); err != nil {
		t.Fatal(err)
	}
	if err := goose.DownToContext(t.Context(), migrationDB, "migrations", 39); err == nil {
		t.Fatal("rollback discarded recovered target")
	}
	if stored, err := store.GetDownloadIntentPlan(t.Context(), owner, intent.ID); err != nil || !reflect.DeepEqual(stored.TVRecoveredTarget, plan.TVRecoveredTarget) {
		t.Fatalf("rollback guard=%+v err=%v", stored, err)
	}
	if _, err := store.db.Pool.Exec(t.Context(), `DELETE FROM download_intents WHERE id=$1`, intent.ID); err != nil {
		t.Fatal(err)
	}
	if err := goose.DownToContext(t.Context(), migrationDB, "migrations", 39); err != nil {
		t.Fatal(err)
	}
	if err := goose.UpToContext(t.Context(), migrationDB, "migrations", 40); err != nil {
		t.Fatal(err)
	}
}
