package database

import (
	"context"
	"encoding/json"
	"errors"
	"strconv"
	"strings"

	"github.com/jackc/pgx/v5"

	"github.com/moviepilot-go/moviepilot-go/internal/mediaserver"
)

// UpsertMediaServerItem applies V2's `server + item_id` upsert for one synced
// media server item.
func (s *Store) UpsertMediaServerItem(ctx context.Context, item mediaserver.Item) error {
	if s == nil || s.db == nil || s.db.Pool == nil {
		return mediaserver.ErrItemStore
	}
	if ctx == nil {
		return mediaserver.ErrInvalidItem
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if !item.Valid() {
		return mediaserver.ErrInvalidItem
	}
	seasonInfo, err := json.Marshal(mediaServerSeasonInfoJSON(item.SeasonInfo))
	if err != nil {
		return mediaserver.ErrInvalidItem
	}
	var tmdbID any
	if item.TMDBID > 0 {
		tmdbID = item.TMDBID
	}
	_, execErr := s.db.Pool.Exec(ctx, `
		INSERT INTO media_server_items (
			server, library, item_id, item_type, title, original_title, year,
			tmdbid, imdbid, tvdbid, path, seasoninfo, lst_mod_date, updated_at
		)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, now())
		ON CONFLICT (server, item_id) DO UPDATE SET
			library = EXCLUDED.library,
			item_type = EXCLUDED.item_type,
			title = EXCLUDED.title,
			original_title = EXCLUDED.original_title,
			year = EXCLUDED.year,
			tmdbid = EXCLUDED.tmdbid,
			imdbid = EXCLUDED.imdbid,
			tvdbid = EXCLUDED.tvdbid,
			path = EXCLUDED.path,
			seasoninfo = EXCLUDED.seasoninfo,
			lst_mod_date = EXCLUDED.lst_mod_date,
			updated_at = now()`,
		strings.TrimSpace(item.Server), mediaServerNullText(item.Library), strings.TrimSpace(item.ItemID),
		item.ItemType, mediaServerNullText(item.Title), mediaServerNullText(item.OriginalTitle),
		mediaServerNullText(item.Year), tmdbID, mediaServerNullText(item.IMDBID),
		mediaServerNullText(item.TVDBID), mediaServerNullText(item.Path), seasonInfo,
		strings.TrimSpace(item.LstModDate))
	if execErr != nil {
		return mediaserver.ErrItemStore
	}
	return nil
}

// DeleteStaleMediaServerItems reproduces V2 `delete_stale`: rows of one server
// whose sync timestamp is NULL or different belong to an earlier run.
func (s *Store) DeleteStaleMediaServerItems(ctx context.Context, server, syncTime string) (int64, error) {
	server = strings.TrimSpace(server)
	syncTime = strings.TrimSpace(syncTime)
	if s == nil || s.db == nil || s.db.Pool == nil {
		return 0, mediaserver.ErrItemStore
	}
	if ctx == nil || server == "" || syncTime == "" {
		return 0, mediaserver.ErrInvalidItem
	}
	if err := ctx.Err(); err != nil {
		return 0, err
	}
	tag, err := s.db.Pool.Exec(ctx, `
		DELETE FROM media_server_items
		WHERE server = $1 AND (lst_mod_date IS NULL OR lst_mod_date <> $2)`, server, syncTime)
	if err != nil {
		return 0, mediaserver.ErrItemStore
	}
	return tag.RowsAffected(), nil
}

// DeleteExcludedMediaServerItems reproduces V2 `delete_excluded_servers`: rows
// of disabled or removed servers, including rows without a server name, are
// dropped before a sync run. An empty list therefore clears the whole cache.
func (s *Store) DeleteExcludedMediaServerItems(ctx context.Context, servers []string) (int64, error) {
	if s == nil || s.db == nil || s.db.Pool == nil {
		return 0, mediaserver.ErrItemStore
	}
	if ctx == nil {
		return 0, mediaserver.ErrInvalidItem
	}
	if err := ctx.Err(); err != nil {
		return 0, err
	}
	if len(servers) == 0 {
		tag, err := s.db.Pool.Exec(ctx, `DELETE FROM media_server_items`)
		if err != nil {
			return 0, mediaserver.ErrItemStore
		}
		return tag.RowsAffected(), nil
	}
	names := make([]string, 0, len(servers))
	for _, server := range servers {
		names = append(names, strings.TrimSpace(server))
	}
	tag, err := s.db.Pool.Exec(ctx, `
		DELETE FROM media_server_items
		WHERE server IS NULL OR NOT (server = ANY($1::text[]))`, names)
	if err != nil {
		return 0, mediaserver.ErrItemStore
	}
	return tag.RowsAffected(), nil
}

// FindMediaServerItemID mirrors V2 `MediaServerOper.get_item_id`. A truthy TMDB
// ID is queried alone; otherwise the title, item type and year filters follow
// V2's empty-value rules. Like V2's un-ordered `.first()` there is no ORDER BY,
// so an ambiguous cache returns an unspecified row. A missing item_id value is
// an ordinary miss.
func (s *Store) FindMediaServerItemID(ctx context.Context, query mediaserver.Query) (string, bool, error) {
	if s == nil || s.db == nil || s.db.Pool == nil {
		return "", false, mediaserver.ErrItemLookup
	}
	if ctx == nil {
		return "", false, mediaserver.ErrItemLookup
	}
	if err := ctx.Err(); err != nil {
		return "", false, err
	}
	if query.Empty() {
		return "", false, nil
	}
	itemType := strings.TrimSpace(query.ItemType)
	title := strings.TrimSpace(query.Title)
	year := strings.TrimSpace(query.Year)

	var row pgx.Row
	switch {
	case query.TMDBID > 0:
		row = s.db.Pool.QueryRow(ctx, `
			SELECT item_id FROM media_server_items
			WHERE tmdbid = $1 AND item_type = $2
			LIMIT 1`, query.TMDBID, itemType)
	case itemType == "" && year == "":
		row = s.db.Pool.QueryRow(ctx, `
			SELECT item_id FROM media_server_items
			WHERE title = $1
			LIMIT 1`, title)
	case year == "":
		row = s.db.Pool.QueryRow(ctx, `
			SELECT item_id FROM media_server_items
			WHERE title = $1 AND item_type = $2
			LIMIT 1`, title, itemType)
	case itemType == "":
		row = s.db.Pool.QueryRow(ctx, `
			SELECT item_id FROM media_server_items
			WHERE title = $1 AND year = $2
			LIMIT 1`, title, year)
	default:
		row = s.db.Pool.QueryRow(ctx, `
			SELECT item_id FROM media_server_items
			WHERE title = $1 AND item_type = $2 AND year = $3
			LIMIT 1`, title, itemType, year)
	}

	var itemID *string
	if err := row.Scan(&itemID); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return "", false, nil
		}
		return "", false, mediaserver.ErrItemLookup
	}
	if itemID == nil || strings.TrimSpace(*itemID) == "" {
		return "", false, nil
	}
	return *itemID, true, nil
}

func mediaServerSeasonInfoJSON(seasonInfo map[int][]int) map[string][]int {
	result := make(map[string][]int, len(seasonInfo))
	for season, episodes := range seasonInfo {
		copied := make([]int, 0, len(episodes))
		copied = append(copied, episodes...)
		result[strconv.Itoa(season)] = copied
	}
	return result
}

func mediaServerNullText(value string) any {
	value = strings.TrimSpace(value)
	if value == "" {
		return nil
	}
	return value
}

var _ mediaserver.ItemStore = (*Store)(nil)
var _ mediaserver.ItemLookup = (*Store)(nil)
