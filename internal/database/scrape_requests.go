package database

import (
	"context"
	"errors"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/moviepilot-go/moviepilot-go/internal/scrapeflow"
)

const (
	maxScrapeLease = time.Hour
	minScrapeLease = time.Second
)

// ClaimScrape atomically leases one due NFO request.
func (s *Store) ClaimScrape(
	ctx context.Context,
	requestID uuid.UUID,
	now time.Time,
	leaseDuration time.Duration,
) (scrapeflow.Lease, bool, error) {
	if requestID == uuid.Nil || now.IsZero() || leaseDuration < minScrapeLease || leaseDuration > maxScrapeLease {
		return scrapeflow.Lease{}, false, scrapeflow.ErrInvalidPlan
	}
	if err := validateScrapeStore(s, ctx); err != nil {
		return scrapeflow.Lease{}, false, err
	}
	token := uuid.New()
	var lease scrapeflow.Lease
	var expiresAt *time.Time
	err := s.db.Pool.QueryRow(ctx, `
		WITH candidate AS (
			SELECT id
			FROM scrape_requests
			WHERE id=$1
			  AND attempt_count < $2
			  AND (
				(state IN ('pending', 'failed') AND next_attempt_at <= $3)
				OR (state='working' AND lease_expires_at <= $3)
			  )
			FOR UPDATE SKIP LOCKED
		), claimed AS (
			UPDATE scrape_requests AS r
			SET state='working', attempt_count=r.attempt_count+1, last_error='',
				lease_token=$4, lease_expires_at=$5, updated_at=$3
			FROM candidate
			WHERE r.id=candidate.id
			RETURNING r.id, r.kind, r.media_path, r.tmdb_id, r.season, r.episode,
				r.state, r.attempt_count, r.next_attempt_at, r.lease_token, r.lease_expires_at
		)
		SELECT id, kind, media_path, tmdb_id, season, episode, state,
			attempt_count, next_attempt_at, lease_token, lease_expires_at
		FROM claimed`, requestID, scrapeflow.MaxAttempts, now.UTC(), token, now.UTC().Add(leaseDuration)).Scan(
		&lease.Request.ID,
		&lease.Request.Kind,
		&lease.Request.MediaPath,
		&lease.Request.TMDBID,
		&lease.Request.Season,
		&lease.Request.Episode,
		&lease.Request.State,
		&lease.Request.AttemptCount,
		&lease.Request.NextAttempt,
		&lease.Token,
		&expiresAt,
	)
	if errors.Is(err, pgx.ErrNoRows) {
		return scrapeflow.Lease{}, false, nil
	}
	if err != nil || expiresAt == nil || !lease.Valid(requestID) {
		return scrapeflow.Lease{}, false, scrapePersistenceError(ctx)
	}
	return lease, true, nil
}

// CompleteScrape marks one leased NFO request complete.
func (s *Store) CompleteScrape(ctx context.Context, requestID, token uuid.UUID, completedAt time.Time) error {
	if requestID == uuid.Nil || token == uuid.Nil || completedAt.IsZero() {
		return scrapeflow.ErrInvalidPlan
	}
	if err := validateScrapeStore(s, ctx); err != nil {
		return err
	}
	result, err := s.db.Pool.Exec(ctx, `
		UPDATE scrape_requests
		SET state='completed', completed_at=COALESCE(completed_at, $3), last_error='',
			lease_token=NULL, lease_expires_at=NULL, updated_at=$3
		WHERE id=$1 AND state='working' AND lease_token=$2
		  AND lease_expires_at > now()`, requestID, token, completedAt.UTC())
	if err != nil || result.RowsAffected() != 1 {
		return scrapePersistenceError(ctx)
	}
	return nil
}

// RescheduleScrape persists a bounded retry or the terminal failed state.
func (s *Store) RescheduleScrape(
	ctx context.Context,
	requestID, token uuid.UUID,
	retryAt time.Time,
	stableErr string,
) error {
	stableErr = normalizeSideEffectError(stableErr)
	if requestID == uuid.Nil || token == uuid.Nil || retryAt.IsZero() || stableErr == "" {
		return scrapeflow.ErrInvalidPlan
	}
	if err := validateScrapeStore(s, ctx); err != nil {
		return err
	}
	result, err := s.db.Pool.Exec(ctx, `
		UPDATE scrape_requests
		SET state='failed',
			next_attempt_at=$3, last_error=$5, lease_token=NULL,
			lease_expires_at=NULL, updated_at=now()
		WHERE id=$1 AND state='working' AND lease_token=$2
		  AND lease_expires_at > now()
		  AND attempt_count <= $4`, requestID, token, retryAt.UTC(), scrapeflow.MaxAttempts, stableErr)
	if err != nil || result.RowsAffected() != 1 {
		return scrapePersistenceError(ctx)
	}
	return nil
}

// RecoverExpiredScrapes releases a bounded page of expired leases.
func (s *Store) RecoverExpiredScrapes(ctx context.Context, now time.Time, limit int) (int64, error) {
	if now.IsZero() || limit < 1 || limit > scrapeflow.SweepPageSize {
		return 0, scrapeflow.ErrInvalidPlan
	}
	if err := validateScrapeStore(s, ctx); err != nil {
		return 0, err
	}
	result, err := s.db.Pool.Exec(ctx, `
		WITH candidates AS (
			SELECT id FROM scrape_requests
			WHERE state='working' AND lease_expires_at <= $1
			ORDER BY lease_expires_at, id
			FOR UPDATE SKIP LOCKED
			LIMIT $2
		)
		UPDATE scrape_requests AS r
		SET state='failed', next_attempt_at=$1, last_error='lease_expired',
			lease_token=NULL, lease_expires_at=NULL, updated_at=$1
		FROM candidates WHERE r.id=candidates.id`, now.UTC(), limit)
	if err != nil {
		return 0, scrapePersistenceError(ctx)
	}
	return result.RowsAffected(), nil
}

// ListDueScrapes returns one bounded page of retryable request IDs.
func (s *Store) ListDueScrapes(ctx context.Context, now time.Time, limit int) ([]uuid.UUID, error) {
	if now.IsZero() || limit < 1 || limit > scrapeflow.SweepPageSize {
		return nil, scrapeflow.ErrInvalidPlan
	}
	if err := validateScrapeStore(s, ctx); err != nil {
		return nil, err
	}
	rows, err := s.db.Pool.Query(ctx, `
		SELECT id FROM scrape_requests
		WHERE state IN ('pending', 'failed') AND attempt_count < $3
		  AND next_attempt_at <= $1
		ORDER BY next_attempt_at, id
		LIMIT $2`, now.UTC(), limit, scrapeflow.MaxAttempts)
	if err != nil {
		return nil, scrapePersistenceError(ctx)
	}
	defer rows.Close()
	ids := make([]uuid.UUID, 0, limit)
	for rows.Next() {
		var id uuid.UUID
		if err := rows.Scan(&id); err != nil || id == uuid.Nil {
			return nil, scrapePersistenceError(ctx)
		}
		ids = append(ids, id)
	}
	if err := rows.Err(); err != nil {
		return nil, scrapePersistenceError(ctx)
	}
	return ids, nil
}

func validateScrapeStore(s *Store, ctx context.Context) error {
	if ctx == nil || s == nil || s.db == nil || s.db.Pool == nil {
		return scrapeflow.ErrPersistence
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	return nil
}

func scrapePersistenceError(ctx context.Context) error {
	if ctx != nil && ctx.Err() != nil {
		return ctx.Err()
	}
	return scrapeflow.ErrPersistence
}

func normalizeSideEffectError(value string) string {
	value = strings.TrimSpace(value)
	if len(value) > 128 {
		value = value[:128]
	}
	return value
}
