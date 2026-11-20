package notification

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"
)

type gateSender struct {
	mu   sync.Mutex
	sent []Message
	err  error
}

func (s *gateSender) Send(_ context.Context, msg Message) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.err != nil {
		return s.err
	}
	s.sent = append(s.sent, msg)
	return nil
}

func (s *gateSender) count() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.sent)
}

func (s *gateSender) titles() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]string, 0, len(s.sent))
	for _, msg := range s.sent {
		out = append(out, msg.Title)
	}
	return out
}

// windowSchedule 构造 09:00–10:00 的允许时段。
func windowSchedule() func() any {
	return func() any {
		return []any{map[string]any{"start": "09:00:00", "end": "10:00:00"}}
	}
}

func newTestGate(t *testing.T, sender Sender, schedule func() any, now func() time.Time) *SendTimeGate {
	t.Helper()
	gate := NewSendTimeGateWithContext(
		slog.New(slog.NewTextHandler(&bytes.Buffer{}, nil)),
		sender, schedule, time.Hour, time.Local, context.Background(),
	)
	gate.now = now
	t.Cleanup(gate.Stop)
	return gate
}

func localClock(hour, minute int) func() time.Time {
	return func() time.Time {
		return time.Date(2026, 11, 19, hour, minute, 0, 0, time.Local)
	}
}

func TestSendTimeGateSendsImmediatelyWithoutSchedule(t *testing.T) {
	sender := &gateSender{}
	gate := newTestGate(t, sender, func() any { return nil }, localClock(3, 0))

	if err := gate.Send(context.Background(), Message{Title: "无时段限制"}); err != nil {
		t.Fatalf("发送失败: %v", err)
	}
	if sender.count() != 1 {
		t.Fatalf("没有时段限制时应立即发送，实际 %d 条", sender.count())
	}
	if pending := len(gate.pending); pending != 0 {
		t.Fatalf("不应入队，实际 %d 条", pending)
	}
}

func TestSendTimeGateQueuesOutsideWindow(t *testing.T) {
	sender := &gateSender{}
	gate := newTestGate(t, sender, windowSchedule(), localClock(3, 0))

	if err := gate.Send(context.Background(), Message{Title: "夜间消息"}); err != nil {
		t.Fatalf("入队不应报错: %v", err)
	}
	if sender.count() != 0 {
		t.Fatal("允许时段外不应立即发送")
	}
	if pending := len(gate.pending); pending != 1 {
		t.Fatalf("应入队 1 条，实际 %d 条", pending)
	}

	// 进入允许时段后，flush 必须把队列发出去。
	gate.now = localClock(9, 30)
	gate.flush()
	if sender.count() != 1 {
		t.Fatalf("时段开启后应发送 1 条，实际 %d 条", sender.count())
	}
	if pending := len(gate.pending); pending != 0 {
		t.Fatalf("发送后队列应清空，实际 %d 条", pending)
	}
}

func TestSendTimeGateImmediatelyBypassesWindow(t *testing.T) {
	sender := &gateSender{}
	gate := newTestGate(t, sender, windowSchedule(), localClock(3, 0))

	if err := gate.Send(context.Background(), Message{Title: "紧急", Immediately: true}); err != nil {
		t.Fatalf("发送失败: %v", err)
	}
	if sender.count() != 1 {
		t.Fatal("Immediately 必须绕过时段限制")
	}
}

func TestSendTimeGateDropsWhenQueueFull(t *testing.T) {
	var logs bytes.Buffer
	sender := &gateSender{}
	gate := NewSendTimeGateWithContext(
		slog.New(slog.NewJSONHandler(&logs, &slog.HandlerOptions{Level: slog.LevelDebug})),
		sender, windowSchedule(), time.Hour, time.Local, context.Background(),
	)
	gate.now = localClock(3, 0)
	gate.limit = 1
	t.Cleanup(gate.Stop)

	for index := 0; index < 3; index++ {
		if err := gate.Send(context.Background(), Message{Title: "第" + string(rune('A'+index)) + "条"}); err != nil {
			t.Fatalf("队列满也不应向调用方报错（本实现的防御性语义：丢弃并告警，V2 是无界队列不丢弃）: %v", err)
		}
	}
	if pending := len(gate.pending); pending != 1 {
		t.Fatalf("队列上限 1，实际 %d 条", pending)
	}
	if !strings.Contains(logs.String(), "queue full") {
		t.Fatalf("丢弃时必须告警，实际日志: %s", logs.String())
	}
}

func TestSendTimeGateFlushStopsWhenWindowCloses(t *testing.T) {
	sender := &gateSender{}
	gate := newTestGate(t, sender, windowSchedule(), localClock(3, 0))

	if err := gate.Send(context.Background(), Message{Title: "一"}); err != nil {
		t.Fatalf("入队失败: %v", err)
	}
	if err := gate.Send(context.Background(), Message{Title: "二"}); err != nil {
		t.Fatalf("入队失败: %v", err)
	}
	if pending := len(gate.pending); pending != 2 {
		t.Fatalf("应入队 2 条，实际 %d 条", pending)
	}

	// 第一次检查在时段内、第二次已在时段外：只允许发一条，剩余保留。
	calls := 0
	gate.now = func() time.Time {
		calls++
		if calls <= 1 {
			return time.Date(2026, 11, 19, 9, 59, 0, 0, time.Local)
		}
		return time.Date(2026, 11, 19, 10, 1, 0, 0, time.Local)
	}
	gate.flush()

	if sender.count() != 1 {
		t.Fatalf("离开时段后必须立即停止发送，实际发了 %d 条", sender.count())
	}
	if pending := len(gate.pending); pending != 1 {
		t.Fatalf("剩余消息应保留在队列中，实际 %d 条", pending)
	}
}

func TestSendTimeGateGuards(t *testing.T) {
	var nilGate *SendTimeGate
	if err := nilGate.Send(context.Background(), Message{}); !errors.Is(err, errSendTimeGateUnavailable) {
		t.Fatalf("nil gate 应返回不可用错误，实际 %v", err)
	}
	nilGate.Stop() // 必须安全

	gate := newTestGate(t, nil, func() any { return nil }, time.Now)
	if err := gate.Send(context.Background(), Message{}); !errors.Is(err, errSendTimeGateUnavailable) {
		t.Fatalf("没有底层发送器时应返回不可用错误，实际 %v", err)
	}

	sender := &gateSender{}
	gate = newTestGate(t, sender, func() any { return nil }, time.Now)
	if err := gate.Send(nil, Message{}); err == nil {
		t.Fatal("nil context 应被拒绝")
	}
	gate.Stop()
	if err := gate.Send(context.Background(), Message{}); err == nil {
		t.Fatal("Stop 之后不应再接受消息")
	}
}

func TestSendTimeGateQueuedSendFailureIsLogged(t *testing.T) {
	var logs bytes.Buffer
	sender := &gateSender{err: errors.New("smtp down")}
	gate := NewSendTimeGateWithContext(
		slog.New(slog.NewJSONHandler(&logs, &slog.HandlerOptions{Level: slog.LevelDebug})),
		sender, windowSchedule(), time.Hour, time.Local, context.Background(),
	)
	gate.now = localClock(3, 0)
	t.Cleanup(gate.Stop)

	if err := gate.Send(context.Background(), Message{Title: "失败消息"}); err != nil {
		t.Fatalf("入队失败: %v", err)
	}
	gate.now = localClock(9, 30)
	gate.flush()

	// 发送失败不能把消息塞回队列（V2 语义：丢弃并记 error 日志）。
	if pending := len(gate.pending); pending != 0 {
		t.Fatalf("发送失败后不应重排，实际 %d 条", pending)
	}
	if !strings.Contains(logs.String(), "queued notification send failed") {
		t.Fatalf("发送失败必须记录 error 日志，实际: %s", logs.String())
	}
}

func TestNewSendTimeGateDefaults(t *testing.T) {
	gate := NewSendTimeGate(nil, &gateSender{}, nil, 0)
	t.Cleanup(gate.Stop)

	if gate.checkInterval != defaultSendTimeCheckInterval {
		t.Fatalf("非正检查间隔应回落到默认值，实际 %v", gate.checkInterval)
	}
	if gate.logger == nil {
		t.Fatal("nil logger 应回落到默认 logger")
	}
	if gate.limit != defaultSendTimeQueueLimit {
		t.Fatalf("默认队列上限 = %d", gate.limit)
	}
	// 缺省 schedule 视为不限制：直接发送。
	if err := gate.Send(context.Background(), Message{Title: "直接发"}); err != nil {
		t.Fatalf("发送失败: %v", err)
	}
}
