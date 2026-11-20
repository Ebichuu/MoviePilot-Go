package database

import (
	"context"
	"errors"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/moviepilot-go/moviepilot-go/internal/downloader"
)

const maxDownloadReconciliationPageSize = 200

var (
	// ErrInvalidDownloadReconciliationQuery indicates an invalid snapshot or cursor.
	ErrInvalidDownloadReconciliationQuery = errors.New("下载对账查询无效")
	// ErrDownloadReconciliationQuery indicates a temporary storage failure.
	ErrDownloadReconciliationQuery = errors.New("查询下载对账目标失败")
)

const listDownloadReconciliationTargetsSQL = `
	SELECT di.id, di.username, p.downloader_id, di.state,
		COALESCE(di.torrent_hash, ''), di.updated_at
	FROM download_intents AS di
	JOIN download_intent_plans AS p ON p.intent_id=di.id
	LEFT JOIN subscriptions AS s
		ON s.id=di.subscription_id AND s.username=di.username
	WHERE (
			(
				di.state IN ('submitting', 'downloading')
				AND (di.subscription_id IS NULL OR s.id IS NOT NULL)
			)
			OR (
				di.state='planned'
				AND s.completed_at IS NULL
				AND s.current_download_intent_id=di.id
				AND s.internal_state IN ('acquired', 'pending_identification')
			)
			OR (
				di.state='failed'
				AND s.completed_at IS NULL
				AND s.current_download_intent_id=di.id
				AND s.internal_state IN ('acquired', 'downloading')
			)
			OR (
				di.state='completed'
				AND di.organization_reconciliation_pending
				AND (di.subscription_id IS NULL OR s.id IS NOT NULL)
				AND NOT EXISTS (
					SELECT 1
					FROM organization_batches AS b
					WHERE b.intent_id=di.id
					OFFSET 0
				)
			)
		)
		AND di.updated_at <= $1
		AND (di.updated_at, di.id) > ($2, $3)
	ORDER BY di.updated_at, di.id
	LIMIT $4`

// ListDownloadReconciliationTargets returns one stable keyset page from a
// fixed cutoff. It never loads resource locators, credentials, or plan secrets.
func (s *Store) ListDownloadReconciliationTargets(
	ctx context.Context,
	cutoff time.Time,
	afterUpdatedAt time.Time,
	afterID uuid.UUID,
	limit int,
) ([]downloader.ReconciliationTarget, error) {
	if ctx == nil || cutoff.IsZero() || limit < 1 || limit > maxDownloadReconciliationPageSize ||
		(afterUpdatedAt.IsZero() != (afterID == uuid.Nil)) {
		return nil, ErrInvalidDownloadReconciliationQuery
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if !downloadIntentStoreReady(s) {
		return nil, ErrDownloadReconciliationQuery
	}

	rows, err := s.db.Pool.Query(
		ctx,
		listDownloadReconciliationTargetsSQL,
		cutoff.UTC(),
		afterUpdatedAt.UTC(),
		afterID,
		limit,
	)
	if err != nil {
		return nil, downloadReconciliationQueryError(ctx)
	}
	defer rows.Close()

	targets := make([]downloader.ReconciliationTarget, 0, limit)
	for rows.Next() {
		var target downloader.ReconciliationTarget
		if err := rows.Scan(
			&target.ID,
			&target.Username,
			&target.DownloaderID,
			&target.State,
			&target.TorrentHash,
			&target.UpdatedAt,
		); err != nil {
			return nil, ErrDownloadReconciliationQuery
		}
		normalized, err := normalizeDownloadReconciliationTarget(target)
		if err != nil {
			return nil, ErrDownloadReconciliationQuery
		}
		targets = append(targets, normalized)
	}
	if err := rows.Err(); err != nil {
		return nil, downloadReconciliationQueryError(ctx)
	}
	return targets, nil
}

func normalizeDownloadReconciliationTarget(
	target downloader.ReconciliationTarget,
) (downloader.ReconciliationTarget, error) {
	target.Username = strings.TrimSpace(target.Username)
	downloaderID, err := uuid.Parse(strings.TrimSpace(target.DownloaderID))
	if err != nil || downloaderID == uuid.Nil || target.ID == uuid.Nil || target.Username == "" || target.UpdatedAt.IsZero() {
		return downloader.ReconciliationTarget{}, ErrDownloadReconciliationQuery
	}
	target.DownloaderID = downloaderID.String()
	if target.State != downloader.IntentPlanned && target.State != downloader.IntentSubmitting && target.State != downloader.IntentDownloading &&
		target.State != downloader.IntentFailed &&
		target.State != downloader.IntentCompleted {
		return downloader.ReconciliationTarget{}, ErrDownloadReconciliationQuery
	}
	hash := strings.TrimSpace(target.TorrentHash)
	if target.State == downloader.IntentPlanned {
		if hash != "" {
			return downloader.ReconciliationTarget{}, ErrDownloadReconciliationQuery
		}
		target.TorrentHash = ""
	} else if target.State == downloader.IntentFailed && hash == "" {
		target.TorrentHash = ""
	} else {
		target.TorrentHash, err = downloader.NormalizeTorrentHash(target.TorrentHash)
		if err != nil {
			return downloader.ReconciliationTarget{}, ErrDownloadReconciliationQuery
		}
	}
	target.UpdatedAt = target.UpdatedAt.UTC()
	return target, nil
}

func downloadReconciliationQueryError(ctx context.Context) error {
	if ctx != nil {
		if err := ctx.Err(); err != nil {
			return err
		}
	}
	return ErrDownloadReconciliationQuery
}

const failOrphanDownloadIntentsSQL = `
	UPDATE download_intents AS di
	SET state='failed',
		last_error='下载意图缺少提交计划，无法对账',
		updated_at=now()
	WHERE di.state IN ('submitting', 'downloading')
		AND di.updated_at <= $1
		AND NOT EXISTS (
			SELECT 1 FROM download_intent_plans AS p WHERE p.intent_id=di.id
		)
		AND (
			di.subscription_id IS NULL
			OR EXISTS (
				SELECT 1 FROM subscriptions AS s
				WHERE s.id=di.subscription_id AND s.username=di.username
					AND s.completed_at IS NOT NULL
			)
		)
	RETURNING di.id`

// FailOrphanDownloadIntents 收尾缺少提交计划的遗留意图。
// 这类记录没有可信的下载器 ID，对账会永远跳过，超过保留期直接判失败，
// 避免意图无限停在 submitting/downloading。
func (s *Store) FailOrphanDownloadIntents(ctx context.Context, olderThan time.Time) (int64, error) {
	if ctx == nil || olderThan.IsZero() {
		return 0, ErrInvalidDownloadReconciliationQuery
	}
	if err := ctx.Err(); err != nil {
		return 0, err
	}
	if !downloadIntentStoreReady(s) {
		return 0, ErrDownloadReconciliationQuery
	}
	tag, err := s.db.Pool.Exec(ctx, failOrphanDownloadIntentsSQL, olderThan.UTC())
	if err != nil {
		return 0, downloadReconciliationQueryError(ctx)
	}
	return tag.RowsAffected(), nil
}
