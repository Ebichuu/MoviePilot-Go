//go:build windows

package app

import (
	"math"
	"testing"
	"time"
)

// 这些测试只在 Windows 上跑：它们验证 Win32 调用确实取到了本机数字，
// 而不是「编译通过」。Linux 上 /proc 读取器由 dashboard_proc_paths_test.go 覆盖。

func TestHostMemoryInfoOnWindows(t *testing.T) {
	total, available, cached, err := hostMemoryInfo()
	if err != nil {
		t.Fatalf("hostMemoryInfo() error = %v", err)
	}
	if total <= 0 {
		t.Fatalf("物理内存总量 = %d，应大于 0", total)
	}
	if available < 0 || available > total {
		t.Fatalf("可用内存 = %d，应在 [0, %d]", available, total)
	}
	// Windows 没有 Linux Cached 的对应口径，这里明确是 0，不是「没读到」。
	if cached != 0 {
		t.Fatalf("cached = %d，Windows 上应为 0", cached)
	}
}

func TestHostCPUSampleOnWindows(t *testing.T) {
	first, err := hostCPUSample()
	if err != nil {
		t.Fatalf("hostCPUSample() error = %v", err)
	}
	if first.idle > first.total {
		t.Fatalf("第一次采样 idle = %d 大于 total = %d", first.idle, first.total)
	}
	time.Sleep(50 * time.Millisecond)
	second, err := hostCPUSample()
	if err != nil {
		t.Fatalf("第二次 hostCPUSample() error = %v", err)
	}
	if second.total <= first.total {
		t.Fatalf("累计时间没有增长: %d → %d", first.total, second.total)
	}
	percent := cpuPercent(first, second)
	if percent < 0 || percent > 100 {
		t.Fatalf("占用率 = %v，应在 [0,100]", percent)
	}
}

func TestHostNetSampleOnWindows(t *testing.T) {
	sample, err := hostNetSample()
	if err != nil {
		t.Fatalf("hostNetSample() error = %v", err)
	}
	if sample.rx < 0 || sample.tx < 0 {
		t.Fatalf("收发字节数不应为负: %+v", sample)
	}
}

// Memory 的 used 是**本进程 RSS**（V2 的 psutil.Process().memory_info().rss），
// total/available 才是整机口径，cached 在 Windows 上恒为 0（psutil 也没有该口径）。
func TestDashboardMemoryOnWindows(t *testing.T) {
	dashboard := newDashboardRuntime(nil, nil, time.Now())
	memory, err := dashboard.Memory(t.Context())
	if err != nil {
		t.Fatalf("Memory() error = %v", err)
	}
	if memory.Total <= 0 || memory.Available <= 0 || memory.Available >= memory.Total {
		t.Fatalf("整机内存口径异常: %+v", memory)
	}
	if memory.Cached != 0 {
		t.Fatalf("Windows 上 cached 应为 0，实际 %d", memory.Cached)
	}
	rss, rssErr := processRSSBytes()
	if rssErr != nil {
		t.Fatalf("processRSSBytes() error = %v", rssErr)
	}
	if rss <= 0 || rss > memory.Total {
		t.Fatalf("本进程 RSS 异常: %d", rss)
	}
	// used 就是本进程 RSS，两次读取之间允许有几 MB 抖动。
	if diff := memory.Used - rss; diff > 64<<20 || diff < -(64<<20) {
		t.Fatalf("used=%d 与进程 RSS=%d 相差过大", memory.Used, rss)
	}
	if want := float64(memory.Used) / float64(memory.Total) * 100; math.Abs(memory.Usage-want) > 1e-9 {
		t.Fatalf("占用率 = %v，期望 %v", memory.Usage, want)
	}
}

// CPU 的第一次调用要在 dashboardCPUWindow 内采样两次，这里确认它真的给出可用占用率。
func TestDashboardCPUOnWindows(t *testing.T) {
	dashboard := newDashboardRuntime(nil, nil, time.Now())
	percent, err := dashboard.CPU(t.Context())
	if err != nil {
		t.Fatalf("CPU() error = %v", err)
	}
	if percent < 0 || percent > 100 {
		t.Fatalf("CPU 占用率 = %v，应在 [0,100]", percent)
	}
	if !dashboard.hasCPUSample {
		t.Fatal("采样后必须留下基线，否则下次会重复等窗口")
	}
}

// 首次 Network 只建基线（0/0），第二次才给出速率——这个语义在 Windows 上必须一样。
func TestDashboardNetworkOnWindows(t *testing.T) {
	dashboard := newDashboardRuntime(nil, nil, time.Now())
	first, firstDown, err := dashboard.Network(t.Context())
	if err != nil {
		t.Fatalf("首次 Network() error = %v", err)
	}
	if first != 0 || firstDown != 0 {
		t.Fatalf("首次采样应返回 0/0，实际 %d/%d", first, firstDown)
	}
	upload, download, err := dashboard.Network(t.Context())
	if err != nil {
		t.Fatalf("二次 Network() error = %v", err)
	}
	if upload < 0 || download < 0 {
		t.Fatalf("速率不应为负: %d/%d", upload, download)
	}
}

func TestDashboardSystemOnWindows(t *testing.T) {
	dashboard := newDashboardRuntime(nil, nil, time.Now())
	system, err := dashboard.System(t.Context())
	if err != nil {
		t.Fatalf("System() error = %v", err)
	}
	if system.Hostname == "" {
		t.Fatal("主机名不应为空")
	}
	if system.OperatingSystem != "windows" {
		t.Fatalf("operating_system = %q，没有 /etc/os-release 时应回落到平台名", system.OperatingSystem)
	}
	// version 由 httpapi 层补，app 侧不填；这里只校验本机读到的部分。
	if system.RuntimeSeconds < 0 {
		t.Fatalf("system = %+v", system)
	}
}
