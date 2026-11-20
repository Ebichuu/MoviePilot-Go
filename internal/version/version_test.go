package version

import (
	"strings"
	"testing"
)

// TestStringDefaults 未注入任何版本信息时应回退到 dev，且不带多余的括号。
func TestStringDefaults(t *testing.T) {
	restore := snapshot()
	defer restore()
	Version, Commit, BuildDate = "", "", ""
	if got := String(); got != "dev" {
		t.Fatalf("默认版本串 = %q，期望 dev", got)
	}
}

// TestStringInjected 注入版本、提交与构建时间后应拼成 "版本 (提交, 时间)"。
func TestStringInjected(t *testing.T) {
	restore := snapshot()
	defer restore()
	Version, Commit, BuildDate = "v1.2.3", "a1b2c3d", "2026-11-20T14:00:00Z"
	got := String()
	if got != "v1.2.3 (a1b2c3d, 2026-11-20T14:00:00Z)" {
		t.Fatalf("版本串 = %q", got)
	}
}

// TestStringPartial 只有版本号时不应输出 "unknown" 占位符。
func TestStringPartial(t *testing.T) {
	restore := snapshot()
	defer restore()
	Version, Commit, BuildDate = "v1.2.3", "unknown", "unknown"
	if got := String(); got != "v1.2.3" {
		t.Fatalf("版本串 = %q", got)
	}

	Version, Commit, BuildDate = "v1.2.3", "  ", "unknown"
	if got := String(); got != "v1.2.3" {
		t.Fatalf("空白提交号应被忽略，实际 %q", got)
	}

	Version, Commit, BuildDate = "v1.2.3", "a1b2c3d", "unknown"
	if got := String(); got != "v1.2.3 (a1b2c3d)" {
		t.Fatalf("只有提交号时 = %q", got)
	}
}

// TestModuleDoesNotPanic 在测试二进制里主模块版本通常是 (devel)，应返回空串而不是崩溃。
func TestModuleDoesNotPanic(t *testing.T) {
	if got := Module(); strings.Contains(got, "(devel)") {
		t.Fatalf("Module 不应返回 (devel)，实际 %q", got)
	}
}

func snapshot() func() {
	oldVersion, oldCommit, oldBuildDate := Version, Commit, BuildDate
	return func() { Version, Commit, BuildDate = oldVersion, oldCommit, oldBuildDate }
}
