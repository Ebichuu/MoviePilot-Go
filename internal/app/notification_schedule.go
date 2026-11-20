package app

import (
	"context"
	"errors"
	"sync/atomic"
)

var errNotificationScheduleUnavailable = errors.New("通知发送时段运行时不可用")

type notificationScheduleStore interface {
	GetSetting(context.Context, string, any) (bool, error)
}

type notificationScheduleValue struct {
	value any
}

type notificationScheduleRuntime struct {
	store   notificationScheduleStore
	key     string
	current atomic.Pointer[notificationScheduleValue]
}

func newNotificationScheduleRuntime(ctx context.Context, store notificationScheduleStore, key string) (*notificationScheduleRuntime, error) {
	runtime := &notificationScheduleRuntime{store: store, key: key}
	if err := runtime.Reload(ctx); err != nil {
		return nil, err
	}
	return runtime, nil
}

func (r *notificationScheduleRuntime) Reload(ctx context.Context) error {
	if r == nil || r.store == nil || r.key == "" || ctx == nil {
		return errNotificationScheduleUnavailable
	}
	var value any
	found, err := r.store.GetSetting(ctx, r.key, &value)
	if err != nil {
		return err
	}
	if !found {
		r.current.Store(nil)
		return nil
	}
	r.current.Store(&notificationScheduleValue{value: value})
	return nil
}

func (r *notificationScheduleRuntime) Schedule(fallback func() any) any {
	if r != nil {
		if value := r.current.Load(); value != nil {
			return value.value
		}
	}
	if fallback != nil {
		return fallback()
	}
	return nil
}
