package app

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"time"
)

// procFixture 用临时目录里的假文件替换五个本机数据源路径。写文件即同时把路径
// 指向它，remove 则让路径指向一个不存在的文件，从而覆盖「读不到文件」的分支。
type procFixture struct {
	t    *testing.T
	root string
}

func newProcFixture(t *testing.T) *procFixture {
	t.Helper()
	requireLinux(t)
	previous := dashboardProcFiles
	t.Cleanup(func() { dashboardProcFiles = previous })
	return &procFixture{t: t, root: t.TempDir()}
}

func (f *procFixture) path(name string) string {
	return filepath.Join(f.root, name)
}

func (f *procFixture) write(name, content string) {
	f.t.Helper()
	path := f.path(name)
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		f.t.Fatalf("写入 %s: %v", path, err)
	}
	f.bind(name, path)
}

func (f *procFixture) remove(name string) {
	f.t.Helper()
	path := f.path(name)
	_ = os.Remove(path)
	f.bind(name, path)
}

func (f *procFixture) bind(name, path string) {
	switch name {
	case "meminfo":
		dashboardProcFiles.meminfo = path
	case "status":
		f.t.Fatalf("status 已不再作为本机数据源")
	case "stat":
		dashboardProcFiles.stat = path
	case "netDev":
		dashboardProcFiles.netDev = path
	case "osRelease":
		dashboardProcFiles.osRelease = path
	default:
		f.t.Fatalf("未知本机数据源 %q", name)
	}
}

// TestDashboardProcReadersInFixtureFiles 在可控的假数据源下校验四个读取器的解析口径，
// 以及 Memory/CPU/Network/System 的取值（含两次采样之间的增量与速率折算）。
func TestDashboardProcReadersInFixtureFiles(t *testing.T) {
	fixture := newProcFixture(t)
	fixture.write("meminfo",
		"MemTotal:        2048000 kB\n"+
			"MemFree:          512000 kB\n"+
			"MemAvailable:    1024000 kB\n"+
			"Cached:           256000 kB\n"+
			"Buffers:           51200 kB\n")
	fixture.write("stat",
		"cpu  100 20 30 400 50 10 5 0 0 0\ncpu0 100 20 30 400 50 10 5 0 0 0\n")
	fixture.write("netDev",
		"Inter-|   Receive                                                |  Transmit\n"+
			" face |bytes    packets errs drop fifo frame compressed multicast|bytes    packets errs drop fifo colls carrier compressed\n"+
			"    lo:  1000      10    0    0    0     0          0         0   1000      10    0    0    0     0       0          0\n"+
			"  eth0:  5000      50    0    0    0     0          0         0   7000      70    0    0    0     0       0          0\n"+
			"  eth1:   100       1    0    0    0     0          0         0    200       2    0    0    0     0       0          0\n"+
			"junk line without colon\n"+
			"  bad0:  1 2 3\n"+
			"  bad1:  x 2 3 4 5 6 7 8 9\n")
	fixture.write("osRelease", "NAME=\"TestOS\"\nPRETTY_NAME=\"Test OS 1.0\"\nID=testos\n")

	total, available, cached, err := readMemoryInfo()
	if err != nil {
		t.Fatalf("readMemoryInfo() error = %v", err)
	}
	if total != 2048000*1024 || available != 1024000*1024 || cached != (256000+51200)*1024 {
		t.Fatalf("内存读取 = (%d, %d, %d)", total, available, cached)
	}

	sample, err := readCPUSample()
	if err != nil {
		t.Fatalf("readCPUSample() error = %v", err)
	}
	if sample.total != 615 || sample.idle != 450 {
		t.Fatalf("CPU 采样 = %+v，期望 {total:615 idle:450}", sample)
	}

	net, err := readNetSample()
	if err != nil {
		t.Fatalf("readNetSample() error = %v", err)
	}
	if net.rx != 5100 || net.tx != 7200 {
		t.Fatalf("网络采样 = %+v，期望 {rx:5100 tx:7200}", net)
	}

	if got := readOperatingSystem(); got != "Test OS 1.0" {
		t.Fatalf("readOperatingSystem() = %q，期望 Test OS 1.0", got)
	}

	// Memory 的 used 是**本进程 RSS**（V2 用 psutil.Process().memory_info().rss），
	// 不是整机已用；这里把 RSS 读取换成固定值来校验口径。
	dashboard := newDashboardRuntime(nil, nil, time.Now())
	originalRSS := dashboardProcessRSS
	t.Cleanup(func() { dashboardProcessRSS = originalRSS })
	dashboardProcessRSS = func() (int64, error) { return 123456789, nil }
	memory, err := dashboard.Memory(context.Background())
	if err != nil {
		t.Fatalf("Memory() error = %v", err)
	}
	wantUsed := int64(123456789)
	if memory.Total != total || memory.Used != wantUsed || memory.Cached != cached || memory.Available != available {
		t.Fatalf("Memory() = %+v, 期望 used=%d", memory, wantUsed)
	}
	if want := float64(wantUsed) / float64(total) * 100; memory.Usage != want {
		t.Fatalf("占用率 = %v，期望 %v", memory.Usage, want)
	}

	// 读不到本进程 RSS 时退回整机已用，至少不让卡片空着。
	dashboardProcessRSS = func() (int64, error) { return 0, errors.New("读不到 RSS") }
	fallback, err := dashboard.Memory(context.Background())
	if err != nil {
		t.Fatalf("Memory() 降级 error = %v", err)
	}
	if fallback.Used != total-available {
		t.Fatalf("降级 used = %d，期望 %d", fallback.Used, total-available)
	}
	dashboardProcessRSS = originalRSS

	// System：零值 started 回落到当前时间，运行秒数接近 0。
	system, err := dashboard.System(context.Background())
	if err != nil {
		t.Fatalf("System() error = %v", err)
	}
	if system.Hostname == "" || system.OperatingSystem != "Test OS 1.0" {
		t.Fatalf("System() = %+v", system)
	}
	if system.RuntimeSeconds < 0 || system.RuntimeSeconds > 60 {
		t.Fatalf("运行秒数 = %d，期望接近 0", system.RuntimeSeconds)
	}
	if started := newDashboardRuntime(nil, nil, time.Time{}).started; started.IsZero() {
		t.Fatal("零值 started 应回落到当前时间")
	}

	// 首次 Network 只建立基线。
	upload, download, err := dashboard.Network(context.Background())
	if err != nil || upload != 0 || download != 0 {
		t.Fatalf("首次 Network() = (%d, %d, %v)", upload, download, err)
	}

	// 首次 CPU 会在窗口内采样两次；静态 /proc/stat 没有增量，占用率为 0。
	first, err := dashboard.CPU(context.Background())
	if err != nil {
		t.Fatalf("首次 CPU() error = %v", err)
	}
	if first != 0 {
		t.Fatalf("静态 /proc/stat 的首次占用率 = %v，期望 0", first)
	}
	// 总增量 500 tick，idle+iowait 增量 400 → 忙碌占比 20%。
	fixture.write("stat", "cpu  200 20 30 800 50 10 5 0 0 0\n")
	second, err := dashboard.CPU(context.Background())
	if err != nil {
		t.Fatalf("二次 CPU() error = %v", err)
	}
	if second != 20 {
		t.Fatalf("二次 CPU() = %v，期望 20", second)
	}

	// 两次采样之间流量增长 1000/2000 字节，1 秒间隔应折算成同量级速率。
	dashboard.netMu.Lock()
	dashboard.lastNet = netSample{rx: net.rx, tx: net.tx}
	dashboard.lastNetAt = time.Now().Add(-time.Second)
	dashboard.hasNet = true
	dashboard.netMu.Unlock()
	fixture.write("netDev",
		"  eth0:  6000      50    0    0    0     0          0         0   9000      70    0    0    0     0       0          0\n")
	upload, download, err = dashboard.Network(context.Background())
	if err != nil {
		t.Fatalf("Network() error = %v", err)
	}
	// 时间差由内部 time.Now() 决定，断言按 ±20% 容差比较量级。
	if download < 800 || download > 1000 || upload < 1600 || upload > 2000 {
		t.Fatalf("速率 = (%d, %d)，期望约 (2000, 1000)", upload, download)
	}
}

// TestDashboardProcReadersWithMissingFiles 覆盖所有「读不到文件」的出口。
func TestDashboardProcReadersWithMissingFiles(t *testing.T) {
	fixture := newProcFixture(t)
	for _, name := range []string{"meminfo", "stat", "netDev", "osRelease"} {
		fixture.remove(name)
	}

	if _, _, _, err := readMemoryInfo(); err == nil {
		t.Fatal("缺少 meminfo 应报错")
	}
	if _, err := readCPUSample(); err == nil {
		t.Fatal("缺少 stat 应报错")
	}
	if _, err := readNetSample(); err == nil {
		t.Fatal("缺少 netDev 应报错")
	}
	if got := readOperatingSystem(); got != runtime.GOOS {
		t.Fatalf("缺少 os-release 时 = %q，期望 %q", got, runtime.GOOS)
	}

	dashboard := &dashboardRuntime{}
	if _, err := dashboard.Memory(context.Background()); err == nil {
		t.Fatal("Memory 应把读取失败上抛")
	}
	if _, err := dashboard.CPU(context.Background()); err == nil {
		t.Fatal("CPU 应把读取失败上抛")
	}
	if _, _, err := dashboard.Network(context.Background()); err == nil {
		t.Fatal("Network 应把读取失败上抛")
	}
}

// TestDashboardProcReadersWithMalformedFiles 逐段破坏假数据源，覆盖所有解析跳过/报错分支。
func TestDashboardProcReadersWithMalformedFiles(t *testing.T) {
	fixture := newProcFixture(t)

	// meminfo：缺冒号、无字段、非数字都跳过，只保留可解析的键。
	fixture.write("meminfo",
		"MemTotal: 1024 kB\nno-colon-line\nMemAvailable:\nBadNumber: abc kB\nBuffers: 512 kB\n")
	total, available, cached, err := readMemoryInfo()
	if err != nil {
		t.Fatalf("readMemoryInfo() error = %v", err)
	}
	if total != 1024*1024 || available != 0 || cached != 512*1024 {
		t.Fatalf("容错后的内存读取 = (%d, %d, %d)", total, available, cached)
	}

	// stat：空文件、字段过少、首字段不是 cpu、非数字 tick 四种坏行。
	fixture.write("stat", "")
	if _, err := readCPUSample(); err == nil {
		t.Fatal("空 stat 应报错")
	}
	fixture.write("stat", "cpu 1 2\n")
	if _, err := readCPUSample(); err == nil {
		t.Fatal("字段过少的 stat 应报错")
	}
	fixture.write("stat", "intr 1 2 3 4 5\n")
	if _, err := readCPUSample(); err == nil {
		t.Fatal("首字段不是 cpu 的 stat 应报错")
	}
	fixture.write("stat", "cpu 1 2 3 4 nope 6\n")
	if _, err := readCPUSample(); err == nil {
		t.Fatal("非数字 tick 应报错")
	}

	// netDev：缺冒号、字段过少、非数字行都跳过，只剩可解析接口。
	fixture.write("netDev", "junk line\n  eth0: 1 2 3\n  eth1: x 2 3 4 5 6 7 8 9\n")
	if sample, err := readNetSample(); err != nil || sample.rx != 0 || sample.tx != 0 {
		t.Fatalf("容错后的网络采样 = (%+v, %v)，期望零值", sample, err)
	}

	// osRelease：没有 PRETTY_NAME、键值不成立或值为空都回落到平台名。
	fixture.write("osRelease", "NAME=\"TestOS\"\nPRETTY_NAME=\"\"\nno-colon\n")
	if got := readOperatingSystem(); got != runtime.GOOS {
		t.Fatalf("无有效 PRETTY_NAME 时 = %q，期望 %q", got, runtime.GOOS)
	}
	fixture.write("osRelease", "PRETTY_NAME=\"Chroot OS\"\n")
	if got := readOperatingSystem(); got != "Chroot OS" {
		t.Fatalf("PRETTY_NAME 去除引号后 = %q，期望 Chroot OS", got)
	}
	// 引号内的空格属于系统名本身，只裁掉引号。
	fixture.write("osRelease", "PRETTY_NAME=\" Spaced OS \"\n")
	if got := readOperatingSystem(); got != " Spaced OS " {
		t.Fatalf("带空格的 PRETTY_NAME = %q", got)
	}
}

// TestDashboardRuntimeCPUAndNetworkEdges 覆盖 CPU 等待窗口被取消、Network 的
// 非正时间差与计数回退钳制。
func TestDashboardRuntimeCPUAndNetworkEdges(t *testing.T) {
	requireLinux(t)

	// 首次 CPU 采样会在窗口内等待；窗口期间取消应立刻返回 ctx.Err()。
	dashboard := &dashboardRuntime{}
	ctx, cancel := context.WithTimeout(t.Context(), time.Millisecond)
	defer cancel()
	if _, err := dashboard.CPU(ctx); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("取消窗口 CPU() = %v，期望超时", err)
	}

	// 已有基线时，读取 /proc/stat 失败也要上抛而不是返回陈旧值。
	fixture := newProcFixture(t)
	fixture.remove("stat")
	cached := &dashboardRuntime{hasCPUSample: true, lastCPUSample: cpuSample{total: 100, idle: 50}}
	if _, err := cached.CPU(context.Background()); err == nil {
		t.Fatal("已有基线时读取失败应报错")
	}

	// 基线时间在未来：时间差非正，直接返回 0/0 而不是负速率。
	future := &dashboardRuntime{hasNet: true, lastNetAt: time.Now().Add(time.Minute)}
	upload, download, err := future.Network(context.Background())
	if err != nil || upload != 0 || download != 0 {
		t.Fatalf("未来基线 Network() = (%d, %d, %v)", upload, download, err)
	}

	// 计数回退（接口重置）时钳制为 0。
	reset := &dashboardRuntime{
		hasNet:    true,
		lastNet:   netSample{rx: 1 << 40, tx: 1 << 40},
		lastNetAt: time.Now().Add(-time.Second),
	}
	upload, download, err = reset.Network(context.Background())
	if err != nil || upload != 0 || download != 0 {
		t.Fatalf("计数回退 Network() = (%d, %d, %v)", upload, download, err)
	}
}
