package database

import (
	"context"
	"strings"

	"github.com/google/uuid"

	"github.com/moviepilot-go/moviepilot-go/internal/organization"
)

// ListOrganizationFileOutcomes returns the complete terminal projection for
// one owned organization batch. Persistence states are translated here so
// organizationflow never depends on database state names.
func (s *Store) ListOrganizationFileOutcomes(
	ctx context.Context,
	username string,
	batchID uuid.UUID,
) ([]organization.FileOutcome, error) {
	username = strings.TrimSpace(username)
	if username == "" || len(username) > maxOrganizationOwnerLength || batchID == uuid.Nil {
		return nil, ErrInvalidOrganizationInput
	}
	if err := validateOrganizationStore(s, ctx); err != nil {
		return nil, err
	}
	rows, err := s.db.Pool.Query(ctx, `
		SELECT f.ordinal, f.state, COALESCE(f.last_error, '')
		FROM organization_files AS f
		JOIN organization_batches AS b ON b.id=f.batch_id
		JOIN download_intents AS di ON di.id=b.intent_id
		WHERE f.batch_id=$1 AND di.username=$2
		ORDER BY f.ordinal, f.id`, batchID, username)
	if err != nil {
		return nil, organizationPersistenceError(ctx)
	}
	defer rows.Close()

	result := make([]organization.FileOutcome, 0)
	seen := make(map[int]struct{})
	for rows.Next() {
		var (
			ordinal int
			state   OrganizationFileState
			errText string
		)
		if err := rows.Scan(&ordinal, &state, &errText); err != nil {
			return nil, organizationPersistenceError(ctx)
		}
		if _, exists := seen[ordinal]; exists || ordinal < 0 {
			return nil, ErrOrganizationPersistence
		}
		seen[ordinal] = struct{}{}
		var outcome organization.FileOutcome
		outcome.Ordinal = ordinal
		switch state {
		case OrganizationFileCompleted:
			if errText != "" {
				return nil, ErrOrganizationPersistence
			}
			outcome.State = organization.FileOutcomeCompleted
		case OrganizationFileTerminalFailed:
			if errText == "" {
				return nil, ErrOrganizationPersistence
			}
			outcome.State = organization.FileOutcomeTerminalFailed
			outcome.Error = errText
		default:
			return nil, ErrOrganizationFilesIncomplete
		}
		result = append(result, outcome)
		if len(result) > maxOrganizationPlanFiles {
			return nil, ErrOrganizationPersistence
		}
	}
	if err := rows.Err(); err != nil {
		return nil, organizationPersistenceError(ctx)
	}
	if len(result) == 0 {
		return nil, ErrOrganizationFilesIncomplete
	}
	return result, nil
}
