package database

import (
	"context"
	"path/filepath"
	"time"

	"github.com/google/uuid"
)

// OrganizationFileRecord 是整理链落库的一条文件记录。
type OrganizationFileRecord struct {
	SourcePath string
	TargetPath string
	State      string
	UpdatedAt  time.Time
}

// ListManualOrganizationHistory 倒序返回最近的整理记录，供「手动整理历史」接口使用。
func (s *Store) ListManualOrganizationHistory(ctx context.Context, limit int) ([]OrganizationFileRecord, error) {
	records := []OrganizationFileRecord{}
	if s == nil || s.db == nil || s.db.Pool == nil {
		return records, nil
	}
	if limit <= 0 || limit > 200 {
		limit = 50
	}
	rows, err := s.db.Pool.Query(ctx, `
		SELECT source_path, target_path, state, updated_at
		FROM organization_files
		ORDER BY id DESC
		LIMIT $1`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var record OrganizationFileRecord
		if scanErr := rows.Scan(&record.SourcePath, &record.TargetPath, &record.State, &record.UpdatedAt); scanErr != nil {
			return nil, scanErr
		}
		records = append(records, record)
	}
	return records, rows.Err()
}

// RecordManualOrganization 把一次手动整理的结果写进整理链（单文件批次）。
func (s *Store) RecordManualOrganization(ctx context.Context, source, target string) error {
	if s == nil || s.db == nil || s.db.Pool == nil {
		return nil
	}
	// organization_files.batch_id 是 organization_batches 的外键，直接写随机 UUID
	// 会违反 batch_id_fkey（历史记录永远落不了库）。手动整理先建一个已完成的
	// 单文件批次，再把文件挂上去。
	tx, err := s.db.Pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(context.Background()) }()
	batchID := uuid.New()
	if _, err := tx.Exec(ctx, `
		INSERT INTO organization_batches (
			id, intent_id, state, source_root, target_root, plan, last_error, completed_at
		) VALUES (
			$1, NULL, 'completed', $2, $3,
			jsonb_build_array(jsonb_build_object(
				'source_path', $4::text, 'target_path', $5::text, 'mode', 'move'
			)),
			'', now()
		)`, batchID, filepath.Dir(source), filepath.Dir(target), source, target); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `
		INSERT INTO organization_files (batch_id, source_path, target_path, state, ordinal, mode, completed_at)
		VALUES ($1, $2, $3, 'completed', 0, 'move', now())`, batchID, source, target); err != nil {
		return err
	}
	return tx.Commit(ctx)
}
