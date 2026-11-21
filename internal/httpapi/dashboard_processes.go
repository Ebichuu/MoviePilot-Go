package httpapi

import (
	"cmp"
	"context"
	"encoding/json"
	"io"
	"math"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"
)

const (
	defaultReleasesURL      = "https://api.github.com/repos/Ebichuu/MoviePilot-Go/releases"
	defaultReleasesCacheTTL = 10 * time.Minute
	maxVersionResponseBytes = 4 << 20
	maxDashboardProcesses   = 256
	linuxClockTicksPerSec   = 100.0
)

var processStartedAt = time.Now()

// ProcessInfo 对应 V2 `schemas.ProcessInfo`（GET /api/v1/dashboard/processes）。
type ProcessInfo struct {
	PID        int     `json:"pid"`
	Name       string  `json:"name"`
	Status     string  `json:"status"`
	CPU        float64 `json:"cpu"`
	Memory     float64 `json:"memory"`
	CreateTime float64 `json:"create_time"`
	RunTime    float64 `json:"run_time"`
}

// VersionFetcher 缓存并拉取远端 GitHub Release 版本列表。
type VersionFetcher struct {
	Endpoint string
	Client   *http.Client
	CacheTTL time.Duration

	mu        sync.Mutex
	cached    []map[string]any
	cachedAt  time.Time
	hasCached bool
}

// NewVersionFetcher 创建带默认缓存与超时的版本列表拉取器。
func NewVersionFetcher(endpoint string) *VersionFetcher {
	if strings.TrimSpace(endpoint) == "" {
		endpoint = defaultReleasesURL
	}
	return &VersionFetcher{
		Endpoint: strings.TrimSpace(endpoint),
		Client:   &http.Client{Timeout: 10 * time.Second},
		CacheTTL: defaultReleasesCacheTTL,
	}
}

// Fetch 返回版本列表；在缓存有效期内直接命中缓存。
func (f *VersionFetcher) Fetch(ctx context.Context) ([]map[string]any, error) {
	if f == nil {
		return nil, http.ErrHandlerTimeout
	}
	ttl := f.CacheTTL
	if ttl <= 0 {
		ttl = defaultReleasesCacheTTL
	}
	f.mu.Lock()
	if f.hasCached && time.Since(f.cachedAt) < ttl {
		cloned := append([]map[string]any(nil), f.cached...)
		f.mu.Unlock()
		return cloned, nil
	}
	f.mu.Unlock()

	endpoint := strings.TrimSpace(f.Endpoint)
	if endpoint == "" {
		endpoint = defaultReleasesURL
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("User-Agent", "MoviePilot-Go/"+backendVersion)

	client := f.Client
	if client == nil {
		client = &http.Client{Timeout: 10 * time.Second}
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 4096))
		return nil, http.ErrNotSupported
	}
	raw, err := io.ReadAll(io.LimitReader(resp.Body, maxVersionResponseBytes))
	if err != nil {
		return nil, err
	}
	var releases []map[string]any
	if err := json.Unmarshal(raw, &releases); err != nil {
		return nil, err
	}
	if releases == nil {
		releases = []map[string]any{}
	}

	f.mu.Lock()
	f.cached = append([]map[string]any(nil), releases...)
	f.cachedAt = time.Now()
	f.hasCached = true
	f.mu.Unlock()

	return releases, nil
}

// dashboardProcesses 对应 V2 `GET /api/v1/dashboard/processes`：返回宿主机或当前进程信息列表。
func (d Dependencies) dashboardProcesses(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, collectProcesses(time.Now()))
}

// systemVersionsHandler 对应 V2 `GET /api/v1/system/versions`：拉取 GitHub Releases 版本列表。
func (d Dependencies) systemVersionsHandler(w http.ResponseWriter, r *http.Request) {
	fetcher := d.Versions
	if fetcher == nil {
		fetcher = defaultVersionFetcher
	}
	releases, err := fetcher.Fetch(r.Context())
	if err != nil {
		if d.Logger != nil {
			d.Logger.Debug("获取远端版本列表失败", "error", err)
		}
		writeJSON(w, http.StatusOK, Response{
			Success: false, Message: "无法获取版本列表", Data: map[string]any{},
		})
		return
	}
	writeJSON(w, http.StatusOK, Response{
		Success: true, Data: releases,
	})
}

var defaultVersionFetcher = NewVersionFetcher(defaultReleasesURL)

func collectProcesses(now time.Time) []ProcessInfo {
	if runtime.GOOS == "linux" {
		if procs := collectLinuxProcProcesses("/proc", now); len(procs) > 0 {
			return procs
		}
	}
	return []ProcessInfo{currentProcessInfo(now)}
}

func currentProcessInfo(now time.Time) ProcessInfo {
	var mem runtime.MemStats
	runtime.ReadMemStats(&mem)
	name := "moviepilot"
	if len(os.Args) > 0 && strings.TrimSpace(os.Args[0]) != "" {
		name = filepath.Base(os.Args[0])
	}
	runSec := now.Sub(processStartedAt).Seconds()
	if runSec < 0 {
		runSec = 0
	}
	memMB := round2(float64(mem.Sys) / (1024 * 1024))
	return ProcessInfo{
		PID:        os.Getpid(),
		Name:       name,
		Status:     "running",
		CPU:        0,
		Memory:     memMB,
		CreateTime: round2(float64(processStartedAt.Unix())),
		RunTime:    round2(runSec),
	}
}

// collectLinuxProcProcesses 以纯 Go 读取 Linux `/proc` 目录构造 V2 `ProcessInfo` 列表。
func collectLinuxProcProcesses(procRoot string, now time.Time) []ProcessInfo {
	entries, err := os.ReadDir(procRoot)
	if err != nil {
		return nil
	}
	bootTime := readLinuxBootTime(filepath.Join(procRoot, "stat"))
	pageSize := float64(os.Getpagesize())
	if pageSize <= 0 {
		pageSize = 4096
	}
	nowSec := float64(now.Unix())
	procs := make([]ProcessInfo, 0, min(len(entries), maxDashboardProcesses))
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		pid, err := strconv.Atoi(entry.Name())
		if err != nil || pid <= 0 {
			continue
		}
		rawStat, err := os.ReadFile(filepath.Join(procRoot, entry.Name(), "stat"))
		if err != nil {
			continue
		}
		parsed, ok := parseLinuxProcStat(string(rawStat))
		if !ok {
			continue
		}
		createTime := 0.0
		runTime := 0.0
		if bootTime > 0 && parsed.StartTicks > 0 {
			createTime = bootTime + float64(parsed.StartTicks)/linuxClockTicksPerSec
			if nowSec > createTime {
				runTime = nowSec - createTime
			}
		}
		memMB := round2(float64(parsed.RSSPages) * pageSize / (1024 * 1024))
		procs = append(procs, ProcessInfo{
			PID:        pid,
			Name:       parsed.Name,
			Status:     mapLinuxProcState(parsed.State),
			CPU:        0,
			Memory:     memMB,
			CreateTime: round2(createTime),
			RunTime:    round2(runTime),
		})
		if len(procs) >= maxDashboardProcesses {
			break
		}
	}
	slices.SortFunc(procs, func(a, b ProcessInfo) int {
		if c := cmp.Compare(b.Memory, a.Memory); c != 0 {
			return c
		}
		return cmp.Compare(a.PID, b.PID)
	})
	return procs
}

type linuxProcStat struct {
	Name       string
	State      string
	StartTicks int64
	RSSPages   int64
}

// parseLinuxProcStat 解析 `/proc/<pid>/stat` 单行内容（兼容进程名中包含空格与括号）。
func parseLinuxProcStat(raw string) (linuxProcStat, bool) {
	openIdx := strings.IndexByte(raw, '(')
	closeIdx := strings.LastIndexByte(raw, ')')
	if openIdx < 0 || closeIdx <= openIdx+1 || closeIdx+2 >= len(raw) {
		return linuxProcStat{}, false
	}
	name := strings.TrimSpace(raw[openIdx+1 : closeIdx])
	fields := strings.Fields(raw[closeIdx+1:])
	// fields[0]=state (field 3), fields[19]=starttime (field 22), fields[21]=rss (field 24)
	if len(fields) < 22 {
		return linuxProcStat{}, false
	}
	startTicks, _ := strconv.ParseInt(fields[19], 10, 64)
	rssPages, _ := strconv.ParseInt(fields[21], 10, 64)
	return linuxProcStat{
		Name:       name,
		State:      fields[0],
		StartTicks: startTicks,
		RSSPages:   rssPages,
	}, true
}

func readLinuxBootTime(statPath string) float64 {
	raw, err := os.ReadFile(statPath)
	if err != nil {
		return 0
	}
	for _, line := range strings.Split(string(raw), "\n") {
		if strings.HasPrefix(line, "btime ") {
			parts := strings.Fields(line)
			if len(parts) >= 2 {
				if v, err := strconv.ParseFloat(parts[1], 64); err == nil {
					return v
				}
			}
		}
	}
	return 0
}

func mapLinuxProcState(state string) string {
	switch state {
	case "R":
		return "running"
	case "S", "I":
		return "sleeping"
	case "D":
		return "disk-sleep"
	case "Z":
		return "zombie"
	case "T", "t":
		return "stopped"
	default:
		return "unknown"
	}
}

func round2(v float64) float64 {
	return math.Round(v*100) / 100
}
