package app

import (
	"context"
	"errors"
	"strconv"
	"strings"

	"github.com/moviepilot-go/moviepilot-go/internal/httpapi"
	"github.com/moviepilot-go/moviepilot-go/internal/mediaserver"
	"github.com/moviepilot-go/moviepilot-go/internal/mediaserver/emby"
)

var errLocalMediaServerLookup = errors.New("media server index lookup failed")

// FindLocalMediaServerItem finds one Emby item by TMDB ID. V2 answers the same
// question from its cache with `.first()`, so a library that holds several
// copies of one movie still counts as "present"; the first match wins and the
// remaining servers are not queried.
func (r *mediaServerRuntime) FindLocalMediaServerItem(
	ctx context.Context,
	query httpapi.LocalMediaExistsQuery,
) (string, bool, error) {
	if r == nil {
		return "", false, nil
	}
	if ctx == nil {
		return "", false, errLocalMediaServerLookup
	}
	if err := ctx.Err(); err != nil {
		return "", false, err
	}
	if query.TMDBID <= 0 {
		// 没有 TMDB ID 时与 V2 一致地查缓存（标题 + 类型 + 年份）。
		return r.findCachedMediaServerItem(ctx, query)
	}

	providerQuery := tmdbProviderID(query.TMDBID)
	providerValue := strconv.FormatInt(query.TMDBID, 10)
	for _, entry := range r.entriesSnapshot("") {
		if err := ctx.Err(); err != nil {
			return "", false, err
		}
		response, err := entry.client.FindItems(ctx, emby.FindItemsOptions{
			AnyProviderIDEquals: providerQuery,
			Recursive:           true,
			Fields:              []string{"ProviderIds"},
			Limit:               2,
		})
		if err != nil {
			if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
				return "", false, err
			}
			return "", false, errLocalMediaServerLookup
		}

		if itemID, found := firstMatchingLocalMediaServerItem(response, providerValue); found {
			return itemID, true, nil
		}
	}
	return "", false, nil
}

func firstMatchingLocalMediaServerItem(response emby.ItemsResponse, tmdbID string) (string, bool) {
	for _, item := range response.Items {
		itemID := strings.TrimSpace(item.ID)
		if itemID == "" || !hasMatchingTMDBProvider(item.ProviderIDs, tmdbID) {
			continue
		}
		return itemID, true
	}
	return "", false
}

// findCachedMediaServerItem answers the title/year/type form of V2's `exists`
// from the synced cache. An empty type keeps V2's "no type filter" rule.
func (r *mediaServerRuntime) findCachedMediaServerItem(
	ctx context.Context,
	query httpapi.LocalMediaExistsQuery,
) (string, bool, error) {
	if r.itemLookup == nil {
		return "", false, nil
	}
	title := strings.TrimSpace(query.Title)
	if title == "" {
		return "", false, nil
	}
	itemID, found, err := r.itemLookup.FindMediaServerItemID(ctx, mediaserver.Query{
		Title:    title,
		ItemType: localMediaItemType(query.Type),
		Year:     strings.TrimSpace(query.Year),
	})
	if err != nil {
		return "", false, errLocalMediaServerLookup
	}
	return itemID, found, nil
}

func localMediaItemType(mtype string) string {
	switch strings.ToLower(strings.TrimSpace(mtype)) {
	case "movie", "movies", mediaserver.ItemTypeMovie:
		return mediaserver.ItemTypeMovie
	case "tv", "tvshow", "series", "show", mediaserver.ItemTypeTV:
		return mediaserver.ItemTypeTV
	default:
		return ""
	}
}

func hasMatchingTMDBProvider(providerIDs map[string]string, tmdbID string) bool {
	found := false
	for provider, value := range providerIDs {
		if !strings.EqualFold(strings.TrimSpace(provider), "tmdb") {
			continue
		}
		if strings.TrimSpace(value) != tmdbID {
			return false
		}
		found = true
	}
	return found
}

var _ httpapi.LocalMediaIndexSource = (*mediaServerRuntime)(nil)
