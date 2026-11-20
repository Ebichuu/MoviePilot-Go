package downloader

import (
	"math"
	"testing"

	"github.com/autobrr/go-qbittorrent"
)

func TestNormalizeQBTorrentState(t *testing.T) {
	cases := []struct {
		state    qbittorrent.TorrentState
		progress float64
		want     string
	}{
		// 进度到 100% 时，无论 qB 报什么状态都算完成。
		{"downloading", 1, TaskStateCompleted},
		{"stalledDL", 1.0, TaskStateCompleted},
		{"pausedDL", 1, TaskStateCompleted},
		{"paused", 0.4, TaskStatePaused},
		{"PausedDL", 0.4, TaskStatePaused},
		{"stoppedUP", 0.4, TaskStatePaused},
		{"allocating", 0.1, TaskStateDownloading},
		{"checkingDL", 0.1, TaskStateDownloading},
		{" forcedDL ", 0.1, TaskStateDownloading},
		{"stalledDL", 0.1, TaskStateDownloading},
		{"uploading", 0.9, TaskStateCompleted},
		{"forcedUP", 0.9, TaskStateCompleted},
		{"queuedUP", 0.9, TaskStateCompleted},
		// 未知状态按「下载中」处理，避免任务在 UI 上消失。
		{"unknown-state", 0.5, TaskStateDownloading},
		// NaN 进度不应命中完成分支。
		{"downloading", math.NaN(), TaskStateDownloading},
	}
	for _, tc := range cases {
		if got := normalizeQBTorrentState(tc.state, tc.progress); got != tc.want {
			t.Fatalf("normalizeQBTorrentState(%q, %v) = %q，期望 %q", tc.state, tc.progress, got, tc.want)
		}
	}
}

func TestObserveQBTorrentState(t *testing.T) {
	cases := []struct {
		state    qbittorrent.TorrentState
		progress float64
		want     TaskObservationState
	}{
		{"error", 0.5, TaskObservationError},
		{"missingFiles", 0.5, TaskObservationMissing},
		{"downloading", 1, TaskObservationCompleted},
		{"downloading", 0.5, TaskObservationActive},
		{"checkingDL", 0.2, TaskObservationActive},
		{"moving", 0.9, TaskObservationActive},
		{"pausedDL", 1, TaskObservationCompleted},
		{"pausedDL", 0.5, TaskObservationPaused},
		{"stoppedDL", 0.5, TaskObservationPaused},
		{"uploading", 0.9, TaskObservationCompleted},
		{"stalledUP", 0.9, TaskObservationCompleted},
		{"weird", 0.5, TaskObservationUnknown},
	}
	for _, tc := range cases {
		if got := observeQBTorrentState(tc.state, tc.progress); got != tc.want {
			t.Fatalf("observeQBTorrentState(%q, %v) = %q，期望 %q", tc.state, tc.progress, got, tc.want)
		}
	}
}

func TestProgressPercent(t *testing.T) {
	cases := []struct {
		progress float64
		want     float64
	}{
		{math.NaN(), 0},
		{-0.5, 0},
		{0, 0},
		{0.5, 50},
		{0.125, 12.5},
		{1, 100},
		{1.5, 100},
	}
	for _, tc := range cases {
		if got := progressPercent(tc.progress); got != tc.want {
			t.Fatalf("progressPercent(%v) = %v，期望 %v", tc.progress, got, tc.want)
		}
	}
}

func TestMoviePilotTaskLabel(t *testing.T) {
	valid := "mp-0123456789abcdef0123456789abcdef"
	label, ok := moviePilotTaskLabel("普通标签, " + valid + " ,另一个")
	if !ok || label != valid {
		t.Fatalf("应能从标签串中挑出任务标签: %q ok=%v", label, ok)
	}
	if _, ok := moviePilotTaskLabel("普通标签"); ok {
		t.Fatal("没有 mp- 前缀时不应命中")
	}
	// 大写十六进制与大写前缀都不算任务标签（V2 只认小写）。
	if _, ok := moviePilotTaskLabel("mp-0123456789ABCDEF0123456789ABCDEF"); ok {
		t.Fatal("大写十六进制不应命中")
	}
	if _, ok := moviePilotTaskLabel("MP-0123456789abcdef0123456789abcdef"); ok {
		t.Fatal("大写前缀不应命中")
	}
	if _, ok := moviePilotTaskLabel(""); ok {
		t.Fatal("空串不应命中")
	}
}

func TestSafeTaskContentRelativePathRejectsEscapes(t *testing.T) {
	rejected := []string{"../outside.mkv", "/etc/passwd", `C:\Windows\notepad.exe`, ""}
	for _, value := range rejected {
		if _, ok := safeTaskContentRelativePath(value); ok {
			t.Fatalf("%q 不应被接受为任务内容相对路径", value)
		}
	}
	for _, value := range []string{"Season 01/Show.S01E01.mkv", "Show.S01E01.mkv"} {
		relative, ok := safeTaskContentRelativePath(value)
		if !ok {
			t.Fatalf("%q 应当被接受", value)
		}
		if relative == "" {
			t.Fatalf("%q 归一化后不应为空", value)
		}
	}
}
