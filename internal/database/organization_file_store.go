package database

import (
	"context"
	"strings"

	"github.com/google/uuid"
)

// ListOrganizationFiles 按主键游标有界读取 owner 的不可变文件计划。
func (s *Store) ListOrganizationFiles(
	ctx context.Context,
	username string,
	batchID uuid.UUID,
	afterID int64,
	limit int,
) ([]OrganizationFile, error) {
	username = strings.TrimSpace(username)
	if username == "" || len(username) > maxOrganizationOwnerLength || batchID == uuid.Nil || afterID < 0 || limit < 1 || limit > maxOrganizationPageSize {
		return nil, ErrInvalidOrganizationInput
	}
	if err := validateOrganizationStore(s, ctx); err != nil {
		return nil, err
	}
	rows, err := s.db.Pool.Query(ctx, `
		SELECT `+organizationFileColumns+`
		FROM organization_files AS f
		JOIN organization_batches AS b ON b.id=f.batch_id
		JOIN download_intents AS di ON di.id=b.intent_id
		WHERE f.batch_id=$1 AND di.username=$2 AND f.id>$3
		ORDER BY f.id
		LIMIT $4`, batchID, username, afterID, limit)
	if err != nil {
		return nil, organizationPersistenceError(ctx)
	}
	defer rows.Close()
	items := make([]OrganizationFile, 0, limit)
	for rows.Next() {
		file, scanErr := scanOrganizationFile(rows)
		if scanErr != nil {
			return nil, organizationPersistenceError(ctx)
		}
		items = append(items, file)
	}
	if err := rows.Err(); err != nil {
		return nil, organizationPersistenceError(ctx)
	}
	return items, nil
}

// ClaimOrganizationFiles 在有效批次租约内有界领取尚未完成的文件。
// 同一租约内刚失败的文件不会立即自旋重试；它会等待下一次批次领取。
func (s *Store) ClaimOrganizationFiles(
	ctx context.Context,
	batchID uuid.UUID,
	token uuid.UUID,
	limit int,
) ([]OrganizationFile, error) {
	if batchID == uuid.Nil || token == uuid.Nil || limit < 1 || limit > maxOrganizationPageSize {
		return nil, ErrInvalidOrganizationInput
	}
	if err := validateOrganizationStore(s, ctx); err != nil {
		return nil, err
	}
	rows, err := s.db.Pool.Query(ctx, `
		WITH batch AS MATERIALIZED (
			SELECT id, updated_at
			FROM organization_batches
			WHERE id=$1 AND state='organizing' AND lease_token=$2
				AND lease_expires_at > now()
			FOR UPDATE
		), candidates AS MATERIALIZED (
			SELECT f.id
			FROM organization_files AS f
			JOIN batch ON batch.id=f.batch_id
			WHERE f.attempt_count < $4
				AND (
					f.state='planned'
					OR (f.state='failed' AND f.updated_at <= batch.updated_at)
				)
			ORDER BY f.ordinal, f.id
			FOR UPDATE OF f SKIP LOCKED
			LIMIT $3
		), claimed AS (
			UPDATE organization_files AS f
			SET state='organizing', attempt_count=f.attempt_count+1,
				last_error='', updated_at=now()
			FROM candidates
			WHERE f.id=candidates.id
			RETURNING f.*
		)
		SELECT claimed.id, claimed.batch_id, claimed.ordinal, claimed.source_path,
			claimed.target_path, claimed.mode, claimed.expected_size,
			claimed.expected_sha256, claimed.state, claimed.attempt_count,
			claimed.last_error, claimed.completed_at, claimed.updated_at
		FROM claimed
		ORDER BY claimed.ordinal, claimed.id`, batchID, token, limit, maxOrganizationAttempts)
	if err != nil {
		return nil, organizationPersistenceError(ctx)
	}
	defer rows.Close()
	items := make([]OrganizationFile, 0, limit)
	for rows.Next() {
		file, scanErr := scanOrganizationFile(rows)
		if scanErr != nil {
			return nil, organizationPersistenceError(ctx)
		}
		items = append(items, file)
	}
	if err := rows.Err(); err != nil {
		return nil, organizationPersistenceError(ctx)
	}
	if len(items) == 0 {
		active, leaseErr := organizationLeaseActive(ctx, s, batchID, token)
		if leaseErr != nil {
			return nil, leaseErr
		}
		if !active {
			if err := organizationLeaseFailure(ctx, s, batchID); err != nil {
				return nil, err
			}
			return nil, ErrOrganizationLeaseConflict
		}
		if err := ctx.Err(); err != nil {
			return nil, err
		}
	}
	return items, nil
}

// CompleteOrganizationFile 幂等提交单文件成功；过期 token 不能覆盖新 worker。
func (s *Store) CompleteOrganizationFile(ctx context.Context, batchID, token uuid.UUID, fileID int64) error {
	if batchID == uuid.Nil || token == uuid.Nil || fileID < 1 {
		return ErrInvalidOrganizationInput
	}
	if err := validateOrganizationStore(s, ctx); err != nil {
		return err
	}
	command, err := s.db.Pool.Exec(ctx, `
		UPDATE organization_files AS f
		SET state='completed', last_error='', completed_at=COALESCE(f.completed_at, now()), updated_at=now()
		FROM organization_batches AS b
		WHERE f.id=$3 AND f.batch_id=$1 AND b.id=f.batch_id
			AND b.state='organizing' AND b.lease_token=$2
			AND b.lease_expires_at > now() AND f.state='organizing'`, batchID, token, fileID)
	if err != nil {
		return organizationPersistenceError(ctx)
	}
	if command.RowsAffected() == 1 {
		return nil
	}
	return organizationFileCASFailure(ctx, s, batchID, token, fileID, OrganizationFileCompleted, "")
}

// FailOrganizationFile 幂等提交稳定、受长度限制的单文件失败。
func (s *Store) FailOrganizationFile(ctx context.Context, batchID, token uuid.UUID, fileID int64, stableErr string) error {
	if batchID == uuid.Nil || token == uuid.Nil || fileID < 1 {
		return ErrInvalidOrganizationInput
	}
	stableErr = sanitizeOrganizationError(stableErr)
	if err := validateOrganizationStore(s, ctx); err != nil {
		return err
	}
	command, err := s.db.Pool.Exec(ctx, `
		UPDATE organization_files AS f
		SET state='failed', last_error=$4, completed_at=NULL, updated_at=now()
		FROM organization_batches AS b
		WHERE f.id=$3 AND f.batch_id=$1 AND b.id=f.batch_id
			AND b.state='organizing' AND b.lease_token=$2
			AND b.lease_expires_at > now() AND f.state='organizing'`, batchID, token, fileID, stableErr)
	if err != nil {
		return organizationPersistenceError(ctx)
	}
	if command.RowsAffected() == 1 {
		return nil
	}
	return organizationFileCASFailure(ctx, s, batchID, token, fileID, OrganizationFileFailed, stableErr)
}

// FinalizeOrganizationFileFailure 幂等提交单文件终态失败；终态文件不会再次领取。
func (s *Store) FinalizeOrganizationFileFailure(
	ctx context.Context,
	batchID uuid.UUID,
	token uuid.UUID,
	fileID int64,
	stableErr string,
) error {
	if batchID == uuid.Nil || token == uuid.Nil || fileID < 1 {
		return ErrInvalidOrganizationInput
	}
	stableErr = sanitizeOrganizationError(stableErr)
	if err := validateOrganizationStore(s, ctx); err != nil {
		return err
	}
	command, err := s.db.Pool.Exec(ctx, `
		UPDATE organization_files AS f
		SET state='terminal_failed', last_error=$4,
			completed_at=COALESCE(f.completed_at, now()), updated_at=now()
		FROM organization_batches AS b
		WHERE f.id=$3 AND f.batch_id=$1 AND b.id=f.batch_id
			AND b.state='organizing' AND b.lease_token=$2
			AND b.lease_expires_at > now() AND f.state='organizing'`, batchID, token, fileID, stableErr)
	if err != nil {
		return organizationPersistenceError(ctx)
	}
	if command.RowsAffected() == 1 {
		return nil
	}
	return organizationFileCASFailure(
		ctx, s, batchID, token, fileID, OrganizationFileTerminalFailed, stableErr,
	)
}
