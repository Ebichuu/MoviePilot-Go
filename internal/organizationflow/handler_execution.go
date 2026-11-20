package organizationflow

import (
	"context"
	"errors"

	"github.com/google/uuid"

	"github.com/moviepilot-go/moviepilot-go/internal/mediaindex"
	"github.com/moviepilot-go/moviepilot-go/internal/organization"
	"github.com/moviepilot-go/moviepilot-go/internal/organizer"
)

// HandleOrganizationSweep recovers expired leases using current execution
// time and re-enqueues one bounded page of due batches.
func (h *Handler) HandleOrganizationSweep(ctx context.Context) error {
	if ctx == nil || h == nil || h.store == nil || h.enqueuer == nil || h.now == nil {
		return ErrInvalidOrganizationHandler
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	now := h.now().UTC()
	if _, err := h.store.RecoverExpiredOrganizationLeases(ctx, now, organizationSweepPageSize); err != nil {
		return h.executionStoreError(ctx, err)
	}
	ids, err := h.store.ListDueOrganizationBatchIDs(ctx, now, organizationSweepPageSize)
	if err != nil {
		return h.executionStoreError(ctx, err)
	}
	if len(ids) > organizationSweepPageSize {
		return ErrOrganizationExecutionStorage
	}
	seen := make(map[uuid.UUID]struct{}, len(ids))
	for _, batchID := range ids {
		if batchID == uuid.Nil {
			return ErrOrganizationExecutionStorage
		}
		if _, exists := seen[batchID]; exists {
			return ErrOrganizationExecutionStorage
		}
		seen[batchID] = struct{}{}
		if err := h.enqueuer.EnqueueOrganization(ctx, OrganizationArgs{BatchID: batchID}); err != nil {
			if contextErr := organizationContextError(ctx, err); contextErr != nil {
				return contextErr
			}
			return ErrOrganizationExecutionStorage
		}
	}
	return nil
}

// HandleOrganization claims only the batch named by the durable job, executes
// bounded file pages, and atomically advances linked subscription state to
// indexed after every file is confirmed.
func (h *Handler) HandleOrganization(ctx context.Context, args OrganizationArgs) (returnErr error) {
	if err := h.validate(ctx, args); err != nil {
		return err
	}
	batch, claimed, err := h.store.ClaimOrganizationBatch(
		ctx,
		args.BatchID,
		h.now().UTC(),
		h.leaseDuration,
	)
	if err != nil {
		return h.executionStoreError(ctx, err)
	}
	if !claimed {
		return nil
	}
	leaseOwned := batch.ID == args.BatchID &&
		batch.LeaseToken != uuid.Nil &&
		batch.State == OrganizationBatchOrganizing
	batchSettled := false
	releaseStableErr := organizationStableFileError
	defer func() {
		if returnErr != nil && leaseOwned && !batchSettled {
			h.releaseOrganizationBatch(ctx, batch, releaseStableErr)
		}
	}()
	if !leaseOwned || batch.SourceRoot == "" {
		return ErrOrganizationExecutionStorage
	}
	heartbeat := h.startHeartbeat(ctx, batch)
	defer heartbeat.Stop()
	workCtx := heartbeat.Context()

	for {
		files, err := h.store.ClaimOrganizationFiles(workCtx, batch.ID, batch.LeaseToken, h.fileClaimSize)
		if err != nil {
			return h.heartbeatOrStoreError(ctx, heartbeat, err)
		}
		if len(files) == 0 {
			break
		}
		if len(files) > h.fileClaimSize {
			return ErrOrganizationExecutionStorage
		}
		for _, file := range files {
			if err := workCtx.Err(); err != nil {
				return h.heartbeatOrStoreError(ctx, heartbeat, err)
			}
			if file.BatchID != batch.ID || file.ID < 1 || file.State != OrganizationFileOrganizing {
				return ErrOrganizationExecutionStorage
			}
			operationPlan := organizer.Plan{
				ID:         batch.Plan.ID,
				TargetRoot: batch.Plan.TargetRoot,
				Operations: []organizer.Operation{{
					Source:         file.SourcePath,
					Target:         file.TargetPath,
					Mode:           file.Mode,
					ExpectedSize:   file.ExpectedSize,
					ExpectedSHA256: file.ExpectedSHA256,
				}},
			}
			if err := h.execute(workCtx, batch.SourceRoot, operationPlan); err != nil {
				if h.terminalResultsEnabled() {
					if workCtx.Err() != nil {
						if handled, heartbeatErr := h.stopHeartbeatError(ctx, heartbeat); handled {
							return heartbeatErr
						}
					}
					if contextErr := organizationContextError(ctx, err); contextErr != nil {
						return contextErr
					}
					if errors.Is(err, ErrOrganizationLeaseConflict) {
						batchSettled = true
						return nil
					}
					terminalStore := h.store.(TerminalOrganizationFileStore)
					if finalizeErr := terminalStore.FinalizeOrganizationFileFailure(
						ctx, batch.ID, batch.LeaseToken, file.ID, organizationDisplayError(err),
					); finalizeErr != nil {
						if errors.Is(finalizeErr, ErrOrganizationLeaseConflict) ||
							errors.Is(finalizeErr, ErrOrganizationBatchNotFound) {
							batchSettled = true
							return nil
						}
						return h.heartbeatOrStoreError(ctx, heartbeat, finalizeErr)
					}
					// V2 emits a Manual event for this file and continues with the
					// remaining files in the same media/season group.
					continue
				}
				if handled, heartbeatErr := h.stopHeartbeatError(ctx, heartbeat); handled {
					return heartbeatErr
				}
				if contextErr := organizationContextError(ctx, err); contextErr != nil {
					return contextErr
				}
				if errors.Is(err, ErrOrganizationLeaseConflict) {
					batchSettled = true
					return nil
				}
				settled, failureErr := h.recordOperationFailure(ctx, batch, file)
				batchSettled = settled
				return failureErr
			}
			if err := h.store.CompleteOrganizationFile(workCtx, batch.ID, batch.LeaseToken, file.ID); err != nil {
				return h.heartbeatOrStoreError(ctx, heartbeat, err)
			}
		}
	}

	releaseStableErr = organizationStableIndexError
	executionPlan := batch.Plan
	var terminalResults []organization.ResultSnapshot
	var terminalFailures []organization.FileFailure
	terminalMode := false
	if h.terminalResultsEnabled() {
		outcomeStore := h.store.(OrganizationOutcomeStore)
		if h.scrapePlanner == nil {
			return ErrOrganizationIndexPlanning
		}
		identityPlan, identityErr := h.scrapePlanner.Plan(workCtx, batch.ID, batch.IntentID, batch.Plan)
		if identityErr != nil || identityPlan.Username == "" {
			return ErrOrganizationIndexPlanning
		}
		outcomes, outcomeErr := outcomeStore.ListOrganizationFileOutcomes(workCtx, identityPlan.Username, batch.ID)
		if outcomeErr != nil {
			return h.heartbeatOrStoreError(ctx, heartbeat, outcomeErr)
		}
		snapshots, snapshotErr := h.loadOrganizationSuccessSnapshots(workCtx, batch, identityPlan.Username)
		switch {
		case snapshotErr == nil:
			terminalResults, terminalFailures, err = organization.ProjectTerminalResults(snapshots, outcomes)
			if err != nil {
				return ErrOrganizationIndexPlanning
			}
			executionPlan = filterOrganizationPlan(batch.Plan, terminalResults)
			terminalMode = true
		case errors.Is(snapshotErr, ErrOrganizationSuccessSnapshotNotFound):
			// Pre-snapshot batches may use the old completion plan only when the
			// immutable input explicitly identifies legacy data and every planned
			// file has a successful terminal outcome. Any failure still needs its
			// missing snapshot to render the Manual event safely.
			if !legacyOrganizationOutcomesComplete(batch.Plan, outcomes) {
				return ErrOrganizationIndexPlanning
			}
			successInput, inputErr := h.sideEffectStore.GetDownloadIntentSuccessInput(
				workCtx, identityPlan.Username, batch.IntentID,
			)
			if inputErr != nil || !isExplicitLegacySuccessInput(batch.IntentID, successInput) {
				return ErrOrganizationIndexPlanning
			}
		default:
			return h.heartbeatOrStoreError(ctx, heartbeat, snapshotErr)
		}
	}
	completionPlan := mediaindex.CompletionPlan{}
	if !terminalMode {
		completionPlan, err = h.completionPlanner.MediaIndexCompletionPlan(workCtx, executionPlan)
	} else if len(executionPlan.Operations) > 0 && hasOrganizationVideoOperation(executionPlan) {
		completionPlan, err = h.completionPlanner.MediaIndexCompletionPlan(workCtx, executionPlan)
	} else if len(executionPlan.Operations) > 0 {
		// No video target can be confirmed by Emby; NFO and notifications still
		// complete the batch exactly as V2 does for subtitle/audio-only results.
		completionPlan = mediaindex.CompletionPlan{}
	}
	if err != nil {
		if handled, heartbeatErr := h.stopHeartbeatError(ctx, heartbeat); handled {
			return heartbeatErr
		}
		if contextErr := organizationContextError(ctx, err); contextErr != nil {
			return contextErr
		}
		if errors.Is(err, mediaindex.ErrRequiredServerUnavailable) {
			releaseStableErr = organizationStableServerUnavailableError
			return ErrOrganizationIndexServerUnavailable
		}
		if errors.Is(err, mediaindex.ErrRequiredServerPathMapping) {
			releaseStableErr = organizationStablePathMappingError
			return ErrOrganizationIndexPathMapping
		}
		return ErrOrganizationIndexPlanning
	}
	var requestID uuid.UUID
	var scrapeIDs, notificationIDs []uuid.UUID
	if terminalMode {
		requestID, scrapeIDs, notificationIDs, err = h.completeBatchWithResults(
			workCtx, batch, completionPlan, executionPlan, terminalResults, terminalFailures,
		)
	} else {
		requestID, scrapeIDs, notificationIDs, err = h.completeBatch(workCtx, batch, completionPlan)
	}
	if err != nil {
		if errors.Is(err, ErrOrganizationLeaseConflict) {
			batchSettled = true
			return nil
		}
		return h.heartbeatOrStoreError(ctx, heartbeat, err)
	}
	batchSettled = true
	if handled, heartbeatErr := h.stopHeartbeatError(ctx, heartbeat); handled {
		return heartbeatErr
	}
	files := len(executionPlan.Operations)
	if terminalMode {
		files = len(terminalResults)
	}
	h.publishOrganizationCompleted(batch, files)
	if requestID != uuid.Nil {
		h.enqueueMediaIndexBestEffort(ctx, requestID)
	}
	h.enqueueScrapeBestEffort(ctx, scrapeIDs)
	h.enqueueNotificationBestEffort(ctx, notificationIDs)

	return nil
}
