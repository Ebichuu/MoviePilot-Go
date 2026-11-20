package app

import (
	"bufio"
	"context"
	"errors"
	"os"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/moviepilot-go/moviepilot-go/internal/database"
	"github.com/moviepilot-go/moviepilot-go/internal/downloader"
	"github.com/moviepilot-go/moviepilot-go/internal/httpapi"
)

// dashboardCPUWindow 是 /proc/stat 两次采样的间隔。250ms 足够稳定，又不至于让
// 仪表盘首屏明显变慢。
const dashboardCPUWindow = 250 * time.Millisecond

// dashboardProcFiles 是本机数据源的文件路径。生产环境固定为 /proc 与 /etc/os-release；
// 单测通过替换这些路径覆盖「读不到文件」「文件内容损坏」等分支，不必 chroot
// 或伪造整个 /proc。
var dashboardProcFiles = struct {
	meminfo   string
	stat      string
	netDev    string
	osRelease string
}{
	meminfo:   "/proc/meminfo",
	stat:      "/proc/stat",
	netDev:    "/proc/net/dev",
	osRelease: "/etc/os-release",
}

// 平台实现：Linux 上 /proc 就是权威来源，Windows 没有 /proc，用 Win32 API 取同一份数据。
// 做成变量是为了让单测能把「平台实现也不可用」这条分支跑出来。
var (
	dashboardHostMemory  = hostMemoryInfo
	dashboardHostCPU     = hostCPUSample
	dashboardHostNetwork = hostNetSample
)

type dashboardMediaCounter interface {
	DashboardMediaCounts(ctx context.Context) (database.DashboardMediaCounts, error)
}

type dashboardTransferReader interface {
	TransferStats(ctx context.Context) (downloader.TransferStats, error)
}

// dashboardRuntime 是仪表盘的数据源：媒体数量来自本地同步缓存，下载器速率与空间来自
// 本机下载器，CPU/内存直接读本机 /proc。所有数据都在本进程可达范围内采集，不依赖
// 外部网络服务。
type dashboardRuntime struct {
	counts  dashboardMediaCounter
	stats   dashboardTransferReader
	started time.Time

	// CPU 使用率按「距上次调用」的 /proc/stat 增量计算，与 V2 `psutil.cpu_percent()`
	// 的非阻塞语义一致：第一次调用自带一次短采样，之后每次返回上一段区间的均值。
	cpuMu         sync.Mutex
	lastCPUSample cpuSample
	hasCPUSample  bool

	// 网络流量同样按「距上次调用」的 /proc/net/dev 增量折算成字节/秒。
	netMu     sync.Mutex
	lastNet   netSample
	lastNetAt time.Time
	hasNet    bool
}

func newDashboardRuntime(
	counts dashboardMediaCounter,
	stats dashboardTransferReader,
	started time.Time,
) *dashboardRuntime {
	if started.IsZero() {
		started = time.Now()
	}
	return &dashboardRuntime{counts: counts, stats: stats, started: started}
}

func (r *dashboardRuntime) Statistic(ctx context.Context) (httpapi.DashboardStatistic, error) {
	if r == nil || r.counts == nil {
		return httpapi.DashboardStatistic{}, errors.New("仪表盘统计不可用")
	}
	counts, err := r.counts.DashboardMediaCounts(ctx)
	if err != nil {
		return httpapi.DashboardStatistic{}, err
	}
	return httpapi.DashboardStatistic{
		MovieCount:      counts.MovieCount,
		TVCount:         counts.TVCount,
		EpisodeCount:    counts.EpisodeCount,
		UserCount:       counts.UserCount,
		MovieCountMonth: counts.MovieCountMonth,
		TVCountMonth:    counts.TVCountMonth,
	}, nil
}

func (r *dashboardRuntime) Storage(ctx context.Context) (httpapi.DashboardStorage, error) {
	stats, err := r.transferStats(ctx)
	if err != nil {
		return httpapi.DashboardStorage{}, err
	}
	// 下载盘总量 = 现有任务占用的空间 + 剩余空间，与 V2 `_build_storage` 的
	// 「total = 已用 + 可用」一致。
	return httpapi.DashboardStorage{
		TotalStorage: stats.UsedSpace + stats.FreeSpace,
		UsedStorage:  stats.UsedSpace,
	}, nil
}

func (r *dashboardRuntime) Downloader(ctx context.Context) (httpapi.DashboardDownloaderInfo, error) {
	stats, err := r.transferStats(ctx)
	if err != nil {
		return httpapi.DashboardDownloaderInfo{}, err
	}
	return httpapi.DashboardDownloaderInfo{
		DownloadSpeed: stats.DownloadSpeed,
		UploadSpeed:   stats.UploadSpeed,
		DownloadSize:  stats.DownloadSize,
		UploadSize:    stats.UploadSize,
		FreeSpace:     stats.FreeSpace,
	}, nil
}

func (r *dashboardRuntime) transferStats(ctx context.Context) (downloader.TransferStats, error) {
	if r == nil || r.stats == nil {
		return downloader.TransferStats{}, errors.New("仪表盘下载器统计不可用")
	}
	if ctx == nil {
		ctx = context.Background()
	}
	stats, err := r.stats.TransferStats(ctx)
	if errors.Is(err, downloader.ErrNoTransferStats) {
		// 一个下载器都没配置是正常状态：仪表盘刚装好就是这副样子。
		// 这里回零值而不是回错误，否则前端每 3 秒轮询一次就刷一条 WARN。
		return downloader.TransferStats{}, nil
	}
	if err != nil {
		return downloader.TransferStats{}, err
	}
	return stats, nil
}

// dashboardProcessRSS 读取本进程常驻内存，测试里可以替换。
var dashboardProcessRSS = processRSSBytes

func (r *dashboardRuntime) Memory(_ context.Context) (httpapi.DashboardMemoryInfo, error) {
	total, available, cached, err := readMemoryInfo()
	if err != nil {
		return httpapi.DashboardMemoryInfo{}, err
	}
	// 口径对齐 V2 v2.15.6 的 DashboardMemoryInfo（app/utils/system.py: memory_usage）：
	//   used  = psutil.Process().memory_info().rss  ← 本进程常驻内存，不是整机已用
	//   usage = used / total * 100
	//   total / available / cached = psutil.virtual_memory()（cached 在 Windows 上恒为 0）
	// 早期这里算的是「整机已用 = total - available」，卡片上显示 27 GB / 22%，
	// 与原版（MoviePilot 自己占的几十 MB、0.0x%）差两个数量级。
	used, rssErr := dashboardProcessRSS()
	if rssErr != nil {
		// 极少数平台读不到本进程 RSS：退回整机已用，至少让卡片有数字。
		// 这是降级口径，不是和 V2 对齐的那条路径。
		used = total - available
	}
	if used < 0 {
		used = 0
	}
	usage := 0.0
	if total > 0 {
		usage = float64(used) / float64(total) * 100
	}
	return httpapi.DashboardMemoryInfo{
		Total: total, Used: used, Cached: cached, Available: available, Usage: usage,
	}, nil
}

func (r *dashboardRuntime) CPU(ctx context.Context) (float64, error) {
	r.cpuMu.Lock()
	previous, hasPrevious := r.lastCPUSample, r.hasCPUSample
	r.cpuMu.Unlock()

	if !hasPrevious {
		first, err := readCPUSample()
		if err != nil {
			return 0, err
		}
		timer := time.NewTimer(dashboardCPUWindow)
		defer timer.Stop()
		select {
		case <-timer.C:
		case <-ctx.Done():
			return 0, ctx.Err()
		}
		current, err := readCPUSample()
		if err != nil {
			return 0, err
		}
		r.cpuMu.Lock()
		r.lastCPUSample, r.hasCPUSample = current, true
		r.cpuMu.Unlock()
		return cpuPercent(first, current), nil
	}

	current, err := readCPUSample()
	if err != nil {
		return 0, err
	}
	r.cpuMu.Lock()
	r.lastCPUSample = current
	r.cpuMu.Unlock()
	return cpuPercent(previous, current), nil
}

// cpuPercent 用两次 /proc/stat 采样算出忙碌占比；没有新增 tick 时返回 0。
func cpuPercent(previous, current cpuSample) float64 {
	totalDelta := current.total - previous.total
	if totalDelta == 0 {
		return 0
	}
	idleDelta := current.idle - previous.idle
	return float64(totalDelta-idleDelta) / float64(totalDelta) * 100
}

// Network 返回 [上传, 下载] 字节/秒。首次调用只建立基线，返回 0；之后按两次调用的
// 时间差折算速率，与 V2 `psutil.net_io_counters()` 的两点采样口径一致。
func (r *dashboardRuntime) Network(_ context.Context) (int64, int64, error) {
	current, err := readNetSample()
	if err != nil {
		return 0, 0, err
	}
	now := time.Now()
	r.netMu.Lock()
	previous, hasPrevious, previousAt := r.lastNet, r.hasNet, r.lastNetAt
	r.lastNet, r.lastNetAt, r.hasNet = current, now, true
	r.netMu.Unlock()
	if !hasPrevious {
		return 0, 0, nil
	}
	elapsed := now.Sub(previousAt).Seconds()
	if elapsed <= 0 {
		return 0, 0, nil
	}
	upload := int64(float64(current.tx-previous.tx) / elapsed)
	download := int64(float64(current.rx-previous.rx) / elapsed)
	if upload < 0 {
		upload = 0
	}
	if download < 0 {
		download = 0
	}
	return upload, download, nil
}

func (r *dashboardRuntime) System(_ context.Context) (httpapi.DashboardSystemInfo, error) {
	hostname, err := os.Hostname()
	if err != nil {
		hostname = ""
	}
	return httpapi.DashboardSystemInfo{
		Hostname:        hostname,
		OperatingSystem: readOperatingSystem(),
		RuntimeSeconds:  int64(time.Since(r.started).Seconds()),
	}, nil
}

// readOperatingSystem 优先读 /etc/os-release 的 PRETTY_NAME，读不到就退回 Go 的
// 平台名（Windows 开发机没有这个文件）。
func readOperatingSystem() string {
	if file, err := os.Open(dashboardProcFiles.osRelease); err == nil {
		defer file.Close()
		scanner := bufio.NewScanner(file)
		for scanner.Scan() {
			key, value, ok := strings.Cut(scanner.Text(), "=")
			if !ok || strings.TrimSpace(key) != "PRETTY_NAME" {
				continue
			}
			if name := strings.Trim(strings.TrimSpace(value), `"`); name != "" {
				return name
			}
		}
	}
	return runtime.GOOS
}

// readMemoryInfo 优先读 /proc/meminfo（Linux 的权威来源）；读不到（Windows 开发机、
// 精简容器）就退回平台实现，避免整张内存卡片显示 0.00 B。
func readMemoryInfo() (total, available, cached int64, err error) {
	if total, available, cached, err = procMemoryInfo(); err == nil {
		return total, available, cached, nil
	}
	return dashboardHostMemory()
}

// procMemoryInfo 读取本机 MemTotal/MemAvailable，缓存取 V2 的 Cached + Buffers。
// 单位与 V2 一致：字节。
func procMemoryInfo() (total, available, cached int64, err error) {
	file, err := os.Open(dashboardProcFiles.meminfo)
	if err != nil {
		return 0, 0, 0, errors.New("读取 /proc/meminfo 失败")
	}
	defer file.Close()

	var buffers int64
	scanner := bufio.NewScanner(file)
	for scanner.Scan() {
		key, value, ok := strings.Cut(scanner.Text(), ":")
		if !ok {
			continue
		}
		fields := strings.Fields(value)
		if len(fields) == 0 {
			continue
		}
		kilobytes, convErr := strconv.ParseInt(fields[0], 10, 64)
		if convErr != nil {
			continue
		}
		switch key {
		case "MemTotal":
			total = kilobytes * 1024
		case "MemAvailable":
			available = kilobytes * 1024
		case "Cached":
			cached += kilobytes * 1024
		case "Buffers":
			buffers += kilobytes * 1024
		}
	}
	if err := scanner.Err(); err != nil {
		return 0, 0, 0, errors.New("读取 /proc/meminfo 失败")
	}
	return total, available, cached + buffers, nil
}

type cpuSample struct {
	total uint64
	idle  uint64
}

// readCPUSample 优先读 /proc/stat，读不到退回平台实现（Windows 用 GetSystemTimes）。
func readCPUSample() (cpuSample, error) {
	sample, err := procCPUSample()
	if err == nil {
		return sample, nil
	}
	return dashboardHostCPU()
}

// procCPUSample 读取 /proc/stat 的第一行。idle 计入 iowait，busy 为其余字段之和，
// 与 psutil 的 cpu_percent 口径一致。
func procCPUSample() (cpuSample, error) {
	file, err := os.Open(dashboardProcFiles.stat)
	if err != nil {
		return cpuSample{}, errors.New("读取 /proc/stat 失败")
	}
	defer file.Close()

	scanner := bufio.NewScanner(file)
	if !scanner.Scan() {
		return cpuSample{}, errors.New("读取 /proc/stat 失败")
	}
	fields := strings.Fields(scanner.Text())
	if len(fields) < 5 || fields[0] != "cpu" {
		return cpuSample{}, errors.New("读取 /proc/stat 失败")
	}
	var sample cpuSample
	for index, field := range fields[1:] {
		value, convErr := strconv.ParseUint(field, 10, 64)
		if convErr != nil {
			return cpuSample{}, errors.New("读取 /proc/stat 失败")
		}
		sample.total += value
		switch index {
		case 3, 4: // idle, iowait
			sample.idle += value
		}
	}
	return sample, nil
}

type netSample struct {
	rx int64
	tx int64
}

// readNetSample 优先读 /proc/net/dev，读不到退回平台实现（Windows 用 GetIfTable2）。
func readNetSample() (netSample, error) {
	sample, err := procNetSample()
	if err == nil {
		return sample, nil
	}
	return dashboardHostNetwork()
}

// procNetSample 汇总 /proc/net/dev 里所有非回环接口的收发字节总数。
func procNetSample() (netSample, error) {
	file, err := os.Open(dashboardProcFiles.netDev)
	if err != nil {
		return netSample{}, errors.New("读取 /proc/net/dev 失败")
	}
	defer file.Close()

	var sample netSample
	scanner := bufio.NewScanner(file)
	for scanner.Scan() {
		name, counters, ok := strings.Cut(scanner.Text(), ":")
		if !ok {
			continue
		}
		name = strings.TrimSpace(name)
		if name == "" || name == "lo" {
			continue
		}
		fields := strings.Fields(counters)
		if len(fields) < 9 {
			continue
		}
		received, receiveErr := strconv.ParseInt(fields[0], 10, 64)
		sent, sendErr := strconv.ParseInt(fields[8], 10, 64)
		if receiveErr != nil || sendErr != nil {
			continue
		}
		sample.rx += received
		sample.tx += sent
	}
	if err := scanner.Err(); err != nil {
		return netSample{}, errors.New("读取 /proc/net/dev 失败")
	}
	return sample, nil
}

var _ httpapi.DashboardSource = (*dashboardRuntime)(nil)
