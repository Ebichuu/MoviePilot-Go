package app

import (
	"context"
	"runtime"
	"testing"
)

// 这些读取器直接读 /proc 与 /etc/os-release，只在 Linux 上成立；
// 其它平台跳过而不是造一个假结论。
func requireLinux(t *testing.T) {
	t.Helper()
	if runtime.GOOS != "linux" {
		t.Skipf("依赖 /proc 的读取器在 %s 上不适用", runtime.GOOS)
	}
}

func TestReadOperatingSystemReturnsName(t *testing.T) {
	requireLinux(t)
	name := readOperatingSystem()
	if name == "" {
		t.Fatal("应返回可读的系统名称，而不是空串")
	}
}

func TestReadMemoryInfoConsistent(t *testing.T) {
	requireLinux(t)
	total, available, cached, err := readMemoryInfo()
	if err != nil {
		t.Fatalf("读取内存信息失败: %v", err)
	}
	if total <= 0 {
		t.Fatalf("总内存应为正数，实际 %d", total)
	}
	if available < 0 || available > total {
		t.Fatalf("可用内存越界: %d / %d", available, total)
	}
	if cached < 0 || cached > total {
		t.Fatalf("缓存内存越界: %d / %d", cached, total)
	}
}

func TestReadCPUSampleAdvances(t *testing.T) {
	requireLinux(t)
	first, err := readCPUSample()
	if err != nil {
		t.Fatalf("首次采样失败: %v", err)
	}
	if first.total <= 0 {
		t.Fatalf("总 CPU tick 应为正数，实际 %d", first.total)
	}
	if first.idle < 0 || first.idle > first.total {
		t.Fatalf("空闲 tick 越界: %d / %d", first.idle, first.total)
	}
	second, err := readCPUSample()
	if err != nil {
		t.Fatalf("二次采样失败: %v", err)
	}
	// 只断言单调不减：容器被限流时也不该出现负增长。
	if second.total < first.total {
		t.Fatalf("总 tick 出现回退: %d -> %d", first.total, second.total)
	}
}

func TestReadNetSampleNonNegative(t *testing.T) {
	requireLinux(t)
	sample, err := readNetSample()
	if err != nil {
		t.Fatalf("读取网络计数失败: %v", err)
	}
	if sample.rx < 0 || sample.tx < 0 {
		t.Fatalf("收发字节不应为负: %+v", sample)
	}
}

func TestDashboardRuntimeMemoryAndCPU(t *testing.T) {
	requireLinux(t)
	r := &dashboardRuntime{}

	memory, err := r.Memory(context.Background())
	if err != nil {
		t.Fatalf("Memory 失败: %v", err)
	}
	if memory.Total <= 0 || memory.Used <= 0 {
		t.Fatalf("内存字段异常: %+v", memory)
	}
	if memory.Usage <= 0 || memory.Usage > 100 {
		t.Fatalf("内存占用率越界: %v", memory.Usage)
	}

	// 首次调用会在 dashboardCPUWindow 内采样两次，直接给出真实占用率
	// （而不是先用累计 tick 冒充瞬时值）。
	first, err := r.CPU(context.Background())
	if err != nil {
		t.Fatalf("首次 CPU 采样失败: %v", err)
	}
	if first < 0 || first > 100 {
		t.Fatalf("首次 CPU 占用率越界: %v", first)
	}
	if !r.hasCPUSample {
		t.Fatal("首次采样后必须记录基线，否则下次会重复等窗口")
	}
	second, err := r.CPU(context.Background())
	if err != nil {
		t.Fatalf("二次 CPU 采样失败: %v", err)
	}
	if second < 0 || second > 100 {
		t.Fatalf("CPU 占用率越界: %v", second)
	}
}

func TestDashboardRuntimeNetworkBaseline(t *testing.T) {
	requireLinux(t)
	r := &dashboardRuntime{}

	// 首次调用只建基线，返回 0/0；否则会把开机以来的累计流量当成瞬时速率。
	upload, download, err := r.Network(context.Background())
	if err != nil {
		t.Fatalf("Network 失败: %v", err)
	}
	if upload != 0 || download != 0 {
		t.Fatalf("首次采样应返回 0/0，实际 %d/%d", upload, download)
	}
	upload, download, err = r.Network(context.Background())
	if err != nil {
		t.Fatalf("二次 Network 失败: %v", err)
	}
	if upload < 0 || download < 0 {
		t.Fatalf("速率不应为负: %d/%d", upload, download)
	}
}

func TestDashboardRuntimeSystemReadable(t *testing.T) {
	requireLinux(t)
	r := &dashboardRuntime{}
	if _, err := r.System(context.Background()); err != nil {
		t.Fatalf("System 失败: %v", err)
	}
}
