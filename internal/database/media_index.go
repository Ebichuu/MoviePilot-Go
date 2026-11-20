package database

import (
	"context"
	"errors"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/moviepilot-go/moviepilot-go/internal/mediaindex"
)

const (
	maxMediaIndexClaimSize = 100
	maxMediaIndexPageSize  = 16
	maxMediaIndexLease     = time.Hour
	minMediaIndexLease     = time.Second
	maxMediaIndexError     = 2048
	maxMediaIndexPath      = 4096
	maxMediaIndexID        = 4096
)

var (
	ErrMediaIndexRequestNotFound = errors.New("媒体入库确认请求不存在")
	ErrMediaIndexLeaseConflict   = errors.New("媒体入库确认请求租约冲突")
	ErrMediaIndexStateConflict   = errors.New("媒体入库确认请求状态冲突")
)

// Claim 按 request ID 原子领取一个媒体入库确认请求。
func (s *Store) Claim(
	ctx context.Context,
	requestID uuid.UUID,
	now time.Time,
	leaseDuration time.Duration,
) (mediaindex.Lease, bool, error) {
	if err := validateMediaIndexCall(s, ctx); err != nil {
		return mediaindex.Lease{}, false, err
	}
	if requestID == uuid.Nil || now.IsZero() || !validMediaIndexLease(leaseDuration) {
		return mediaindex.Lease{}, false, mediaindex.ErrInvalidRequest
	}
	tx, err := s.db.Pool.Begin(ctx)
	if err != nil {
		return mediaindex.Lease{}, false, mediaIndexPersistenceError(ctx)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	token := uuid.New()
	expiresAt := now.UTC().Add(leaseDuration)
	var (
		lease          mediaindex.Lease
		serverID       uuid.UUID
		leaseExpiresAt *time.Time
	)
	err = tx.QueryRow(ctx, `
		WITH candidate AS (
			SELECT r.id
			FROM media_index_requests AS r
			WHERE r.id=$1
			  AND (
				(r.state IN ('pending', 'failed') AND r.next_attempt_at <= $2)
				OR (r.state='working' AND r.lease_expires_at <= $2)
			  )
			  AND NOT EXISTS (
				SELECT 1 FROM scrape_requests AS scrape
				WHERE scrape.batch_id=r.batch_id
				  AND (
					scrape.state IN ('pending', 'working')
					OR (scrape.state='failed' AND scrape.attempt_count < 5)
				  )
			  )
			FOR UPDATE SKIP LOCKED
		), claimed AS (
			UPDATE media_index_requests AS r
			SET state='working', attempt_count=r.attempt_count+1,
				last_error='', lease_token=$3, lease_expires_at=$4, updated_at=$2
			FROM candidate
			WHERE r.id=candidate.id
			RETURNING r.id, r.server_id, r.server_name, r.server_type, r.server_fingerprint, r.state,
				r.attempt_count, r.refresh_requested_at, r.next_attempt_at, r.last_error,
				r.lease_token, r.lease_expires_at
		)
		SELECT id, server_id, server_name, server_type, server_fingerprint, state, attempt_count,
			refresh_requested_at, next_attempt_at, last_error, lease_token, lease_expires_at
		FROM claimed`, requestID, now.UTC(), token, expiresAt).Scan(
		&lease.Request.ID,
		&serverID,
		&lease.Request.Server.Name,
		&lease.Request.Server.Type,
		&lease.Request.Server.Fingerprint,
		&lease.Request.State,
		&lease.Request.AttemptCount,
		&lease.Request.RefreshRequestedAt,
		&lease.Request.NextAttemptAt,
		&lease.Request.LastError,
		&lease.LeaseToken,
		&leaseExpiresAt,
	)
	if errors.Is(err, pgx.ErrNoRows) {
		return mediaindex.Lease{}, false, nil
	}
	if err != nil {
		return mediaindex.Lease{}, false, mediaIndexPersistenceError(ctx)
	}
	lease.Request.Server.ID = serverID.String()
	if leaseExpiresAt == nil || lease.LeaseToken == uuid.Nil || lease.Request.State != mediaindex.RequestWorking ||
		!lease.Request.Server.Valid() || lease.Request.AttemptCount < 1 {
		return mediaindex.Lease{}, false, mediaindex.ErrPersistence
	}
	if err := tx.Commit(ctx); err != nil {
		return mediaindex.Lease{}, false, mediaIndexPersistenceError(ctx)
	}
	return lease, true, nil
}

// Renew 使用 token CAS 延长工作租约。
func (s *Store) Renew(ctx context.Context, requestID, token uuid.UUID, now, leaseUntil time.Time) error {
	if err := validateMediaIndexCall(s, ctx); err != nil {
		return err
	}
	if requestID == uuid.Nil || token == uuid.Nil || now.IsZero() || leaseUntil.IsZero() ||
		!validMediaIndexLease(leaseUntil.Sub(now)) {
		return mediaindex.ErrInvalidRequest
	}
	result, err := s.db.Pool.Exec(ctx, `
		UPDATE media_index_requests
		SET lease_expires_at=$4, updated_at=$3
		WHERE id=$1 AND state='working' AND lease_token=$2
		  AND lease_expires_at > $3`, requestID, token, now.UTC(), leaseUntil.UTC())
	if err != nil {
		return mediaIndexPersistenceError(ctx)
	}
	if result.RowsAffected() != 1 {
		return mediaindex.ErrLeaseLost
	}
	return nil
}

// RenewMediaIndexLease 保留旧调用方兼容性。
func (s *Store) RenewMediaIndexLease(ctx context.Context, requestID, token uuid.UUID, now time.Time, leaseDuration time.Duration) error {
	return s.Renew(ctx, requestID, token, now, now.Add(leaseDuration))
}

// Reschedule 释放当前租约并安排下一次确认，保留稳定、受长度限制的错误分类。
func (s *Store) Reschedule(ctx context.Context, requestID, token uuid.UUID, retryAt time.Time, stableErr string) error {
	if err := validateMediaIndexCall(s, ctx); err != nil {
		return err
	}
	stableErr = normalizeMediaIndexError(stableErr)
	if requestID == uuid.Nil || token == uuid.Nil || retryAt.IsZero() {
		return mediaindex.ErrInvalidRequest
	}
	result, err := s.db.Pool.Exec(ctx, `
		UPDATE media_index_requests
		SET state='failed', next_attempt_at=$3, last_error=$4,
			lease_token=NULL, lease_expires_at=NULL, updated_at=now()
		WHERE id=$1 AND state='working' AND lease_token=$2
		  AND lease_expires_at > now()`, requestID, token, retryAt.UTC(), stableErr)
	if err != nil {
		return mediaIndexPersistenceError(ctx)
	}
	if result.RowsAffected() != 1 {
		return ErrMediaIndexLeaseConflict
	}
	return nil
}

// RecordRefresh 记录已经向服务器发出刷新请求，并把请求安排到确认阶段。
func (s *Store) RecordRefresh(ctx context.Context, requestID, token uuid.UUID, refreshedAt, nextAttemptAt time.Time) error {
	if err := validateMediaIndexCall(s, ctx); err != nil {
		return err
	}
	if requestID == uuid.Nil || token == uuid.Nil || refreshedAt.IsZero() || nextAttemptAt.IsZero() {
		return mediaindex.ErrInvalidRequest
	}
	result, err := s.db.Pool.Exec(ctx, `
		UPDATE media_index_requests
		SET state='failed', refresh_requested_at=COALESCE(refresh_requested_at, $3),
			next_attempt_at=$4, last_error='', lease_token=NULL,
			lease_expires_at=NULL, updated_at=$3
		WHERE id=$1 AND state='working' AND lease_token=$2
		  AND lease_expires_at > now()`, requestID, token, refreshedAt.UTC(), nextAttemptAt.UTC())
	if err != nil {
		return mediaIndexPersistenceError(ctx)
	}
	if result.RowsAffected() != 1 {
		return ErrMediaIndexLeaseConflict
	}
	return nil
}

// ListPendingFiles 返回当前有效租约下尚未确认的一页文件。
func (s *Store) ListPendingFiles(ctx context.Context, requestID, token uuid.UUID, limit int) ([]mediaindex.File, error) {
	if err := validateMediaIndexCall(s, ctx); err != nil {
		return nil, err
	}
	if requestID == uuid.Nil || token == uuid.Nil || limit < 1 || limit > maxMediaIndexPageSize {
		return nil, mediaindex.ErrInvalidRequest
	}
	rows, err := s.db.Pool.Query(ctx, `
		SELECT f.id, f.request_id, f.local_path, f.remote_path, f.path_style,
			f.case_sensitive, f.expected_size, f.expected_sha256, f.media_type,
			f.season, f.episode, f.provider_id, f.confirmed_item_id
		FROM media_index_files AS f
		JOIN media_index_requests AS r ON r.id=f.request_id
		WHERE f.request_id=$1 AND r.state='working' AND r.lease_token=$2
		  AND r.lease_expires_at > now() AND f.confirmed_at IS NULL
		ORDER BY f.ordinal, f.id
		LIMIT $3`, requestID, token, limit)
	if err != nil {
		return nil, mediaIndexPersistenceError(ctx)
	}
	defer rows.Close()
	items := make([]mediaindex.File, 0, limit)
	for rows.Next() {
		var item mediaindex.File
		if err := rows.Scan(
			&item.ID, &item.RequestID, &item.LocalPath, &item.RemotePath, &item.PathStyle,
			&item.CaseSensitive, &item.ExpectedSize, &item.ExpectedSHA256, &item.MediaType,
			&item.Season, &item.Episode, &item.ProviderID, &item.ConfirmedItemID,
		); err != nil {
			return nil, mediaIndexPersistenceError(ctx)
		}
		if !item.Valid(requestID) {
			return nil, mediaindex.ErrPersistence
		}
		items = append(items, item)
	}
	if err := rows.Err(); err != nil {
		return nil, mediaIndexPersistenceError(ctx)
	}
	return items, nil
}

// ConfirmFile 以 token CAS 确认一个文件；重复确认同一 item ID 幂等成功。
func (s *Store) ConfirmFile(ctx context.Context, requestID, token uuid.UUID, fileID int64, itemID string) error {
	if err := validateMediaIndexCall(s, ctx); err != nil {
		return err
	}
	itemID = strings.TrimSpace(itemID)
	if requestID == uuid.Nil || token == uuid.Nil || fileID < 1 || itemID == "" || len(itemID) > maxMediaIndexID || strings.ContainsAny(itemID, "\x00\r\n") {
		return mediaindex.ErrInvalidRequest
	}
	result, err := s.db.Pool.Exec(ctx, `
		UPDATE media_index_files AS f
		SET confirmed_item_id=$4, confirmed_at=COALESCE(f.confirmed_at, now()), updated_at=now()
		WHERE f.id=$3 AND f.request_id=$1
		  AND (
			(f.confirmed_at IS NOT NULL AND f.confirmed_item_id=$4)
			OR (
				f.confirmed_at IS NULL
				AND EXISTS (
					SELECT 1 FROM media_index_requests AS r
					WHERE r.id=$1 AND r.state='working' AND r.lease_token=$2
					  AND r.lease_expires_at > now()
				)
			)
		  )`, requestID, token, fileID, itemID)
	if err != nil {
		return mediaIndexPersistenceError(ctx)
	}
	if result.RowsAffected() == 1 {
		return nil
	}
	return ErrMediaIndexLeaseConflict
}

// ConfirmRequest 在所有文件确认后原子完成请求，并兼容推进升级前遗留的
// pending_index 订阅状态。
func (s *Store) ConfirmRequest(ctx context.Context, requestID, token uuid.UUID, now time.Time) (returnConfirmed bool, returnErr error) {
	if err := validateMediaIndexCall(s, ctx); err != nil {
		return false, err
	}
	if requestID == uuid.Nil || token == uuid.Nil || now.IsZero() {
		return false, mediaindex.ErrInvalidRequest
	}
	tx, err := s.db.Pool.Begin(ctx)
	if err != nil {
		return false, mediaIndexPersistenceError(ctx)
	}
	defer func() {
		rollbackErr := tx.Rollback(ctx)
		if rollbackErr != nil && !errors.Is(rollbackErr, pgx.ErrTxClosed) && returnErr == nil {
			returnErr = mediaIndexPersistenceError(ctx)
		}
	}()
	var (
		batchID       uuid.UUID
		state         mediaindex.RequestState
		leaseToken    *uuid.UUID
		leaseExpires  *time.Time
		refreshAt     *time.Time
		leaseValid    bool
		expectedFiles int
	)
	err = tx.QueryRow(ctx, `
		SELECT batch_id, state, lease_token, lease_expires_at, refresh_requested_at,
			expected_file_count,
			COALESCE(lease_expires_at > now(), FALSE)
		FROM media_index_requests WHERE id=$1 FOR UPDATE`, requestID).Scan(
		&batchID, &state, &leaseToken, &leaseExpires, &refreshAt, &expectedFiles, &leaseValid)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, ErrMediaIndexRequestNotFound
	}
	if err != nil {
		return false, mediaIndexPersistenceError(ctx)
	}
	if state == mediaindex.RequestConfirmed {
		if err := tx.Commit(ctx); err != nil {
			return false, mediaIndexPersistenceError(ctx)
		}
		return true, nil
	}
	if state != mediaindex.RequestWorking || leaseToken == nil || *leaseToken != token || leaseExpires == nil || !leaseValid {
		return false, ErrMediaIndexLeaseConflict
	}
	if refreshAt == nil {
		return false, ErrMediaIndexStateConflict
	}
	var total, confirmed int64
	if err := tx.QueryRow(ctx, `
		SELECT count(*), count(*) FILTER (WHERE confirmed_at IS NOT NULL)
		FROM media_index_files WHERE request_id=$1`, requestID).Scan(&total, &confirmed); err != nil {
		return false, mediaIndexPersistenceError(ctx)
	}
	if total < 1 || total != int64(expectedFiles) {
		return false, ErrMediaIndexStateConflict
	}
	if total != confirmed {
		if err := tx.Commit(ctx); err != nil {
			return false, mediaIndexPersistenceError(ctx)
		}
		return false, nil
	}
	result, err := tx.Exec(ctx, `
		UPDATE media_index_requests
		SET state='confirmed', completed_at=COALESCE(completed_at, $2),
			lease_token=NULL, lease_expires_at=NULL, last_error='', updated_at=$2
		WHERE id=$1 AND state='working' AND lease_token=$3`, requestID, now.UTC(), token)
	if err != nil || result.RowsAffected() != 1 {
		return false, mediaIndexPersistenceError(ctx)
	}
	if err := completeLegacyPendingIndexSubscription(ctx, tx, batchID); err != nil {
		return false, err
	}
	if err := unblockOrganizationNotifications(ctx, tx, batchID, now.UTC()); err != nil {
		return false, err
	}
	if err := tx.Commit(ctx); err != nil {
		return false, mediaIndexPersistenceError(ctx)
	}
	return true, nil
}

func unblockOrganizationNotifications(ctx context.Context, tx pgx.Tx, batchID uuid.UUID, now time.Time) error {
	if _, err := tx.Exec(ctx, `
		UPDATE notification_events
		SET published_at=COALESCE(published_at, $2)
		WHERE batch_id=$1 AND published_at IS NULL`, batchID, now); err != nil {
		return organizationPersistenceError(ctx)
	}
	if _, err := tx.Exec(ctx, `
		UPDATE notifications
		SET state='pending', next_attempt_at=$2, last_error='', sent_at=NULL,
			lease_token=NULL, lease_expires_at=NULL, updated_at=$2
		WHERE batch_id=$1 AND state='blocked'`, batchID, now); err != nil {
		return organizationPersistenceError(ctx)
	}
	return nil
}

// RecoverExpired 将过期工作租约释放为可重试状态；下一次 Claim 会递增尝试次数。
func (s *Store) RecoverExpired(ctx context.Context, now time.Time, limit int) (int64, error) {
	if err := validateMediaIndexCall(s, ctx); err != nil {
		return 0, err
	}
	if now.IsZero() || limit < 1 || limit > maxMediaIndexClaimSize {
		return 0, mediaindex.ErrInvalidRequest
	}
	result, err := s.db.Pool.Exec(ctx, `
		WITH candidates AS (
			SELECT id
			FROM media_index_requests
			WHERE state='working' AND lease_expires_at <= $1
			ORDER BY lease_expires_at, id
			FOR UPDATE SKIP LOCKED
			LIMIT $2
		)
		UPDATE media_index_requests AS r
		SET state='failed', last_error='lease_expired', next_attempt_at=$1,
			lease_token=NULL, lease_expires_at=NULL, updated_at=$1
		FROM candidates
		WHERE r.id=candidates.id`, now.UTC(), limit)
	if err != nil {
		return 0, mediaIndexPersistenceError(ctx)
	}
	return result.RowsAffected(), nil
}

// ListDue 返回当前可入队的请求 ID，不改变其状态。
func (s *Store) ListDue(ctx context.Context, now time.Time, limit int) ([]uuid.UUID, error) {
	if err := validateMediaIndexCall(s, ctx); err != nil {
		return nil, err
	}
	if now.IsZero() || limit < 1 || limit > maxMediaIndexClaimSize {
		return nil, mediaindex.ErrInvalidRequest
	}
	rows, err := s.db.Pool.Query(ctx, `
		SELECT r.id
		FROM media_index_requests AS r
		WHERE r.state IN ('pending', 'failed') AND r.next_attempt_at <= $1
		  AND NOT EXISTS (
			SELECT 1 FROM scrape_requests AS scrape
			WHERE scrape.batch_id=r.batch_id
			  AND (
				scrape.state IN ('pending', 'working')
				OR (scrape.state='failed' AND scrape.attempt_count < 5)
			  )
		  )
		ORDER BY r.next_attempt_at, r.id
		LIMIT $2`, now.UTC(), limit)
	if err != nil {
		return nil, mediaIndexPersistenceError(ctx)
	}
	defer rows.Close()
	ids := make([]uuid.UUID, 0, limit)
	for rows.Next() {
		var id uuid.UUID
		if err := rows.Scan(&id); err != nil {
			return nil, mediaIndexPersistenceError(ctx)
		}
		ids = append(ids, id)
	}
	if err := rows.Err(); err != nil {
		return nil, mediaIndexPersistenceError(ctx)
	}
	return ids, nil
}

func validateMediaIndexCall(s *Store, ctx context.Context) error {
	if ctx == nil || s == nil || s.db == nil || s.db.Pool == nil {
		return mediaindex.ErrInvalidRequest
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	return nil
}

func validMediaIndexLease(duration time.Duration) bool {
	return duration >= minMediaIndexLease && duration <= maxMediaIndexLease
}

func normalizeMediaIndexError(value string) string {
	value = strings.TrimSpace(value)
	if len(value) > maxMediaIndexError {
		value = value[:maxMediaIndexError]
	}
	return value
}

func mediaIndexPersistenceError(ctx context.Context) error {
	if ctx != nil {
		if err := ctx.Err(); err != nil {
			return err
		}
	}
	return mediaindex.ErrPersistence
}
