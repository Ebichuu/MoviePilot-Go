package database

import (
	"context"
	"errors"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/moviepilot-go/moviepilot-go/internal/organization"
	"github.com/moviepilot-go/moviepilot-go/internal/organizer"
)

// CreateOrganizationBatch 原子创建下载意图的不可变整理计划及文件清单。
// 同一意图重放相同计划会返回已有批次，不同计划返回冲突。
func (s *Store) CreateOrganizationBatch(
	ctx context.Context,
	username string,
	intentID uuid.UUID,
	sourceRoot string,
	plan organizer.Plan,
	snapshots []organization.SuccessSnapshot,
) (result OrganizationBatch, returnErr error) {
	username, sourceRoot, plan, encoded, batchID, err := normalizeOrganizationPlan(username, intentID, sourceRoot, plan)
	if err != nil {
		return OrganizationBatch{}, err
	}
	snapshotOwner, snapshots, _, err := normalizeOrganizationSuccessSnapshots(username, snapshots)
	if err != nil || snapshotOwner != username || snapshots[0].BatchID != batchID ||
		snapshots[0].IntentID != intentID || !organizationSnapshotsMatchPlan(snapshots, plan) {
		return OrganizationBatch{}, ErrInvalidOrganizationSuccessSnapshot
	}
	if err := validateOrganizationStore(s, ctx); err != nil {
		return OrganizationBatch{}, err
	}

	tx, err := s.db.Pool.Begin(ctx)
	if err != nil {
		return OrganizationBatch{}, organizationPersistenceError(ctx)
	}
	defer func() {
		rollbackErr := tx.Rollback(ctx)
		if rollbackErr != nil && !errors.Is(rollbackErr, pgx.ErrTxClosed) && returnErr == nil {
			returnErr = organizationPersistenceError(ctx)
		}
	}()

	result, err = scanOrganizationBatch(tx.QueryRow(
		ctx,
		createOrganizationBatchSQL,
		batchID,
		intentID,
		username,
		sourceRoot,
		plan.TargetRoot,
		encoded,
	))
	if errors.Is(err, pgx.ErrNoRows) {
		existing, getErr := scanOrganizationBatch(tx.QueryRow(ctx, getOrganizationBatchByIntentSQL, intentID, username))
		if errors.Is(getErr, pgx.ErrNoRows) {
			return OrganizationBatch{}, ErrOrganizationBatchNotFound
		}
		if getErr != nil {
			return OrganizationBatch{}, organizationPersistenceError(ctx)
		}
		if !sameOrganizationPlan(existing, sourceRoot, plan) {
			return OrganizationBatch{}, ErrOrganizationPlanConflict
		}
		if _, err := s.CreateOrganizationSuccessSnapshotsTx(ctx, tx, username, snapshots); err != nil {
			return OrganizationBatch{}, err
		}
		if err := tx.Commit(ctx); err != nil {
			return OrganizationBatch{}, organizationPersistenceError(ctx)
		}
		return existing, nil
	}
	if err != nil {
		return OrganizationBatch{}, organizationPersistenceError(ctx)
	}

	ordinals := make([]int32, len(plan.Operations))
	sources := make([]string, len(plan.Operations))
	targets := make([]string, len(plan.Operations))
	modes := make([]string, len(plan.Operations))
	expectedSizes := make([]int64, len(plan.Operations))
	expectedSHA256s := make([]string, len(plan.Operations))
	for index, operation := range plan.Operations {
		ordinals[index] = int32(index)
		sources[index] = operation.Source
		targets[index] = operation.Target
		modes[index] = string(operation.Mode)
		expectedSizes[index] = operation.ExpectedSize
		expectedSHA256s[index] = operation.ExpectedSHA256
	}
	command, err := tx.Exec(ctx, `
		INSERT INTO organization_files (
			batch_id, ordinal, source_path, target_path, mode,
			expected_size, expected_sha256, state, last_error
		)
		SELECT $1, file.ordinal, file.source_path, file.target_path, file.mode,
			file.expected_size, file.expected_sha256, 'planned', ''
		FROM unnest(
			$2::integer[], $3::text[], $4::text[], $5::text[], $6::bigint[], $7::text[]
		) AS file(ordinal, source_path, target_path, mode, expected_size, expected_sha256)`,
		batchID, ordinals, sources, targets, modes, expectedSizes, expectedSHA256s)
	if err != nil || command.RowsAffected() != int64(len(plan.Operations)) {
		return OrganizationBatch{}, organizationPersistenceError(ctx)
	}
	if _, err := s.CreateOrganizationSuccessSnapshotsTx(ctx, tx, username, snapshots); err != nil {
		return OrganizationBatch{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return OrganizationBatch{}, organizationPersistenceError(ctx)
	}
	return result, nil
}

func organizationSnapshotsMatchPlan(snapshots []organization.SuccessSnapshot, plan organizer.Plan) bool {
	seen := make([]bool, len(plan.Operations))
	for _, snapshot := range snapshots {
		for _, file := range snapshot.Files {
			if file.Ordinal < 0 || file.Ordinal >= len(plan.Operations) || seen[file.Ordinal] ||
				file.SizeBytes != plan.Operations[file.Ordinal].ExpectedSize {
				return false
			}
			seen[file.Ordinal] = true
		}
	}
	for _, found := range seen {
		if !found {
			return false
		}
	}
	return true
}

// GetOrganizationBatch 按 owner 和批次 ID 读取计划，不泄露其他用户记录。
func (s *Store) GetOrganizationBatch(ctx context.Context, username string, batchID uuid.UUID) (OrganizationBatch, error) {
	username = strings.TrimSpace(username)
	if username == "" || len(username) > maxOrganizationOwnerLength || batchID == uuid.Nil {
		return OrganizationBatch{}, ErrInvalidOrganizationInput
	}
	if err := validateOrganizationStore(s, ctx); err != nil {
		return OrganizationBatch{}, err
	}
	batch, err := scanOrganizationBatch(s.db.Pool.QueryRow(ctx, getOrganizationBatchByIDSQL, batchID, username))
	if err == nil {
		return batch, nil
	}
	if errors.Is(err, pgx.ErrNoRows) {
		return OrganizationBatch{}, ErrOrganizationBatchNotFound
	}
	return OrganizationBatch{}, organizationPersistenceError(ctx)
}

// GetOrganizationBatchByIntent 按 owner 和下载意图读取已持久化的不可变批次。
// 该方法用于批次创建成功但任务入队失败后的安全重投。
func (s *Store) GetOrganizationBatchByIntent(ctx context.Context, username string, intentID uuid.UUID) (OrganizationBatch, error) {
	username = strings.TrimSpace(username)
	if username == "" || len(username) > maxOrganizationOwnerLength || intentID == uuid.Nil {
		return OrganizationBatch{}, ErrInvalidOrganizationInput
	}
	if err := validateOrganizationStore(s, ctx); err != nil {
		return OrganizationBatch{}, err
	}
	batch, err := scanOrganizationBatch(s.db.Pool.QueryRow(ctx, getOrganizationBatchByIntentSQL, intentID, username))
	if err == nil {
		return batch, nil
	}
	if errors.Is(err, pgx.ErrNoRows) {
		return OrganizationBatch{}, ErrOrganizationBatchNotFound
	}
	return OrganizationBatch{}, organizationPersistenceError(ctx)
}

// ListDueOrganizationBatchIDs 只读枚举可重新入队的批次 ID，不领取或修改租约。
func (s *Store) ListDueOrganizationBatchIDs(ctx context.Context, now time.Time, limit int) ([]uuid.UUID, error) {
	if now.IsZero() || limit < 1 || limit > maxOrganizationClaimSize {
		return nil, ErrInvalidOrganizationInput
	}
	if err := validateOrganizationStore(s, ctx); err != nil {
		return nil, err
	}
	rows, err := s.db.Pool.Query(ctx, listDueOrganizationBatchIDsSQL, now.UTC(), limit, maxOrganizationAttempts)
	if err != nil {
		return nil, organizationPersistenceError(ctx)
	}
	defer rows.Close()
	ids := make([]uuid.UUID, 0, limit)
	for rows.Next() {
		var id uuid.UUID
		if err := rows.Scan(&id); err != nil || id == uuid.Nil {
			return nil, organizationPersistenceError(ctx)
		}
		ids = append(ids, id)
	}
	if err := rows.Err(); err != nil {
		return nil, organizationPersistenceError(ctx)
	}
	return ids, nil
}

// ClaimOrganizationBatches 使用 SKIP LOCKED 有界领取到期批次，并原子把关联
// 逐集和父订阅从 pending_organization 推进到 organizing。
func (s *Store) ClaimOrganizationBatches(
	ctx context.Context,
	now time.Time,
	limit int,
	leaseDuration time.Duration,
) ([]OrganizationBatch, error) {
	if now.IsZero() || limit < 1 || limit > maxOrganizationClaimSize || leaseDuration < minOrganizationLease || leaseDuration > maxOrganizationLease {
		return nil, ErrInvalidOrganizationInput
	}
	if err := validateOrganizationStore(s, ctx); err != nil {
		return nil, err
	}
	token := uuid.New()
	now = now.UTC()
	rows, err := s.db.Pool.Query(ctx, claimOrganizationBatchesSQL, now, limit, token, now.Add(leaseDuration), maxOrganizationAttempts)
	if err != nil {
		return nil, organizationPersistenceError(ctx)
	}
	defer rows.Close()
	items := make([]OrganizationBatch, 0, limit)
	for rows.Next() {
		batch, scanErr := scanOrganizationBatch(rows)
		if scanErr != nil {
			return nil, organizationPersistenceError(ctx)
		}
		items = append(items, batch)
	}
	if err := rows.Err(); err != nil {
		return nil, organizationPersistenceError(ctx)
	}
	return items, nil
}

// ClaimOrganizationBatch 仅领取指定批次，供按 BatchID 唯一投递的任务使用。
// 已完成、尚未到期、已有活动租约或不满足关联状态的批次返回 false、nil。
// 过期 organizing 批次会在同一语句中回收执行中文件并签发新租约。
func (s *Store) ClaimOrganizationBatch(
	ctx context.Context,
	batchID uuid.UUID,
	now time.Time,
	leaseDuration time.Duration,
) (OrganizationBatch, bool, error) {
	if batchID == uuid.Nil || now.IsZero() || leaseDuration < minOrganizationLease || leaseDuration > maxOrganizationLease {
		return OrganizationBatch{}, false, ErrInvalidOrganizationInput
	}
	if err := validateOrganizationStore(s, ctx); err != nil {
		return OrganizationBatch{}, false, err
	}
	now = now.UTC()
	token := uuid.New()
	batch, err := scanOrganizationBatch(s.db.Pool.QueryRow(
		ctx,
		claimOrganizationBatchSQL,
		batchID,
		now,
		token,
		now.Add(leaseDuration),
		maxOrganizationAttempts,
	))
	if err == nil {
		return batch, true, nil
	}
	if errors.Is(err, pgx.ErrNoRows) {
		return OrganizationBatch{}, false, nil
	}
	return OrganizationBatch{}, false, organizationPersistenceError(ctx)
}

// RenewOrganizationBatchLease 使用批次和 token 做 CAS 延长有效租约。
func (s *Store) RenewOrganizationBatchLease(
	ctx context.Context,
	batchID uuid.UUID,
	token uuid.UUID,
	now time.Time,
	leaseDuration time.Duration,
) error {
	if batchID == uuid.Nil || token == uuid.Nil || now.IsZero() || leaseDuration < minOrganizationLease || leaseDuration > maxOrganizationLease {
		return ErrInvalidOrganizationInput
	}
	if err := validateOrganizationStore(s, ctx); err != nil {
		return err
	}
	now = now.UTC()
	command, err := s.db.Pool.Exec(ctx, `
		UPDATE organization_batches
		SET lease_expires_at=$4, updated_at=$3
		WHERE id=$1 AND state='organizing' AND lease_token=$2
			AND lease_expires_at > $3`, batchID, token, now, now.Add(leaseDuration))
	if err != nil {
		return organizationPersistenceError(ctx)
	}
	if command.RowsAffected() == 1 {
		return nil
	}
	return organizationLeaseFailure(ctx, s, batchID)
}
