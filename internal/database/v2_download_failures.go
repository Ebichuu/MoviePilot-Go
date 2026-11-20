package database

import (
	"context"
	"errors"
	"regexp"
	"strings"
	"time"

	"github.com/moviepilot-go/moviepilot-go/internal/subscription"
)

var (
	// ErrInvalidV2DownloadFailure 表示调用方传入了不能由 V2 生成的冷却事实。
	ErrInvalidV2DownloadFailure = errors.New("V2 下载失败冷却事实无效")
	// ErrV2DownloadFailurePersistence 隐藏数据库结构和候选敏感信息。
	ErrV2DownloadFailurePersistence = errors.New("V2 下载失败冷却持久化失败")
	v2DownloadFailureFingerprintRE  = regexp.MustCompile(`^[0-9a-f]{64}$`)
)

// ListActiveV2DownloadFailureFingerprints 返回 V2 当前仍处于冷却期的全局资源指纹。
func (s *Store) ListActiveV2DownloadFailureFingerprints(
	ctx context.Context,
	fingerprints []string,
	now time.Time,
) (map[string]struct{}, error) {
	normalized := normalizeV2DownloadFailureFingerprints(fingerprints)
	if len(normalized) == 0 {
		return map[string]struct{}{}, nil
	}
	if s == nil || s.db == nil || s.db.Pool == nil {
		return nil, ErrV2DownloadFailurePersistence
	}
	rows, err := s.db.Pool.Query(ctx, `
		SELECT fingerprint
		FROM download_failures
		WHERE fingerprint = ANY($1::text[])
		  AND next_retry_at > $2`, normalized, now.Truncate(time.Second))
	if err != nil {
		return nil, v2DownloadFailurePersistenceError(ctx)
	}
	defer rows.Close()
	active := make(map[string]struct{}, len(normalized))
	for rows.Next() {
		var fingerprint string
		if err := rows.Scan(&fingerprint); err != nil {
			return nil, v2DownloadFailurePersistenceError(ctx)
		}
		active[strings.TrimSpace(fingerprint)] = struct{}{}
	}
	if err := rows.Err(); err != nil {
		return nil, v2DownloadFailurePersistenceError(ctx)
	}
	return active, nil
}

// RecordV2DownloadFailure 原子新增或刷新一条 V2 资源失败冷却记录。
func (s *Store) RecordV2DownloadFailure(
	ctx context.Context,
	failure subscription.V2DownloadFailure,
	failedAt time.Time,
	retryAfter time.Duration,
) error {
	failure.Fingerprint = strings.ToLower(strings.TrimSpace(failure.Fingerprint))
	if !validV2DownloadFailure(failure, failedAt, retryAfter) {
		return ErrInvalidV2DownloadFailure
	}
	if s == nil || s.db == nil || s.db.Pool == nil {
		return ErrV2DownloadFailurePersistence
	}
	failedAt = failedAt.Truncate(time.Second)
	_, err := s.db.Pool.Exec(ctx, `
		INSERT INTO download_failures (
			fingerprint, media_type, title, year, media_source, media_id,
			seasons, episodes, site, site_name, torrent_id, torrent_name,
			torrent_size, downloader, source, error_message, retry_count,
			first_failed_at, last_failed_at, next_retry_at
		) VALUES (
			$1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15,$16,1,$17,$17,$18
		)
		ON CONFLICT (fingerprint) DO UPDATE SET
			media_type=EXCLUDED.media_type,
			title=EXCLUDED.title,
			year=EXCLUDED.year,
			media_source=EXCLUDED.media_source,
			media_id=EXCLUDED.media_id,
			seasons=EXCLUDED.seasons,
			episodes=EXCLUDED.episodes,
			site=EXCLUDED.site,
			site_name=EXCLUDED.site_name,
			torrent_id=EXCLUDED.torrent_id,
			torrent_name=EXCLUDED.torrent_name,
			torrent_size=EXCLUDED.torrent_size,
			downloader=EXCLUDED.downloader,
			source=EXCLUDED.source,
			error_message=EXCLUDED.error_message,
			retry_count=download_failures.retry_count + 1,
			last_failed_at=EXCLUDED.last_failed_at,
			next_retry_at=EXCLUDED.next_retry_at`,
		failure.Fingerprint,
		failure.MediaType,
		failure.Title,
		failure.Year,
		failure.MediaSource,
		failure.MediaID,
		failure.Season,
		failure.Episodes,
		failure.SiteID,
		failure.SiteName,
		failure.ResourceKey,
		failure.TorrentName,
		failure.TorrentSize,
		failure.Downloader,
		failure.Source,
		failure.ErrorMessage,
		failedAt,
		failedAt.Add(retryAfter),
	)
	if err != nil {
		return v2DownloadFailurePersistenceError(ctx)
	}
	return nil
}

func normalizeV2DownloadFailureFingerprints(input []string) []string {
	seen := make(map[string]struct{}, len(input))
	result := make([]string, 0, len(input))
	for _, fingerprint := range input {
		fingerprint = strings.ToLower(strings.TrimSpace(fingerprint))
		if !v2DownloadFailureFingerprintRE.MatchString(fingerprint) {
			continue
		}
		if _, exists := seen[fingerprint]; exists {
			continue
		}
		seen[fingerprint] = struct{}{}
		result = append(result, fingerprint)
	}
	return result
}

func validV2DownloadFailure(
	failure subscription.V2DownloadFailure,
	failedAt time.Time,
	retryAfter time.Duration,
) bool {
	if !v2DownloadFailureFingerprintRE.MatchString(failure.Fingerprint) || failedAt.IsZero() {
		return false
	}
	if retryAfter != subscription.V2DownloadFailureTransientTTL &&
		retryAfter != subscription.V2DownloadFailureResourceTTL {
		return false
	}
	return len([]rune(failure.Source)) <= 1_000 && len([]rune(failure.ErrorMessage)) <= 1_000
}

func v2DownloadFailurePersistenceError(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	return ErrV2DownloadFailurePersistence
}
