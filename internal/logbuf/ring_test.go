package logbuf

import (
	"bytes"
	"log/slog"
	"strings"
	"testing"
	"time"
)

func TestBufferRingTailAndSubscribe(t *testing.T) {
	t.Parallel()

	buf := New(3)
	buf.Append("line-1")
	buf.Append("line-2")
	buf.Append("line-3")
	buf.Append("line-4")

	all := buf.All()
	if len(all) != 3 || all[0] != "line-2" || all[2] != "line-4" {
		t.Fatalf("All() = %#v, want [line-2 line-3 line-4]", all)
	}
	tail := buf.Tail(2)
	if len(tail) != 2 || tail[0] != "line-3" || tail[1] != "line-4" {
		t.Fatalf("Tail(2) = %#v, want [line-3 line-4]", tail)
	}

	ch, unsub := buf.Subscribe(4)
	defer unsub()

	buf.Append("live-5")
	select {
	case got := <-ch:
		if got != "live-5" {
			t.Fatalf("live line = %q, want live-5", got)
		}
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for live log")
	}
}

func TestHandlerTeesToDownstreamAndBuffer(t *testing.T) {
	t.Parallel()

	var stdout bytes.Buffer
	base := slog.NewJSONHandler(&stdout, &slog.HandlerOptions{Level: slog.LevelInfo})
	buf := New(16)
	logger := slog.New(NewHandler(base, buf)).With("component", "test")

	logger.Info("服务启动", "port", 3000)

	if !strings.Contains(stdout.String(), `"msg":"服务启动"`) {
		t.Fatalf("downstream stdout missing message: %s", stdout.String())
	}
	lines := buf.All()
	if len(lines) != 1 {
		t.Fatalf("len(lines) = %d, want 1", len(lines))
	}
	if !strings.Contains(lines[0], "[INFO] 服务启动") || !strings.Contains(lines[0], "component=test") || !strings.Contains(lines[0], "port=3000") {
		t.Fatalf("formatted line = %q", lines[0])
	}
}
