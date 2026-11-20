package httpapi

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
)

type fakeDashboardSource struct {
	statistic  DashboardStatistic
	storage    DashboardStorage
	downloader DashboardDownloaderInfo
	memory     DashboardMemoryInfo
	cpu        float64
	system     DashboardSystemInfo
	upload     int64
	download   int64
	err        error
}

func (f fakeDashboardSource) Statistic(context.Context) (DashboardStatistic, error) {
	return f.statistic, f.err
}

func (f fakeDashboardSource) Storage(context.Context) (DashboardStorage, error) {
	return f.storage, f.err
}

func (f fakeDashboardSource) Downloader(context.Context) (DashboardDownloaderInfo, error) {
	return f.downloader, f.err
}

func (f fakeDashboardSource) Memory(context.Context) (DashboardMemoryInfo, error) {
	return f.memory, f.err
}

func (f fakeDashboardSource) CPU(context.Context) (float64, error) {
	return f.cpu, f.err
}

func (f fakeDashboardSource) Network(context.Context) (int64, int64, error) {
	return f.upload, f.download, f.err
}

func (f fakeDashboardSource) System(context.Context) (DashboardSystemInfo, error) {
	return f.system, f.err
}

func decodeDashboardBody(t *testing.T, handler http.HandlerFunc, target any) {
	t.Helper()
	response := httptest.NewRecorder()
	handler(response, httptest.NewRequest(http.MethodGet, "/", nil))
	if response.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", response.Code)
	}
	if err := json.Unmarshal(response.Body.Bytes(), target); err != nil {
		t.Fatalf("解析响应: %v", err)
	}
}

// 仪表盘接口必须原样返回数据源的本机数据，而不是 V2 的零值占位。
func TestDashboardHandlersReturnLocalData(t *testing.T) {
	dependencies := Dependencies{Dashboard: fakeDashboardSource{
		statistic:  DashboardStatistic{MovieCount: 12, TVCount: 3, EpisodeCount: 40, UserCount: 2},
		storage:    DashboardStorage{TotalStorage: 3000, UsedStorage: 1000},
		downloader: DashboardDownloaderInfo{DownloadSpeed: 1024, FreeSpace: 2048},
		memory:     DashboardMemoryInfo{Total: 8000, Used: 400, Available: 7000},
		cpu:        12.5,
		system:     DashboardSystemInfo{Hostname: "media-host", RuntimeSeconds: 90},
	}}

	var statistic DashboardStatistic
	decodeDashboardBody(t, dependencies.dashboardStatistic, &statistic)
	if statistic.MovieCount != 12 || statistic.EpisodeCount != 40 || statistic.UserCount != 2 {
		t.Fatalf("statistic = %#v", statistic)
	}

	var storage DashboardStorage
	decodeDashboardBody(t, dependencies.dashboardStorage, &storage)
	if storage.TotalStorage != 3000 || storage.UsedStorage != 1000 {
		t.Fatalf("storage = %#v", storage)
	}

	var downloader DashboardDownloaderInfo
	decodeDashboardBody(t, dependencies.dashboardDownloaderInfo, &downloader)
	if downloader.DownloadSpeed != 1024 || downloader.FreeSpace != 2048 {
		t.Fatalf("downloader = %#v", downloader)
	}

	var memory DashboardMemoryInfo
	decodeDashboardBody(t, dependencies.dashboardMemoryInfo, &memory)
	if memory.Total != 8000 || memory.Available != 7000 {
		t.Fatalf("memory = %#v", memory)
	}

	var usage float64
	decodeDashboardBody(t, dependencies.dashboardCPUUsage, &usage)
	if usage != 12.5 {
		t.Fatalf("cpu = %v, want 12.5", usage)
	}

	var system DashboardSystemInfo
	decodeDashboardBody(t, dependencies.dashboardSystemInfo, &system)
	if system.Hostname != "media-host" || system.RuntimeSeconds != 90 {
		t.Fatalf("system = %#v", system)
	}
	// 数据源没给版本时由 handler 补上后端版本，前端不会显示空版本。
	if system.Version != backendVersion {
		t.Fatalf("version = %q, want %q", system.Version, backendVersion)
	}
}

// 没有装载数据源或数据源失败时，接口仍按 V2 的零值返回 200，页面不会整块报错。
func TestDashboardHandlersFallBackToZeroValues(t *testing.T) {
	tests := []struct {
		name         string
		dependencies Dependencies
		handler      http.HandlerFunc
		check        func(t *testing.T, body string)
	}{
		{
			name: "未装载媒体统计", dependencies: Dependencies{}, handler: Dependencies{}.dashboardStatistic,
			check: func(t *testing.T, body string) {
				t.Helper()
				var value DashboardStatistic
				if err := json.Unmarshal([]byte(body), &value); err != nil || value.MovieCount != 0 {
					t.Fatalf("body = %s", body)
				}
			},
		},
		{
			name: "统计失败仍返回零值", dependencies: Dependencies{Dashboard: fakeDashboardSource{err: errors.New("upstream")}},
			handler: Dependencies{Dashboard: fakeDashboardSource{err: errors.New("upstream")}}.dashboardStatistic,
			check: func(t *testing.T, body string) {
				t.Helper()
				var value DashboardStatistic
				if err := json.Unmarshal([]byte(body), &value); err != nil || value.TVCount != 0 {
					t.Fatalf("body = %s", body)
				}
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			response := httptest.NewRecorder()
			tt.handler(response, httptest.NewRequest(http.MethodGet, "/", nil))
			if response.Code != http.StatusOK {
				t.Fatalf("status = %d, want 200", response.Code)
			}
			tt.check(t, response.Body.String())
		})
	}
}
