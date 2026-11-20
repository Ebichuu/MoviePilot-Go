// Package mediaserver holds the media-server-independent projection of the
// MoviePilot V2 `mediaserveritem` sync cache. The cache is the fact source for
// V2's `itemid -> get_iteminfo` movie fast path; River only wakes the sync
// task, and Redis never participates in cache decisions.
//
// The package deliberately does not import a concrete HTTP client, the
// database adapter or River, so consumers own the narrow seams they need.
package mediaserver

import (
	"context"
	"errors"
	"strings"
	"time"
)

// ItemType reproduces the two media types V2 persists for synced items.
const (
	ItemTypeMovie = "电影"
	ItemTypeTV    = "电视剧"
)

// Item is one row of the V2 `mediaserveritem` cache.
//
// TMDBID uses zero for V2's absent/NULL value: V2 only queries by TMDB ID when
// the argument is truthy. SeasonInfo mirrors V2's `seasoninfo` JSON object as
// season -> episodes.
type Item struct {
	Server        string
	Library       string
	ItemID        string
	ItemType      string
	Title         string
	OriginalTitle string
	Year          string
	TMDBID        int64
	IMDBID        string
	TVDBID        string
	Path          string
	SeasonInfo    map[int][]int
	LstModDate    string
}

// Valid rejects rows the sync task must never persist. V2 upserts any payload
// whose server and item ID are non-empty and derives stale rows from
// `lst_mod_date`; the remaining checks keep impossible values out of the cache
// instead of silently widening the later lookup.
func (i Item) Valid() bool {
	if strings.TrimSpace(i.Server) == "" || strings.TrimSpace(i.ItemID) == "" ||
		strings.TrimSpace(i.LstModDate) == "" {
		return false
	}
	switch i.ItemType {
	case ItemTypeMovie, ItemTypeTV:
	default:
		return false
	}
	for season, episodes := range i.SeasonInfo {
		if season < 0 || season > 999 {
			return false
		}
		for _, episode := range episodes {
			if episode < 1 || episode > 5000 {
				return false
			}
		}
	}
	return true
}

// Query mirrors the lookup precedence of V2 `MediaServerOper.get_item_id`:
// a non-zero TMDB ID is used on its own, otherwise title/item type/year apply
// with V2's empty-value rules. V2's movie caller passes `mtype="电影"` and no
// year, so Year stays empty on that path.
type Query struct {
	TMDBID   int64
	ItemType string
	Title    string
	Year     string
}

// Empty reports the lookup V2 answers with an ordinary miss: neither a TMDB ID
// nor a title was supplied.
func (q Query) Empty() bool {
	return q.TMDBID <= 0 && strings.TrimSpace(q.Title) == ""
}

// ItemLookup is the read seam owned by the subscription flow.
type ItemLookup interface {
	FindMediaServerItemID(context.Context, Query) (string, bool, error)
}

// ItemStore is the write seam used by the scheduled sync task.
type ItemStore interface {
	UpsertMediaServerItem(context.Context, Item) error
	DeleteStaleMediaServerItems(context.Context, string, string) (int64, error)
	DeleteExcludedMediaServerItems(context.Context, []string) (int64, error)
}

// SyncSchedule is one enabled media server and its effective sync interval. V2
// registers one interval schedule per enabled server and skips servers without
// a positive effective interval.
type SyncSchedule struct {
	Server   string
	Interval time.Duration
}

var (
	// ErrInvalidItem marks a row or sync argument that must not reach storage.
	ErrInvalidItem = errors.New("媒体服务器缓存条目无效")
	// ErrItemStore marks a cache write or maintenance failure.
	ErrItemStore = errors.New("媒体服务器缓存写入失败")
	// ErrItemLookup marks a cache read failure.
	ErrItemLookup = errors.New("媒体服务器缓存查询失败")
)
