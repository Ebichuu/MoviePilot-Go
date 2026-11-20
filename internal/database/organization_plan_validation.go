package database

import (
	"encoding/json"
	"path/filepath"
	"strings"

	"github.com/google/uuid"

	"github.com/moviepilot-go/moviepilot-go/internal/organizer"
)

func normalizeOrganizationPlan(
	username string,
	intentID uuid.UUID,
	sourceRoot string,
	plan organizer.Plan,
) (string, string, organizer.Plan, []byte, uuid.UUID, error) {
	username = strings.TrimSpace(username)
	if username == "" || len(username) > maxOrganizationOwnerLength || intentID == uuid.Nil || sourceRoot == "" {
		return "", "", organizer.Plan{}, nil, uuid.Nil, ErrInvalidOrganizationInput
	}
	root, err := filepath.Abs(sourceRoot)
	if err != nil || len(root) > maxOrganizationPathLength || strings.IndexByte(root, 0) >= 0 {
		return "", "", organizer.Plan{}, nil, uuid.Nil, ErrInvalidOrganizationInput
	}
	canonical, err := organizer.BuildPlan(plan.TargetRoot, plan.Operations)
	if err != nil || plan.ID == "" || plan.ID != canonical.ID || len(canonical.Operations) > maxOrganizationPlanFiles || len(canonical.TargetRoot) > maxOrganizationPathLength {
		return "", "", organizer.Plan{}, nil, uuid.Nil, ErrInvalidOrganizationInput
	}
	seenTargets := make(map[string]struct{}, len(canonical.Operations))
	for _, operation := range canonical.Operations {
		if len(operation.Source) > maxOrganizationPathLength || len(operation.Target) > maxOrganizationPathLength ||
			strings.IndexByte(operation.Source, 0) >= 0 || strings.IndexByte(operation.Target, 0) >= 0 ||
			!organizationPathWithinRoot(root, operation.Source) {
			return "", "", organizer.Plan{}, nil, uuid.Nil, ErrInvalidOrganizationInput
		}
		if _, exists := seenTargets[operation.Target]; exists {
			return "", "", organizer.Plan{}, nil, uuid.Nil, ErrInvalidOrganizationInput
		}
		seenTargets[operation.Target] = struct{}{}
	}
	encoded, err := json.Marshal(canonical.Operations)
	if err != nil || len(encoded) > maxOrganizationPlanBytes {
		return "", "", organizer.Plan{}, nil, uuid.Nil, ErrInvalidOrganizationInput
	}
	batchID := uuid.NewSHA1(intentID, []byte(canonical.ID))
	return username, root, canonical, encoded, batchID, nil
}

func organizationPathWithinRoot(root, path string) bool {
	relative, err := filepath.Rel(root, path)
	return err == nil && relative != ".." && !strings.HasPrefix(relative, ".."+string(filepath.Separator))
}

func sameOrganizationPlan(batch OrganizationBatch, sourceRoot string, plan organizer.Plan) bool {
	if batch.SourceRoot != sourceRoot || batch.Plan.ID != plan.ID || batch.Plan.TargetRoot != plan.TargetRoot || len(batch.Plan.Operations) != len(plan.Operations) {
		return false
	}
	for index := range plan.Operations {
		if batch.Plan.Operations[index] != plan.Operations[index] {
			return false
		}
	}
	return true
}
