package httpapi

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// nettestTarget 是服务端内置的网络测试目标。前端只能拿到 id/name/icon，
// 真正的 URL 与代理策略留在服务端，避免用户可控 SSRF（与 V2 `_build_nettest_rules`
// 的设计一致）。
type nettestTarget struct {
	ID    string
	Name  string
	Icon  string
	URL   string
	Proxy bool
}

// nettestTargets 复刻 V2 内置目标的核心集合：TMDB 的 API 与站点、豆瓣、Bangumi、
// GitHub 与 PyPI。URL 里不含任何密钥，缺失 TMDB Key 时也能测通域名可达性。
func nettestTargets() []nettestTarget {
	return []nettestTarget{
		{ID: "tmdb_api", Name: "api.themoviedb.org", Icon: "tmdb",
			URL: "https://api.themoviedb.org/3/configuration", Proxy: true},
		{ID: "tmdb_api_alt", Name: "api.tmdb.org", Icon: "tmdb",
			URL: "https://api.tmdb.org/3/configuration", Proxy: true},
		{ID: "tmdb_web", Name: "www.themoviedb.org", Icon: "tmdb",
			URL: "https://www.themoviedb.org", Proxy: true},
		{ID: "douban", Name: "movie.douban.com", Icon: "douban",
			URL: "https://movie.douban.com", Proxy: true},
		{ID: "bangumi", Name: "api.bgm.tv", Icon: "bangumi",
			URL: "https://api.bgm.tv/v0/subjects/2", Proxy: true},
		{ID: "github", Name: "github.com", Icon: "github",
			URL: "https://github.com/jxxghp/MoviePilot", Proxy: true},
		{ID: "github_raw", Name: "raw.githubusercontent.com", Icon: "github",
			URL: "https://raw.githubusercontent.com/jxxghp/MoviePilot/v2/README.md", Proxy: true},
		{ID: "pypi", Name: "pypi.org", Icon: "python",
			URL: "https://pypi.org/simple/", Proxy: false},
	}
}

// systemNettestTargets 对应 V2 `GET /system/nettest/targets`。
func systemNettestTargets(w http.ResponseWriter, _ *http.Request) {
	targets := nettestTargets()
	items := make([]map[string]string, 0, len(targets))
	for _, target := range targets {
		items = append(items, map[string]string{
			"id": target.ID, "name": target.Name, "icon": target.Icon,
		})
	}
	writeSuccess(w, items)
}

// systemNettest 对应 V2 `GET /system/nettest?target_id=...`：只测内置目标，
// 通过则按 V2 的口径返回「连接成功，耗时 N ms」。
func systemNettest(w http.ResponseWriter, r *http.Request) {
	targetID := strings.TrimSpace(r.URL.Query().Get("target_id"))
	var target nettestTarget
	found := false
	for _, candidate := range nettestTargets() {
		if candidate.ID == targetID {
			target, found = candidate, true
			break
		}
	}
	if !found {
		writeJSON(w, http.StatusOK, Response{Success: false, Message: "测试目标不存在"})
		return
	}
	if parsed, err := url.Parse(target.URL); err != nil || parsed.Scheme != "https" {
		writeJSON(w, http.StatusOK, Response{Success: false, Message: "测试目标地址不安全"})
		return
	}

	started := time.Now()
	ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
	defer cancel()
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, target.URL, nil)
	if err != nil {
		writeJSON(w, http.StatusOK, Response{Success: false, Message: "测试请求构造失败"})
		return
	}
	request.Header.Set("User-Agent", "MoviePilot-Go-NetTest/1.0")
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		writeJSON(w, http.StatusOK, Response{
			Success: false, Message: "连接失败：" + nettestErrorMessage(err),
		})
		return
	}
	defer func() { _ = response.Body.Close() }()

	elapsed := time.Since(started)
	if response.StatusCode >= http.StatusBadRequest {
		writeJSON(w, http.StatusOK, Response{
			Success: false, Message: fmt.Sprintf("连接失败：HTTP %d", response.StatusCode),
		})
		return
	}
	writeJSON(w, http.StatusOK, Response{
		Success: true, Message: fmt.Sprintf("连接成功，耗时 %.0f ms", float64(elapsed.Microseconds())/1000),
	})
}

// nettestErrorMessage 把底层网络错误压缩成一句可读提示，避免把内部地址细节透出。
func nettestErrorMessage(err error) string {
	message := err.Error()
	switch {
	case strings.Contains(message, "context deadline exceeded"), strings.Contains(message, "Client.Timeout"):
		return "连接超时"
	case strings.Contains(message, "no such host"):
		return "域名解析失败"
	case strings.Contains(message, "connection refused"):
		return "连接被拒绝"
	case strings.Contains(message, "certificate"):
		return "证书校验失败"
	}
	return "网络不可达"
}

// systemUsageStatistic 对应 V2 `GET /system/usage/statistic`：安装版本统计来自中心端，
// 无外网时 V2 返回空数据，这里保持同一分支。
func systemUsageStatistic(w http.ResponseWriter, _ *http.Request) {
	writeSuccess(w, map[string]any{})
}
