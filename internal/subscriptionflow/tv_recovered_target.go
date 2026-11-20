package subscriptionflow

import (
	"context"
	"errors"
	"strconv"
	"strings"
	"time"

	"github.com/moviepilot-go/moviepilot-go/internal/metadata"
	"github.com/moviepilot-go/moviepilot-go/internal/subscription"
)

// History rows cannot stand in for note here: V2's writer leaves note untouched
// while the subscription total remains zero, so a later batch may download the
// same coordinates again unless note or the media library supplies them.
func (h *Handler) withRecoveredTVMissingEpisodes(
	ctx context.Context, target persistedSubscription,
	resolution subscription.TVTargetResolution, total int, query subscription.Query,
	libraryQuery ExistingEpisodesQuery,
) (subscription.Query, error) {
	snapshot := resolution.Snapshot
	snapshot.TotalEpisode = total
	runtime, err := subscription.DescribeTVTarget(snapshot)
	if err != nil {
		return subscription.Query{}, ErrSubscriptionConfiguration
	}
	// DownloadChain reads the library before SubscribeChain applies start.
	fullSnapshot := snapshot
	fullSnapshot.StartEpisode = 1
	full, _ := subscription.DescribeTVTarget(fullSnapshot)
	existing, err := h.excludeExistingEpisodesWithShape(ctx, libraryQuery, target.Season, target.Config, full.TargetEpisodes)
	if err != nil {
		return subscription.Query{}, err
	}
	query.MissingEpisodes = existing.Episodes
	if snapshot.StartEpisode > 1 {
		query.MissingEpisodes = make([]subscription.Episode, 0, len(existing.Episodes))
		for _, episode := range existing.Episodes {
			if episode.Episode >= snapshot.StartEpisode {
				query.MissingEpisodes = append(query.MissingEpisodes, episode)
			}
		}
	}
	existing.Episodes = query.MissingEpisodes
	query.FullSeasonMissing = ordinaryTVFullSeasonMissing(runtime, nil, existing)
	query.TVRecoveredTarget = &subscription.TVRecoveredTarget{
		TotalEpisode: total, MissingEpisodes: append([]subscription.Episode{}, query.MissingEpisodes...),
	}
	return query, nil
}

func recoveredTVLibraryQuery(target persistedSubscription, detail metadata.Media) ExistingEpisodesQuery {
	query := ExistingEpisodesQuery{Title: strings.TrimSpace(detail.Title), MediaSource: target.MediaSource, MediaID: target.MediaID}
	if detail.Year > 0 {
		query.Year = strconv.Itoa(detail.Year)
	}
	if tmdbID := strings.TrimSpace(detail.TMDBID); tmdbID != "" {
		query.MediaSource, query.MediaID = "themoviedb", tmdbID
	}
	return query
}

type recoveredTVSubscriptionProgressStore interface {
	RecordRecoveredTVSubscriptionProgress(context.Context, string, int64, subscription.State, time.Time, int) error
}

func (h *Handler) finishRecoveredTVTarget(ctx context.Context, target persistedSubscription, lack int) error {
	store, ok := h.store.(recoveredTVSubscriptionProgressStore)
	if !ok {
		return ErrSubscriptionSearchStorage
	}
	err := store.RecordRecoveredTVSubscriptionProgress(ctx, target.Username, target.ID, target.InternalState, target.UpdatedAt, lack)
	if err == nil || subscriptionTargetNotFound(err) {
		return nil
	}
	if errors.Is(err, errSubscriptionSearchStateConflict) {
		return ErrSubscriptionTargetChanged
	}
	return stableContextError(ctx, err, ErrSubscriptionSearchStorage)
}

func recoveredTVPlanTarget(query subscription.Query, expected []subscription.Episode) *subscription.TVRecoveredTarget {
	if query.TVRecoveredTarget == nil {
		return nil
	}
	return &subscription.TVRecoveredTarget{
		TotalEpisode:     query.TVRecoveredTarget.TotalEpisode,
		MissingEpisodes:  append([]subscription.Episode{}, query.TVRecoveredTarget.MissingEpisodes...),
		ExpectedCoverage: append([]subscription.Episode{}, expected...),
	}
}
