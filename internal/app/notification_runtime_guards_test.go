package app

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	"github.com/moviepilot-go/moviepilot-go/internal/notification"
)

// cancellingNotificationStore 在读取设置时取消上下文，用来验证「取消」比
// 「加载失败」优先被上报。
type cancellingNotificationStore struct {
	cancel context.CancelFunc
}

func (s cancellingNotificationStore) LoadNotificationSettings(context.Context) (json.RawMessage, bool, error) {
	s.cancel()
	return nil, false, errors.New("database password-secret")
}

func TestLoadNotificationRuntimeGuards(t *testing.T) {
	t.Parallel()

	if _, err := loadNotificationRuntime(nil, &appNotificationRuntimeTestStore{}, nil); err == nil ||
		err.Error() != "通知运行时上下文不可用" {
		t.Fatalf("nil ctx error = %v", err)
	}
	if _, err := loadNotificationRuntime(t.Context(), nil, nil); err == nil ||
		err.Error() != "通知设置存储不可用" {
		t.Fatalf("nil store error = %v", err)
	}
	// 没给 logger 时回落到默认 logger，而不是在 Reload 里解引用 nil。
	runtime, err := loadNotificationRuntime(t.Context(), &appNotificationRuntimeTestStore{}, nil)
	if err != nil {
		t.Fatalf("loadNotificationRuntime() error = %v", err)
	}
	if runtime.logger == nil {
		t.Fatal("未填充默认 logger")
	}
	if runtime.Registry() == nil {
		t.Fatal("成功加载后 Registry() = nil")
	}
}

func TestNotificationRuntimeReloadGuards(t *testing.T) {
	t.Parallel()

	store := &appNotificationRuntimeTestStore{}
	runtime := &notificationRuntime{store: store, registry: notification.NewRegistry(nil)}
	if err := runtime.Reload(nil); err == nil || err.Error() != "通知运行时上下文不可用" {
		t.Fatalf("nil ctx error = %v", err)
	}

	cancelled, cancel := context.WithCancel(t.Context())
	cancel()
	if err := runtime.Reload(cancelled); !errors.Is(err, context.Canceled) {
		t.Fatalf("已取消 error = %v", err)
	}

	// 缺少注册表（半装配）时明确不可用，不能让后续 Replace 在 nil 上 panic。
	if err := (&notificationRuntime{store: store}).Reload(t.Context()); err == nil ||
		err.Error() != "通知运行时不可用" {
		t.Fatalf("nil registry error = %v", err)
	}
}

func TestNotificationRuntimeReloadReportsCancellationDuringLoad(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithCancel(t.Context())
	runtime := &notificationRuntime{
		store:    cancellingNotificationStore{cancel: cancel},
		registry: notification.NewRegistry(nil),
	}
	// 存储报「数据库不可用」的同时上下文被取消：调度层需要看到取消原因，
	// 而不是笼统的「加载通知设置失败」。
	if err := runtime.Reload(ctx); !errors.Is(err, context.Canceled) {
		t.Fatalf("Reload() error = %v", err)
	}
}
