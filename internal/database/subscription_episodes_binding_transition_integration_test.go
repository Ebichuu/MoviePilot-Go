//go:build integration

package database

import (
	"context"
	"errors"
	"testing"

	"github.com/google/uuid"

	"github.com/moviepilot-go/moviepilot-go/internal/subscription"
)

func TestSubscriptionEpisodesPostgresBindingFailuresAreAtomic(t *testing.T) {
	store := newSiteStatisticsIntegrationStore(t)
	insertSubscriptionIntegrationUsers(t, store, "episode-binding-owner")

	t.Run("missing episode rejects the entire batch", func(t *testing.T) {
		subscriptionID := createSubscriptionIntegrationFixture(
			t, store, "episode-binding-owner", "episode-binding-missing",
		)
		episodes := []subscription.Episode{
			{Season: 1, Episode: 1},
			{Season: 1, Episode: 2},
		}
		if err := store.UpsertSubscriptionEpisodes(
			t.Context(), "episode-binding-owner", subscriptionID, episodes,
		); err != nil {
			t.Fatalf("initialize episodes: %v", err)
		}
		intentID := createBoundSubscriptionEpisodeIntent(
			t, store, "episode-binding-owner", subscriptionID, "episode-binding-missing-intent",
		)

		err := store.BindSubscriptionEpisodes(
			t.Context(), "episode-binding-owner", subscriptionID, intentID,
			[]subscription.Episode{episodes[0], {Season: 1, Episode: 99}},
		)
		if !errors.Is(err, ErrSubscriptionEpisodeNotFound) {
			t.Fatalf("binding error = %v, want ErrSubscriptionEpisodeNotFound", err)
		}
		assertSubscriptionEpisodeIntentIDs(
			t, store, "episode-binding-owner", subscriptionID, map[int]uuid.UUID{},
		)
		for _, episode := range episodes {
			assertSubscriptionEpisodeState(
				t, store, "episode-binding-owner", subscriptionID, episode, SubscriptionEpisodeMissing,
			)
		}
	})

	t.Run("intent belonging to another subscription changes nothing", func(t *testing.T) {
		targetSubscriptionID := createSubscriptionIntegrationFixture(
			t, store, "episode-binding-owner", "episode-binding-target",
		)
		otherSubscriptionID := createSubscriptionIntegrationFixture(
			t, store, "episode-binding-owner", "episode-binding-other",
		)
		episodes := []subscription.Episode{
			{Season: 2, Episode: 1},
			{Season: 2, Episode: 2},
		}
		if err := store.UpsertSubscriptionEpisodes(
			t.Context(), "episode-binding-owner", targetSubscriptionID, episodes,
		); err != nil {
			t.Fatalf("initialize target episodes: %v", err)
		}
		foreignIntentID := createBoundSubscriptionEpisodeIntent(
			t, store, "episode-binding-owner", otherSubscriptionID, "episode-binding-foreign-intent",
		)

		err := store.BindSubscriptionEpisodes(
			t.Context(), "episode-binding-owner", targetSubscriptionID, foreignIntentID, episodes,
		)
		if !errors.Is(err, ErrSubscriptionEpisodeNotFound) {
			t.Fatalf("binding error = %v, want ErrSubscriptionEpisodeNotFound", err)
		}
		assertSubscriptionEpisodeIntentIDs(
			t, store, "episode-binding-owner", targetSubscriptionID, map[int]uuid.UUID{},
		)
		for _, episode := range episodes {
			assertSubscriptionEpisodeState(
				t, store, "episode-binding-owner", targetSubscriptionID, episode, SubscriptionEpisodeMissing,
			)
		}
	})

	t.Run("historical intent changes nothing", func(t *testing.T) {
		subscriptionID := createSubscriptionIntegrationFixture(
			t, store, "episode-binding-owner", "episode-binding-historical",
		)
		episodes := []subscription.Episode{
			{Season: 2, Episode: 3},
			{Season: 2, Episode: 4},
		}
		if err := store.UpsertSubscriptionEpisodes(
			t.Context(), "episode-binding-owner", subscriptionID, episodes,
		); err != nil {
			t.Fatalf("initialize historical episodes: %v", err)
		}
		currentIntentID := createBoundSubscriptionEpisodeIntent(
			t, store, "episode-binding-owner", subscriptionID, "episode-binding-current-intent",
		)
		historicalIntentID := createHistoricalSubscriptionEpisodeIntent(
			t, store, "episode-binding-owner", subscriptionID, "episode-binding-historical-intent",
		)

		err := store.BindSubscriptionEpisodes(
			t.Context(), "episode-binding-owner", subscriptionID, historicalIntentID, episodes,
		)
		if !errors.Is(err, ErrSubscriptionEpisodeNotFound) {
			t.Fatalf("historical binding error = %v, want ErrSubscriptionEpisodeNotFound", err)
		}
		assertSubscriptionEpisodeIntentIDs(
			t, store, "episode-binding-owner", subscriptionID, map[int]uuid.UUID{},
		)
		for _, episode := range episodes {
			assertSubscriptionEpisodeState(
				t, store, "episode-binding-owner", subscriptionID, episode, SubscriptionEpisodeMissing,
			)
		}
		assertSubscriptionCurrentIntent(
			t, store, "episode-binding-owner", subscriptionID, currentIntentID,
		)
	})

	t.Run("one conflicting episode rejects the entire batch", func(t *testing.T) {
		subscriptionID := createSubscriptionIntegrationFixture(
			t, store, "episode-binding-owner", "episode-binding-conflict",
		)
		episodes := []subscription.Episode{
			{Season: 3, Episode: 1},
			{Season: 3, Episode: 2},
			{Season: 3, Episode: 3},
		}
		if err := store.UpsertSubscriptionEpisodes(
			t.Context(), "episode-binding-owner", subscriptionID, episodes,
		); err != nil {
			t.Fatalf("initialize conflict episodes: %v", err)
		}
		firstIntentID := createBoundSubscriptionEpisodeIntent(
			t, store, "episode-binding-owner", subscriptionID, "episode-binding-first-intent",
		)
		if err := store.BindSubscriptionEpisodes(
			t.Context(), "episode-binding-owner", subscriptionID, firstIntentID, episodes[:1],
		); err != nil {
			t.Fatalf("bind first intent: %v", err)
		}
		secondIntentID := createHistoricalSubscriptionEpisodeIntent(
			t, store, "episode-binding-owner", subscriptionID, "episode-binding-second-intent",
		)
		setSubscriptionCurrentIntent(
			t, store, "episode-binding-owner", subscriptionID, secondIntentID,
		)

		err := store.BindSubscriptionEpisodes(
			t.Context(), "episode-binding-owner", subscriptionID, secondIntentID, episodes,
		)
		if !errors.Is(err, ErrSubscriptionEpisodeBindingConflict) {
			t.Fatalf("binding error = %v, want ErrSubscriptionEpisodeBindingConflict", err)
		}
		assertSubscriptionEpisodeIntentIDs(
			t, store, "episode-binding-owner", subscriptionID,
			map[int]uuid.UUID{episodes[0].Episode: firstIntentID},
		)
		assertSubscriptionEpisodeState(
			t, store, "episode-binding-owner", subscriptionID, episodes[0], SubscriptionEpisodeAcquired,
		)
		for _, episode := range episodes[1:] {
			assertSubscriptionEpisodeState(
				t, store, "episode-binding-owner", subscriptionID, episode, SubscriptionEpisodeMissing,
			)
		}
	})
}

func TestSubscriptionEpisodesPostgresReplayRequiresExactBindingSet(t *testing.T) {
	store := newSiteStatisticsIntegrationStore(t)
	insertSubscriptionIntegrationUsers(t, store, "episode-replay-owner")
	subscriptionID := createSubscriptionIntegrationFixture(
		t, store, "episode-replay-owner", "episode-replay-exact-set",
	)
	episodes := []subscription.Episode{
		{Season: 4, Episode: 1},
		{Season: 4, Episode: 2},
		{Season: 4, Episode: 3},
	}
	if err := store.UpsertSubscriptionEpisodes(
		t.Context(), "episode-replay-owner", subscriptionID, episodes,
	); err != nil {
		t.Fatalf("initialize replay episodes: %v", err)
	}
	intentID := createBoundSubscriptionEpisodeIntent(
		t, store, "episode-replay-owner", subscriptionID, "episode-replay-intent",
	)
	if err := store.BindSubscriptionEpisodes(
		t.Context(), "episode-replay-owner", subscriptionID, intentID, episodes,
	); err != nil {
		t.Fatalf("bind replay episodes: %v", err)
	}
	if err := store.CompareAndSwapSubscriptionPreciseSearchState(
		t.Context(), "episode-replay-owner", subscriptionID, subscription.Acquired,
	); err != nil {
		t.Fatalf("advance subscription to acquired: %v", err)
	}

	cases := []struct {
		name     string
		episodes []subscription.Episode
	}{
		{
			name:     "one episode missing from replay",
			episodes: episodes[:2],
		},
		{
			name: "one extra episode in replay",
			episodes: []subscription.Episode{
				episodes[0], episodes[1], episodes[2], {Season: 4, Episode: 4},
			},
		},
		{
			name: "one episode replaced in replay",
			episodes: []subscription.Episode{
				episodes[0], episodes[1], {Season: 4, Episode: 4},
			},
		},
	}
	for _, tt := range cases {
		t.Run(tt.name, func(t *testing.T) {
			tx, err := store.db.Pool.Begin(t.Context())
			if err != nil {
				t.Fatalf("begin replay verification transaction: %v", err)
			}
			defer func() { _ = tx.Rollback(context.Background()) }()

			err = store.VerifySubscriptionEpisodesBoundToIntentTx(
				t.Context(), tx, "episode-replay-owner", subscriptionID, intentID, tt.episodes,
			)
			if !errors.Is(err, ErrSubscriptionEpisodeBindingConflict) {
				t.Fatalf("verification error = %v, want ErrSubscriptionEpisodeBindingConflict", err)
			}
		})
	}

	updated, err := store.TransitionSubscriptionEpisodesByIntent(
		t.Context(), "episode-replay-owner", intentID,
		[]SubscriptionEpisodeState{SubscriptionEpisodeAcquired}, SubscriptionEpisodeDownloading,
	)
	if err != nil || updated != int64(len(episodes)) {
		t.Fatalf("advance bound episodes = (%d, %v), want (%d, nil)", updated, err, len(episodes))
	}
	tx, err := store.db.Pool.Begin(t.Context())
	if err != nil {
		t.Fatalf("begin progressed replay transaction: %v", err)
	}
	defer func() { _ = tx.Rollback(context.Background()) }()
	if err := store.VerifySubscriptionEpisodesBoundToIntentTx(
		t.Context(), tx, "episode-replay-owner", subscriptionID, intentID, episodes,
	); err != nil {
		t.Fatalf("verify progressed exact replay: %v", err)
	}
	if err := tx.Rollback(t.Context()); err != nil {
		t.Fatalf("close progressed replay transaction: %v", err)
	}

	newCurrentIntentID := createHistoricalSubscriptionEpisodeIntent(
		t, store, "episode-replay-owner", subscriptionID, "episode-replay-new-current-intent",
	)
	setSubscriptionCurrentIntent(
		t, store, "episode-replay-owner", subscriptionID, newCurrentIntentID,
	)
	historicalTx, err := store.db.Pool.Begin(t.Context())
	if err != nil {
		t.Fatalf("begin historical replay transaction: %v", err)
	}
	defer func() { _ = historicalTx.Rollback(context.Background()) }()
	if err := store.VerifySubscriptionEpisodesBoundToIntentTx(
		t.Context(), historicalTx, "episode-replay-owner", subscriptionID, intentID, episodes,
	); !errors.Is(err, ErrSubscriptionEpisodeNotFound) {
		t.Fatalf("historical replay verification error = %v, want ErrSubscriptionEpisodeNotFound", err)
	}
}

func TestSubscriptionEpisodesPostgresIntentTransitionClassifiesNoMatchAndMissingTargets(t *testing.T) {
	store := newSiteStatisticsIntegrationStore(t)
	insertSubscriptionIntegrationUsers(t, store, "episode-transition-owner", "episode-transition-other")
	subscriptionID := createSubscriptionIntegrationFixture(
		t, store, "episode-transition-owner", "episode-transition-active",
	)
	episodes := []subscription.Episode{
		{Season: 5, Episode: 1},
		{Season: 5, Episode: 2},
	}
	if err := store.UpsertSubscriptionEpisodes(
		t.Context(), "episode-transition-owner", subscriptionID, episodes,
	); err != nil {
		t.Fatalf("initialize transition episodes: %v", err)
	}
	intentID := createBoundSubscriptionEpisodeIntent(
		t, store, "episode-transition-owner", subscriptionID, "episode-transition-intent",
	)
	if err := store.BindSubscriptionEpisodes(
		t.Context(), "episode-transition-owner", subscriptionID, intentID, episodes,
	); err != nil {
		t.Fatalf("bind transition episodes: %v", err)
	}

	updated, err := store.TransitionSubscriptionEpisodesByIntent(
		t.Context(), "episode-transition-owner", intentID,
		[]SubscriptionEpisodeState{SubscriptionEpisodeDownloading},
		SubscriptionEpisodePendingOrganization,
	)
	if err != nil || updated != 0 {
		t.Fatalf("valid no-match transition = (%d, %v), want (0, nil)", updated, err)
	}

	cases := []struct {
		name     string
		username string
		intentID uuid.UUID
	}{
		{
			name:     "wrong owner",
			username: "episode-transition-other",
			intentID: intentID,
		},
		{
			name:     "unknown intent",
			username: "episode-transition-owner",
			intentID: uuid.MustParse("aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee"),
		},
	}
	for _, tt := range cases {
		t.Run(tt.name, func(t *testing.T) {
			updated, err := store.TransitionSubscriptionEpisodesByIntent(
				t.Context(), tt.username, tt.intentID,
				[]SubscriptionEpisodeState{SubscriptionEpisodeAcquired},
				SubscriptionEpisodeDownloading,
			)
			if updated != 0 || !errors.Is(err, ErrSubscriptionEpisodeNotFound) {
				t.Fatalf("transition = (%d, %v), want (0, ErrSubscriptionEpisodeNotFound)", updated, err)
			}
		})
	}

	historicalIntentID := createHistoricalSubscriptionEpisodeIntent(
		t, store, "episode-transition-owner", subscriptionID, "episode-transition-historical-intent",
	)
	updated, err = store.TransitionSubscriptionEpisodesByIntent(
		t.Context(), "episode-transition-owner", historicalIntentID,
		[]SubscriptionEpisodeState{SubscriptionEpisodeAcquired},
		SubscriptionEpisodeDownloading,
	)
	if updated != 0 || !errors.Is(err, ErrSubscriptionEpisodeNotFound) {
		t.Fatalf("historical intent transition = (%d, %v), want (0, ErrSubscriptionEpisodeNotFound)", updated, err)
	}
	for _, episode := range episodes {
		assertSubscriptionEpisodeState(
			t, store, "episode-transition-owner", subscriptionID, episode, SubscriptionEpisodeAcquired,
		)
	}
	assertSubscriptionCurrentIntent(
		t, store, "episode-transition-owner", subscriptionID, intentID,
	)

	completedSubscriptionID := createSubscriptionIntegrationFixture(
		t, store, "episode-transition-owner", "episode-transition-completed",
	)
	completedEpisode := subscription.Episode{Season: 6, Episode: 1}
	if err := store.UpsertSubscriptionEpisodes(
		t.Context(), "episode-transition-owner", completedSubscriptionID,
		[]subscription.Episode{completedEpisode},
	); err != nil {
		t.Fatalf("initialize completed subscription episode: %v", err)
	}
	completedIntentID := createBoundSubscriptionEpisodeIntent(
		t, store, "episode-transition-owner", completedSubscriptionID, "episode-transition-completed-intent",
	)
	if err := store.BindSubscriptionEpisodes(
		t.Context(), "episode-transition-owner", completedSubscriptionID, completedIntentID,
		[]subscription.Episode{completedEpisode},
	); err != nil {
		t.Fatalf("bind completed subscription episode: %v", err)
	}
	if _, err := store.db.Pool.Exec(
		t.Context(), `UPDATE subscriptions
			SET internal_state=$1, v2_state='', completed_at=now(), updated_at=now()
			WHERE id=$2`, subscription.Completed, completedSubscriptionID,
	); err != nil {
		t.Fatalf("complete subscription fixture: %v", err)
	}

	updated, err = store.TransitionSubscriptionEpisodesByIntent(
		t.Context(), "episode-transition-owner", completedIntentID,
		[]SubscriptionEpisodeState{SubscriptionEpisodeAcquired},
		SubscriptionEpisodeDownloading,
	)
	if updated != 0 || !errors.Is(err, ErrSubscriptionEpisodeNotFound) {
		t.Fatalf("completed subscription transition = (%d, %v), want (0, ErrSubscriptionEpisodeNotFound)", updated, err)
	}
}

func TestSubscriptionEpisodesPostgresIntentTransitionRejectsIncompatibleBoundStateAtomically(t *testing.T) {
	store := newSiteStatisticsIntegrationStore(t)
	const username = "episode-transition-state-set-owner"
	insertSubscriptionIntegrationUsers(t, store, username)
	subscriptionID := createSubscriptionIntegrationFixture(
		t, store, username, "episode-transition-state-set",
	)
	episodes := []subscription.Episode{
		{Season: 8, Episode: 1},
		{Season: 8, Episode: 2},
	}
	if err := store.UpsertSubscriptionEpisodes(t.Context(), username, subscriptionID, episodes); err != nil {
		t.Fatalf("initialize state-set episodes: %v", err)
	}
	intentID := createBoundSubscriptionEpisodeIntent(
		t, store, username, subscriptionID, "episode-transition-state-set-intent",
	)
	if err := store.BindSubscriptionEpisodes(
		t.Context(), username, subscriptionID, intentID, episodes,
	); err != nil {
		t.Fatalf("bind state-set episodes: %v", err)
	}
	if _, err := store.db.Pool.Exec(t.Context(), `
		UPDATE subscription_episodes
		SET state='missing', updated_at=now()
		WHERE subscription_id=$1 AND season=$2 AND episode=$3`,
		subscriptionID, episodes[1].Season, episodes[1].Episode,
	); err != nil {
		t.Fatalf("prepare incompatible bound state: %v", err)
	}

	updated, err := store.TransitionSubscriptionEpisodesByIntent(
		t.Context(), username, intentID,
		[]SubscriptionEpisodeState{SubscriptionEpisodeAcquired},
		SubscriptionEpisodeDownloading,
	)
	if updated != 0 || !errors.Is(err, ErrSubscriptionEpisodeStateConflict) {
		t.Fatalf("incompatible transition = (%d, %v), want (0, ErrSubscriptionEpisodeStateConflict)", updated, err)
	}
	assertSubscriptionEpisodeState(t, store, username, subscriptionID, episodes[0], SubscriptionEpisodeAcquired)
	assertSubscriptionEpisodeState(t, store, username, subscriptionID, episodes[1], SubscriptionEpisodeMissing)
}

func TestSubscriptionEpisodesPostgresIntentTransitionAcceptsLaterBoundStates(t *testing.T) {
	store := newSiteStatisticsIntegrationStore(t)
	const username = "episode-transition-later-state-owner"
	insertSubscriptionIntegrationUsers(t, store, username)
	subscriptionID := createSubscriptionIntegrationFixture(
		t, store, username, "episode-transition-later-state",
	)
	episodes := []subscription.Episode{
		{Season: 9, Episode: 1},
		{Season: 9, Episode: 2},
	}
	if err := store.UpsertSubscriptionEpisodes(t.Context(), username, subscriptionID, episodes); err != nil {
		t.Fatalf("initialize later-state episodes: %v", err)
	}
	intentID := createBoundSubscriptionEpisodeIntent(
		t, store, username, subscriptionID, "episode-transition-later-state-intent",
	)
	if err := store.BindSubscriptionEpisodes(
		t.Context(), username, subscriptionID, intentID, episodes,
	); err != nil {
		t.Fatalf("bind later-state episodes: %v", err)
	}
	if _, err := store.db.Pool.Exec(t.Context(), `
		UPDATE subscription_episodes
		SET state='pending_organization', updated_at=now()
		WHERE subscription_id=$1 AND season=$2 AND episode=$3`,
		subscriptionID, episodes[1].Season, episodes[1].Episode,
	); err != nil {
		t.Fatalf("prepare later bound state: %v", err)
	}

	updated, err := store.TransitionSubscriptionEpisodesByIntent(
		t.Context(), username, intentID,
		[]SubscriptionEpisodeState{SubscriptionEpisodeAcquired},
		SubscriptionEpisodeDownloading,
	)
	if err != nil || updated != 1 {
		t.Fatalf("later-state transition = (%d, %v), want (1, nil)", updated, err)
	}
	assertSubscriptionEpisodeState(t, store, username, subscriptionID, episodes[0], SubscriptionEpisodeDownloading)
	assertSubscriptionEpisodeState(t, store, username, subscriptionID, episodes[1], SubscriptionEpisodePendingOrganization)
}

func TestSubscriptionEpisodesPostgresSummaryIncludesPendingIndex(t *testing.T) {
	store := newSiteStatisticsIntegrationStore(t)
	insertSubscriptionIntegrationUsers(t, store, "episode-summary-owner")
	subscriptionID := createSubscriptionIntegrationFixture(
		t, store, "episode-summary-owner", "episode-summary-pending-index",
	)
	episodes := []subscription.Episode{
		{Season: 7, Episode: 1},
		{Season: 7, Episode: 2},
	}
	if err := store.UpsertSubscriptionEpisodes(
		t.Context(), "episode-summary-owner", subscriptionID, episodes,
	); err != nil {
		t.Fatalf("initialize summary episodes: %v", err)
	}
	if err := store.CompareAndSwapSubscriptionEpisodeState(
		t.Context(), "episode-summary-owner", subscriptionID, episodes[0],
		SubscriptionEpisodeMissing, SubscriptionEpisodeAcquired,
	); err != nil {
		t.Fatalf("advance summary episode to acquired: %v", err)
	}
	if _, err := store.db.Pool.Exec(
		t.Context(), `UPDATE subscription_episodes SET state='pending_index', updated_at=now()
			WHERE subscription_id=$1 AND season=$2 AND episode=$3`,
		subscriptionID, episodes[0].Season, episodes[0].Episode,
	); err != nil {
		t.Fatalf("advance summary episode to pending index: %v", err)
	}

	summary, err := store.SummarizeSubscriptionEpisodes(
		t.Context(), "episode-summary-owner", subscriptionID,
	)
	if err != nil {
		t.Fatalf("summarize episodes: %v", err)
	}
	if summary.Total != 2 || summary.Missing != 1 || summary.PendingIndex != 1 ||
		summary.Acquired != 0 || summary.Downloading != 0 ||
		summary.PendingOrganization != 0 || summary.Organizing != 0 || summary.Indexed != 0 {
		t.Fatalf("summary = %#v, want one missing and one pending index", summary)
	}
}

func assertSubscriptionCurrentIntent(
	t *testing.T,
	store *Store,
	username string,
	subscriptionID int64,
	want uuid.UUID,
) {
	t.Helper()
	var current *uuid.UUID
	if err := store.db.Pool.QueryRow(t.Context(), `
		SELECT current_download_intent_id
		FROM subscriptions
		WHERE id=$1 AND username=$2`, subscriptionID, username,
	).Scan(&current); err != nil {
		t.Fatalf("read current download intent: %v", err)
	}
	if current == nil || *current != want {
		t.Fatalf("current download intent = %v, want %s", current, want)
	}
}

func setSubscriptionCurrentIntent(
	t *testing.T,
	store *Store,
	username string,
	subscriptionID int64,
	intentID uuid.UUID,
) {
	t.Helper()
	result, err := store.db.Pool.Exec(t.Context(), `
		UPDATE subscriptions
		SET current_download_intent_id=$1, updated_at=now()
		WHERE id=$2 AND username=$3`, intentID, subscriptionID, username)
	if err != nil {
		t.Fatalf("set current download intent: %v", err)
	}
	if result.RowsAffected() != 1 {
		t.Fatalf("set current download intent rows = %d, want 1", result.RowsAffected())
	}
}
