//go:build integration

package database

import (
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/pressly/goose/v3"

	"github.com/moviepilot-go/moviepilot-go/internal/subscription"
)

func TestResetActiveSubscriptionPostgresPreservesCurrentIntentAndBoundEpisode(t *testing.T) {
	store := newSiteStatisticsIntegrationStore(t)
	const username = "reset-public-mutation-owner"
	insertSubscriptionIntegrationUsers(t, store, username)
	id := createSubscriptionIntegrationFixture(t, store, username, "reset-public-mutation")
	episodes := []subscription.Episode{{Season: 1, Episode: 1}, {Season: 1, Episode: 2}, {Season: 1, Episode: 3}}
	if err := store.UpsertSubscriptionEpisodes(t.Context(), username, id, episodes); err != nil {
		t.Fatal(err)
	}
	intentID := createBoundSubscriptionEpisodeIntent(t, store, username, id, "reset-current-intent")
	if err := store.BindSubscriptionEpisodes(t.Context(), username, id, intentID, episodes[2:]); err != nil {
		t.Fatal(err)
	}
	if _, err := store.db.Pool.Exec(t.Context(), `
		UPDATE subscriptions
		SET current_download_intent_id=$1, internal_state='acquired', v2_state='R',
			config=config || '{"total_episode":3,"start_episode":1,"lack_episode":1,"note":[1,2],"manual_total_episode":1,"current_priority":60,"episode_priority":{"1":100}}'::jsonb,
			updated_at=clock_timestamp()
		WHERE id=$2`, intentID, id); err != nil {
		t.Fatal(err)
	}
	observed, err := store.GetAccessibleActiveSubscription(t.Context(), username, id, false)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.ResetActiveSubscription(t.Context(), observed); err != nil {
		t.Fatal(err)
	}
	var state subscription.State
	var v2State string
	var current *uuid.UUID
	var raw []byte
	if err := store.db.Pool.QueryRow(t.Context(), `SELECT internal_state,v2_state,current_download_intent_id,config FROM subscriptions WHERE id=$1`, id).Scan(&state, &v2State, &current, &raw); err != nil {
		t.Fatal(err)
	}
	var config map[string]any
	if err := json.Unmarshal(raw, &config); err != nil {
		t.Fatal(err)
	}
	if state != subscription.Acquired || v2State != "R" || current == nil || *current != intentID ||
		config["lack_episode"] != float64(3) || config["manual_total_episode"] != float64(0) || config["current_priority"] != nil {
		t.Fatalf("reset parent=(%s,%s,%v) config=%#v", state, v2State, current, config)
	}
	var boundState subscription.SubscriptionEpisodeState
	if err := store.db.Pool.QueryRow(t.Context(), `SELECT state FROM subscription_episodes WHERE subscription_id=$1 AND episode=3 AND download_intent_id=$2`, id, intentID).Scan(&boundState); err != nil {
		t.Fatal(err)
	}
	if boundState != subscription.SubscriptionEpisodeAcquired {
		t.Fatalf("bound episode state=%s", boundState)
	}
}

func TestApplySubscriptionPublicMutationPostgresReplacesColumnsAndTVTargetAtomically(t *testing.T) {
	store := newSiteStatisticsIntegrationStore(t)
	const username = "public-update-owner"
	insertSubscriptionIntegrationUsers(t, store, username)
	id := createSubscriptionIntegrationFixture(t, store, username, "public-update")
	if _, err := store.db.Pool.Exec(t.Context(), `UPDATE subscriptions SET config=config || '{"keyword":"old","total_episode":2,"start_episode":1,"lack_episode":1,"manual_total_episode":0,"note":[1]}'::jsonb WHERE id=$1`, id); err != nil {
		t.Fatal(err)
	}
	observed, err := store.GetAccessibleActiveSubscription(t.Context(), username, id, false)
	if err != nil {
		t.Fatal(err)
	}
	name, mediaType, year, source, mediaID := "更新剧", "电视剧", "2026", "themoviedb", "99"
	season := 2
	target, err := ResolveSubscriptionTVPublicTarget(observed, season, 2, 4)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.ApplySubscriptionPublicMutation(t.Context(), SubscriptionPublicMutation{
		Username: username, SubscriptionID: id, ExpectedUpdatedAt: observed.UpdatedAt,
		Name: &name, MediaType: &mediaType, Year: &year, Season: &season,
		MediaSource: &source, MediaID: &mediaID, ReplacePublic: true, Target: &target,
		Config: map[string]any{"keyword": nil, "total_episode": 4, "start_episode": 2, "sites": []int{}},
	}); err != nil {
		t.Fatal(err)
	}
	updated, err := store.GetAccessibleActiveSubscription(t.Context(), username, id, false)
	if err != nil {
		t.Fatal(err)
	}
	if updated.Name != name || updated.MediaType != mediaType || updated.Year != year || updated.MediaSource != source || updated.MediaID != mediaID ||
		updated.Season == nil || *updated.Season != season || updated.Config["keyword"] != nil || updated.Config["total_episode"] != float64(4) ||
		updated.Config["lack_episode"] != float64(3) || updated.Config["manual_total_episode"] != true {
		t.Fatalf("updated subscription=%#v", updated)
	}
	var coordinates []int
	rows, err := store.db.Pool.Query(t.Context(), `SELECT episode FROM subscription_episodes WHERE subscription_id=$1 AND season=2 ORDER BY episode`, id)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	for rows.Next() {
		var episode int
		if err := rows.Scan(&episode); err != nil {
			t.Fatal(err)
		}
		coordinates = append(coordinates, episode)
	}
	if len(coordinates) != 3 || coordinates[0] != 2 || coordinates[2] != 4 {
		t.Fatalf("target coordinates=%v", coordinates)
	}
	if _, err := store.ApplySubscriptionPublicMutation(t.Context(), SubscriptionPublicMutation{
		Username: username, SubscriptionID: id, ExpectedUpdatedAt: observed.UpdatedAt,
		Name: &name, MediaType: &mediaType, Config: map[string]any{}, ReplacePublic: true,
	}); !errors.Is(err, ErrSubscriptionPublicMutationConflict) {
		t.Fatalf("stale mutation error=%v", err)
	}
}

func TestAcceptedIntentSurvivesConcurrentPublicTypeNull(t *testing.T) {
	store := newSiteStatisticsIntegrationStore(t)
	const username = "public-null-current-intent-owner"
	insertSubscriptionIntegrationUsers(t, store, username)
	id := createSubscriptionIntegrationFixture(t, store, username, "public-null-current-intent")
	episode := []subscription.Episode{{Season: 1, Episode: 1}}
	if err := store.UpsertSubscriptionEpisodes(t.Context(), username, id, episode); err != nil {
		t.Fatal(err)
	}
	intentID := createBoundSubscriptionEpisodeIntent(t, store, username, id, "public-null-current-intent")
	if err := store.BindSubscriptionEpisodes(t.Context(), username, id, intentID, episode); err != nil {
		t.Fatal(err)
	}
	// pgx 的 prepared statement 不允许一次多条语句，拆成两次 Exec。
	if _, err := store.db.Pool.Exec(t.Context(),
		`UPDATE subscriptions SET internal_state='acquired',v2_state='R',updated_at=clock_timestamp() WHERE id=$1`, id); err != nil {
		t.Fatal(err)
	}
	if _, err := store.db.Pool.Exec(t.Context(),
		`UPDATE download_intents SET state='submitting',updated_at=clock_timestamp() WHERE id=$1`, intentID); err != nil {
		t.Fatal(err)
	}
	observed, err := store.GetAccessibleActiveSubscription(t.Context(), username, id, false)
	if err != nil {
		t.Fatal(err)
	}
	name := observed.Name
	if _, err := store.ApplySubscriptionPublicMutation(t.Context(), SubscriptionPublicMutation{
		Username: username, SubscriptionID: id, ExpectedUpdatedAt: observed.UpdatedAt,
		Name: &name, MediaType: nil, Config: map[string]any{}, ReplacePublic: true,
	}); err != nil {
		t.Fatal(err)
	}
	accepted, _, err := store.AcceptDownloadIntent(t.Context(), username, intentID, "0123456789ABCDEF0123456789ABCDEF01234567")
	if err != nil || string(accepted.State) != "downloading" {
		t.Fatalf("accept after type NULL=%#v err=%v", accepted, err)
	}
	var mediaType *string
	var current *uuid.UUID
	var episodeState subscription.SubscriptionEpisodeState
	if err := store.db.Pool.QueryRow(t.Context(), `
		SELECT s.media_type,s.current_download_intent_id,se.state
		FROM subscriptions s JOIN subscription_episodes se ON se.subscription_id=s.id
		WHERE s.id=$1 AND se.download_intent_id=$2`, id, intentID).Scan(&mediaType, &current, &episodeState); err != nil {
		t.Fatal(err)
	}
	if mediaType != nil || current == nil || *current != intentID || episodeState != subscription.SubscriptionEpisodeAcquired {
		t.Fatalf("parent after acceptance mediaType=%v current=%v episode=%s", mediaType, current, episodeState)
	}
}

func TestNullableSubscriptionMediaTypeMigrationPostgres(t *testing.T) {
	store := newSiteStatisticsIntegrationStore(t)
	var schema string
	if err := store.db.Pool.QueryRow(t.Context(), `SELECT current_schema()`).Scan(&schema); err != nil {
		t.Fatal(err)
	}
	migrationDB, _ := openSubscriptionV2StateMigrationDB(t, schema)
	goose.SetBaseFS(migrationFS)
	ctx := t.Context()
	if err := goose.DownToContext(ctx, migrationDB, "migrations", 35); err != nil {
		t.Fatalf("down migration 036: %v", err)
	}
	if err := goose.UpToContext(ctx, migrationDB, "migrations", 36); err != nil {
		t.Fatalf("up migration 036: %v", err)
	}
	const username = "nullable-media-type-owner"
	insertSubscriptionIntegrationUsers(t, store, username)
	var id int64
	if err := store.db.Pool.QueryRow(ctx, `INSERT INTO subscriptions(username,media_type,title,internal_state,v2_state,config) VALUES($1,NULL,'Nullable type','pending_search','N','{}') RETURNING id`, username).Scan(&id); err != nil {
		t.Fatal(err)
	}
	item, err := store.GetAccessibleActiveSubscription(ctx, username, id, false)
	if err != nil || !item.MediaTypeNull || item.V2()["type"] != nil {
		t.Fatalf("nullable item=%#v err=%v", item, err)
	}
	if items, err := store.ListSubscriptionsForPreciseSearch(ctx, time.Now().Add(time.Hour), time.Time{}, 0, 10); err != nil || len(items) != 0 {
		t.Fatalf("NULL type precise targets=%#v err=%v", items, err)
	}
	if items, err := store.ListSubscriptionsForRSS(ctx, time.Now().Add(time.Hour), time.Time{}, 0, 10); err != nil || len(items) != 0 {
		t.Fatalf("NULL type RSS targets=%#v err=%v", items, err)
	}
	if err := goose.DownToContext(ctx, migrationDB, "migrations", 35); err == nil {
		t.Fatal("down migration accepted an existing NULL media type")
	}
	if _, err := store.db.Pool.Exec(ctx, `DELETE FROM subscriptions WHERE id=$1`, id); err != nil {
		t.Fatal(err)
	}
	if err := goose.DownToContext(ctx, migrationDB, "migrations", 35); err != nil {
		t.Fatalf("down migration after repair: %v", err)
	}
	if err := goose.UpToContext(ctx, migrationDB, "migrations", 36); err != nil {
		t.Fatalf("restore migration 036: %v", err)
	}
}
