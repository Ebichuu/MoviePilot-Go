package database

import (
	"context"
	"errors"
	"strings"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/moviepilot-go/moviepilot-go/internal/scrapeflow"
)

const maxScrapeSourceEpisodes = 10_000

// LoadScrapeSource reads only the persisted identity needed to derive an NFO
// plan. Database structs and query errors never cross the scrapeflow boundary.
func (s *Store) LoadScrapeSource(ctx context.Context, intentID uuid.UUID) (scrapeflow.Source, error) {
	if ctx == nil || intentID == uuid.Nil || s == nil || s.db == nil || s.db.Pool == nil {
		return scrapeflow.Source{}, scrapeflow.ErrPersistence
	}
	if err := ctx.Err(); err != nil {
		return scrapeflow.Source{}, err
	}

	var source scrapeflow.Source
	var subscriptionID *int64
	if err := s.db.Pool.QueryRow(ctx, `
		SELECT di.username, di.subscription_id,
			COALESCE(s.media_type, ''), COALESCE(s.media_source, ''),
			COALESCE(s.media_id, ''), COALESCE(s.season, 0)
		FROM download_intents AS di
		LEFT JOIN subscriptions AS s ON s.id=di.subscription_id
		WHERE di.id=$1`, intentID).Scan(
		&source.Username, &subscriptionID, &source.MediaType,
		&source.MediaSource, &source.MediaID, &source.Season,
	); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return scrapeflow.Source{}, scrapeflow.ErrPersistence
		}
		if ctxErr := ctx.Err(); ctxErr != nil {
			return scrapeflow.Source{}, ctxErr
		}
		return scrapeflow.Source{}, scrapeflow.ErrPersistence
	}
	if subscriptionID == nil || *subscriptionID < 1 {
		source.Username = strings.TrimSpace(source.Username)
		if source.Username == "" {
			return scrapeflow.Source{}, scrapeflow.ErrPersistence
		}
		return source, nil
	}
	source.SubscriptionID = *subscriptionID

	rows, err := s.db.Pool.Query(ctx, `
		SELECT season, episode
		FROM subscription_episodes
		WHERE subscription_id=$1 AND download_intent_id=$2
		ORDER BY season, episode
		LIMIT $3`, source.SubscriptionID, intentID, maxScrapeSourceEpisodes+1)
	if err != nil {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return scrapeflow.Source{}, ctxErr
		}
		return scrapeflow.Source{}, scrapeflow.ErrPersistence
	}
	defer rows.Close()
	for rows.Next() {
		var episode scrapeflow.Episode
		if err := rows.Scan(&episode.Season, &episode.Episode); err != nil {
			return scrapeflow.Source{}, scrapeflow.ErrPersistence
		}
		if episode.Season < 0 || episode.Season > 999 || episode.Episode < 1 || episode.Episode > 5000 {
			return scrapeflow.Source{}, scrapeflow.ErrPersistence
		}
		source.Episodes = append(source.Episodes, episode)
		if len(source.Episodes) > maxScrapeSourceEpisodes {
			return scrapeflow.Source{}, scrapeflow.ErrPersistence
		}
	}
	if err := rows.Err(); err != nil {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return scrapeflow.Source{}, ctxErr
		}
		return scrapeflow.Source{}, scrapeflow.ErrPersistence
	}
	source.Username = strings.TrimSpace(source.Username)
	if source.Username == "" {
		return scrapeflow.Source{}, scrapeflow.ErrPersistence
	}
	return source, nil
}

var _ scrapeflow.SourceStore = (*Store)(nil)
