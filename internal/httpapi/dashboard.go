package httpapi

import (
	"context"
	"net/http"
)

// DashboardStatistic 对应 V2 `schemas.Statistic`（GET /dashboard/statistic）。
type DashboardStatistic struct {
	MovieCount        int64 `json:"movie_count"`
	TVCount           int64 `json:"tv_count"`
	EpisodeCount      int64 `json:"episode_count"`
	UserCount         int64 `json:"user_count"`
	MovieCountMonth   int64 `json:"movie_count_month"`
	TVCountMonth      int64 `json:"tv_count_month"`
	EpisodeCountMonth int64 `json:"episode_count_month"`
}

// DashboardStorage 对应 V2 `schemas.Storage`（GET /dashboard/storage），单位为字节。
type DashboardStorage struct {
	TotalStorage float64 `json:"total_storage"`
	UsedStorage  float64 `json:"used_storage"`
}

// DashboardDownloaderInfo 对应 V2 `schemas.DownloaderInfo`（GET /dashboard/downloader）。
type DashboardDownloaderInfo struct {
	DownloadSpeed float64 `json:"download_speed"`
	UploadSpeed   float64 `json:"upload_speed"`
	DownloadSize  float64 `json:"download_size"`
	UploadSize    float64 `json:"upload_size"`
	FreeSpace     float64 `json:"free_space"`
}

// DashboardMemoryInfo 对应 V2 `schemas.DashboardMemoryInfo`（GET /dashboard/memory）。
// total/available/cached 取自本机 /proc/meminfo（Windows 走 GlobalMemoryStatusEx），
// used 是整机已用 = total - available，不是本进程 RSS。
type DashboardMemoryInfo struct {
	Total     int64   `json:"total"`
	Used      int64   `json:"used"`
	Cached    int64   `json:"cached"`
	Available int64   `json:"available"`
	Usage     float64 `json:"usage"`
}

// DashboardSystemInfo 对应 V2 `schemas.DashboardSystemInfo`（GET /dashboard/system）。
type DashboardSystemInfo struct {
	Hostname        string `json:"hostname"`
	OperatingSystem string `json:"operating_system"`
	RuntimeSeconds  int64  `json:"runtime"`
	Version         string `json:"version"`
}

// DashboardSource 提供仪表盘需要的本地真实数据：本机 CPU/内存、本机下载器的速率与
// 空间、媒体服务器同步缓存里的条目数。装载为空时各接口返回 V2 的零值，前端不会因为
// 404 整块报错。
type DashboardSource interface {
	Statistic(ctx context.Context) (DashboardStatistic, error)
	Storage(ctx context.Context) (DashboardStorage, error)
	Downloader(ctx context.Context) (DashboardDownloaderInfo, error)
	Memory(ctx context.Context) (DashboardMemoryInfo, error)
	CPU(ctx context.Context) (float64, error)
	System(ctx context.Context) (DashboardSystemInfo, error)
	// Network 返回 [上传, 下载] 两个方向的字节/秒。
	Network(ctx context.Context) (upload int64, download int64, err error)
}

// dashboardFailure 记录仪表盘数据源失败。这些错误每页都会重复出现，因此只在 WARN
// 级别记录稳定文案，避免把上游细节写进响应。
func (d Dependencies) dashboardFailure(message string, err error) {
	if d.Logger == nil || err == nil {
		return
	}
	d.Logger.Warn(message, "error", err)
}

func (d Dependencies) dashboardStatistic(w http.ResponseWriter, r *http.Request) {
	statistic := DashboardStatistic{}
	if d.Dashboard != nil {
		value, err := d.Dashboard.Statistic(r.Context())
		if err != nil {
			d.dashboardFailure("仪表盘媒体统计失败", err)
		} else {
			statistic = value
		}
	}
	writeJSON(w, http.StatusOK, statistic)
}

func (d Dependencies) dashboardStorage(w http.ResponseWriter, r *http.Request) {
	storage := DashboardStorage{}
	if d.Dashboard != nil {
		value, err := d.Dashboard.Storage(r.Context())
		if err != nil {
			d.dashboardFailure("仪表盘存储统计失败", err)
		} else {
			storage = value
		}
	}
	writeJSON(w, http.StatusOK, storage)
}

func (d Dependencies) dashboardDownloaderInfo(w http.ResponseWriter, r *http.Request) {
	info := DashboardDownloaderInfo{}
	if d.Dashboard != nil {
		value, err := d.Dashboard.Downloader(r.Context())
		if err != nil {
			d.dashboardFailure("仪表盘下载器统计失败", err)
		} else {
			info = value
		}
	}
	writeJSON(w, http.StatusOK, info)
}

func (d Dependencies) dashboardMemoryInfo(w http.ResponseWriter, r *http.Request) {
	info := DashboardMemoryInfo{}
	if d.Dashboard != nil {
		value, err := d.Dashboard.Memory(r.Context())
		if err != nil {
			d.dashboardFailure("仪表盘内存统计失败", err)
		} else {
			info = value
		}
	}
	writeJSON(w, http.StatusOK, info)
}

func (d Dependencies) dashboardCPUUsage(w http.ResponseWriter, r *http.Request) {
	usage := 0.0
	if d.Dashboard != nil {
		value, err := d.Dashboard.CPU(r.Context())
		if err != nil {
			d.dashboardFailure("仪表盘 CPU 统计失败", err)
		} else {
			usage = value
		}
	}
	writeJSON(w, http.StatusOK, usage)
}

func (d Dependencies) dashboardSystemInfo(w http.ResponseWriter, r *http.Request) {
	info := DashboardSystemInfo{}
	if d.Dashboard != nil {
		value, err := d.Dashboard.System(r.Context())
		if err != nil {
			d.dashboardFailure("仪表盘系统信息失败", err)
		} else {
			info = value
		}
	}
	if info.Version == "" {
		info.Version = backendVersion
	}
	writeJSON(w, http.StatusOK, info)
}

// dashboardNetwork 对应 V2 `GET /dashboard/network`（List[int]：[上行, 下行] 字节/秒）。
func (d Dependencies) dashboardNetwork(w http.ResponseWriter, r *http.Request) {
	var upload, download int64
	if d.Dashboard != nil {
		up, down, err := d.Dashboard.Network(r.Context())
		if err != nil {
			d.dashboardFailure("仪表盘网络流量失败", err)
		} else {
			upload, download = up, down
		}
	}
	writeJSON(w, http.StatusOK, []int64{upload, download})
}

// dashboardTransfer 对应 V2 `GET /dashboard/transfer`（List[int]：最近 days 天每天的
// 整理数量），数据来自整理成功快照。
func (d Dependencies) dashboardTransfer(w http.ResponseWriter, r *http.Request) {
	days := parseHistoryInt(r.URL.Query().Get("days"), 7, 1, 31)
	counts := make([]int, days)
	if d.Store != nil {
		values, err := d.Store.CountTransferHistoryByDay(r.Context(), days)
		if err != nil {
			d.dashboardFailure("仪表盘整理统计失败", err)
		} else {
			counts = values
		}
	}
	writeJSON(w, http.StatusOK, counts)
}

// dashboardScheduleProgress 对应 V2 `GET /dashboard/schedule/{job_id}/progress`。
// River 周期任务没有进度总线，按 V2 的「后台服务不存在」分支返回。
func dashboardScheduleProgress(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, Response{
		Success: false, Message: "后台服务不存在", Data: map[string]any{},
	})
}

// systemGlobalUser 对应 V2 `GET /system/global/user`：登录后前端读取的业务开关与
// 分享管理权限。本仓库没有 AI 助手与订阅分享后端，因此开关固定关闭、分享管理跟随
// 当前用户是否为超管。
func (d Dependencies) systemGlobalUser(w http.ResponseWriter, r *http.Request) {
	claims, _ := claimsFromContext(r.Context())
	writeSuccess(w, map[string]any{
		"AI_AGENT_ENABLE":          false,
		"AI_AGENT_HIDE_ENTRY":      true,
		"LLM_SUPPORT_AUDIO_INPUT":  false,
		"LLM_SUPPORT_AUDIO_OUTPUT": false,
		"RECOGNIZE_SOURCE":         "",
		"SEARCH_SOURCE":            "",
		"AI_RECOMMEND_ENABLED":     false,
		"USER_UNIQUE_ID":           claims.Username,
		"SUBSCRIBE_SHARE_MANAGE":   claims.SuperUser,
		"WORKFLOW_SHARE_MANAGE":    claims.SuperUser,
	})
}

// pluginRemotes 对应 V2 `GET /plugin/remotes?token=moviepilot`（List[dict]）。
// 首版不装载 Python 插件联邦组件，返回空列表。
func pluginRemotes(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, []any{})
}
