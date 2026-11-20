package app

import (
	"bytes"
	"log/slog"
	"strings"
	"testing"
)

func TestBrushEnabledSwitch(t *testing.T) {
	cases := map[string]bool{
		"":      false,
		"0":     false,
		"false": false,
		"off":   false,
		"1":     true,
		"true":  true,
		"TRUE":  true,
		"yes":   true,
		"on":    true,
	}
	for value, want := range cases {
		t.Setenv("MOVIEPILOT_BRUSH_ENABLED", value)
		if got := brushEnabled(); got != want {
			t.Fatalf("brushEnabled() 在 %q 下返回 %v，期望 %v", value, got, want)
		}
	}
}

// 刷流只是占位开关：启动日志必须把「开了也没实现」说清楚，
// 否则用户会以为刷流已经在跑——这是本项目最容易被误读的状态。
func TestLogBrushSwitchExplainsPlaceholder(t *testing.T) {
	var buffer bytes.Buffer
	logger := slog.New(slog.NewJSONHandler(&buffer, &slog.HandlerOptions{Level: slog.LevelDebug}))

	t.Setenv("MOVIEPILOT_BRUSH_ENABLED", "")
	logBrushSwitch(logger)
	disabled := buffer.String()
	if strings.Contains(disabled, `"level":"WARN"`) {
		t.Fatalf("未启用时不应告警: %s", disabled)
	}
	if !strings.Contains(disabled, "刷流未启用") {
		t.Fatalf("未启用时应留下 debug 说明: %s", disabled)
	}

	buffer.Reset()
	t.Setenv("MOVIEPILOT_BRUSH_ENABLED", "true")
	logBrushSwitch(logger)
	enabled := buffer.String()
	if !strings.Contains(enabled, `"level":"WARN"`) {
		t.Fatalf("启用时必须告警: %s", enabled)
	}
	if !strings.Contains(enabled, "刷流能力尚未实现") {
		t.Fatalf("告警必须说明能力未实现: %s", enabled)
	}
	if strings.Contains(enabled, `"level":"INFO"`) {
		t.Fatalf("不应把占位开关记成正常启用: %s", enabled)
	}

	// 没有 logger 时不能 panic（早期启动阶段可能还没初始化日志）。
	logBrushSwitch(nil)
}
