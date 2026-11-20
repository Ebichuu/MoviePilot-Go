//go:build integration

package database

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/moviepilot-go/moviepilot-go/internal/downloader"
	"github.com/moviepilot-go/moviepilot-go/internal/subscription"
)

func TestRecoverSubscriptionDownloadIntentPostgresReleasesBoundState(t *testing.T) {
	store := newSiteStatisticsIntegrationStore(t)
	insertSubscriptionIntegrationUsers(
		t,
		store,
		"recovery-planned-owner",
		"recovery-submitting-owner",
		"recovery-failed-owner",
	)

	tests := []struct {
		name         string
		username     string
		intentState  downloader.IntentState
		episodeState SubscriptionEpisodeState
		parentState  subscription.State
	}{
		{
			name:         "planned releases acquired episode",
			username:     "recovery-planned-owner",
			intentState:  downloader.IntentPlanned,
			episodeState: SubscriptionEpisodeAcquired,
			parentState:  subscription.Acquired,
		},
		{
			name:         "submitting releases acquired episode",
			username:     "recovery-submitting-owner",
			intentState:  downloader.IntentSubmitting,
			episodeState: SubscriptionEpisodeAcquired,
			parentState:  subscription.Acquired,
		},
		{
			name:         "failed releases downloading episode",
			username:     "recovery-failed-owner",
			intentState:  downloader.IntentFailed,
			episodeState: SubscriptionEpisodeDownloading,
			parentState:  subscription.Downloading,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			fixture := prepareSubscriptionDownloadRecoveryFixture(
				t,
				store,
				tt.username,
				"recovery-"+string(tt.intentState),
				tt.intentState,
				tt.episodeState,
				tt.parentState,
			)

			err := store.RecoverSubscriptionDownloadIntent(
				t.Context(),
				tt.username,
				fixture.intentID,
				[]downloader.IntentState{tt.intentState},
				&fixture.intentUpdatedAt,
			)
			if err != nil {
				t.Fatalf("RecoverSubscriptionDownloadIntent() error = %v", err)
			}

			assertSubscriptionDownloadRecoveryState(
				t,
				store,
				fixture,
				downloader.IntentFailed,
				SubscriptionEpisodeMissing,
				uuid.Nil,
				subscription.PendingSearch,
				"N",
			)
		})
	}
}

func TestRecoverSubscriptionDownloadIntentPostgresPreservesPendingIdentification(t *testing.T) {
	store := newSiteStatisticsIntegrationStore(t)
	const username = "recovery-pending-identification-owner"
	insertSubscriptionIntegrationUsers(t, store, username)
	fixture := prepareSubscriptionDownloadRecoveryFixture(
		t,
		store,
		username,
		"recovery-pending-identification",
		downloader.IntentPlanned,
		SubscriptionEpisodeAcquired,
		subscription.Acquired,
	)
	if _, err := store.db.Pool.Exec(t.Context(), `
		UPDATE subscriptions
		SET internal_state=$1, v2_state='P', updated_at=now()
		WHERE id=$2`, subscription.PendingIdentification, fixture.subscriptionID); err != nil {
		t.Fatalf("prepare pending-identification recovery parent: %v", err)
	}

	if err := store.RecoverSubscriptionDownloadIntent(
		t.Context(), username, fixture.intentID,
		[]downloader.IntentState{downloader.IntentPlanned}, &fixture.intentUpdatedAt,
	); err != nil {
		t.Fatalf("recover pending-identification intent: %v", err)
	}
	assertSubscriptionDownloadRecoveryState(
		t,
		store,
		fixture,
		downloader.IntentFailed,
		SubscriptionEpisodeMissing,
		uuid.Nil,
		subscription.PendingIdentification,
		"P",
	)

	var failedUpdatedAt time.Time
	if err := store.db.Pool.QueryRow(t.Context(), `
		SELECT updated_at FROM download_intents WHERE id=$1`, fixture.intentID,
	).Scan(&failedUpdatedAt); err != nil {
		t.Fatalf("read pending-identification failed intent lease: %v", err)
	}
	if err := store.RecoverSubscriptionDownloadIntent(
		t.Context(), username, fixture.intentID,
		[]downloader.IntentState{downloader.IntentFailed}, &failedUpdatedAt,
	); err != nil {
		t.Fatalf("replay pending-identification recovery: %v", err)
	}
	assertSubscriptionDownloadRecoveryState(
		t,
		store,
		fixture,
		downloader.IntentFailed,
		SubscriptionEpisodeMissing,
		uuid.Nil,
		subscription.PendingIdentification,
		"P",
	)
}

func TestRecoverSubscriptionDownloadIntentPostgresRetryExhaustionPreservesExternalSideEffectStates(t *testing.T) {
	store := newSiteStatisticsIntegrationStore(t)
	insertSubscriptionIntegrationUsers(
		t,
		store,
		"recovery-exhausted-submitting-owner",
		"recovery-exhausted-downloading-owner",
	)
	tests := []struct {
		name         string
		username     string
		intentState  downloader.IntentState
		episodeState SubscriptionEpisodeState
		parentState  subscription.State
	}{
		{
			name:         "submitting",
			username:     "recovery-exhausted-submitting-owner",
			intentState:  downloader.IntentSubmitting,
			episodeState: SubscriptionEpisodeAcquired,
			parentState:  subscription.Acquired,
		},
		{
			name:         "downloading",
			username:     "recovery-exhausted-downloading-owner",
			intentState:  downloader.IntentDownloading,
			episodeState: SubscriptionEpisodeDownloading,
			parentState:  subscription.Downloading,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			fixture := prepareSubscriptionDownloadRecoveryFixture(
				t,
				store,
				tt.username,
				"recovery-exhausted-"+tt.name,
				tt.intentState,
				tt.episodeState,
				tt.parentState,
			)

			err := store.RecoverSubscriptionDownloadIntent(
				t.Context(),
				tt.username,
				fixture.intentID,
				[]downloader.IntentState{downloader.IntentPlanned, downloader.IntentFailed},
				nil,
			)
			if !errors.Is(err, ErrDownloadIntentStateConflict) {
				t.Fatalf("retry exhaustion recovery error = %v, want ErrDownloadIntentStateConflict", err)
			}
			assertSubscriptionDownloadRecoveryState(
				t,
				store,
				fixture,
				tt.intentState,
				tt.episodeState,
				fixture.intentID,
				tt.parentState,
				"R",
			)
		})
	}
}

func TestRecoverSubscriptionDownloadIntentPostgresReleasesMovieWithoutEpisodes(t *testing.T) {
	store := newDownloadIntentPlanIntegrationStore(t)
	const username = "recovery-movie-owner"
	insertSubscriptionIntegrationUsers(t, store, username)
	subscriptionID := createSubscriptionIntegrationFixture(t, store, username, "recovery-movie")
	siteID := insertDownloadIntentPlanIntegrationSite(t, store, "recovery-movie-site")
	intent := mustDatabaseDownloadIntent(t, username, "recovery-movie-intent")
	stored, err := store.CreateDownloadIntent(t.Context(), username, intent)
	if err != nil {
		t.Fatalf("create movie intent: %v", err)
	}
	if _, err := store.CreateDownloadIntentPlan(
		t.Context(), username,
		downloadIntentPlanIntegrationFixture(stored.ID, siteID, "recovery-movie-resource"),
	); err != nil {
		t.Fatalf("create movie plan: %v", err)
	}
	tx, err := store.db.Pool.Begin(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback(context.Background()) }()
	if err := store.BindDownloadIntentSubscriptionTx(
		t.Context(), tx, username, stored.ID, subscriptionID,
	); err != nil {
		t.Fatalf("bind movie intent: %v", err)
	}
	if err := store.CompareAndSwapSubscriptionPreciseSearchStateTx(
		t.Context(), tx, username, subscriptionID, subscription.Acquired,
	); err != nil {
		t.Fatalf("acquire movie subscription: %v", err)
	}
	if err := tx.Commit(t.Context()); err != nil {
		t.Fatal(err)
	}
	if _, err := store.CompareAndSwapDownloadIntent(
		t.Context(), username, stored.ID,
		[]downloader.IntentState{downloader.IntentPlanned},
		downloader.IntentSubmitting, databaseIntentV1Hash,
	); err != nil {
		t.Fatalf("submit movie intent: %v", err)
	}
	_, err = store.CompareAndSwapDownloadIntent(
		t.Context(), username, stored.ID,
		[]downloader.IntentState{downloader.IntentSubmitting},
		downloader.IntentFailed, "",
	)
	if err != nil {
		t.Fatalf("fail movie intent: %v", err)
	}
	var failedUpdatedAt time.Time
	if err := store.db.Pool.QueryRow(t.Context(), `
		SELECT updated_at
		FROM download_intents
		WHERE id=$1 AND username=$2`, stored.ID, username,
	).Scan(&failedUpdatedAt); err != nil {
		t.Fatalf("read failed movie lease: %v", err)
	}

	targets, err := store.ListDownloadReconciliationTargets(
		t.Context(), failedUpdatedAt.Add(time.Second), time.Time{}, uuid.Nil, 10,
	)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, target := range targets {
		found = found || target.ID == stored.ID
	}
	if !found {
		t.Fatalf("failed movie intent missing from reconciliation targets: %#v", targets)
	}
	if err := store.RecoverSubscriptionDownloadIntent(
		t.Context(), username, stored.ID,
		[]downloader.IntentState{downloader.IntentFailed}, &failedUpdatedAt,
	); err != nil {
		t.Fatalf("recover movie intent: %v", err)
	}

	var (
		parentState  subscription.State
		v2State      string
		current      *uuid.UUID
		episodeCount int
	)
	if err := store.db.Pool.QueryRow(t.Context(), `
		SELECT internal_state, v2_state, current_download_intent_id,
			(SELECT count(*) FROM subscription_episodes WHERE subscription_id=subscriptions.id)
		FROM subscriptions
		WHERE id=$1 AND username=$2`, subscriptionID, username,
	).Scan(&parentState, &v2State, &current, &episodeCount); err != nil {
		t.Fatal(err)
	}
	if parentState != subscription.PendingSearch || v2State != "N" || current != nil || episodeCount != 0 {
		t.Fatalf(
			"recovered movie = state %q/%q current %v episodes %d",
			parentState, v2State, current, episodeCount,
		)
	}
}

func TestRecoverSubscriptionDownloadIntentPostgresLeaseConflictIsAtomic(t *testing.T) {
	store := newSiteStatisticsIntegrationStore(t)
	const username = "recovery-lease-owner"
	insertSubscriptionIntegrationUsers(t, store, username)
	fixture := prepareSubscriptionDownloadRecoveryFixture(
		t,
		store,
		username,
		"recovery-stale-lease",
		downloader.IntentSubmitting,
		SubscriptionEpisodeDownloading,
		subscription.Downloading,
	)

	currentUpdatedAt := fixture.intentUpdatedAt.Add(time.Second)
	if _, err := store.db.Pool.Exec(t.Context(), `
		UPDATE download_intents
		SET updated_at=$1
		WHERE id=$2 AND username=$3`, currentUpdatedAt, fixture.intentID, username); err != nil {
		t.Fatalf("advance intent lease: %v", err)
	}

	err := store.RecoverSubscriptionDownloadIntent(
		t.Context(),
		username,
		fixture.intentID,
		[]downloader.IntentState{downloader.IntentSubmitting},
		&fixture.intentUpdatedAt,
	)
	if !errors.Is(err, ErrDownloadIntentStateConflict) {
		t.Fatalf("stale lease error = %v, want ErrDownloadIntentStateConflict", err)
	}

	assertSubscriptionDownloadRecoveryState(
		t,
		store,
		fixture,
		downloader.IntentSubmitting,
		SubscriptionEpisodeDownloading,
		fixture.intentID,
		subscription.Downloading,
		"R",
	)
	var storedUpdatedAt time.Time
	if err := store.db.Pool.QueryRow(t.Context(), `
		SELECT updated_at FROM download_intents WHERE id=$1`, fixture.intentID).Scan(&storedUpdatedAt); err != nil {
		t.Fatalf("read intent lease after conflict: %v", err)
	}
	if !storedUpdatedAt.Equal(currentUpdatedAt) {
		t.Fatalf("intent lease changed after conflict: got %s, want %s", storedUpdatedAt, currentUpdatedAt)
	}
}

func TestRecoverSubscriptionDownloadIntentPostgresOwnerIsolation(t *testing.T) {
	store := newSiteStatisticsIntegrationStore(t)
	const (
		username = "recovery-owner"
		other    = "recovery-other"
	)
	insertSubscriptionIntegrationUsers(t, store, username, other)
	fixture := prepareSubscriptionDownloadRecoveryFixture(
		t,
		store,
		username,
		"recovery-owner-isolation",
		downloader.IntentSubmitting,
		SubscriptionEpisodeAcquired,
		subscription.Acquired,
	)

	err := store.RecoverSubscriptionDownloadIntent(
		t.Context(),
		other,
		fixture.intentID,
		[]downloader.IntentState{downloader.IntentSubmitting},
		nil,
	)
	if !errors.Is(err, ErrDownloadIntentNotFound) {
		t.Fatalf("other owner error = %v, want ErrDownloadIntentNotFound", err)
	}
	assertSubscriptionDownloadRecoveryState(
		t,
		store,
		fixture,
		downloader.IntentSubmitting,
		SubscriptionEpisodeAcquired,
		fixture.intentID,
		subscription.Acquired,
		"R",
	)
}

func TestRecoverSubscriptionDownloadIntentPostgresReplayAndConflict(t *testing.T) {
	store := newSiteStatisticsIntegrationStore(t)
	const username = "recovery-replay-owner"
	insertSubscriptionIntegrationUsers(t, store, username)
	fixture := prepareSubscriptionDownloadRecoveryFixture(
		t,
		store,
		username,
		"recovery-replay",
		downloader.IntentSubmitting,
		SubscriptionEpisodeAcquired,
		subscription.Acquired,
	)

	if err := store.RecoverSubscriptionDownloadIntent(
		t.Context(), username, fixture.intentID,
		[]downloader.IntentState{downloader.IntentSubmitting}, nil,
	); err != nil {
		t.Fatalf("initial recovery: %v", err)
	}
	if err := store.RecoverSubscriptionDownloadIntent(
		t.Context(), username, fixture.intentID,
		[]downloader.IntentState{downloader.IntentFailed}, nil,
	); err != nil {
		t.Fatalf("idempotent failed replay: %v", err)
	}
	if err := store.RecoverSubscriptionDownloadIntent(
		t.Context(), username, fixture.intentID,
		[]downloader.IntentState{downloader.IntentSubmitting}, nil,
	); !errors.Is(err, ErrDownloadIntentStateConflict) {
		t.Fatalf("stale state replay error = %v, want ErrDownloadIntentStateConflict", err)
	}

	assertSubscriptionDownloadRecoveryState(
		t,
		store,
		fixture,
		downloader.IntentFailed,
		SubscriptionEpisodeMissing,
		uuid.Nil,
		subscription.PendingSearch,
		"N",
	)
}

func TestRecoverSubscriptionDownloadIntentPostgresDoesNotRollbackNewIntent(t *testing.T) {
	store := newSiteStatisticsIntegrationStore(t)
	const username = "recovery-new-intent-owner"
	insertSubscriptionIntegrationUsers(t, store, username)
	fixture := prepareSubscriptionDownloadRecoveryFixture(
		t, store, username, "recovery-old-intent",
		downloader.IntentFailed, SubscriptionEpisodeAcquired, subscription.Acquired,
	)
	var siteID int64
	if err := store.db.Pool.QueryRow(t.Context(), `
		INSERT INTO sites (name, url, adapter)
		VALUES ('recovery-old-intent-site', 'https://recovery.invalid', 'fixture')
		RETURNING id`).Scan(&siteID); err != nil {
		t.Fatalf("create old intent site: %v", err)
	}
	if _, err := store.db.Pool.Exec(t.Context(), `
		INSERT INTO download_intent_plans (
			intent_id, site_id, downloader_id, resolver_kind, resolver_version,
			resource_id, resource_secret, save_path, paused
		) VALUES ($1, $2, $3, 'fixture', 1, 'recovery-old-intent', '', '', false)`,
		fixture.intentID, siteID, "2cf24282-78e7-4b32-9f07-761efc29280c",
	); err != nil {
		t.Fatalf("create old intent plan: %v", err)
	}
	if err := store.RecoverSubscriptionDownloadIntent(
		t.Context(), username, fixture.intentID,
		[]downloader.IntentState{downloader.IntentFailed}, nil,
	); err != nil {
		t.Fatalf("recover old intent: %v", err)
	}

	newIntentID := createBoundSubscriptionEpisodeIntent(
		t, store, username, fixture.subscriptionID, "recovery-new-intent",
	)
	tx, err := store.db.Pool.Begin(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback(context.Background()) }()
	if err := store.CompareAndSwapSubscriptionPreciseSearchStateTx(
		t.Context(), tx, username, fixture.subscriptionID, subscription.Acquired,
	); err != nil {
		t.Fatalf("acquire subscription for new intent: %v", err)
	}
	if err := store.BindSubscriptionEpisodesTx(
		t.Context(), tx, username, fixture.subscriptionID, newIntentID,
		[]subscription.Episode{fixture.episode},
	); err != nil {
		t.Fatalf("bind new intent: %v", err)
	}
	if err := tx.Commit(t.Context()); err != nil {
		t.Fatal(err)
	}

	if err := store.RecoverSubscriptionDownloadIntent(
		t.Context(), username, fixture.intentID,
		[]downloader.IntentState{downloader.IntentFailed}, nil,
	); !errors.Is(err, ErrDownloadIntentStateConflict) {
		t.Fatalf("replay old intent recovery error = %v, want state conflict", err)
	}
	assertSubscriptionDownloadRecoveryState(
		t, store, fixture,
		downloader.IntentFailed, SubscriptionEpisodeAcquired, newIntentID,
		subscription.Acquired, "R",
	)
	targets, err := store.ListDownloadReconciliationTargets(
		t.Context(), time.Now().UTC().Add(time.Second), time.Time{}, uuid.Nil, 10,
	)
	if err != nil {
		t.Fatal(err)
	}
	for _, target := range targets {
		if target.ID == fixture.intentID {
			t.Fatalf("old recovered intent was selected again: %#v", target)
		}
	}
}

func TestRecoverSubscriptionDownloadIntentPostgresPreservesPausedAndCompletedSubscriptions(t *testing.T) {
	store := newSiteStatisticsIntegrationStore(t)
	insertSubscriptionIntegrationUsers(t, store, "recovery-paused-owner", "recovery-completed-owner")
	tests := []struct {
		name      string
		username  string
		state     subscription.State
		v2State   string
		completed bool
	}{
		{name: "paused", username: "recovery-paused-owner", state: subscription.Paused, v2State: "S"},
		{name: "completed", username: "recovery-completed-owner", state: subscription.Completed, completed: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			fixture := prepareSubscriptionDownloadRecoveryFixture(
				t, store, tt.username, "recovery-"+tt.name,
				downloader.IntentSubmitting, SubscriptionEpisodeAcquired, subscription.Acquired,
			)
			if _, err := store.db.Pool.Exec(t.Context(), `
				UPDATE subscriptions
				SET internal_state=$1, v2_state=$2,
					completed_at=CASE WHEN $3 THEN now() ELSE NULL END
				WHERE id=$4 AND username=$5`,
				tt.state, tt.v2State, tt.completed, fixture.subscriptionID, tt.username,
			); err != nil {
				t.Fatalf("prepare subscription boundary: %v", err)
			}
			err := store.RecoverSubscriptionDownloadIntent(
				t.Context(), tt.username, fixture.intentID,
				[]downloader.IntentState{downloader.IntentSubmitting}, nil,
			)
			if !errors.Is(err, ErrDownloadIntentStateConflict) {
				t.Fatalf("boundary recovery error = %v, want state conflict", err)
			}
			assertSubscriptionDownloadRecoveryState(
				t, store, fixture,
				downloader.IntentSubmitting, SubscriptionEpisodeAcquired, fixture.intentID,
				tt.state, tt.v2State,
			)
		})
	}
}

func TestRecoverSubscriptionDownloadIntentPostgresHandlesUnboundIntent(t *testing.T) {
	store := newSiteStatisticsIntegrationStore(t)
	const username = "recovery-unbound-owner"
	insertSubscriptionIntegrationUsers(t, store, username)
	intent := mustDatabaseDownloadIntent(t, username, "recovery-unbound")
	stored, err := store.CreateDownloadIntent(t.Context(), username, intent)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.CompareAndSwapDownloadIntent(
		t.Context(), username, stored.ID,
		[]downloader.IntentState{downloader.IntentPlanned}, downloader.IntentSubmitting, "",
	); err != nil {
		t.Fatal(err)
	}
	if err := store.RecoverSubscriptionDownloadIntent(
		t.Context(), username, stored.ID,
		[]downloader.IntentState{downloader.IntentSubmitting}, nil,
	); err != nil {
		t.Fatal(err)
	}
	got, err := store.GetDownloadIntent(t.Context(), username, stored.ID)
	if err != nil || got.State != downloader.IntentFailed {
		t.Fatalf("unbound intent = %#v, %v", got, err)
	}
}

func TestRecoverSubscriptionDownloadIntentPostgresRacesStateAdvanceAtomically(t *testing.T) {
	store := newSiteStatisticsIntegrationStore(t)
	const username = "recovery-race-owner"
	insertSubscriptionIntegrationUsers(t, store, username)
	fixture := prepareSubscriptionDownloadRecoveryFixture(
		t, store, username, "recovery-race",
		downloader.IntentSubmitting, SubscriptionEpisodeAcquired, subscription.Acquired,
	)

	start := make(chan struct{})
	errorsByOperation := make(chan error, 2)
	var workers sync.WaitGroup
	workers.Add(2)
	go func() {
		defer workers.Done()
		<-start
		_, err := store.CompareAndSwapDownloadIntent(
			t.Context(), username, fixture.intentID,
			[]downloader.IntentState{downloader.IntentSubmitting},
			downloader.IntentDownloading, databaseIntentV1Hash,
		)
		errorsByOperation <- err
	}()
	go func() {
		defer workers.Done()
		<-start
		errorsByOperation <- store.RecoverSubscriptionDownloadIntent(
			t.Context(), username, fixture.intentID,
			[]downloader.IntentState{downloader.IntentSubmitting}, &fixture.intentUpdatedAt,
		)
	}()
	close(start)
	workers.Wait()
	close(errorsByOperation)

	succeeded := 0
	conflicted := 0
	for err := range errorsByOperation {
		switch {
		case err == nil:
			succeeded++
		case errors.Is(err, ErrDownloadIntentStateConflict):
			conflicted++
		default:
			t.Fatalf("concurrent operation error = %v", err)
		}
	}
	if succeeded != 1 || conflicted != 1 {
		t.Fatalf("concurrent outcomes = success %d conflict %d", succeeded, conflicted)
	}

	intent, err := store.GetDownloadIntent(t.Context(), username, fixture.intentID)
	if err != nil {
		t.Fatal(err)
	}
	switch intent.State {
	case downloader.IntentFailed:
		assertSubscriptionDownloadRecoveryState(
			t, store, fixture,
			downloader.IntentFailed, SubscriptionEpisodeMissing, uuid.Nil,
			subscription.PendingSearch, "N",
		)
	case downloader.IntentDownloading:
		assertSubscriptionDownloadRecoveryState(
			t, store, fixture,
			downloader.IntentDownloading, SubscriptionEpisodeAcquired, fixture.intentID,
			subscription.Acquired, "R",
		)
	default:
		t.Fatalf("unexpected final intent state %q", intent.State)
	}
}

type subscriptionDownloadRecoveryFixture struct {
	username        string
	subscriptionID  int64
	episode         subscription.Episode
	intentID        uuid.UUID
	intentUpdatedAt time.Time
}

func prepareSubscriptionDownloadRecoveryFixture(
	t *testing.T,
	store *Store,
	username string,
	fixtureKey string,
	intentState downloader.IntentState,
	episodeState SubscriptionEpisodeState,
	parentState subscription.State,
) subscriptionDownloadRecoveryFixture {
	t.Helper()
	subscriptionID := createSubscriptionIntegrationFixture(t, store, username, fixtureKey)
	episode := subscription.Episode{Season: 1, Episode: 1}
	if err := store.UpsertSubscriptionEpisodes(
		t.Context(), username, subscriptionID, []subscription.Episode{episode},
	); err != nil {
		t.Fatalf("initialize subscription episode: %v", err)
	}
	intentID := createBoundSubscriptionEpisodeIntent(t, store, username, subscriptionID, fixtureKey+"-intent")

	tx, err := store.db.Pool.Begin(t.Context())
	if err != nil {
		t.Fatalf("begin episode binding transaction: %v", err)
	}
	defer func() { _ = tx.Rollback(context.Background()) }()
	if err := store.BindSubscriptionEpisodesTx(
		t.Context(), tx, username, subscriptionID, intentID, []subscription.Episode{episode},
	); err != nil {
		t.Fatalf("bind episode to intent: %v", err)
	}
	if err := store.CompareAndSwapSubscriptionPreciseSearchStateTx(
		t.Context(), tx, username, subscriptionID, subscription.Acquired,
	); err != nil {
		t.Fatalf("advance subscription to acquired: %v", err)
	}
	if err := tx.Commit(t.Context()); err != nil {
		t.Fatalf("commit episode binding transaction: %v", err)
	}

	if episodeState == SubscriptionEpisodeDownloading {
		if _, err := store.TransitionSubscriptionEpisodesByIntent(
			t.Context(), username, intentID,
			[]SubscriptionEpisodeState{SubscriptionEpisodeAcquired},
			SubscriptionEpisodeDownloading,
		); err != nil {
			t.Fatalf("advance episode to downloading: %v", err)
		}
	}
	if parentState == subscription.Downloading {
		if err := store.CompareAndSwapSubscriptionStateByIntent(
			t.Context(), username, intentID, subscription.Acquired, subscription.Downloading,
		); err != nil {
			t.Fatalf("advance subscription to downloading: %v", err)
		}
	}

	switch intentState {
	case downloader.IntentPlanned:
	case downloader.IntentSubmitting, downloader.IntentFailed, downloader.IntentDownloading:
		if _, err := store.CompareAndSwapDownloadIntent(
			t.Context(), username, intentID,
			[]downloader.IntentState{downloader.IntentPlanned},
			downloader.IntentSubmitting,
			"",
		); err != nil {
			t.Fatalf("advance intent to submitting: %v", err)
		}
		if intentState == downloader.IntentFailed {
			if _, err := store.CompareAndSwapDownloadIntent(
				t.Context(), username, intentID,
				[]downloader.IntentState{downloader.IntentSubmitting},
				downloader.IntentFailed,
				"",
			); err != nil {
				t.Fatalf("advance intent to failed: %v", err)
			}
		}
		if intentState == downloader.IntentDownloading {
			if _, err := store.CompareAndSwapDownloadIntent(
				t.Context(), username, intentID,
				[]downloader.IntentState{downloader.IntentSubmitting},
				downloader.IntentDownloading,
				databaseIntentV1Hash,
			); err != nil {
				t.Fatalf("advance intent to downloading: %v", err)
			}
		}
	default:
		t.Fatalf("unsupported recovery fixture intent state %q", intentState)
	}

	var intentUpdatedAt time.Time
	if err := store.db.Pool.QueryRow(t.Context(), `
		SELECT updated_at FROM download_intents WHERE id=$1 AND username=$2`, intentID, username,
	).Scan(&intentUpdatedAt); err != nil {
		t.Fatalf("read intent lease: %v", err)
	}
	return subscriptionDownloadRecoveryFixture{
		username: username, subscriptionID: subscriptionID, episode: episode,
		intentID: intentID, intentUpdatedAt: intentUpdatedAt,
	}
}

func assertSubscriptionDownloadRecoveryState(
	t *testing.T,
	store *Store,
	fixture subscriptionDownloadRecoveryFixture,
	wantIntent downloader.IntentState,
	wantEpisode SubscriptionEpisodeState,
	wantBinding uuid.UUID,
	wantParent subscription.State,
	wantV2 string,
) {
	t.Helper()
	var intentState downloader.IntentState
	if err := store.db.Pool.QueryRow(t.Context(), `
		SELECT state FROM download_intents WHERE id=$1 AND username=$2`, fixture.intentID, fixture.username,
	).Scan(&intentState); err != nil {
		t.Fatalf("read recovered intent: %v", err)
	}
	if intentState != wantIntent {
		t.Fatalf("intent state = %q, want %q", intentState, wantIntent)
	}

	var (
		episodeState SubscriptionEpisodeState
		binding      *uuid.UUID
	)
	if err := store.db.Pool.QueryRow(t.Context(), `
		SELECT state, download_intent_id
		FROM subscription_episodes
		WHERE subscription_id=$1 AND season=$2 AND episode=$3`,
		fixture.subscriptionID, fixture.episode.Season, fixture.episode.Episode,
	).Scan(&episodeState, &binding); err != nil {
		t.Fatalf("read recovered episode: %v", err)
	}
	if episodeState != wantEpisode {
		t.Fatalf("episode state = %q, want %q", episodeState, wantEpisode)
	}
	if wantBinding == uuid.Nil {
		if binding != nil {
			t.Fatalf("episode binding = %s, want NULL", *binding)
		}
	} else if binding == nil || *binding != wantBinding {
		t.Fatalf("episode binding = %v, want %s", binding, wantBinding)
	}

	var (
		parentState subscription.State
		v2State     string
		current     *uuid.UUID
	)
	if err := store.db.Pool.QueryRow(t.Context(), `
		SELECT internal_state, v2_state, current_download_intent_id
		FROM subscriptions WHERE id=$1 AND username=$2`,
		fixture.subscriptionID, fixture.username,
	).Scan(&parentState, &v2State, &current); err != nil {
		t.Fatalf("read recovered subscription: %v", err)
	}
	if parentState != wantParent || v2State != wantV2 {
		t.Fatalf("subscription state = (%q, %q), want (%q, %q)", parentState, v2State, wantParent, wantV2)
	}
	if wantBinding == uuid.Nil {
		if current != nil {
			t.Fatalf("subscription current intent = %s, want NULL", *current)
		}
	} else if current == nil || *current != wantBinding {
		t.Fatalf("subscription current intent = %v, want %s", current, wantBinding)
	}
}
