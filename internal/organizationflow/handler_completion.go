package organizationflow

import (
	"context"
	"errors"
	"path/filepath"
	"strings"

	"github.com/google/uuid"

	"github.com/moviepilot-go/moviepilot-go/internal/mediaindex"
	"github.com/moviepilot-go/moviepilot-go/internal/notificationflow"
	"github.com/moviepilot-go/moviepilot-go/internal/organization"
	"github.com/moviepilot-go/moviepilot-go/internal/organizer"
)

func (h *Handler) completeBatch(
	ctx context.Context,
	batch OrganizationBatch,
	completionPlan mediaindex.CompletionPlan,
) (uuid.UUID, []uuid.UUID, []uuid.UUID, error) {
	if h.sideEffectStore == nil {
		requestID, _, err := h.store.CompleteOrganizationBatchForIndexing(ctx, batch.ID, batch.LeaseToken, completionPlan)
		return requestID, nil, nil, err
	}
	if h.scrapePlanner == nil || h.notificationPlanner == nil || h.scrapeEnqueuer == nil || h.notificationEnqueuer == nil {
		return uuid.Nil, nil, nil, ErrInvalidOrganizationHandler
	}
	scrapePlan, err := h.scrapePlanner.Plan(ctx, batch.ID, batch.IntentID, batch.Plan)
	if err != nil {
		return uuid.Nil, nil, nil, ErrOrganizationIndexPlanning
	}
	snapshots, snapshotErr := h.sideEffectStore.ListOrganizationSuccessSnapshots(
		ctx, scrapePlan.Username, batch.ID,
	)
	useLegacyPlan := false
	switch {
	case snapshotErr == nil:
		var snapshotSetValid bool
		useLegacyPlan, snapshotSetValid = classifyOrganizationSuccessSnapshots(
			batch, scrapePlan.Username, snapshots,
		)
		if !snapshotSetValid {
			return uuid.Nil, nil, nil, ErrOrganizationIndexPlanning
		}
	case errors.Is(snapshotErr, ErrOrganizationSuccessSnapshotNotFound):
		successInput, inputErr := h.sideEffectStore.GetDownloadIntentSuccessInput(
			ctx, scrapePlan.Username, batch.IntentID,
		)
		if inputErr != nil || !isExplicitLegacySuccessInput(batch.IntentID, successInput) {
			return uuid.Nil, nil, nil, ErrOrganizationIndexPlanning
		}
		useLegacyPlan = true
	default:
		return uuid.Nil, nil, nil, ErrOrganizationIndexPlanning
	}
	var notificationPlan notificationflow.Plan
	if useLegacyPlan {
		notificationPlan, err = h.notificationPlanner.Plan(batch.ID, scrapePlan.Username, batch.Plan)
	} else {
		notificationPlan, err = h.notificationPlanner.PlanSuccess(
			batch.ID, scrapePlan.Username, snapshots, h.appDomain,
		)
	}
	if err != nil {
		return uuid.Nil, nil, nil, ErrOrganizationIndexPlanning
	}
	requestID, _, scrapeIDs, notificationIDs, err := h.sideEffectStore.CompleteOrganizationBatchWithSideEffects(
		ctx, batch.ID, batch.LeaseToken, completionPlan, scrapePlan, notificationPlan,
	)
	return requestID, scrapeIDs, notificationIDs, err
}
func (h *Handler) completeBatchWithResults(
	ctx context.Context,
	batch OrganizationBatch,
	completionPlan mediaindex.CompletionPlan,
	executionPlan organizer.Plan,
	results []organization.ResultSnapshot,
	failures []organization.FileFailure,
) (uuid.UUID, []uuid.UUID, []uuid.UUID, error) {
	if h.sideEffectStore == nil || h.scrapePlanner == nil || h.notificationPlanner == nil {
		if len(failures) != 0 {
			return uuid.Nil, nil, nil, ErrInvalidOrganizationHandler
		}
		requestID, _, err := h.store.CompleteOrganizationBatchForIndexing(
			ctx, batch.ID, batch.LeaseToken, completionPlan,
		)
		return requestID, nil, nil, err
	}
	identityPlan, err := h.scrapePlanner.Plan(ctx, batch.ID, batch.IntentID, executionPlan)
	if err != nil {
		return uuid.Nil, nil, nil, ErrOrganizationIndexPlanning
	}
	terminalPlanner, ok := h.notificationPlanner.(TerminalNotificationPlanBuilder)
	if !ok {
		return uuid.Nil, nil, nil, ErrInvalidOrganizationHandler
	}
	notificationPlan, err := terminalPlanner.PlanResults(
		batch.ID, identityPlan.Username, results, failures, h.appDomain,
	)
	if err != nil {
		return uuid.Nil, nil, nil, ErrOrganizationIndexPlanning
	}
	requestID, _, scrapeIDs, notificationIDs, err := h.sideEffectStore.CompleteOrganizationBatchWithSideEffects(
		ctx, batch.ID, batch.LeaseToken, completionPlan, identityPlan, notificationPlan,
	)
	return requestID, scrapeIDs, notificationIDs, err
}
func (h *Handler) loadOrganizationSuccessSnapshots(
	ctx context.Context,
	batch OrganizationBatch,
	username string,
) ([]organization.SuccessSnapshot, error) {
	if h.sideEffectStore == nil {
		return nil, ErrOrganizationSuccessSnapshotNotFound
	}
	return h.sideEffectStore.ListOrganizationSuccessSnapshots(ctx, username, batch.ID)
}
func filterOrganizationPlan(plan organizer.Plan, results []organization.ResultSnapshot) organizer.Plan {
	ordinals := make(map[int]struct{})
	for _, result := range results {
		for _, file := range result.Snapshot.Files {
			ordinals[file.Ordinal] = struct{}{}
		}
	}
	filtered := organizer.Plan{ID: plan.ID, TargetRoot: plan.TargetRoot}
	filtered.Operations = make([]organizer.Operation, 0, len(ordinals))
	for ordinal, operation := range plan.Operations {
		if _, ok := ordinals[ordinal]; ok {
			filtered.Operations = append(filtered.Operations, operation)
		}
	}
	return filtered
}
func hasOrganizationVideoOperation(plan organizer.Plan) bool {
	for _, operation := range plan.Operations {
		switch strings.ToLower(filepath.Ext(operation.Target)) {
		case ".mkv", ".mp4", ".avi", ".mov", ".ts", ".m2ts", ".wmv", ".flv", ".webm", ".iso":
			return true
		}
	}
	return false
}
func (h *Handler) terminalResultsEnabled() bool {
	if h == nil || h.store == nil || h.sideEffectStore == nil || h.scrapePlanner == nil ||
		h.notificationPlanner == nil {
		return false
	}
	_, hasTerminalFiles := h.store.(TerminalOrganizationFileStore)
	_, hasOutcomes := h.store.(OrganizationOutcomeStore)
	_, hasTerminalPlanner := h.notificationPlanner.(TerminalNotificationPlanBuilder)
	return hasTerminalFiles && hasOutcomes && hasTerminalPlanner
}
func legacyOrganizationOutcomesComplete(
	plan organizer.Plan,
	outcomes []organization.FileOutcome,
) bool {
	if len(plan.Operations) == 0 || len(outcomes) != len(plan.Operations) {
		return false
	}
	seen := make([]bool, len(plan.Operations))
	for _, outcome := range outcomes {
		if outcome.Ordinal < 0 || outcome.Ordinal >= len(seen) || seen[outcome.Ordinal] ||
			outcome.State != organization.FileOutcomeCompleted || outcome.Error != "" {
			return false
		}
		seen[outcome.Ordinal] = true
	}
	return true
}
func isExplicitLegacySuccessInput(intentID uuid.UUID, input organization.SuccessInput) bool {
	normalized, err := organization.NormalizeSuccessInput(input)
	return err == nil && normalized.IntentID == intentID && normalized.Legacy && normalized.Version == 0
}
func classifyOrganizationSuccessSnapshots(
	batch OrganizationBatch,
	username string,
	snapshots []organization.SuccessSnapshot,
) (legacy bool, valid bool) {
	username = strings.TrimSpace(username)
	if batch.ID == uuid.Nil || batch.IntentID == uuid.Nil || username == "" || len(snapshots) == 0 {
		return false, false
	}
	seenGroups := make(map[string]struct{}, len(snapshots))
	for index, snapshot := range snapshots {
		normalized, err := organization.NormalizeSuccessSnapshot(snapshot)
		if err != nil || normalized.BatchID != batch.ID || normalized.IntentID != batch.IntentID ||
			normalized.Username != username {
			return false, false
		}
		if _, exists := seenGroups[normalized.GroupKey]; exists {
			return false, false
		}
		seenGroups[normalized.GroupKey] = struct{}{}
		if index == 0 {
			legacy = normalized.Legacy
			continue
		}
		if normalized.Legacy != legacy {
			return false, false
		}
	}
	if legacy && len(snapshots) != 1 {
		return false, false
	}
	return legacy, true
}
