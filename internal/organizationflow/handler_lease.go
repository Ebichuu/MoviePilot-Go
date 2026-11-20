package organizationflow

import (
	"context"
	"errors"
	"strings"
	"sync"
	"time"
)

func organizationDisplayError(err error) string {
	if err == nil {
		return "未知"
	}
	value := strings.TrimSpace(err.Error())
	var builder strings.Builder
	for _, character := range value {
		if character < 0x20 || character == 0x7f {
			continue
		}
		encoded := string(character)
		if builder.Len()+len(encoded) > 2048 {
			break
		}
		builder.WriteString(encoded)
	}
	value = strings.TrimSpace(builder.String())
	if value == "" {
		return "未知"
	}
	lower := strings.ToLower(value)
	for _, marker := range []string{"authorization", "cookie", "passkey", "token=", "secret="} {
		if strings.Contains(lower, marker) {
			return organizationStableFileError
		}
	}
	return value
}
func (h *Handler) startHeartbeat(ctx context.Context, batch OrganizationBatch) *leaseHeartbeat {
	workCtx, cancel := context.WithCancel(ctx)
	heartbeat := &leaseHeartbeat{ctx: workCtx, cancel: cancel, done: make(chan struct{})}
	go func() {
		defer close(heartbeat.done)
		ticker := time.NewTicker(h.heartbeatInterval)
		defer ticker.Stop()
		for {
			select {
			case <-workCtx.Done():
				return
			case <-ticker.C:
				err := h.store.RenewOrganizationBatchLease(
					workCtx,
					batch.ID,
					batch.LeaseToken,
					h.now().UTC(),
					h.leaseDuration,
				)
				if err != nil {
					if workCtx.Err() != nil && errors.Is(err, context.Canceled) {
						return
					}
					heartbeat.setError(err)
					cancel()
					return
				}
			}
		}
	}()
	return heartbeat
}
func (h *Handler) recordOperationFailure(
	ctx context.Context,
	batch OrganizationBatch,
	file OrganizationFile,
) (bool, error) {
	if err := h.store.FailOrganizationFile(
		ctx,
		batch.ID,
		batch.LeaseToken,
		file.ID,
		organizationStableFileError,
	); err != nil {
		if errors.Is(err, ErrOrganizationLeaseConflict) ||
			errors.Is(err, ErrOrganizationBatchNotFound) {
			return true, nil
		}
		return false, h.executionStoreError(ctx, err)
	}
	if err := h.store.FailOrganizationBatch(
		ctx,
		batch.ID,
		batch.LeaseToken,
		h.now().UTC(),
		organizationStableFileError,
	); err != nil {
		if errors.Is(err, ErrOrganizationLeaseConflict) ||
			errors.Is(err, ErrOrganizationBatchNotFound) {
			return true, nil
		}
		return false, h.executionStoreError(ctx, err)
	}
	return true, ErrOrganizationFileOperation
}
func (h *Handler) releaseOrganizationBatch(
	ctx context.Context,
	batch OrganizationBatch,
	stableErr string,
) {
	releaseCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), organizationReleaseTimeout)
	defer cancel()
	if err := h.store.FailOrganizationBatch(
		releaseCtx,
		batch.ID,
		batch.LeaseToken,
		h.now().UTC(),
		stableErr,
	); err != nil {
		if errors.Is(err, ErrOrganizationLeaseConflict) ||
			errors.Is(err, ErrOrganizationBatchNotFound) {
			return
		}
		return
	}
}
func (h *Handler) validate(ctx context.Context, args OrganizationArgs) error {
	if ctx == nil || h == nil || h.store == nil || h.enqueuer == nil || h.completionPlanner == nil ||
		h.mediaIndexEnqueuer == nil || h.now == nil || h.execute == nil ||
		h.leaseDuration <= 0 || h.leaseDuration > time.Hour ||
		h.heartbeatInterval <= 0 || h.heartbeatInterval >= h.leaseDuration ||
		h.fileClaimSize < 1 || h.fileClaimSize > 500 {
		return ErrInvalidOrganizationHandler
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := args.Validate(); err != nil {
		return ErrInvalidOrganizationHandler
	}
	return nil
}
func (h *Handler) executionStoreError(ctx context.Context, err error) error {
	if contextErr := organizationContextError(ctx, err); contextErr != nil {
		return contextErr
	}
	if errors.Is(err, ErrOrganizationLeaseConflict) ||
		errors.Is(err, ErrOrganizationBatchNotFound) {
		return nil
	}
	return ErrOrganizationExecutionStorage
}
func (h *Handler) heartbeatOrStoreError(
	ctx context.Context,
	heartbeat *leaseHeartbeat,
	storeErr error,
) error {
	if handled, heartbeatErr := h.stopHeartbeatError(ctx, heartbeat); handled {
		return heartbeatErr
	}
	return h.executionStoreError(ctx, storeErr)
}
func (h *Handler) stopHeartbeatError(
	ctx context.Context,
	heartbeat *leaseHeartbeat,
) (bool, error) {
	if heartbeat == nil {
		return false, nil
	}
	workContextErr := heartbeat.Context().Err()
	if heartbeatErr := heartbeat.Stop(); heartbeatErr != nil {
		if ctx != nil && ctx.Err() != nil {
			return true, ctx.Err()
		}
		if errors.Is(heartbeatErr, ErrOrganizationLeaseConflict) ||
			errors.Is(heartbeatErr, ErrOrganizationBatchNotFound) {
			return true, nil
		}
		return true, ErrOrganizationExecutionStorage
	}
	if contextErr := organizationContextError(ctx, workContextErr); contextErr != nil {
		return true, contextErr
	}
	return false, nil
}
func organizationContextError(ctx context.Context, err error) error {
	if ctx != nil {
		if contextErr := ctx.Err(); contextErr != nil {
			return contextErr
		}
	}
	if errors.Is(err, context.Canceled) {
		return context.Canceled
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return context.DeadlineExceeded
	}
	return nil
}

type leaseHeartbeat struct {
	ctx    context.Context
	cancel context.CancelFunc
	done   chan struct{}
	once   sync.Once
	mu     sync.Mutex
	err    error
}

func (h *leaseHeartbeat) Context() context.Context {
	return h.ctx
}
func (h *leaseHeartbeat) Stop() error {
	if h == nil {
		return nil
	}
	h.once.Do(func() {
		h.cancel()
		<-h.done
	})
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.err
}
func (h *leaseHeartbeat) setError(err error) {
	h.mu.Lock()
	h.err = err
	h.mu.Unlock()
}
