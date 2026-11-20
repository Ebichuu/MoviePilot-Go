package app

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/moviepilot-go/moviepilot-go/internal/notification"
)

// TestLoadNotificationRuntimePropagatesReloadFailure 覆盖装配阶段的 Reload 失败
// 与「加载通知设置失败」的纯错误出口。
func TestLoadNotificationRuntimePropagatesReloadFailure(t *testing.T) {
	store := &appNotificationRuntimeTestStore{err: errors.New("数据库不可用")}
	runtime, err := loadNotificationRuntime(t.Context(), store, nil)
	if err == nil {
		t.Fatal("存储报错时装配应失败")
	}
	if runtime != nil {
		t.Fatalf("失败时不应返回运行时: %+v", runtime)
	}
	if err.Error() != "加载通知设置失败" {
		t.Fatalf("错误 = %v，期望稳定提示", err)
	}
}

// TestNotificationRuntimeReloadCancelledWhileWaiting 覆盖「拿到重载锁之后才被取消」
// 的第二次检查：重载必须直接返回 context 错误。
func TestNotificationRuntimeReloadCancelledWhileWaiting(t *testing.T) {
	started := make(chan struct{}, 1)
	release := make(chan struct{})
	store := &appNotificationRuntimeTestStore{loadStarted: started, loadRelease: release}
	runtime := &notificationRuntime{
		store:    store,
		registry: notification.NewRegistry(discardDoctorLogger()),
		logger:   discardDoctorLogger(),
	}

	// 第一个重载占住锁并停在加载阶段。
	firstDone := make(chan error, 1)
	go func() { firstDone <- runtime.Reload(t.Context()) }()
	select {
	case <-started:
	case <-time.After(5 * time.Second):
		t.Fatal("第一轮重载未进入加载阶段")
	}

	// 第二个重载排队等锁；在此期间取消它的上下文，放行第一个。
	ctx, cancel := context.WithCancel(t.Context())
	secondDone := make(chan error, 1)
	go func() { secondDone <- runtime.Reload(ctx) }()
	time.Sleep(20 * time.Millisecond)
	cancel()
	close(release)

	if err := <-firstDone; err != nil {
		t.Fatalf("第一轮重载 error = %v", err)
	}
	if err := <-secondDone; !errors.Is(err, context.Canceled) {
		t.Fatalf("排队中被取消的重载 error = %v，期望 context.Canceled", err)
	}
}
