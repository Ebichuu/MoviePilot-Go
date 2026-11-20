package database

import (
	"context"
	"errors"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

const ensureRSSCursorSQL = `
	INSERT INTO site_rss_cursors (site_id, endpoint)
	VALUES ($1, $2)
	ON CONFLICT (site_id) DO UPDATE SET
		endpoint=EXCLUDED.endpoint,
		etag=CASE WHEN site_rss_cursors.endpoint=EXCLUDED.endpoint THEN site_rss_cursors.etag ELSE '' END,
		last_modified=CASE WHEN site_rss_cursors.endpoint=EXCLUDED.endpoint THEN site_rss_cursors.last_modified ELSE '' END,
		high_water_published_at=CASE WHEN site_rss_cursors.endpoint=EXCLUDED.endpoint THEN site_rss_cursors.high_water_published_at ELSE NULL END,
		high_water_item_key=CASE WHEN site_rss_cursors.endpoint=EXCLUDED.endpoint THEN site_rss_cursors.high_water_item_key ELSE '' END,
		next_poll_at=CASE WHEN site_rss_cursors.endpoint=EXCLUDED.endpoint THEN site_rss_cursors.next_poll_at ELSE now() END,
		failure_count=CASE WHEN site_rss_cursors.endpoint=EXCLUDED.endpoint THEN site_rss_cursors.failure_count ELSE 0 END,
		last_error=CASE WHEN site_rss_cursors.endpoint=EXCLUDED.endpoint THEN site_rss_cursors.last_error ELSE '' END,
		last_attempt_at=CASE WHEN site_rss_cursors.endpoint=EXCLUDED.endpoint THEN site_rss_cursors.last_attempt_at ELSE NULL END,
		last_success_at=CASE WHEN site_rss_cursors.endpoint=EXCLUDED.endpoint THEN site_rss_cursors.last_success_at ELSE NULL END,
		lease_token=CASE WHEN site_rss_cursors.endpoint=EXCLUDED.endpoint THEN site_rss_cursors.lease_token ELSE NULL END,
		lease_expires_at=CASE WHEN site_rss_cursors.endpoint=EXCLUDED.endpoint THEN site_rss_cursors.lease_expires_at ELSE NULL END,
		updated_at=now()
	RETURNING site_id, endpoint, etag, last_modified, high_water_published_at,
		high_water_item_key, next_poll_at, last_attempt_at, last_success_at,
		failure_count, last_error, lease_token, lease_expires_at, updated_at`

func (s *Store) EnsureRSSCursor(ctx context.Context, siteID int64, endpoint string) (RSSCursor, error) {
	if err := validateRSSStore(s, ctx); err != nil {
		return RSSCursor{}, err
	}
	endpoint = strings.TrimSpace(endpoint)
	if siteID < 1 || endpoint == "" || len(endpoint) > maxRSSCursorEndpoint || containsRSSSecretMarker(endpoint) {
		return RSSCursor{}, ErrInvalidRSSInput
	}
	row := s.db.Pool.QueryRow(ctx, ensureRSSCursorSQL, siteID, endpoint)
	cursor, err := scanRSSCursor(row)
	if err != nil {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return RSSCursor{}, ctxErr
		}
		return RSSCursor{}, ErrRSSPersistence
	}
	return cursor, nil
}

// ListDueRSSCursors 返回固定时间截面的到期游标。过期租约也会返回，供恢复
// 或重新领取；limit 由调用方提供但永远不会超过 maxRSSItemsLimit。
func (s *Store) ListDueRSSCursors(ctx context.Context, now time.Time, limit int) ([]RSSCursor, error) {
	if err := validateRSSStore(s, ctx); err != nil {
		return nil, err
	}
	if now.IsZero() || limit < 1 || limit > maxRSSItemsLimit {
		return nil, ErrInvalidRSSInput
	}
	rows, err := s.db.Pool.Query(ctx, `
		SELECT cursor.site_id, cursor.endpoint, cursor.etag, cursor.last_modified,
			cursor.high_water_published_at, cursor.high_water_item_key,
			cursor.next_poll_at, cursor.last_attempt_at, cursor.last_success_at,
			cursor.failure_count, cursor.last_error, cursor.lease_token,
			cursor.lease_expires_at, cursor.updated_at
		FROM site_rss_cursors AS cursor
		JOIN sites AS site ON site.id=cursor.site_id
		WHERE cursor.next_poll_at <= $1 AND site.enabled=TRUE
		  AND (cursor.lease_expires_at IS NULL OR cursor.lease_expires_at <= $1)
		ORDER BY cursor.next_poll_at, cursor.site_id
		LIMIT $2`, now.UTC(), limit)
	if err != nil {
		return nil, rssPersistenceError(ctx)
	}
	defer rows.Close()
	cursors := make([]RSSCursor, 0, limit)
	for rows.Next() {
		cursor, scanErr := scanRSSCursor(rows)
		if scanErr != nil {
			return nil, ErrRSSPersistence
		}
		cursors = append(cursors, cursor)
	}
	if err := rows.Err(); err != nil {
		return nil, rssPersistenceError(ctx)
	}
	return cursors, nil
}

// TryAcquireRSSLease 以站点和 endpoint 做 CAS，只有到期且没有有效租约时
// 才成功。未到期游标返回 false,nil；有效租约返回 ErrRSSLeaseConflict。
func (s *Store) TryAcquireRSSLease(ctx context.Context, siteID int64, endpoint string, now time.Time, leaseDuration time.Duration) (RSSCursor, bool, error) {
	if err := validateRSSStore(s, ctx); err != nil {
		return RSSCursor{}, false, err
	}
	endpoint = strings.TrimSpace(endpoint)
	if siteID < 1 || endpoint == "" || len(endpoint) > maxRSSCursorEndpoint || containsRSSSecretMarker(endpoint) || now.IsZero() || leaseDuration < time.Second || leaseDuration > time.Hour {
		return RSSCursor{}, false, ErrInvalidRSSInput
	}
	token := uuid.New()
	expiresAt := now.UTC().Add(leaseDuration)
	row := s.db.Pool.QueryRow(ctx, `
		UPDATE site_rss_cursors
		SET lease_token=$4, lease_expires_at=$5, last_attempt_at=$3, updated_at=now()
		WHERE site_id=$1 AND endpoint=$2
		  AND next_poll_at <= $3
		  AND (lease_expires_at IS NULL OR lease_expires_at <= $3)
		RETURNING site_id, endpoint, etag, last_modified, high_water_published_at,
			high_water_item_key, next_poll_at, last_attempt_at, last_success_at,
			failure_count, last_error, lease_token, lease_expires_at, updated_at`, siteID, endpoint, now.UTC(), token, expiresAt)
	cursor, err := scanRSSCursor(row)
	if err == nil {
		return cursor, true, nil
	}
	if ctxErr := ctx.Err(); ctxErr != nil {
		return RSSCursor{}, false, ctxErr
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return RSSCursor{}, false, ErrRSSPersistence
	}
	var leaseExpires *time.Time
	var nextPoll time.Time
	if err := s.db.Pool.QueryRow(ctx, `SELECT next_poll_at, lease_expires_at FROM site_rss_cursors WHERE site_id=$1 AND endpoint=$2`, siteID, endpoint).Scan(&nextPoll, &leaseExpires); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return RSSCursor{}, false, ErrRSSCursorNotFound
		}
		return RSSCursor{}, false, rssPersistenceError(ctx)
	}
	if leaseExpires != nil && leaseExpires.After(now.UTC()) {
		return RSSCursor{}, false, ErrRSSLeaseConflict
	}
	return RSSCursor{}, false, nil
}

// CommitRSSPoll 原子提交一次成功的 RSS 轮询。HTTP 请求不应在数据库事务
// 内执行；token 和 endpoint 必须同时匹配且租约仍有效。
func (s *Store) CommitRSSPoll(ctx context.Context, siteID int64, endpoint string, token uuid.UUID, result RSSPollResult, nextPollAt time.Time) (returnErr error) {
	if err := validateRSSStore(s, ctx); err != nil {
		return err
	}
	endpoint = strings.TrimSpace(endpoint)
	if siteID < 1 || endpoint == "" || len(endpoint) > maxRSSCursorEndpoint || containsRSSSecretMarker(endpoint) || token == uuid.Nil ||
		result.ObservedAt.IsZero() || nextPollAt.IsZero() || !nextPollAt.After(result.ObservedAt) || len(result.Items) > maxRSSItemsLimit {
		return ErrInvalidRSSInput
	}
	if result.NotModified && (len(result.Items) != 0 || result.ReplaceCache) {
		return ErrInvalidRSSInput
	}
	if result.ReplaceCache && len(result.Items) == 0 {
		return ErrInvalidRSSInput
	}
	preparedItems, err := s.prepareRSSLocatorSecrets(result.Items)
	if err != nil {
		return err
	}
	validatedItems, err := normalizeRSSItems(preparedItems)
	if err != nil {
		return err
	}
	tx, err := s.db.Pool.Begin(ctx)
	if err != nil {
		return rssPersistenceError(ctx)
	}
	defer func() {
		rollbackErr := tx.Rollback(ctx)
		if rollbackErr != nil && !errors.Is(rollbackErr, pgx.ErrTxClosed) && returnErr == nil {
			returnErr = rssPersistenceError(ctx)
		}
	}()
	err = tx.QueryRow(ctx, `
		SELECT lease_expires_at FROM site_rss_cursors
		WHERE site_id=$1 AND endpoint=$2 AND lease_token=$3
			AND lease_expires_at > now()
		FOR UPDATE`, siteID, endpoint, token).Scan(new(time.Time))
	if errors.Is(err, pgx.ErrNoRows) {
		return ErrRSSLeaseConflict
	}
	if err != nil {
		return rssPersistenceError(ctx)
	}
	if !result.ReplaceCache {
		rows, queryErr := tx.Query(ctx, `
			SELECT title, COALESCE(resource->>'description', '')
			FROM site_rss_items
			WHERE site_id=$1
			ORDER BY id DESC
			LIMIT $2`, siteID, maxRSSCacheItems)
		if queryErr != nil {
			return rssPersistenceError(ctx)
		}
		cachedSignatures := make(map[string]struct{}, maxRSSCacheItems)
		for rows.Next() {
			var title, description string
			if scanErr := rows.Scan(&title, &description); scanErr != nil {
				rows.Close()
				return rssPersistenceError(ctx)
			}
			cachedSignatures[rssCacheSignature(title, description)] = struct{}{}
		}
		if rowsErr := rows.Err(); rowsErr != nil {
			rows.Close()
			return rssPersistenceError(ctx)
		}
		rows.Close()
		validatedItems = filterRSSItemsByCacheSignatures(cachedSignatures, validatedItems)
	} else {
		if _, err := tx.Exec(ctx, `
			DELETE FROM site_rss_items
			WHERE site_id=$1`, siteID); err != nil {
			return rssPersistenceError(ctx)
		}
	}
	for _, item := range validatedItems {
		_, err = tx.Exec(ctx, `
			INSERT INTO site_rss_items
				(site_id, item_key, guid, download_id, download_secret, detail_url, download_url, title,
				 recognized_media_title, media_recognize_fail_count, published_at, resource, first_seen_at, last_seen_at, state)
			VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12::jsonb,$13,$13,'pending')
			ON CONFLICT (site_id,item_key) DO UPDATE SET
				last_seen_at=GREATEST(site_rss_items.last_seen_at, EXCLUDED.last_seen_at),
				recognized_media_title=CASE WHEN EXCLUDED.recognized_media_title <> ''
					THEN EXCLUDED.recognized_media_title ELSE site_rss_items.recognized_media_title END,
				media_recognize_fail_count=CASE
					WHEN EXCLUDED.recognized_media_title <> '' THEN 0
					ELSE site_rss_items.media_recognize_fail_count END,
				updated_at=now()`,
			siteID, item.ItemKey, item.GUID, item.DownloadID, item.ResourceSecret, item.DetailURL, item.DownloadURL,
			item.Title, item.RecognizedMediaTitle, item.MediaRecognizeFailCount, item.PublishedAt, item.Resource, result.ObservedAt.UTC())
		if err != nil {
			return rssPersistenceError(ctx)
		}
	}
	var highWaterPublishedAt *time.Time
	var highWaterItemKey string
	for _, item := range validatedItems {
		if item.PublishedAt == nil {
			continue
		}
		if highWaterPublishedAt == nil || item.PublishedAt.After(*highWaterPublishedAt) ||
			(item.PublishedAt.Equal(*highWaterPublishedAt) && item.ItemKey > highWaterItemKey) {
			published := item.PublishedAt.UTC()
			highWaterPublishedAt = &published
			highWaterItemKey = item.ItemKey
		}
	}
	var cursorResult int64
	if result.NotModified {
		commandTag, updateErr := tx.Exec(ctx, `
			UPDATE site_rss_cursors
			SET etag=COALESCE(NULLIF($4,''),etag),
				last_modified=COALESCE(NULLIF($5,''),last_modified),
				next_poll_at=$6, last_success_at=now(), failure_count=0,
				last_error='', lease_token=NULL, lease_expires_at=NULL, updated_at=now()
			WHERE site_id=$1 AND endpoint=$2 AND lease_token=$3
				AND lease_expires_at > now()`, siteID, endpoint, token, cleanRSSHeader(result.ETag), cleanRSSHeader(result.LastModified), nextPollAt.UTC())
		err = updateErr
		cursorResult = commandTag.RowsAffected()
	} else if highWaterPublishedAt == nil {
		commandTag, updateErr := tx.Exec(ctx, `
			UPDATE site_rss_cursors
			SET etag=$4, last_modified=$5, next_poll_at=$6, last_success_at=now(),
				failure_count=0, last_error='', lease_token=NULL, lease_expires_at=NULL, updated_at=now()
			WHERE site_id=$1 AND endpoint=$2 AND lease_token=$3
				AND lease_expires_at > now()`, siteID, endpoint, token, cleanRSSHeader(result.ETag), cleanRSSHeader(result.LastModified), nextPollAt.UTC())
		err = updateErr
		cursorResult = commandTag.RowsAffected()
	} else {
		commandTag, updateErr := tx.Exec(ctx, `
			UPDATE site_rss_cursors
			SET etag=$4, last_modified=$5, high_water_published_at=GREATEST(COALESCE(high_water_published_at, $6), $6),
				high_water_item_key=CASE WHEN high_water_published_at IS NULL OR $6 > high_water_published_at OR ($6 = high_water_published_at AND $7 > high_water_item_key) THEN $7 ELSE high_water_item_key END,
				next_poll_at=$8, last_success_at=now(), failure_count=0, last_error='',
				lease_token=NULL, lease_expires_at=NULL, updated_at=now()
			WHERE site_id=$1 AND endpoint=$2 AND lease_token=$3
				AND lease_expires_at > now()`, siteID, endpoint, token, cleanRSSHeader(result.ETag), cleanRSSHeader(result.LastModified), *highWaterPublishedAt, highWaterItemKey, nextPollAt.UTC())
		err = updateErr
		cursorResult = commandTag.RowsAffected()
	}
	if err != nil {
		return rssPersistenceError(ctx)
	}
	if cursorResult != 1 {
		return ErrRSSLeaseConflict
	}
	if err := tx.Commit(ctx); err != nil {
		return rssPersistenceError(ctx)
	}
	return nil
}

// FailRSSPoll 记录稳定、脱敏的失败信息并释放游标租约。过期 token 不能
// 覆盖后来 worker 的成功结果。
func (s *Store) FailRSSPoll(ctx context.Context, siteID int64, endpoint string, token uuid.UUID, retryAt time.Time, stableErr string) error {
	if err := validateRSSStore(s, ctx); err != nil {
		return err
	}
	endpoint = strings.TrimSpace(endpoint)
	if siteID < 1 || endpoint == "" || len(endpoint) > maxRSSCursorEndpoint || containsRSSSecretMarker(endpoint) || token == uuid.Nil || retryAt.IsZero() {
		return ErrInvalidRSSInput
	}
	result, err := s.db.Pool.Exec(ctx, `
		UPDATE site_rss_cursors
		SET next_poll_at=$4, failure_count=LEAST(failure_count+1, 2147483647),
			last_error=$5, lease_token=NULL, lease_expires_at=NULL, updated_at=now()
		WHERE site_id=$1 AND endpoint=$2 AND lease_token=$3
			AND lease_expires_at > now()`, siteID, endpoint, token, retryAt.UTC(), sanitizeRSSError(stableErr))
	if err != nil {
		return rssPersistenceError(ctx)
	}
	if result.RowsAffected() == 1 {
		return nil
	}
	var exists bool
	if err := s.db.Pool.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM site_rss_cursors WHERE site_id=$1 AND endpoint=$2)`, siteID, endpoint).Scan(&exists); err != nil {
		return rssPersistenceError(ctx)
	}
	if !exists {
		return ErrRSSCursorNotFound
	}
	// A zero-row update with an existing cursor is a stale/expired token.
	var currentToken *uuid.UUID
	if err := s.db.Pool.QueryRow(ctx, `SELECT lease_token FROM site_rss_cursors WHERE site_id=$1 AND endpoint=$2`, siteID, endpoint).Scan(&currentToken); err != nil {
		return rssPersistenceError(ctx)
	}
	return ErrRSSLeaseConflict
}
