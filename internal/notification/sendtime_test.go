package notification

import (
	"encoding/json"
	"testing"
	"time"
)

func at(hour, minute int) time.Time {
	return time.Date(2026, 11, 19, hour, minute, 0, 0, time.UTC)
}

func TestParseSendTimeScheduleAcceptsAllSources(t *testing.T) {
	want := SendTimePeriod{StartMinutes: 8 * 60, EndMinutes: 22*60 + 30}

	sources := map[string]any{
		"list":            []any{map[string]any{"start": "08:00", "end": "22:30"}},
		"单对象":             map[string]any{"start": "08:00", "end": "22:30"},
		"json 字符串":        `[{"start":"08:00","end":"22:30"}]`,
		"json 字节":         []byte(`[{"start":"08:00","end":"22:30"}]`),
		"json RawMessage": json.RawMessage(`[{"start":"08:00","end":"22:30"}]`),
		"带秒的时间":           []any{map[string]any{"start": "08:00:00", "end": "22:30:59"}},
	}
	for name, source := range sources {
		periods := ParseSendTimeSchedule(source)
		if len(periods) != 1 {
			t.Fatalf("%s: 应解析出 1 个时段，实际 %+v", name, periods)
		}
		if periods[0] != want {
			t.Fatalf("%s: 时段解析错误 %+v，期望 %+v", name, periods[0], want)
		}
	}
}

func TestParseSendTimeScheduleSkipsInvalidItems(t *testing.T) {
	periods := ParseSendTimeSchedule([]any{
		"不是对象",
		42,
		map[string]any{"start": "08:00"},
		map[string]any{"end": "22:30"},
		map[string]any{"start": "", "end": "22:30"},
		map[string]any{"start": "8-9", "end": "22:30"},
		map[string]any{"start": 800, "end": "22:30"},
		map[string]any{"start": "09:00", "end": "10:00"},
	})
	if len(periods) != 1 {
		t.Fatalf("只应保留 1 个合法时段，实际 %+v", periods)
	}
	if periods[0].StartMinutes != 9*60 || periods[0].EndMinutes != 10*60 {
		t.Fatalf("保留的时段不对: %+v", periods[0])
	}
}

func TestParseSendTimeScheduleEmptyInputs(t *testing.T) {
	for name, source := range map[string]any{
		"nil":    nil,
		"空串":     "",
		"空白串":    "   ",
		"空列表":    []any{},
		"坏 JSON": "{not json",
		"空字节":    []byte(""),
		"全部非法项":  []any{"x"},
	} {
		if periods := ParseSendTimeSchedule(source); len(periods) != 0 {
			t.Fatalf("%s: 应返回空时段，实际 %+v", name, periods)
		}
	}
}

func TestParseClockMinutes(t *testing.T) {
	valid := map[string]int{
		"08:00":     480,
		"8:5":       485,
		"23:59":     1439,
		"08:00:00":  480,
		"00:00":     0,
		" 09 : 30 ": 570,
	}
	for text, want := range valid {
		if got, ok := parseClockMinutes(text); !ok || got != want {
			t.Fatalf("parseClockMinutes(%q) = %d/%v，期望 %d", text, got, ok, want)
		}
	}
	for _, value := range []any{"0800", "ab:cd", "", ":30", "10:", 800, nil} {
		if got, ok := parseClockMinutes(value); ok {
			t.Fatalf("非法时间 %#v 不应通过，实际 %d", value, got)
		}
	}
}

func TestWithinSendTimeWithoutPeriodsAlwaysAllows(t *testing.T) {
	if !WithinSendTime(nil, at(3, 0)) {
		t.Fatal("没有配置时段时应始终允许发送")
	}
	if !WithinSendTime([]SendTimePeriod{}, at(3, 0)) {
		t.Fatal("空时段列表应始终允许发送")
	}
}

func TestWithinSendTimeSameDayPeriod(t *testing.T) {
	periods := []SendTimePeriod{{StartMinutes: 8 * 60, EndMinutes: 22*60 + 30}}
	cases := map[string]struct {
		at   time.Time
		want bool
	}{
		"起点整点":   {at(8, 0), true},
		"终点整分":   {at(22, 30), true},
		"中间":     {at(12, 0), true},
		"起点前一分钟": {at(7, 59), false},
		"终点后一分钟": {at(22, 31), false},
		"凌晨":     {at(0, 0), false},
	}
	for name, testCase := range cases {
		if got := WithinSendTime(periods, testCase.at); got != testCase.want {
			t.Fatalf("%s: WithinSendTime(%v) = %v，期望 %v", name, testCase.at, got, testCase.want)
		}
	}
}

func TestWithinSendTimeCrossMidnightPeriod(t *testing.T) {
	// 22:00–06:00 是跨午夜时段：判定为 current >= start 或 current <= end。
	periods := []SendTimePeriod{{StartMinutes: 22 * 60, EndMinutes: 6 * 60}}
	cases := map[string]struct {
		at   time.Time
		want bool
	}{
		"深夜":  {at(23, 0), true},
		"凌晨":  {at(3, 0), true},
		"终点":  {at(6, 0), true},
		"起点":  {at(22, 0), true},
		"白天":  {at(12, 0), false},
		"终点后": {at(6, 1), false},
	}
	for name, testCase := range cases {
		if got := WithinSendTime(periods, testCase.at); got != testCase.want {
			t.Fatalf("%s: WithinSendTime(%v) = %v，期望 %v", name, testCase.at, got, testCase.want)
		}
	}
}

func TestWithinSendTimeMultiplePeriodsAnyMatch(t *testing.T) {
	periods := []SendTimePeriod{
		{StartMinutes: 7 * 60, EndMinutes: 9 * 60},
		{StartMinutes: 20 * 60, EndMinutes: 22 * 60},
	}
	if !WithinSendTime(periods, at(8, 0)) || !WithinSendTime(periods, at(21, 0)) {
		t.Fatal("落在任一时段内即应允许")
	}
	if WithinSendTime(periods, at(12, 0)) {
		t.Fatal("落在所有时段之外应禁止")
	}
}
