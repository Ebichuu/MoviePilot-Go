package database

import (
	"encoding/json"
	"path/filepath"
	"time"

	"github.com/google/uuid"

	"github.com/moviepilot-go/moviepilot-go/internal/organizer"
)

func scanOrganizationBatch(row rowScanner) (OrganizationBatch, error) {
	var (
		batch        OrganizationBatch
		targetRoot   string
		encoded      []byte
		leaseToken   *uuid.UUID
		leaseExpires *time.Time
	)
	if err := row.Scan(
		&batch.ID,
		&batch.IntentID,
		&batch.State,
		&batch.SourceRoot,
		&targetRoot,
		&encoded,
		&batch.LastError,
		&batch.AttemptCount,
		&batch.NextAttemptAt,
		&leaseToken,
		&leaseExpires,
		&batch.CompletedAt,
		&batch.CreatedAt,
		&batch.UpdatedAt,
	); err != nil {
		return OrganizationBatch{}, err
	}
	var operations []organizer.Operation
	if len(encoded) > maxOrganizationPlanBytes || json.Unmarshal(encoded, &operations) != nil {
		return OrganizationBatch{}, ErrOrganizationPersistence
	}
	plan, err := organizer.BuildPlan(targetRoot, operations)
	if err != nil || batch.ID == uuid.Nil || batch.IntentID == uuid.Nil ||
		len(batch.SourceRoot) == 0 || len(batch.SourceRoot) > maxOrganizationPathLength || !filepath.IsAbs(batch.SourceRoot) ||
		len(targetRoot) == 0 || len(targetRoot) > maxOrganizationPathLength || !filepath.IsAbs(targetRoot) ||
		len(batch.LastError) > maxOrganizationErrorLength || batch.NextAttemptAt.IsZero() ||
		!validOrganizationBatchState(batch.State) || batch.AttemptCount < 0 || batch.AttemptCount > maxOrganizationAttempts ||
		(batch.State == OrganizationBatchOrganizing) != (leaseToken != nil && leaseExpires != nil) ||
		(batch.State == OrganizationBatchCompleted) != (batch.CompletedAt != nil) {
		return OrganizationBatch{}, ErrOrganizationPersistence
	}
	seenTargets := make(map[string]struct{}, len(plan.Operations))
	for _, operation := range plan.Operations {
		if len(operation.Source) == 0 || len(operation.Source) > maxOrganizationPathLength ||
			len(operation.Target) == 0 || len(operation.Target) > maxOrganizationPathLength ||
			!organizationPathWithinRoot(batch.SourceRoot, operation.Source) {
			return OrganizationBatch{}, ErrOrganizationPersistence
		}
		if _, exists := seenTargets[operation.Target]; exists {
			return OrganizationBatch{}, ErrOrganizationPersistence
		}
		seenTargets[operation.Target] = struct{}{}
	}
	batch.Plan = plan
	if leaseToken != nil {
		batch.LeaseToken = *leaseToken
	}
	batch.LeaseExpiresAt = leaseExpires
	return batch, nil
}

func scanOrganizationFile(row rowScanner) (OrganizationFile, error) {
	var file OrganizationFile
	if err := row.Scan(
		&file.ID,
		&file.BatchID,
		&file.Ordinal,
		&file.SourcePath,
		&file.TargetPath,
		&file.Mode,
		&file.ExpectedSize,
		&file.ExpectedSHA256,
		&file.State,
		&file.AttemptCount,
		&file.LastError,
		&file.CompletedAt,
		&file.UpdatedAt,
	); err != nil {
		return OrganizationFile{}, err
	}
	if file.ID < 1 || file.BatchID == uuid.Nil || file.Ordinal < 0 ||
		len(file.SourcePath) == 0 || len(file.SourcePath) > maxOrganizationPathLength || !filepath.IsAbs(file.SourcePath) ||
		len(file.TargetPath) == 0 || len(file.TargetPath) > maxOrganizationPathLength || !filepath.IsAbs(file.TargetPath) ||
		!validOrganizationMode(file.Mode) || file.ExpectedSize < 0 || !validOrganizationSHA256(file.ExpectedSHA256) ||
		!validOrganizationFileState(file.State) || file.AttemptCount < 0 ||
		file.AttemptCount > maxOrganizationAttempts || len(file.LastError) > maxOrganizationErrorLength {
		return OrganizationFile{}, ErrOrganizationPersistence
	}
	terminal := file.State == OrganizationFileCompleted || file.State == OrganizationFileTerminalFailed
	if terminal != (file.CompletedAt != nil) ||
		(file.State == OrganizationFileTerminalFailed && len(file.LastError) == 0) {
		return OrganizationFile{}, ErrOrganizationPersistence
	}
	return file, nil
}
