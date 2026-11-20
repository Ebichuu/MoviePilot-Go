package notificationflow

import (
	"context"
	"errors"
	"time"

	"github.com/google/uuid"

	"github.com/moviepilot-go/moviepilot-go/internal/notification"
)

const (
	leaseDuration = 2 * time.Minute
	retryDelay    = 2 * time.Minute
)

// Service executes one persisted channel delivery.
type Service struct {
	store  Store
	sender notification.Sender
	now    func() time.Time
}

// NewService creates a notification executor.
func NewService(store Store, sender notification.Sender) (*Service, error) {
	if store == nil || sender == nil {
		return nil, ErrInvalidPlan
	}
	return &Service{store: store, sender: sender, now: time.Now}, nil
}

// Handle performs at most one external delivery attempt.
func (s *Service) Handle(ctx context.Context, requestID uuid.UUID) error {
	if ctx == nil || s == nil || s.store == nil || s.sender == nil || s.now == nil || requestID == uuid.Nil {
		return ErrInvalidPlan
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	now := s.now().UTC()
	lease, claimed, err := s.store.ClaimNotification(ctx, requestID, now, leaseDuration)
	if err != nil {
		return ErrPersistence
	}
	if !claimed {
		return nil
	}
	if !lease.Valid(requestID) {
		return ErrPersistence
	}
	if err := s.sender.Send(ctx, lease.Request.Message); err != nil {
		if contextErr := stableContextError(ctx, err); contextErr != nil {
			return contextErr
		}
		if err := s.store.RescheduleNotification(ctx, requestID, lease.Token, now.Add(retryDelay), "delivery_failed"); err != nil {
			return ErrPersistence
		}
		return nil
	}
	if err := s.store.CompleteNotification(ctx, requestID, lease.Token, now); err != nil {
		return ErrPersistence
	}
	return nil
}

// SweepHandler recovers expired leases and enqueues a bounded due page.
type SweepHandler struct {
	store           SweepStore
	enqueuer        Enqueuer
	businessStore   BusinessOutboxStore
	businessPlanner BusinessPlanner
	now             func() time.Time
}

// BusinessOutboxStore atomically publishes one pending business fact together
// with its history and channel requests.
type BusinessOutboxStore interface {
	PublishNextBusinessNotification(context.Context, BusinessPlanner, time.Time) ([]uuid.UUID, error)
}

// NewSweepHandler creates a bounded PostgreSQL recovery sweep.
func NewSweepHandler(store SweepStore, enqueuer Enqueuer) (*SweepHandler, error) {
	if store == nil || enqueuer == nil {
		return nil, ErrInvalidPlan
	}
	return &SweepHandler{store: store, enqueuer: enqueuer, now: time.Now}, nil
}

// SetBusinessOutbox enables bounded publication of frozen business facts.
func (h *SweepHandler) SetBusinessOutbox(store BusinessOutboxStore, planner BusinessPlanner) {
	if h == nil {
		return
	}
	h.businessStore = store
	h.businessPlanner = planner
}

// Handle performs one recovery pass.
func (h *SweepHandler) Handle(ctx context.Context) error {
	if ctx == nil || h == nil || h.store == nil || h.enqueuer == nil || h.now == nil {
		return ErrInvalidPlan
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	now := h.now().UTC()
	if h.businessStore != nil && h.businessPlanner != nil {
		if _, err := h.businessStore.PublishNextBusinessNotification(ctx, h.businessPlanner, now); err != nil {
			return ErrPersistence
		}
	}
	if _, err := h.store.RecoverExpiredNotifications(ctx, now, SweepPageSize); err != nil {
		return ErrPersistence
	}
	ids, err := h.store.ListDueNotifications(ctx, now, SweepPageSize)
	if err != nil || len(ids) > SweepPageSize {
		return ErrPersistence
	}
	seen := make(map[uuid.UUID]struct{}, len(ids))
	for _, id := range ids {
		if id == uuid.Nil {
			return ErrPersistence
		}
		if _, exists := seen[id]; exists {
			return ErrPersistence
		}
		seen[id] = struct{}{}
		if err := h.enqueuer.EnqueueNotification(ctx, id); err != nil {
			if contextErr := stableContextError(ctx, err); contextErr != nil {
				return contextErr
			}
			return ErrPersistence
		}
	}
	return nil
}

func stableContextError(ctx context.Context, err error) error {
	if ctx != nil && ctx.Err() != nil {
		return ctx.Err()
	}
	if errors.Is(err, context.Canceled) {
		return context.Canceled
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return context.DeadlineExceeded
	}
	return nil
}
