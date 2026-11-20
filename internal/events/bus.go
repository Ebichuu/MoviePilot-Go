// Package events 提供一个进程内的轻量域事件总线。
//
// 用途：把「下载完成 / 整理完成 / 订阅完成」这类业务事实广播给多个消费者
// （统计落库、未来插件、外部集成），而不是让完成路径直接依赖每一个消费者。
// 设计取舍：
//   - 同步 Fan-out、有界缓冲、满了丢弃并记日志：事件是旁路，绝不允许阻塞主流程。
//   - 没有持久化：需要可靠投递的事实（如通知 outbox）仍走各自的数据库表。
package events

import (
	"context"
	"log/slog"
	"sync"
	"sync/atomic"
	"time"
)

// 业务事件类型。命名沿用「领域.动作」，便于后续扩展与过滤。
const (
	TypeDownloadCompleted     = "download.completed"
	TypeOrganizationCompleted = "organization.completed"
	TypeSubscribeCompleted    = "subscribe.completed"
)

// Event 是一条业务事实。Payload 只放完成时需要展示或统计的字段。
type Event struct {
	Type     string
	Username string
	At       time.Time
	Payload  map[string]any
}

// Handler 消费一条事件。
type Handler func(context.Context, Event)

type subscriber struct {
	name    string
	handler Handler
	ch      chan Event
}

// Bus 是进程内事件总线。零值不可用，请使用 New。
type Bus struct {
	logger   *slog.Logger
	queue    int
	mu       sync.RWMutex
	subs     []*subscriber
	closed   atomic.Bool
	dropped  atomic.Int64
	handled  atomic.Int64
	startWG  sync.WaitGroup
	stopOnce sync.Once
}

// New 创建总线。queue 是每个订阅者的缓冲长度，<=0 时取 64。
func New(logger *slog.Logger, queue int) *Bus {
	if logger == nil {
		logger = slog.Default()
	}
	if queue <= 0 {
		queue = 64
	}
	return &Bus{logger: logger, queue: queue}
}

// Subscribe 注册消费者。handler 为 nil 或不含 name 时忽略。
func (b *Bus) Subscribe(name string, handler Handler) {
	if b == nil || handler == nil || name == "" || b.closed.Load() {
		return
	}
	sub := &subscriber{name: name, handler: handler, ch: make(chan Event, b.queue)}
	b.mu.Lock()
	b.subs = append(b.subs, sub)
	b.mu.Unlock()
	b.startWG.Add(1)
	go b.run(sub)
}

func (b *Bus) run(sub *subscriber) {
	defer b.startWG.Done()
	for event := range sub.ch {
		func() {
			defer func() {
				if recovered := recover(); recovered != nil {
					b.logger.Error("事件消费者 panic", "subscriber", sub.name, "type", event.Type, "panic", recovered)
				}
			}()
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			sub.handler(ctx, event)
			b.handled.Add(1)
		}()
	}
}

// Publish 异步广播一条事件，永不阻塞调用方。
func (b *Bus) Publish(event Event) {
	if b == nil || b.closed.Load() {
		return
	}
	if event.At.IsZero() {
		event.At = time.Now().UTC()
	}
	b.mu.RLock()
	subs := b.subs
	b.mu.RUnlock()
	for _, sub := range subs {
		select {
		case sub.ch <- event:
		default:
			// 缓冲满说明消费者跟不上：丢弃并留证，避免拖慢下载/整理主流程。
			dropped := b.dropped.Add(1)
			b.logger.Warn("事件被丢弃：消费者过载", "subscriber", sub.name, "type", event.Type, "dropped", dropped)
		}
	}
}

// Close 停止接收新事件并等待消费者退出。
func (b *Bus) Close() {
	if b == nil {
		return
	}
	b.stopOnce.Do(func() {
		b.closed.Store(true)
		b.mu.Lock()
		subs := b.subs
		b.subs = nil
		b.mu.Unlock()
		for _, sub := range subs {
			close(sub.ch)
		}
		b.startWG.Wait()
	})
}

// Stats 返回已处理与已丢弃的事件数量，便于观测。
func (b *Bus) Stats() (handled, dropped int64) {
	if b == nil {
		return 0, 0
	}
	return b.handled.Load(), b.dropped.Load()
}
