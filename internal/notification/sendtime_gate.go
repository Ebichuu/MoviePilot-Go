package notification

import (
	"context"
	"errors"
	"log/slog"
	"sync"
	"time"
)

const (
	defaultSendTimeCheckInterval = 10 * time.Second
	defaultSendTimeQueueLimit    = 1024
	queuedSendTimeout            = 30 * time.Second
)

var errSendTimeGateUnavailable = errors.New("notification send-time gate unavailable")

// SendTimeGate 复刻 V2 `MessageQueueManager`（app/helper/message.py:602–786）：
// 在允许发送时段外把消息放入有界的进程内队列，由监控循环在时段开启后按顺序
// 发送；`Immediately` 或未配置时段时直接发送。
//
// 与原版一致，队列是进程内的，不落库、不跨重启保留；这是 R11 明确要求保留的
// “进程内延时队列语义”，不改成 River 持久延时。
type SendTimeGate struct {
	logger        *slog.Logger
	sender        Sender
	schedule      func() any
	checkInterval time.Duration
	limit         int
	now           func() time.Time
	location      *time.Location
	ctx           context.Context
	cancel        context.CancelFunc

	mu      sync.Mutex
	pending []Message

	stopOnce sync.Once
	doneCh   chan struct{}
}

var _ Sender = (*SendTimeGate)(nil)

// NewSendTimeGate 创建发送时段门禁。schedule 返回 V2 `NotificationSendTime`
// 原始配置值；返回 nil 表示不限制（始终允许）。
func NewSendTimeGate(logger *slog.Logger, sender Sender, schedule func() any, checkInterval time.Duration) *SendTimeGate {
	return NewSendTimeGateWithContext(logger, sender, schedule, checkInterval, time.Local, context.Background())
}

// NewSendTimeGateWithContext creates a gate whose clock uses location and
// whose queued deliveries are canceled with lifetime or Stop.
func NewSendTimeGateWithContext(
	logger *slog.Logger,
	sender Sender,
	schedule func() any,
	checkInterval time.Duration,
	location *time.Location,
	lifetime context.Context,
) *SendTimeGate {
	if logger == nil {
		logger = slog.Default()
	}
	if checkInterval <= 0 {
		checkInterval = defaultSendTimeCheckInterval
	}
	if schedule == nil {
		schedule = func() any { return nil }
	}
	if location == nil {
		location = time.Local
	}
	if lifetime == nil {
		lifetime = context.Background()
	}
	ctx, cancel := context.WithCancel(lifetime)
	gate := &SendTimeGate{
		logger:        logger,
		sender:        sender,
		schedule:      schedule,
		checkInterval: checkInterval,
		limit:         defaultSendTimeQueueLimit,
		now:           time.Now,
		location:      location,
		ctx:           ctx,
		cancel:        cancel,
		doneCh:        make(chan struct{}),
	}
	go gate.monitor()
	return gate
}

// Send 实现 V2 `send_message`：直接发送或入队。
func (g *SendTimeGate) Send(ctx context.Context, msg Message) error {
	if g == nil || g.sender == nil {
		return errSendTimeGateUnavailable
	}
	if ctx == nil {
		return errors.New("notification context is nil")
	}
	if err := g.ctx.Err(); err != nil {
		return err
	}
	if msg.Immediately || WithinSendTime(g.periods(), g.now().In(g.location)) {
		return g.sender.Send(ctx, msg)
	}
	if !g.enqueue(msg) {
		g.logger.WarnContext(ctx, "notification send-time queue full, message dropped",
			slog.String("type", string(msg.Type)),
			slog.String("title", msg.Title))
	}
	return nil
}

// Stop 停止监控循环并等待其退出，等价 V2 `MessageQueueManager.stop`。
func (g *SendTimeGate) Stop() {
	if g == nil {
		return
	}
	g.stopOnce.Do(g.cancel)
	<-g.doneCh
}

func (g *SendTimeGate) periods() []SendTimePeriod {
	return ParseSendTimeSchedule(g.schedule())
}

func (g *SendTimeGate) enqueue(msg Message) bool {
	g.mu.Lock()
	defer g.mu.Unlock()
	if len(g.pending) >= g.limit {
		return false
	}
	g.pending = append(g.pending, msg)
	return true
}

func (g *SendTimeGate) monitor() {
	defer close(g.doneCh)
	ticker := time.NewTicker(g.checkInterval)
	defer ticker.Stop()
	for {
		select {
		case <-g.ctx.Done():
			return
		case <-ticker.C:
			g.flush()
		}
	}
}

// flush 等价 V2 `_monitor_loop` 的内层循环：发送前与每条之间都重新检查时段，
// 一旦离开允许时段立即停止，剩余消息保留在队列中。
func (g *SendTimeGate) flush() {
	for {
		if !WithinSendTime(g.periods(), g.now().In(g.location)) || g.ctx.Err() != nil {
			return
		}
		msg, ok := g.pop()
		if !ok {
			return
		}
		g.sendQueued(msg)
	}
}

func (g *SendTimeGate) pop() (Message, bool) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if len(g.pending) == 0 {
		return Message{}, false
	}
	msg := g.pending[0]
	g.pending = g.pending[1:]
	return msg, true
}

func (g *SendTimeGate) sendQueued(msg Message) {
	ctx, cancel := context.WithTimeout(g.ctx, queuedSendTimeout)
	defer cancel()
	if err := g.sender.Send(ctx, msg); err != nil {
		g.logger.ErrorContext(ctx, "queued notification send failed",
			slog.String("type", string(msg.Type)),
			slog.String("title", msg.Title))
	}
}
