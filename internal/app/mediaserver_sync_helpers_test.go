package app

import (
	"testing"
	"time"

	"github.com/moviepilot-go/moviepilot-go/internal/mediaconfig"
)

// TestMediaServerEntryMatchesSelector 覆盖按 ID / 名称挑选服务器的三种命中方式与 nil 输入。
func TestMediaServerEntryMatchesSelector(t *testing.T) {
	entry := &mediaServerEntry{setting: mediaconfig.Server{ID: "2cc1444f-854d-4adb-8cd8-2a6bb63585df", Name: "Home"}}
	cases := []struct {
		selector string
		want     bool
	}{
		{"2cc1444f-854d-4adb-8cd8-2a6bb63585df", true},
		{"Home", true},
		{"home", true},
		{" Home ", false}, // 选择器本身不做裁剪
		{"other", false},
		{"", false},
	}
	for _, testCase := range cases {
		if got := mediaServerEntryMatchesSelector(entry, testCase.selector); got != testCase.want {
			t.Fatalf("mediaServerEntryMatchesSelector(%q) = %v，期望 %v", testCase.selector, got, testCase.want)
		}
	}
	// ID 必须精确匹配（大小写不敏感只用于名称）。
	if mediaServerEntryMatchesSelector(entry, "2CC1444F-854D-4ADB-8CD8-2A6BB63585DF") {
		t.Fatal("ID 匹配应区分大小写")
	}
	if mediaServerEntryMatchesSelector(nil, "home") {
		t.Fatal("nil 条目不应命中")
	}
}

// TestMediaServerRuntimeTimeAndLogFallbacks 覆盖 nil 运行态与缺省时钟/日志时的兜底。
func TestMediaServerRuntimeTimeAndLogFallbacks(t *testing.T) {
	var nilRuntime *mediaServerRuntime
	if nilRuntime.currentTime().IsZero() {
		t.Fatal("nil 运行态应回退到 time.Now()")
	}
	// 缺省 logger 时日志调用必须是空操作。
	nilRuntime.logWarn("ignored", "key", "value")
	(&mediaServerRuntime{}).logWarn("ignored")

	fixed := time.Date(2026, 5, 1, 12, 0, 0, 0, time.UTC)
	runtime := &mediaServerRuntime{now: func() time.Time { return fixed }}
	if got := runtime.currentTime(); !got.Equal(fixed) {
		t.Fatalf("currentTime() = %v，期望 %v", got, fixed)
	}
}
