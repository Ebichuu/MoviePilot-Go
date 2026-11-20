package notificationflow

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/moviepilot-go/moviepilot-go/internal/notification"
)

type sweepBusinessStoreFake struct {
	ids []uuid.UUID
	err error
}

func (s *sweepBusinessStoreFake) PublishNextBusinessNotification(context.Context, BusinessPlanner, time.Time) ([]uuid.UUID, error) {
	return s.ids, s.err
}

type adjustableNotificationStore struct {
	lease        Lease
	claimed      bool
	claimErr     error
	completeErr  error
	reschedErr   error
	completed    bool
	rescheduled  bool
	reschedCause string
}

func (s *adjustableNotificationStore) ClaimNotification(context.Context, uuid.UUID, time.Time, time.Duration) (Lease, bool, error) {
	return s.lease, s.claimed, s.claimErr
}

func (s *adjustableNotificationStore) CompleteNotification(context.Context, uuid.UUID, uuid.UUID, time.Time) error {
	s.completed = true
	return s.completeErr
}

func (s *adjustableNotificationStore) RescheduleNotification(_ context.Context, _, _ uuid.UUID, _ time.Time, stable string) error {
	s.rescheduled = true
	s.reschedCause = stable
	return s.reschedErr
}

type adjustableSweepStore struct {
	recoverErr error
	listErr    error
	ids        []uuid.UUID
}

func (s *adjustableSweepStore) RecoverExpiredNotifications(context.Context, time.Time, int) (int64, error) {
	return 0, s.recoverErr
}

func (s *adjustableSweepStore) ListDueNotifications(context.Context, time.Time, int) ([]uuid.UUID, error) {
	return append([]uuid.UUID(nil), s.ids...), s.listErr
}

type adjustableEnqueuer struct {
	err  error
	ids  []uuid.UUID
	fail int
}

func (e *adjustableEnqueuer) EnqueueNotification(_ context.Context, id uuid.UUID) error {
	e.ids = append(e.ids, id)
	if e.fail > 0 {
		e.fail--
		return e.err
	}
	return nil
}

// usableLease 返回一个可以通过 Lease.Valid 的租约。
func usableLease(id uuid.UUID) Lease {
	return Lease{Token: uuid.New(), Request: Request{
		ID: id, Channel: notification.ChannelTelegram, AttemptCount: 1,
		Message: notification.Message{Channel: notification.ChannelTelegram, Title: "t", Text: "x"},
	}}
}

func TestNewServiceAndSweepHandlerValidation(t *testing.T) {
	t.Parallel()
	if _, err := NewService(nil, senderFake{}); !errors.Is(err, ErrInvalidPlan) {
		t.Fatalf("nil store error = %v", err)
	}
	if _, err := NewService(&adjustableNotificationStore{}, nil); !errors.Is(err, ErrInvalidPlan) {
		t.Fatalf("nil sender error = %v", err)
	}
	if _, err := NewSweepHandler(nil, &adjustableEnqueuer{}); !errors.Is(err, ErrInvalidPlan) {
		t.Fatalf("nil sweep store error = %v", err)
	}
	if _, err := NewSweepHandler(&adjustableSweepStore{}, nil); !errors.Is(err, ErrInvalidPlan) {
		t.Fatalf("nil enqueuer error = %v", err)
	}
	// nil 接收者上调用 SetBusinessOutbox 不应 panic。
	var handler *SweepHandler
	handler.SetBusinessOutbox(&sweepBusinessStoreFake{}, &Planner{})
}

func TestServiceHandleGuardsAndPersistenceErrors(t *testing.T) {
	t.Parallel()
	id := uuid.New()

	cases := []struct {
		name    string
		service func(*testing.T) *Service
		ctx     context.Context
		id      uuid.UUID
		want    error
	}{
		{
			name: "nil 上下文",
			service: func(t *testing.T) *Service {
				service, _ := NewService(&adjustableNotificationStore{}, senderFake{})
				return service
			},
			ctx:  nil,
			id:   id,
			want: ErrInvalidPlan,
		},
		{
			name: "零请求 ID",
			service: func(t *testing.T) *Service {
				service, _ := NewService(&adjustableNotificationStore{}, senderFake{})
				return service
			},
			ctx:  context.Background(),
			want: ErrInvalidPlan,
		},
		{
			name: "缺少时钟",
			service: func(t *testing.T) *Service {
				service, _ := NewService(&adjustableNotificationStore{}, senderFake{})
				service.now = nil
				return service
			},
			ctx:  context.Background(),
			id:   id,
			want: ErrInvalidPlan,
		},
		{
			name: "上下文已取消",
			service: func(t *testing.T) *Service {
				service, _ := NewService(&adjustableNotificationStore{}, senderFake{})
				return service
			},
			ctx: func() context.Context {
				ctx, cancel := context.WithCancel(context.Background())
				cancel()
				return ctx
			}(),
			id:   id,
			want: context.Canceled,
		},
		{
			name: "认领失败",
			service: func(t *testing.T) *Service {
				service, _ := NewService(&adjustableNotificationStore{claimErr: errors.New("db down")}, senderFake{})
				return service
			},
			ctx:  context.Background(),
			id:   id,
			want: ErrPersistence,
		},
		{
			name: "租约无效",
			service: func(t *testing.T) *Service {
				service, _ := NewService(&adjustableNotificationStore{claimed: true}, senderFake{})
				return service
			},
			ctx:  context.Background(),
			id:   id,
			want: ErrPersistence,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			service := tc.service(t)
			if err := service.Handle(tc.ctx, tc.id); !errors.Is(err, tc.want) {
				t.Fatalf("Handle error = %v, want %v", err, tc.want)
			}
		})
	}

	var nilService *Service
	if err := nilService.Handle(context.Background(), id); !errors.Is(err, ErrInvalidPlan) {
		t.Fatalf("nil service error = %v", err)
	}
}

func TestServiceHandleSkipsUnclaimedRequest(t *testing.T) {
	t.Parallel()
	store := &adjustableNotificationStore{claimed: false}
	service, _ := NewService(store, senderFake{})
	if err := service.Handle(context.Background(), uuid.New()); err != nil {
		t.Fatalf("未认领请求应静默返回: %v", err)
	}
	if store.completed || store.rescheduled {
		t.Fatalf("未认领请求产生写入: %#v", store)
	}
}

func TestServiceHandleCompletesSuccessfulDelivery(t *testing.T) {
	t.Parallel()
	id := uuid.New()
	store := &adjustableNotificationStore{claimed: true, lease: usableLease(id)}
	service, _ := NewService(store, senderFake{})
	if err := service.Handle(context.Background(), id); err != nil {
		t.Fatalf("成功投递: %v", err)
	}
	if !store.completed || store.rescheduled {
		t.Fatalf("成功投递未完成: %#v", store)
	}
}

func TestServiceHandleMapsFinalPersistenceFailures(t *testing.T) {
	t.Parallel()
	id := uuid.New()

	t.Run("完成写入失败", func(t *testing.T) {
		store := &adjustableNotificationStore{claimed: true, lease: usableLease(id), completeErr: errors.New("db")}
		service, _ := NewService(store, senderFake{})
		if err := service.Handle(context.Background(), id); !errors.Is(err, ErrPersistence) {
			t.Fatalf("完成写入失败 = %v", err)
		}
	})

	t.Run("重排写入失败", func(t *testing.T) {
		store := &adjustableNotificationStore{claimed: true, lease: usableLease(id), reschedErr: errors.New("db")}
		service, _ := NewService(store, senderFake{err: errors.New("boom")})
		if err := service.Handle(context.Background(), id); !errors.Is(err, ErrPersistence) {
			t.Fatalf("重排写入失败 = %v", err)
		}
	})
}

func TestServiceHandleStabilizesContextErrors(t *testing.T) {
	t.Parallel()
	id := uuid.New()

	t.Run("发送期间取消", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		store := &adjustableNotificationStore{claimed: true, lease: usableLease(id)}
		service, _ := NewService(store, senderFunc(func(context.Context, notification.Message) error {
			cancel()
			return errors.New("client gone")
		}))
		if err := service.Handle(ctx, id); !errors.Is(err, context.Canceled) {
			t.Fatalf("取消错误 = %v", err)
		}
		if store.rescheduled {
			t.Fatal("取消不应触发重排")
		}
	})

	t.Run("发送超时", func(t *testing.T) {
		store := &adjustableNotificationStore{claimed: true, lease: usableLease(id)}
		service, _ := NewService(store, senderFake{err: context.DeadlineExceeded})
		if err := service.Handle(context.Background(), id); !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("超时错误 = %v", err)
		}
		if store.rescheduled {
			t.Fatal("超时不应触发重排")
		}
	})
}

func TestSweepHandlerGuards(t *testing.T) {
	t.Parallel()
	if err := (&SweepHandler{}).Handle(nil); !errors.Is(err, ErrInvalidPlan) {
		t.Fatalf("nil 上下文 = %v", err)
	}
	handler, _ := NewSweepHandler(&adjustableSweepStore{}, &adjustableEnqueuer{})
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := handler.Handle(ctx); !errors.Is(err, context.Canceled) {
		t.Fatalf("取消上下文 = %v", err)
	}
	var nilHandler *SweepHandler
	if err := nilHandler.Handle(context.Background()); !errors.Is(err, ErrInvalidPlan) {
		t.Fatalf("nil handler = %v", err)
	}
}

func TestSweepHandlerOutboxAndStoreFailures(t *testing.T) {
	t.Parallel()

	t.Run("业务出箱失败", func(t *testing.T) {
		handler, _ := NewSweepHandler(&adjustableSweepStore{}, &adjustableEnqueuer{})
		handler.SetBusinessOutbox(&sweepBusinessStoreFake{err: errors.New("db")}, &Planner{})
		if err := handler.Handle(context.Background()); !errors.Is(err, ErrPersistence) {
			t.Fatalf("业务出箱失败 = %v", err)
		}
	})

	t.Run("业务出箱成功", func(t *testing.T) {
		enqueuer := &adjustableEnqueuer{}
		handler, _ := NewSweepHandler(&adjustableSweepStore{}, enqueuer)
		handler.SetBusinessOutbox(&sweepBusinessStoreFake{ids: []uuid.UUID{uuid.New()}}, &Planner{})
		if err := handler.Handle(context.Background()); err != nil {
			t.Fatalf("业务出箱成功 = %v", err)
		}
	})

	t.Run("恢复过期租约失败", func(t *testing.T) {
		handler, _ := NewSweepHandler(&adjustableSweepStore{recoverErr: errors.New("db")}, &adjustableEnqueuer{})
		if err := handler.Handle(context.Background()); !errors.Is(err, ErrPersistence) {
			t.Fatalf("恢复失败 = %v", err)
		}
	})

	t.Run("列出到期行失败", func(t *testing.T) {
		handler, _ := NewSweepHandler(&adjustableSweepStore{listErr: errors.New("db")}, &adjustableEnqueuer{})
		if err := handler.Handle(context.Background()); !errors.Is(err, ErrPersistence) {
			t.Fatalf("列出失败 = %v", err)
		}
	})

	t.Run("超出扫描页", func(t *testing.T) {
		ids := make([]uuid.UUID, SweepPageSize+1)
		for index := range ids {
			ids[index] = uuid.New()
		}
		handler, _ := NewSweepHandler(&adjustableSweepStore{ids: ids}, &adjustableEnqueuer{})
		if err := handler.Handle(context.Background()); !errors.Is(err, ErrPersistence) {
			t.Fatalf("超出扫描页 = %v", err)
		}
	})
}

func TestSweepHandlerRejectsDuplicateAndNilIDs(t *testing.T) {
	t.Parallel()

	t.Run("零 ID", func(t *testing.T) {
		handler, _ := NewSweepHandler(&adjustableSweepStore{ids: []uuid.UUID{uuid.Nil}}, &adjustableEnqueuer{})
		if err := handler.Handle(context.Background()); !errors.Is(err, ErrPersistence) {
			t.Fatalf("零 ID = %v", err)
		}
	})

	t.Run("重复 ID", func(t *testing.T) {
		id := uuid.New()
		handler, _ := NewSweepHandler(&adjustableSweepStore{ids: []uuid.UUID{id, id}}, &adjustableEnqueuer{})
		if err := handler.Handle(context.Background()); !errors.Is(err, ErrPersistence) {
			t.Fatalf("重复 ID = %v", err)
		}
	})
}

func TestSweepHandlerEnqueueFailures(t *testing.T) {
	t.Parallel()
	id := uuid.New()

	t.Run("入队失败", func(t *testing.T) {
		handler, _ := NewSweepHandler(&adjustableSweepStore{ids: []uuid.UUID{id}}, &adjustableEnqueuer{err: errors.New("queue down"), fail: 1})
		if err := handler.Handle(context.Background()); !errors.Is(err, ErrPersistence) {
			t.Fatalf("入队失败 = %v", err)
		}
	})

	t.Run("入队期间取消", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		enqueuer := &adjustableEnqueuer{err: errors.New("queue down"), fail: 1}
		handler, _ := NewSweepHandler(&adjustableSweepStore{ids: []uuid.UUID{id}}, enqueuer)
		enqueuer.fail = 1
		enqueuer.err = errors.New("queue down")
		// 取消发生在入队内部：用一个包装器在返回错误前取消上下文。
		handler.enqueuer = &cancelingEnqueuer{cancel: cancel, err: errors.New("queue down")}
		if err := handler.Handle(ctx); !errors.Is(err, context.Canceled) {
			t.Fatalf("入队期间取消 = %v", err)
		}
	})
}

type cancelingEnqueuer struct {
	cancel context.CancelFunc
	err    error
}

func (e *cancelingEnqueuer) EnqueueNotification(context.Context, uuid.UUID) error {
	e.cancel()
	return e.err
}

type senderFunc func(context.Context, notification.Message) error

func (f senderFunc) Send(ctx context.Context, message notification.Message) error {
	return f(ctx, message)
}
