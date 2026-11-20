//go:build integration

package database

import (
	"context"
	"errors"
	"reflect"
	"testing"

	"github.com/google/uuid"

	"github.com/moviepilot-go/moviepilot-go/internal/scrapeflow"
	"github.com/moviepilot-go/moviepilot-go/internal/subscription"
)

func TestLoadScrapeSourcePostgres(t *testing.T) {
	store := newSiteStatisticsIntegrationStore(t)

	t.Run("movie identity", func(t *testing.T) {
		username := "scrape-source-movie-" + uuid.NewString()
		insertSubscriptionIntegrationUsers(t, store, username)
		subscriptionID, err := store.CreateSubscription(t.Context(), NewSubscription{
			Username: username, Name: "Movie", MediaType: "电影", Year: "2026",
			MediaSource: "tmdb", MediaID: "42", State: subscription.PendingSearch,
		})
		if err != nil {
			t.Fatal(err)
		}
		intentID := insertScrapeSourceIntent(t, store, username, subscriptionID)
		source, err := store.LoadScrapeSource(t.Context(), intentID)
		if err != nil {
			t.Fatal(err)
		}
		want := scrapeflow.Source{
			Username: username, SubscriptionID: subscriptionID, MediaType: "电影",
			MediaSource: "tmdb", MediaID: "42",
		}
		if !reflect.DeepEqual(source, want) {
			t.Fatalf("LoadScrapeSource() = %#v, want %#v", source, want)
		}
	})

	t.Run("TV episodes belong to current intent only", func(t *testing.T) {
		username := "scrape-source-tv-" + uuid.NewString()
		insertSubscriptionIntegrationUsers(t, store, username)
		season := 2
		subscriptionID, err := store.CreateSubscription(t.Context(), NewSubscription{
			Username: username, Name: "Show", MediaType: "电视剧", Year: "2026", Season: &season,
			MediaSource: "themoviedb", MediaID: "1399", State: subscription.PendingSearch,
		})
		if err != nil {
			t.Fatal(err)
		}
		intentID := insertScrapeSourceIntent(t, store, username, subscriptionID)
		otherIntentID := insertScrapeSourceIntent(t, store, username, subscriptionID)
		if _, err := store.db.Pool.Exec(t.Context(), `
			INSERT INTO subscription_episodes (
				subscription_id, season, episode, state, download_intent_id
			) VALUES
				($1,2,3,'pending_organization',$2),
				($1,2,1,'pending_organization',$2),
				($1,2,2,'pending_organization',$3),
				($1,2,4,'missing',NULL)`, subscriptionID, intentID, otherIntentID); err != nil {
			t.Fatal(err)
		}

		source, err := store.LoadScrapeSource(t.Context(), intentID)
		if err != nil {
			t.Fatal(err)
		}
		wantEpisodes := []scrapeflow.Episode{{Season: 2, Episode: 1}, {Season: 2, Episode: 3}}
		if source.Username != username || source.SubscriptionID != subscriptionID ||
			source.MediaType != "电视剧" || source.MediaSource != "themoviedb" ||
			source.MediaID != "1399" || source.Season != 2 ||
			!reflect.DeepEqual(source.Episodes, wantEpisodes) {
			t.Fatalf("LoadScrapeSource() = %#v, want episodes %#v", source, wantEpisodes)
		}
	})

	t.Run("standalone intent", func(t *testing.T) {
		username := "scrape-source-standalone-" + uuid.NewString()
		insertSubscriptionIntegrationUsers(t, store, username)
		intentID := insertScrapeSourceIntent(t, store, username, 0)
		source, err := store.LoadScrapeSource(t.Context(), intentID)
		if err != nil {
			t.Fatal(err)
		}
		if source.Username != username || source.SubscriptionID != 0 || source.MediaType != "" ||
			source.MediaSource != "" || source.MediaID != "" || source.Season != 0 || len(source.Episodes) != 0 {
			t.Fatalf("standalone source = %#v", source)
		}
	})
}

func TestLoadScrapeSourcePostgresSanitizesFailures(t *testing.T) {
	store := newSiteStatisticsIntegrationStore(t)
	if _, err := store.LoadScrapeSource(t.Context(), uuid.New()); !errors.Is(err, scrapeflow.ErrPersistence) {
		t.Fatalf("missing intent error = %v, want %v", err, scrapeflow.ErrPersistence)
	}
	cancelled, cancel := context.WithCancel(t.Context())
	cancel()
	if _, err := store.LoadScrapeSource(cancelled, uuid.New()); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled error = %v, want context.Canceled", err)
	}
}

func insertScrapeSourceIntent(t *testing.T, store *Store, username string, subscriptionID int64) uuid.UUID {
	t.Helper()
	intentID := uuid.New()
	var linkedSubscription any
	if subscriptionID > 0 {
		linkedSubscription = subscriptionID
	}
	if _, err := store.db.Pool.Exec(t.Context(), `
		INSERT INTO download_intents (
			id, username, subscription_id, candidate_key, label, state
		) VALUES ($1,$2,$3,$4,$5,'completed')`, intentID, username, linkedSubscription,
		"sha256"+uuid.NewString()+uuid.NewString(), "mp-"+uuid.NewString()); err != nil {
		t.Fatal(err)
	}
	return intentID
}
