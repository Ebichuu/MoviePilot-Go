package logbuf

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"
)

const (
	defaultCapacity = 2000
	maxLineBytes    = 4096
)

// Buffer 是并发安全的进程内环形日志缓冲，同时支持实时订阅（供 Web SSE 推流）。
// 慢消费者会丢弃超出缓冲的新行，绝不阻塞业务写日志。
type Buffer struct {
	mu       sync.RWMutex
	lines    []string
	head     int
	count    int
	capacity int
	subs     map[uint64]chan string
	nextSub  uint64
}

// New 创建固定容量的内存日志缓冲。
func New(capacity int) *Buffer {
	if capacity <= 0 {
		capacity = defaultCapacity
	}
	return &Buffer{
		lines:    make([]string, capacity),
		capacity: capacity,
		subs:     make(map[uint64]chan string),
	}
}

// Append 追加一行日志并异步广播给所有实时订阅者。
func (b *Buffer) Append(line string) {
	if b == nil {
		return
	}
	line = strings.TrimRight(line, "\r\n")
	if line == "" {
		return
	}
	if len(line) > maxLineBytes {
		line = line[:maxLineBytes] + "..."
	}

	b.mu.Lock()
	if b.count < b.capacity {
		b.lines[(b.head+b.count)%b.capacity] = line
		b.count++
	} else {
		b.lines[b.head] = line
		b.head = (b.head + 1) % b.capacity
	}
	for _, ch := range b.subs {
		select {
		case ch <- line:
		default:
		}
	}
	b.mu.Unlock()
}

// Tail 返回最近 n 条日志（按时间升序）；n <= 0 时返回全部缓存日志。
func (b *Buffer) Tail(n int) []string {
	if b == nil {
		return nil
	}
	b.mu.RLock()
	defer b.mu.RUnlock()

	if b.count == 0 {
		return []string{}
	}
	take := b.count
	if n > 0 && n < take {
		take = n
	}
	start := (b.head + b.count - take) % b.capacity
	out := make([]string, take)
	for i := 0; i < take; i++ {
		out[i] = b.lines[(start+i)%b.capacity]
	}
	return out
}

// All 返回缓冲区内所有日志（按时间升序）。
func (b *Buffer) All() []string {
	return b.Tail(0)
}

// Subscribe 注册一个实时日志订阅通道并返回取消函数。
func (b *Buffer) Subscribe(bufSize int) (<-chan string, func()) {
	if bufSize <= 0 {
		bufSize = 128
	}
	ch := make(chan string, bufSize)
	if b == nil {
		close(ch)
		return ch, func() {}
	}
	b.mu.Lock()
	b.nextSub++
	id := b.nextSub
	b.subs[id] = ch
	b.mu.Unlock()

	var once sync.Once
	unsubscribe := func() {
		once.Do(func() {
			b.mu.Lock()
			if existing, ok := b.subs[id]; ok {
				delete(b.subs, id)
				close(existing)
			}
			b.mu.Unlock()
		})
	}
	return ch, unsubscribe
}

// Handler 包装底层 slog.Handler：透传原有输出的同时写入内存环形缓冲。
type Handler struct {
	downstream slog.Handler
	buf        *Buffer
	attrs      []slog.Attr
	group      string
}

// NewHandler 构造分流日志处理器。
func NewHandler(downstream slog.Handler, buf *Buffer) slog.Handler {
	if buf == nil {
		return downstream
	}
	return &Handler{downstream: downstream, buf: buf}
}

func (h *Handler) Enabled(ctx context.Context, level slog.Level) bool {
	if h.downstream != nil {
		return h.downstream.Enabled(ctx, level)
	}
	return true
}

func (h *Handler) Handle(ctx context.Context, record slog.Record) error {
	h.buf.Append(h.formatRecord(record))
	if h.downstream != nil {
		return h.downstream.Handle(ctx, record)
	}
	return nil
}

func (h *Handler) WithAttrs(attrs []slog.Attr) slog.Handler {
	combined := make([]slog.Attr, 0, len(h.attrs)+len(attrs))
	combined = append(combined, h.attrs...)
	combined = append(combined, attrs...)
	var down slog.Handler
	if h.downstream != nil {
		down = h.downstream.WithAttrs(attrs)
	}
	return &Handler{
		downstream: down,
		buf:        h.buf,
		attrs:      combined,
		group:      h.group,
	}
}

func (h *Handler) WithGroup(name string) slog.Handler {
	if name == "" {
		return h
	}
	nextGroup := name
	if h.group != "" {
		nextGroup = h.group + "." + name
	}
	var down slog.Handler
	if h.downstream != nil {
		down = h.downstream.WithGroup(name)
	}
	return &Handler{
		downstream: down,
		buf:        h.buf,
		attrs:      h.attrs,
		group:      nextGroup,
	}
}

func (h *Handler) formatRecord(record slog.Record) string {
	ts := record.Time
	if ts.IsZero() {
		ts = time.Now()
	}
	var sb strings.Builder
	sb.WriteString(ts.Format("2006-01-02 15:04:05"))
	sb.WriteString(" [")
	sb.WriteString(record.Level.String())
	sb.WriteString("] ")
	sb.WriteString(record.Message)

	writeAttr := func(attr slog.Attr) {
		attr.Value = attr.Value.Resolve()
		if attr.Equal(slog.Attr{}) {
			return
		}
		key := attr.Key
		if h.group != "" && key != "" {
			key = h.group + "." + key
		}
		sb.WriteByte(' ')
		if key != "" {
			sb.WriteString(key)
			sb.WriteByte('=')
		}
		sb.WriteString(fmt.Sprint(attr.Value.Any()))
	}

	for _, attr := range h.attrs {
		writeAttr(attr)
	}
	record.Attrs(func(attr slog.Attr) bool {
		writeAttr(attr)
		return true
	})
	return sb.String()
}
