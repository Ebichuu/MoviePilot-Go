package app

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strconv"
	"strings"

	"github.com/moviepilot-go/moviepilot-go/internal/mediaserver"
	"github.com/moviepilot-go/moviepilot-go/internal/mediaserver/emby"
	"github.com/moviepilot-go/moviepilot-go/internal/subscription"
	"github.com/moviepilot-go/moviepilot-go/internal/subscriptionflow"
)

func (r *mediaServerRuntime) FindExistingMovie(
	ctx context.Context,
	query subscriptionflow.ExistingMovieQuery,
) (bool, error) {
	if ctx == nil {
		return false, errors.New("媒体服务器请求失败")
	}
	if err := ctx.Err(); err != nil {
		return false, err
	}
	title := strings.TrimSpace(query.Title)
	if title == "" {
		return false, nil
	}
	year := strings.TrimSpace(query.Year)
	tmdbID := subscriptionTMDBID(query.MediaSource, query.MediaID)
	// V2 resolves one bare cached item ID without any server condition and then
	// broadcasts it across the configured servers in order.
	cachedItemID := r.cachedMovieItemID(ctx, tmdbID, title)
	for _, entry := range r.entriesSnapshot("") {
		if cachedItemID != "" {
			userID := r.entryUserID(ctx, entry)
			if userID != "" {
				_, found, err := entry.client.GetItemInfo(ctx, userID, cachedItemID)
				if err != nil {
					if contextErr := mediaServerSubscriptionContextError(ctx, err); contextErr != nil {
						return false, contextErr
					}
					// A failed or missing detail lookup falls through to the
					// title search on the same server, exactly like V2.
				} else if found {
					return true, nil
				}
			}
		}
		found, err := findEmbyMovie(ctx, entry.client, title, year, tmdbID)
		if err != nil {
			if contextErr := mediaServerSubscriptionContextError(ctx, err); contextErr != nil {
				return false, contextErr
			}
			continue
		}
		if found {
			return true, nil
		}
	}
	return false, nil
}

// cachedMovieItemID reproduces V2's `MediaServerOper.get_item_id` lookup for
// the movie path: item type `电影`, the recognized title and, when the main
// provider is TMDB, the TMDB ID. V2 passes no year on this path. A cache read
// failure is an ordinary miss so the live Emby fallback still runs, which keeps
// the completion chain fail-open.
func (r *mediaServerRuntime) cachedMovieItemID(ctx context.Context, tmdbID, title string) string {
	_, lookup := r.itemCache()
	if lookup == nil {
		return ""
	}
	query := mediaserver.Query{ItemType: mediaserver.ItemTypeMovie, Title: title}
	if parsed, err := strconv.ParseInt(strings.TrimSpace(tmdbID), 10, 64); err == nil && parsed > 0 {
		query.TMDBID = parsed
	}
	itemID, found, err := lookup.FindMediaServerItemID(ctx, query)
	if err != nil {
		r.logWarn("媒体服务器缓存查询失败", "error", err)
		return ""
	}
	if !found {
		return ""
	}
	return strings.TrimSpace(itemID)
}

func findEmbyMovie(
	ctx context.Context,
	client *emby.Client,
	title string,
	year string,
	tmdbID string,
) (bool, error) {
	response, err := client.SearchMovies(ctx, emby.SearchMoviesOptions{
		SearchTerm: title,
		Limit:      10,
	})
	if err != nil {
		return false, err
	}
	if response.StartIndex != 0 || len(response.Items) > 10 || response.TotalRecordCount < len(response.Items) {
		return false, errors.New("媒体服务器返回了无效结果")
	}
	for _, item := range response.Items {
		if item.Name != title ||
			(year != "" && mediaServerProductionYear(item.ProductionYear) != year) {
			continue
		}
		if tmdbID != "" {
			providerID, present := embyTMDBProviderID(item.ProviderIDs)
			if !present || providerID != tmdbID {
				continue
			}
		}
		return true, nil
	}
	return false, nil
}

func (r *mediaServerRuntime) FindExistingEpisodes(
	ctx context.Context,
	query subscriptionflow.ExistingEpisodesQuery,
) ([]subscription.Episode, error) {
	if ctx == nil {
		return nil, errors.New("媒体服务器请求失败")
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	title := strings.TrimSpace(query.Title)
	if title == "" {
		return []subscription.Episode{}, nil
	}
	tmdbID := subscriptionTMDBID(query.MediaSource, query.MediaID)
	for _, entry := range r.entriesSnapshot("") {
		series, found, err := findEmbySeries(ctx, entry.client, title, strings.TrimSpace(query.Year), tmdbID)
		if err != nil {
			if contextErr := mediaServerSubscriptionContextError(ctx, err); contextErr != nil {
				return nil, contextErr
			}
			continue
		}
		if !found {
			continue
		}
		episodes, err := listEmbySeriesEpisodes(ctx, entry.client, series.ID)
		if err != nil {
			if contextErr := mediaServerSubscriptionContextError(ctx, err); contextErr != nil {
				return nil, contextErr
			}
			continue
		}
		// V2 returns the first configured server that contains any episodes.
		if len(episodes) > 0 {
			return episodes, nil
		}
	}
	return []subscription.Episode{}, nil
}

func findEmbySeries(
	ctx context.Context,
	client *emby.Client,
	title string,
	year string,
	tmdbID string,
) (emby.Item, bool, error) {
	response, err := client.SearchSeries(ctx, emby.SearchSeriesOptions{
		SearchTerm: title,
		Limit:      10,
	})
	if err != nil {
		return emby.Item{}, false, err
	}
	if response.StartIndex != 0 || len(response.Items) > 10 || response.TotalRecordCount < len(response.Items) {
		return emby.Item{}, false, errors.New("媒体服务器返回了无效结果")
	}
	for _, item := range response.Items {
		if item.Name != title ||
			(year != "" && mediaServerProductionYear(item.ProductionYear) != year) {
			continue
		}
		if tmdbID != "" {
			providerID, present := embyTMDBProviderID(item.ProviderIDs)
			if present && providerID != "" && providerID != tmdbID {
				return emby.Item{}, false, nil
			}
		}
		if strings.TrimSpace(item.ID) == "" {
			return emby.Item{}, false, nil
		}
		return item, true, nil
	}
	return emby.Item{}, false, nil
}

func listEmbySeriesEpisodes(
	ctx context.Context,
	client *emby.Client,
	seriesID string,
) ([]subscription.Episode, error) {
	found := make(map[subscription.Episode]struct{})
	for start := 0; start < existingEpisodeMaxItems; {
		response, err := client.ListEpisodes(ctx, emby.ListEpisodesOptions{
			SeriesID: seriesID, StartIndex: start, Limit: existingEpisodePageSize,
		})
		if err != nil {
			return nil, err
		}
		if response.StartIndex != start || len(response.Items) > existingEpisodePageSize ||
			response.TotalRecordCount < start+len(response.Items) ||
			response.TotalRecordCount > existingEpisodeMaxItems {
			return nil, errors.New("媒体服务器返回了无效分页")
		}
		for _, item := range response.Items {
			if item.ParentIndexNumber == nil || item.IndexNumber == nil ||
				*item.ParentIndexNumber < 0 || *item.ParentIndexNumber > 999 ||
				*item.IndexNumber < 1 || *item.IndexNumber > 5000 {
				continue
			}
			found[subscription.Episode{
				Season: *item.ParentIndexNumber, Episode: *item.IndexNumber,
			}] = struct{}{}
		}
		next := start + len(response.Items)
		if next >= response.TotalRecordCount {
			break
		}
		if len(response.Items) == 0 || next >= existingEpisodeMaxItems {
			return nil, errors.New("媒体服务器结果超过有界分页限制")
		}
		start = next
	}
	result := make([]subscription.Episode, 0, len(found))
	for episode := range found {
		result = append(result, episode)
	}
	slices.SortFunc(result, func(left, right subscription.Episode) int {
		if left.Season != right.Season {
			return left.Season - right.Season
		}
		return left.Episode - right.Episode
	})
	return result, nil
}

func subscriptionTMDBID(source, id string) string {
	switch strings.ToLower(strings.TrimSpace(source)) {
	case "tmdb", "themoviedb":
		id = strings.TrimSpace(id)
		if id != "" {
			return id
		}
	}
	return ""
}

func embyTMDBProviderID(providerIDs map[string]string) (string, bool) {
	for provider, value := range providerIDs {
		if strings.EqualFold(strings.TrimSpace(provider), "tmdb") {
			return strings.TrimSpace(value), true
		}
	}
	return "", false
}

func mediaServerProductionYear(year int) string {
	if year <= 0 {
		return ""
	}
	return fmt.Sprintf("%d", year)
}

func mediaServerSubscriptionContextError(ctx context.Context, err error) error {
	if ctx != nil {
		if contextErr := ctx.Err(); contextErr != nil {
			return contextErr
		}
	}
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return err
	}
	return nil
}

var _ subscriptionflow.ExistingEpisodesSource = (*mediaServerRuntime)(nil)
var _ subscriptionflow.ExistingMovieSource = (*mediaServerRuntime)(nil)
