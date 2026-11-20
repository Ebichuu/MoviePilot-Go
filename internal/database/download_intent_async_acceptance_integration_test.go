//go:build integration

package database

import (
	"errors"
	"fmt"
	"reflect"
	"strconv"
	"testing"

	"github.com/moviepilot-go/moviepilot-go/internal/downloader"
	"github.com/moviepilot-go/moviepilot-go/internal/subscription"
)

func TestAsyncDownloadAcceptancePostgresCurrentTVStateAndFacts(t *testing.T) {
	store := newDownloadIntentPlanIntegrationStore(t)
	const owner = "async-tv-acceptance"
	insertDownloadIntentIntegrationUsers(t, store, owner, "async-tv-other")
	site := insertDownloadIntentPlanIntegrationSite(t, store, "async-tv-site")
	episodes := recoveredTargetFixture(1).MissingEpisodes
	for index, tc := range []struct {
		name      string
		batch     bool
		recovered bool
		best      bool
		complete  bool
		pending   bool
		want      subscription.State
	}{
		{name: "ordinary batch partial N", batch: true, want: subscription.PendingSearch},
		{name: "ordinary async partial R", want: subscription.Active},
		{name: "ordinary async complete", complete: true, want: subscription.Completed},
		{name: "ordinary P partial", pending: true, want: subscription.PendingIdentification},
		{name: "ordinary P full", pending: true, complete: true, want: subscription.PendingIdentification},
		{name: "recovered batch partial N", batch: true, recovered: true, want: subscription.PendingSearch},
		{name: "recovered async partial R", recovered: true, want: subscription.Active},
		{name: "recovered async complete", recovered: true, complete: true, want: subscription.Completed},
		{name: "recovered P partial", recovered: true, pending: true, want: subscription.PendingIdentification},
		{name: "recovered P full", recovered: true, pending: true, complete: true, want: subscription.PendingIdentification},
		{name: "best version retains existing R", best: true, want: subscription.Active},
	} {
		t.Run(tc.name, func(t *testing.T) {
			key := fmt.Sprintf("async-tv-%d", index)
			coverage := episodes[:1]
			lack := 2
			if tc.complete {
				coverage, lack = episodes, 0
			}
			var bound []subscription.Episode
			if !tc.recovered && !tc.best {
				bound = coverage
			}
			fixture := prepareDownloadIntentAcceptanceFixture(t, store, owner, key, "电视剧", episodes, bound, "")
			config := map[string]any{"total_episode": 3, "start_episode": 1, "lack_episode": 9,
				"best_version": tc.best, "note": []int{}, "current_priority": 70}
			plan := downloadIntentPlanIntegrationFixture(fixture.intent.ID, site, key)
			plan.TVPriority, plan.TVCoverage = r08MoviePriority(90), coverage
			if tc.recovered {
				config["total_episode"] = 0
				config["note"] = []int{9}
				config["episode_priority"] = map[string]int{"9": 70}
				plan.TVCoverage = []subscription.Episode{{Season: 1, Episode: 101}}
				plan.TVRecoveredTarget = recoveredTargetFixture(1)
				if tc.complete {
					plan.TVRecoveredTarget = recoveredTargetFixture(1, 2, 3)
				}
				old := createDownloadIntentAcceptanceIntent(t, store, owner, key+"-history")
				if _, err := store.db.Pool.Exec(t.Context(), `UPDATE subscription_episodes SET state='indexed',download_intent_id=$2 WHERE subscription_id=$1`, fixture.subscriptionID, old.ID); err != nil {
					t.Fatal(err)
				}
			}
			setTVQualityIntegrationConfig(t, store, fixture.subscriptionID, config)
			if _, err := store.db.Pool.Exec(t.Context(), `UPDATE subscriptions SET season=1 WHERE id=$1`, fixture.subscriptionID); err != nil {
				t.Fatal(err)
			}
			if tc.pending {
				if _, err := store.db.Pool.Exec(t.Context(), `UPDATE subscriptions SET internal_state='pending_identification',v2_state='P' WHERE id=$1`, fixture.subscriptionID); err != nil {
					t.Fatal(err)
				}
			}
			if _, err := store.CreateDownloadIntentPlan(t.Context(), owner, plan); err != nil {
				t.Fatal(err)
			}
			beforeConfig := readTVQualityConfig(t, store, fixture.subscriptionID)
			beforeEpisodes := readDownloadIntentAcceptanceEpisodeStates(t, store, fixture.subscriptionID)
			if _, _, err := store.AcceptDownloadIntentAndFinishSearch(t.Context(), "async-tv-other", fixture.intent.ID, databaseIntentV1Hash); !errors.Is(err, ErrDownloadIntentNotFound) {
				t.Fatalf("foreign owner=%v", err)
			}
			accept := store.AcceptDownloadIntentAndFinishSearch
			if tc.batch {
				accept = store.AcceptDownloadIntent
			}
			accepted, released, err := accept(t.Context(), owner, fixture.intent.ID, databaseIntentV1Hash)
			if err != nil || !released || accepted.State != downloader.IntentDownloading || accepted.TorrentHash != databaseIntentV1Hash {
				t.Fatalf("accepted=%+v released=%v err=%v", accepted, released, err)
			}
			parent := readDownloadIntentAcceptanceParent(t, store, fixture.subscriptionID)
			projected, _ := subscription.V2State(tc.want)
			if parent.state != tc.want || parent.v2State != projected || parent.currentIntent != nil || parent.completed != (tc.want == subscription.Completed) {
				t.Fatalf("parent=%+v", parent)
			}
			wantConfig := beforeConfig
			wantConfig["lack_episode"] = float64(lack)
			if !tc.recovered {
				var note []any
				priorities := make(map[string]any)
				for _, episode := range coverage {
					note = append(note, float64(episode.Episode))
					priorities[strconv.Itoa(episode.Episode)] = float64(90)
				}
				wantConfig["note"], wantConfig["episode_priority"] = note, priorities
				if tc.best {
					// 旧整体优先级回填未覆盖的目标集，接受事实只提升 E01。
					wantConfig["lack_episode"] = float64(0)
					wantConfig["current_priority"] = float64(70)
					priorities["2"], priorities["3"] = float64(70), float64(70)
				}
			}
			if after := readTVQualityConfig(t, store, fixture.subscriptionID); !reflect.DeepEqual(after, wantConfig) {
				t.Fatalf("config=%v want=%v", after, wantConfig)
			}
			afterEpisodes := readDownloadIntentAcceptanceEpisodeStates(t, store, fixture.subscriptionID)
			if tc.recovered || tc.best {
				if !reflect.DeepEqual(afterEpisodes, beforeEpisodes) {
					t.Fatal("independent facts changed historical episode rows")
				}
			} else {
				for _, episode := range coverage {
					if afterEpisodes[episode] != subscription.SubscriptionEpisodeIndexed {
						t.Fatalf("accepted episode %v state=%s", episode, afterEpisodes[episode])
					}
				}
			}
			if _, replayed, err := store.AcceptDownloadIntentAndFinishSearch(t.Context(), owner, fixture.intent.ID, databaseIntentV1Hash); err != nil || !replayed {
				t.Fatalf("replayed=%v err=%v", replayed, err)
			}
			if !reflect.DeepEqual(parent, readDownloadIntentAcceptanceParent(t, store, fixture.subscriptionID)) ||
				!reflect.DeepEqual(wantConfig, readTVQualityConfig(t, store, fixture.subscriptionID)) {
				t.Fatal("already released replay changed parent or progress")
			}
		})
	}
}

func TestAsyncDownloadAcceptancePostgresReleasedResetAndProtectedParent(t *testing.T) {
	store := newDownloadIntentPlanIntegrationStore(t)
	const owner = "async-tv-protection"
	insertDownloadIntentIntegrationUsers(t, store, owner)
	site := insertDownloadIntentPlanIntegrationSite(t, store, "async-tv-protection-site")
	episodes := recoveredTargetFixture(1).MissingEpisodes
	for index, mode := range []string{"user changed state to N", "public reset", "lost batch finally N", "other current intent", "paused", "invalid config", "invalid plan facts"} {
		t.Run(mode, func(t *testing.T) {
			key := fmt.Sprintf("async-protection-%d", index)
			fixture := prepareDownloadIntentAcceptanceFixture(t, store, owner, key, "电视剧", episodes, episodes[:1], "")
			setTVQualityIntegrationConfig(t, store, fixture.subscriptionID, map[string]any{"total_episode": 3, "note": []int{}})
			plan := downloadIntentPlanIntegrationFixture(fixture.intent.ID, site, key)
			plan.TVPriority, plan.TVCoverage = r08MoviePriority(90), episodes[:1]
			if _, err := store.CreateDownloadIntentPlan(t.Context(), owner, plan); err != nil {
				t.Fatal(err)
			}
			var err error
			switch mode {
			case "user changed state to N":
				if _, _, err := store.AcceptDownloadIntentAndFinishSearch(t.Context(), owner, fixture.intent.ID, databaseIntentV1Hash); err != nil {
					t.Fatal(err)
				}
				_, err = store.db.Pool.Exec(t.Context(), `UPDATE subscriptions SET internal_state='pending_search',v2_state='N',updated_at=now() WHERE id=$1`, fixture.subscriptionID)
			case "public reset":
				beforeAcceptance := mustActiveSubscription(t, store, owner, fixture.subscriptionID)
				if _, released, err := store.AcceptDownloadIntentAndFinishSearch(t.Context(), owner, fixture.intent.ID, databaseIntentV1Hash); err != nil || !released {
					t.Fatalf("accept before reset: released=%v err=%v", released, err)
				}
				current := mustActiveSubscription(t, store, owner, fixture.subscriptionID)
				currentParent := readDownloadIntentAcceptanceParent(t, store, fixture.subscriptionID)
				if _, err := store.ResetActiveSubscription(t.Context(), beforeAcceptance); !errors.Is(err, ErrSubscriptionPublicMutationConflict) {
					t.Fatalf("stale reset version=%v", err)
				}
				if !reflect.DeepEqual(currentParent, readDownloadIntentAcceptanceParent(t, store, fixture.subscriptionID)) ||
					!reflect.DeepEqual(current.Config, readTVQualityConfig(t, store, fixture.subscriptionID)) {
					t.Fatal("stale reset changed accepted facts")
				}
				resetVersion, resetErr := store.ResetActiveSubscription(t.Context(), current)
				if resetErr != nil {
					t.Fatal(resetErr)
				}
				reset := mustActiveSubscription(t, store, owner, fixture.subscriptionID)
				resetParent := readDownloadIntentAcceptanceParent(t, store, fixture.subscriptionID)
				if reset.InternalState != subscription.Active || !reset.UpdatedAt.Equal(resetVersion) || !reset.UpdatedAt.After(current.UpdatedAt) ||
					resetParent.currentIntent != nil || reset.Config["lack_episode"] != float64(3) || reset.Config["current_priority"] != nil ||
					!reflect.DeepEqual(reset.Config["note"], []any{}) || !reflect.DeepEqual(reset.Config["episode_priority"], map[string]any{}) {
					t.Fatalf("public reset parent=%+v config=%v version=%s", resetParent, reset.Config, resetVersion)
				}
			case "lost batch finally N":
				_, _, err = store.AcceptDownloadIntent(t.Context(), owner, fixture.intent.ID, databaseIntentV1Hash)
			case "other current intent":
				other := createDownloadIntentAcceptanceIntent(t, store, owner, key+"-new")
				_, err = store.db.Pool.Exec(t.Context(), `UPDATE subscriptions SET current_download_intent_id=$2 WHERE id=$1`, fixture.subscriptionID, other.ID)
			case "paused":
				_, err = store.db.Pool.Exec(t.Context(), `UPDATE subscriptions SET internal_state='paused',v2_state='S' WHERE id=$1`, fixture.subscriptionID)
			case "invalid config":
				_, err = store.db.Pool.Exec(t.Context(), `UPDATE subscriptions SET config='{"total_episode":"oops"}'::jsonb WHERE id=$1`, fixture.subscriptionID)
			case "invalid plan facts":
				_, err = store.db.Pool.Exec(t.Context(), `UPDATE download_intent_plans SET tv_coverage='[{"season":1,"episode":0}]'::jsonb WHERE intent_id=$1`, fixture.intent.ID)
			}
			if err != nil {
				t.Fatal(err)
			}
			before := readDownloadIntentAcceptanceParent(t, store, fixture.subscriptionID)
			beforeConfig := readTVQualityConfig(t, store, fixture.subscriptionID)
			accepted, released, err := store.AcceptDownloadIntentAndFinishSearch(t.Context(), owner, fixture.intent.ID, databaseIntentV1Hash)
			wantReleased := mode == "user changed state to N" || mode == "public reset" || mode == "lost batch finally N"
			if err != nil || accepted.State != downloader.IntentDownloading || released != wantReleased {
				t.Fatalf("accepted=%+v released=%v err=%v", accepted, released, err)
			}
			if !reflect.DeepEqual(before, readDownloadIntentAcceptanceParent(t, store, fixture.subscriptionID)) ||
				!reflect.DeepEqual(beforeConfig, readTVQualityConfig(t, store, fixture.subscriptionID)) {
				t.Fatal("old or protected intent changed parent")
			}
		})
	}
}

func TestAsyncDownloadAcceptancePostgresParentWriteFailureRollsBackAllFacts(t *testing.T) {
	store := newDownloadIntentPlanIntegrationStore(t)
	const owner = "async-tv-rollback"
	insertDownloadIntentIntegrationUsers(t, store, owner)
	site := insertDownloadIntentPlanIntegrationSite(t, store, "async-tv-rollback-site")
	episodes := recoveredTargetFixture(1).MissingEpisodes
	fixture := prepareDownloadIntentAcceptanceFixture(t, store, owner, "async-tv-rollback", "电视剧", episodes, episodes[:1], "")
	setTVQualityIntegrationConfig(t, store, fixture.subscriptionID, map[string]any{"total_episode": 3, "note": []int{}})
	plan := downloadIntentPlanIntegrationFixture(fixture.intent.ID, site, "async-tv-rollback")
	plan.TVPriority, plan.TVCoverage = r08MoviePriority(90), episodes[:1]
	if _, err := store.CreateDownloadIntentPlan(t.Context(), owner, plan); err != nil {
		t.Fatal(err)
	}
	if _, err := store.db.Pool.Exec(t.Context(), `CREATE FUNCTION reject_async_acceptance_parent_write() RETURNS trigger LANGUAGE plpgsql AS $$
		BEGIN IF NEW.internal_state='active' THEN RAISE EXCEPTION 'fixture parent write failure'; END IF; RETURN NEW; END;
		$$`); err != nil {
		t.Fatal(err)
	}
	if _, err := store.db.Pool.Exec(t.Context(), `CREATE TRIGGER reject_async_acceptance_parent BEFORE UPDATE ON subscriptions FOR EACH ROW EXECUTE FUNCTION reject_async_acceptance_parent_write()`); err != nil {
		t.Fatal(err)
	}
	before := readDownloadIntentAcceptanceParent(t, store, fixture.subscriptionID)
	beforeConfig := readTVQualityConfig(t, store, fixture.subscriptionID)
	beforeEpisodes := readDownloadIntentAcceptanceEpisodeStates(t, store, fixture.subscriptionID)
	if _, _, err := store.AcceptDownloadIntentAndFinishSearch(t.Context(), owner, fixture.intent.ID, databaseIntentV1Hash); !errors.Is(err, ErrDownloadIntentPersistence) {
		t.Fatalf("write failure=%v", err)
	}
	intent, err := store.GetDownloadIntent(t.Context(), owner, fixture.intent.ID)
	if err != nil || intent.State != downloader.IntentSubmitting || intent.TorrentHash != "" ||
		!reflect.DeepEqual(before, readDownloadIntentAcceptanceParent(t, store, fixture.subscriptionID)) ||
		!reflect.DeepEqual(beforeConfig, readTVQualityConfig(t, store, fixture.subscriptionID)) ||
		!reflect.DeepEqual(beforeEpisodes, readDownloadIntentAcceptanceEpisodeStates(t, store, fixture.subscriptionID)) {
		t.Fatalf("failed transaction committed partial facts: intent=%+v err=%v", intent, err)
	}
}
