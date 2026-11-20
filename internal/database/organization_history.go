package database

import (
	"context"
	"path/filepath"
	"strings"
	"time"

	"github.com/google/uuid"
)

// OrganizationFileRecord 是整理链落库的一条文件记录。
type OrganizationFileRecord struct {
	SourcePath string
	TargetPath string
	Mode       string
	State      string
	LastError  string
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
		SELECT source_path, target_path, mode, state, last_error, updated_at
		FROM organization_files
		ORDER BY id DESC
		LIMIT $1`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var record OrganizationFileRecord
		if scanErr := rows.Scan(&record.SourcePath, &record.TargetPath, &record.Mode, &record.State, &record.LastError, &record.UpdatedAt); scanErr != nil {
			return nil, scanErr
		}
		records = append(records, record)
	}
	return records, rows.Err()
}

// RecordManualOrganization 把一次手动整理的结果写进整理链（单文件批次，默认 move）。
func (s *Store) RecordManualOrganization(ctx context.Context, source, target string) error {
	return s.RecordManualOrganizationWithMode(ctx, source, target, "move")
}

// RecordManualOrganizationWithMode 把一次手动/目录监控整理成功的结果落库。
func (s *Store) RecordManualOrganizationWithMode(ctx context.Context, source, target, mode string) error {
	if s == nil || s.db == nil || s.db.Pool == nil {
		return nil
	}
	mode = normalizeOrganizationHistoryMode(mode)
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
				'source_path', $4::text, 'target_path', $5::text, 'mode', $6::text
			)),
			'', now()
		)`, batchID, filepath.Dir(source), filepath.Dir(target), source, target, mode); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `
		INSERT INTO organization_files (batch_id, source_path, target_path, state, ordinal, mode, completed_at)
		VALUES ($1, $2, $3, 'completed', 0, $4, now())`, batchID, source, target, mode); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

// RecordFailedOrganization 把一次手动/目录监控整理的失败原因落库到 organization_files，
// 使失败记录可在 Web 端「整理历史 → 失败」中查看与排查。
func (s *Store) RecordFailedOrganization(ctx context.Context, source, target, mode, errMsg string) error {
	if s == nil || s.db == nil || s.db.Pool == nil {
		return nil
	}
	mode = normalizeOrganizationHistoryMode(mode)
	errMsg = strings.TrimSpace(errMsg)
	if errMsg == "" {
		errMsg = "整理失败"
	}
	if len(errMsg) > 2000 {
		errMsg = errMsg[:2000]
	}
	if target == "" {
		target = source
	}
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
			$1, NULL, 'failed', $2, $3,
			jsonb_build_array(jsonb_build_object(
				'source_path', $4::text, 'target_path', $5::text, 'mode', $6::text
			)),
			$7, NULL
		)`, batchID, filepath.Dir(source), filepath.Dir(target), source, target, mode, errMsg); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `
		INSERT INTO organization_files (batch_id, source_path, target_path, state, ordinal, mode, last_error, completed_at)
		VALUES ($1, $2, $3, 'terminal_failed', 0, $4, $5, now())`, batchID, source, target, mode, errMsg); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

func normalizeOrganizationHistoryMode(mode string) string {
	switch strings.ToLower(strings.TrimSpace(mode)) {
	case "copy":
		return "copy"
	case "hardlink", "link":
		return "hardlink"
	case "symlink", "softlink":
		return "symlink"
	default:
		return "move"
	}
}
