package app

import (
	"context"
	"errors"
	"testing"
)

type notificationScheduleStoreFake struct {
	value any
	found bool
	err   error
	key   string
	calls int
}

func (s *notificationScheduleStoreFake) GetSetting(_ context.Context, key string, out any) (bool, error) {
	s.calls++
	s.key = key
	if s.err != nil {
		return false, s.err
	}
	if !s.found {
		return false, nil
	}
	target, ok := out.(*any)
	if !ok {
		return false, errors.New("目标类型不符")
	}
	*target = s.value
	return true, nil
}

func TestNewNotificationScheduleRuntimeGuards(t *testing.T) {
	t.Parallel()

	if _, err := newNotificationScheduleRuntime(t.Context(), nil, "notification_schedule"); !errors.Is(err, errNotificationScheduleUnavailable) {
		t.Fatalf("nil store error = %v", err)
	}
	// 键为空同样不可用：否则会去读一个无意义的设置项。
	if _, err := newNotificationScheduleRuntime(t.Context(), &notificationScheduleStoreFake{}, ""); !errors.Is(err, errNotificationScheduleUnavailable) {
		t.Fatalf("空 key error = %v", err)
	}
	runtime, err := newNotificationScheduleRuntime(t.Context(), &notificationScheduleStoreFake{found: true, value: "8-23"}, "notification_schedule")
	if err != nil || runtime == nil {
		t.Fatalf("构造 = (%#v, %v)", runtime, err)
	}
	if got := runtime.Schedule(nil); got != "8-23" {
		t.Fatalf("初始化后取值 = %#v", got)
	}
}

func TestNotificationScheduleRuntimeReload(t *testing.T) {
	t.Parallel()

	store := &notificationScheduleStoreFake{found: true, value: "8-23"}
	runtime, err := newNotificationScheduleRuntime(t.Context(), store, "notification_schedule")
	if err != nil {
		t.Fatalf("构造失败: %v", err)
	}
	if store.key != "notification_schedule" {
		t.Fatalf("读取的设置键 = %q", store.key)
	}

	// 设置更新：重新加载后立刻生效，不需要重建运行时。
	store.value = "0-24"
	if err := runtime.Reload(t.Context()); err != nil {
		t.Fatalf("Reload() error = %v", err)
	}
	if got := runtime.Schedule(nil); got != "0-24" {
		t.Fatalf("重载后取值 = %#v", got)
	}

	// 设置被删除：清空本地值，回落到调用方给的默认值。
	store.found = false
	if err := runtime.Reload(t.Context()); err != nil {
		t.Fatalf("Reload(缺失) error = %v", err)
	}
	fallbackCalls := 0
	fallback := func() any {
		fallbackCalls++
		return "默认时段"
	}
	if got := runtime.Schedule(fallback); got != "默认时段" {
		t.Fatalf("缺失时取值 = %#v", got)
	}
	if fallbackCalls != 1 {
		t.Fatalf("默认值调用次数 = %d", fallbackCalls)
	}

	// 重载失败保留上一次的可用值，不能让一轮数据库抖动把发送时段清空。
	store.found = true
	store.value = "9-22"
	if err := runtime.Reload(t.Context()); err != nil {
		t.Fatalf("Reload() error = %v", err)
	}
	store.err = errors.New("数据库不可用")
	if err := runtime.Reload(t.Context()); !errors.Is(err, store.err) {
		t.Fatalf("Reload() 错误 = %v", err)
	}
	if got := runtime.Schedule(nil); got != "9-22" {
		t.Fatalf("失败后取值 = %#v", got)
	}
}

func TestNotificationScheduleRuntimeReloadGuards(t *testing.T) {
	t.Parallel()

	store := &notificationScheduleStoreFake{found: true, value: "8-23"}
	runtime, err := newNotificationScheduleRuntime(t.Context(), store, "notification_schedule")
	if err != nil {
		t.Fatalf("构造失败: %v", err)
	}

	var nilRuntime *notificationScheduleRuntime
	if err := nilRuntime.Reload(t.Context()); !errors.Is(err, errNotificationScheduleUnavailable) {
		t.Fatalf("nil runtime error = %v", err)
	}
	if err := (&notificationScheduleRuntime{key: "k"}).Reload(t.Context()); !errors.Is(err, errNotificationScheduleUnavailable) {
		t.Fatalf("nil store error = %v", err)
	}
	if err := (&notificationScheduleRuntime{store: store}).Reload(t.Context()); !errors.Is(err, errNotificationScheduleUnavailable) {
		t.Fatalf("空 key error = %v", err)
	}
	if err := runtime.Reload(nil); !errors.Is(err, errNotificationScheduleUnavailable) {
		t.Fatalf("nil ctx error = %v", err)
	}
	// 校验失败不能顺手清掉已加载的值。
	if got := runtime.Schedule(nil); got != "8-23" {
		t.Fatalf("守卫失败后取值 = %#v", got)
	}
}

func TestNotificationScheduleRuntimeScheduleFallback(t *testing.T) {
	t.Parallel()

	var nilRuntime *notificationScheduleRuntime
	if got := nilRuntime.Schedule(nil); got != nil {
		t.Fatalf("nil 运行时且无默认值 = %#v", got)
	}
	if got := nilRuntime.Schedule(func() any { return "默认" }); got != "默认" {
		t.Fatalf("nil 运行时取值 = %#v", got)
	}

	// 有加载值时不调用默认值（默认值可能读数据库，不能白跑）。
	calls := 0
	runtime, err := newNotificationScheduleRuntime(t.Context(), &notificationScheduleStoreFake{found: true, value: "8-23"}, "k")
	if err != nil {
		t.Fatalf("构造失败: %v", err)
	}
	got := runtime.Schedule(func() any {
		calls++
		return "默认"
	})
	if got != "8-23" || calls != 0 {
		t.Fatalf("有值时 = %#v, 默认值调用 %d 次", got, calls)
	}
}
