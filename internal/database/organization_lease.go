package database

import (
	"context"
	"errors"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

func organizationFileCASFailure(
	ctx context.Context,
	s *Store,
	batchID uuid.UUID,
	token uuid.UUID,
	fileID int64,
	want OrganizationFileState,
	wantError string,
) error {
	var (
		fileState  OrganizationFileState
		lastError  string
		batchState OrganizationBatchState
		leaseToken *uuid.UUID
		leaseEnd   *time.Time
	)
	err := s.db.Pool.QueryRow(ctx, `
		SELECT f.state, f.last_error, b.state, b.lease_token, b.lease_expires_at
		FROM organization_files AS f
		JOIN organization_batches AS b ON b.id=f.batch_id
		WHERE f.id=$1 AND f.batch_id=$2`, fileID, batchID).Scan(&fileState, &lastError, &batchState, &leaseToken, &leaseEnd)
	if errors.Is(err, pgx.ErrNoRows) {
		return ErrOrganizationBatchNotFound
	}
	if err != nil {
		return organizationPersistenceError(ctx)
	}
	if want == OrganizationFileTerminalFailed && fileState == want && lastError != wantError {
		return ErrOrganizationFileStateConflict
	}
	if fileState == want && (want != OrganizationFileFailed && want != OrganizationFileTerminalFailed || lastError == wantError) {
		if batchState == OrganizationBatchCompleted || (batchState == OrganizationBatchOrganizing && leaseToken != nil && *leaseToken == token && leaseEnd != nil && leaseEnd.After(time.Now().UTC())) {
			return nil
		}
	}
	if want == OrganizationFileTerminalFailed && batchState == OrganizationBatchCompleted {
		return ErrOrganizationFileStateConflict
	}
	if batchState != OrganizationBatchOrganizing || leaseToken == nil || *leaseToken != token || leaseEnd == nil || !leaseEnd.After(time.Now().UTC()) {
		return ErrOrganizationLeaseConflict
	}
	return ErrOrganizationFileStateConflict
}

func organizationLeaseFailure(ctx context.Context, s *Store, batchID uuid.UUID) error {
	var exists bool
	if err := s.db.Pool.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM organization_batches WHERE id=$1)`, batchID).Scan(&exists); err != nil {
		return organizationPersistenceError(ctx)
	}
	if !exists {
		return ErrOrganizationBatchNotFound
	}
	return ErrOrganizationLeaseConflict
}

func organizationLeaseActive(ctx context.Context, s *Store, batchID, token uuid.UUID) (bool, error) {
	var active bool
	err := s.db.Pool.QueryRow(ctx, `
		SELECT EXISTS(
			SELECT 1 FROM organization_batches
			WHERE id=$1 AND state='organizing' AND lease_token=$2 AND lease_expires_at > now()
		)`, batchID, token).Scan(&active)
	if err != nil {
		return false, organizationPersistenceError(ctx)
	}
	return active, nil
}
