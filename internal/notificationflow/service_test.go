package notificationflow

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/moviepilot-go/moviepilot-go/internal/notification"
)

func TestServiceHandlePersistsPerChannelOutcome(t *testing.T) {
	t.Parallel()
	id := uuid.New()
	store := &notificationStoreFake{claimed: true, lease: Lease{Token: uuid.New(), Request: Request{
		ID: id, Channel: notification.ChannelTelegram, AttemptCount: 1,
		Message: notification.Message{Channel: notification.ChannelTelegram, Title: "done", Text: "done"},
	}}}
	service, err := NewService(store, senderFake{err: errors.New("token=secret")})
	if err != nil {
		t.Fatal(err)
	}
	if err := service.Handle(t.Context(), id); err != nil {
		t.Fatal(err)
	}
	if !store.rescheduled || store.completed || store.stableErr != "delivery_failed" {
		t.Fatalf("store = %#v", store)
	}
}

func TestSweepHandlerEnqueuesUniqueDueRows(t *testing.T) {
	t.Parallel()
	first := uuid.New()
	second := uuid.New()
	enqueuer := &notificationEnqueuerFake{}
	handler, err := NewSweepHandler(&notificationSweepStoreFake{ids: []uuid.UUID{first, second}}, enqueuer)
	if err != nil {
		t.Fatal(err)
	}
	if err := handler.Handle(t.Context()); err != nil {
		t.Fatal(err)
	}
	if len(enqueuer.ids) != 2 || enqueuer.ids[0] != first || enqueuer.ids[1] != second {
		t.Fatalf("enqueued = %v", enqueuer.ids)
	}
}

type senderFake struct{ err error }

func (s senderFake) Send(context.Context, notification.Message) error { return s.err }

type notificationStoreFake struct {
	lease       Lease
	claimed     bool
	completed   bool
	rescheduled bool
	stableErr   string
}

func (s *notificationStoreFake) ClaimNotification(context.Context, uuid.UUID, time.Time, time.Duration) (Lease, bool, error) {
	return s.lease, s.claimed, nil
}
func (s *notificationStoreFake) CompleteNotification(context.Context, uuid.UUID, uuid.UUID, time.Time) error {
	s.completed = true
	return nil
}
func (s *notificationStoreFake) RescheduleNotification(_ context.Context, _, _ uuid.UUID, _ time.Time, stable string) error {
	s.rescheduled = true
	s.stableErr = stable
	return nil
}

type notificationSweepStoreFake struct{ ids []uuid.UUID }

func (s *notificationSweepStoreFake) RecoverExpiredNotifications(context.Context, time.Time, int) (int64, error) {
	return 0, nil
}
func (s *notificationSweepStoreFake) ListDueNotifications(context.Context, time.Time, int) ([]uuid.UUID, error) {
	return append([]uuid.UUID(nil), s.ids...), nil
}

type notificationEnqueuerFake struct{ ids []uuid.UUID }

func (e *notificationEnqueuerFake) EnqueueNotification(_ context.Context, id uuid.UUID) error {
	e.ids = append(e.ids, id)
	return nil
}
