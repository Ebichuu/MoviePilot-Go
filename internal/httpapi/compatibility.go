package httpapi

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
)

const (
	userConfigKeyPrefix = "user-config:v1:"
	userConfigKeyLimit  = 256
	systemMessagePeriod = 3 * time.Second
)

// UserConfigStore 是用户界面配置所需的最小持久化契约。
type UserConfigStore interface {
	GetSetting(context.Context, string, any) (bool, error)
	SetSetting(context.Context, string, any, bool) error
}

func (d Dependencies) registerPublicCompatibility(router chi.Router) {
	router.Post("/mfa/passkey/authenticate/start", d.passkeyAuthenticateStart)
	router.Post("/mfa/passkey/authenticate/finish", d.passkeyAuthenticateFinish)
	router.With(authenticateResource(d.Auth)).Get("/system/message", d.systemMessage)
	// V2 的 `*2` 系列是同一批只读接口的 API_TOKEN 版本，供外部脚本免登录调用，
	// 因此必须注册在鉴权分组之外，只由 requireAPIToken 校验 ?token=。
	router.With(d.requireAPIToken).Get("/dashboard/cpu2", d.dashboardCPUUsage)
	router.With(d.requireAPIToken).Get("/dashboard/memory2", d.dashboardMemoryInfo)
	router.With(d.requireAPIToken).Get("/dashboard/downloader2", d.dashboardDownloaderInfo)
	router.With(d.requireAPIToken).Get("/dashboard/statistic2", d.dashboardStatistic)
	router.With(d.requireAPIToken).Get("/dashboard/storage2", d.dashboardStorage)
	router.With(d.requireAPIToken).Get("/dashboard/network2", d.dashboardNetwork)
	router.With(d.requireAPIToken).Get("/dashboard/schedule2", d.dashboardSchedule)
	router.With(d.requireAPIToken).Get("/dashboard/schedule2/{job_id}/progress", dashboardScheduleProgress)
	router.With(d.requireAPIToken).Get("/system/runscheduler2", d.systemRunScheduler)
	router.With(d.requireAPIToken).Get("/subscribe/list", d.listAllSubscriptions)
	// 识别接口的 API_TOKEN 版本与登录版本共用同一处理逻辑。
	router.With(d.requireAPIToken).Get("/media/recognize2", d.recognizeMedia)
	router.With(d.requireAPIToken).Get("/media/recognize_file2", d.mediaRecognizeFile)
	// 图片代理与实时日志用资源令牌或 Bearer 鉴权：<img> 与 EventSource 无法带 Authorization 头。
	router.With(authenticateResource(d.Auth)).Get("/system/img/*", d.systemImageProxy)
	router.With(authenticateResource(d.Auth)).Get("/system/cache/image", d.systemCacheImage)
	router.With(d.authenticateLoggingSuperuser).Get("/system/logging", d.systemLogging)
	// Webhook 与 Seerr 联动入口支持 API_TOKEN 免登录回调（同时兼容 Bearer JWT）。
	router.With(d.requireAPITokenOrBearer).Get("/webhook/", webhookProbe)
	router.With(d.requireAPITokenOrBearer).Post("/webhook/", d.handleWebhook)
	router.Post("/subscribe/seerr", d.seerrSubscribe)
}

func (d Dependencies) registerProtectedCompatibility(router chi.Router) {
	router.Get("/message/notification", d.listNotificationHistory)
	router.Delete("/message/notification", d.clearNotificationHistory)
	router.Post("/message/webpush/subscribe", d.subscribeWebPush)
	router.Post("/message/webpush/send", d.sendWebPush)
	router.With(d.requireManage).Post("/storage/list", d.storageList)
	router.With(d.requireManage).Post("/storage/mkdir", d.storageMkdir)
	router.With(d.requireManage).Post("/storage/delete", d.storageDelete)
	router.With(d.requireManage).Post("/storage/rename", d.storageRename)
	router.With(d.requireManage).Post("/storage/download", d.storageDownload)
	router.With(d.requireManage).Post("/storage/image", d.storageImage)
	router.Get("/storage/qrcode/{name}", d.storageQRCode)
	router.Get("/storage/auth_url/{name}", d.storageAuthURL)
	router.Get("/storage/check/{name}", d.storageCheck)
	router.With(d.requireAdmin).Post("/storage/save/{name}", d.storageSave)
	router.With(d.requireAdmin).Get("/storage/reset/{name}", d.storageReset)
	router.With(d.requireAdmin).Get("/storage/usage/{name}", d.storageUsage)
	router.With(d.requireAdmin).Get("/storage/transtype/{name}", d.storageTransType)
	router.With(d.requireAdmin).Get("/plugin/dashboard/meta", emptyList)
	// 插件页首屏会读统计与文件夹分组；Go 侧没有 Python 运行时，但分组配置可以真实
	// 持久化，统计按 V2 的「未开启分享」分支返回空对象。
	router.Get("/plugin/statistic", pluginStatistic)
	router.With(d.requireAdmin).Get("/plugin/folders", d.pluginFolders)
	router.With(d.requireAdmin).Post("/plugin/folders", d.savePluginFolders)
	router.With(d.requireAdmin).Post("/plugin/folders/{folder_name}", d.createPluginFolder)
	router.With(d.requireAdmin).Delete("/plugin/folders/{folder_name}", d.deletePluginFolder)
	router.With(d.requireAdmin).Put("/plugin/folders/{folder_name}/plugins", d.updateFolderPlugins)
	// 仪表盘：真实数据来自本机（/proc）、本机下载器与本地同步缓存。
	router.With(d.requireAdmin).Get("/dashboard/schedule", d.dashboardSchedule)
	router.With(d.requireAdmin).Get("/dashboard/cpu", d.dashboardCPUUsage)
	router.With(d.requireAdmin).Get("/dashboard/memory", d.dashboardMemoryInfo)
	router.With(d.requireAdmin).Get("/dashboard/downloader", d.dashboardDownloaderInfo)
	router.With(d.requireAdmin).Get("/dashboard/statistic", d.dashboardStatistic)
	router.With(d.requireAdmin).Get("/dashboard/events", d.dashboardEventStatistics)
	router.With(d.requireAdmin).Get("/dashboard/storage", d.dashboardStorage)
	router.With(d.requireAdmin).Get("/dashboard/system", d.dashboardSystemInfo)
	router.With(d.requireAdmin).Get("/dashboard/processes", d.dashboardProcesses)
	router.With(d.requireAdmin).Get("/dashboard/network", d.dashboardNetwork)
	router.With(d.requireAdmin).Get("/dashboard/transfer", d.dashboardTransfer)
	router.With(d.requireAdmin).Get("/dashboard/schedule/{job_id}/progress", dashboardScheduleProgress)

	router.Get("/transfer/queue", d.transferQueueList)
	router.Get("/history/transfer", d.transferHistory)
	router.Get("/history/download", d.downloadHistory)
	router.Get("/transfer/now", d.transferNowList)
	router.Delete("/transfer/queue", d.removeTransferQueueItem)
	// 插件：清单/安装/启停/配置走 Go 原生注册表；页面、看板、文件、历史、评分、
	// 版本发布、克隆这些依赖 Python 插件运行时的接口继续诚实拒绝。
	router.With(d.requireAdmin).Get("/plugin/{plugin_id}", d.getPlugin)
	router.With(d.requireAdmin).Put("/plugin/{plugin_id}", d.savePluginConfig)
	router.With(d.requireAdmin).Delete("/plugin/{plugin_id}", d.uninstallPlugin)
	router.Get("/plugin/dashboard/{plugin_id}", refusal("插件看板需要 Python 运行时：本项目没有插件页面渲染能力"))
	router.Get("/plugin/dashboard/{plugin_id}/{key}", refusal("插件看板需要 Python 运行时：本项目没有插件页面渲染能力"))
	router.Get("/plugin/file/{plugin_id}/*", refusal("插件静态文件需要 Python 运行时：本项目没有插件包"))
	// 前端取插件静态文件时也可能不带路径段（`/plugin/file/{id}?path=...`）：
	// 只有通配路由时这里会落到 404「接口不存在」，与「明确拒绝」不是一回事。
	router.Get("/plugin/file/{plugin_id}", refusal("插件静态文件需要 Python 运行时：本项目没有插件包"))
	router.With(d.requireAdmin).Get("/plugin/form/{plugin_id}", d.pluginForm)
	router.Get("/plugin/history/{plugin_id}", refusal("插件版本历史来自远端仓库：本项目不跟踪插件包版本"))
	router.With(d.requireAdmin).Get("/plugin/install/{plugin_id}", d.installPlugin)
	router.Get("/plugin/page/{plugin_id}", refusal("插件页面需要 Python/Vue 渲染运行时：本项目不执行插件前端"))
	router.Get("/plugin/rating", refusal("插件评分依赖插件中心：本项目不连接远端插件库"))
	router.Get("/plugin/rating/{plugin_id}", refusal("插件评分依赖插件中心：本项目不连接远端插件库"))
	router.Post("/plugin/rating/{plugin_id}", refusal("插件评分依赖插件中心：本项目不连接远端插件库"))
	router.Get("/plugin/releases/{plugin_id}", refusal("插件版本列表来自远端仓库：本项目不跟踪插件包版本"))
	router.With(d.requireAdmin).Get("/plugin/reload/{plugin_id}", d.reloadPlugin)
	router.With(d.requireAdmin).Get("/plugin/reset/{plugin_id}", d.resetPlugin)
	router.Post("/plugin/clone/{plugin_id}", refusal("插件克隆需要插件包仓库：本项目不跟踪插件包"))
	router.With(d.requireAdmin).Post("/plugin/{plugin_id}/action", d.runPluginAction)
	router.Post("/subscribe/share", d.createSubscribeShare)
	router.Get("/subscribe/shares", d.listSubscribeShares)
	router.Delete("/subscribe/share/{share_id}", d.deleteSubscribeShare)
	router.Get("/subscribe/share/statistics", d.subscribeShareStatistics)
	router.Post("/subscribe/follow", d.followSubscribeShare)
	router.Get("/subscribe/follow", d.listSubscribeFollows)
	router.Delete("/subscribe/follow", d.unfollowSubscribeShare)
	router.Post("/subscribe/fork", d.forkSubscribeShare)
	router.Get("/subscribe/check", d.checkSubscribeShare)
	router.Get("/subscribe/refresh", d.refreshSubscribeShare)
	router.Get("/subscribe/files/{subscribe_id}", d.subscribeFileList)
	router.Get("/subscribe/search/{subscribe_id}", d.subscribeSearchByID)
	router.Post("/transfer/manual", d.manualTransfer)
	router.Post("/transfer/manual/history", d.manualTransferHistory)
	router.Post("/transfer/manual/target-path", d.manualTransferTargetPath)
	router.Get("/search/subtitle/media/{mediaid}", d.searchSubtitleMedia)
	router.Get("/search/subtitle/media/{mediaid}/stream", d.searchSubtitleMediaStream)
	router.Get("/search/subtitle/title", d.searchSubtitleTitle)
	router.Get("/search/subtitle/title/stream", d.searchSubtitleTitleStream)
	router.Post("/search/recommend", refusal("未配置 AI 模型，无法生成推荐"))
	router.Get("/subscribe/popular", refusal("未接入插件市场，无法获取热门订阅"))
	router.Get("/transfer/name", d.transferName)
	router.Post("/transfer/episode-format/recommend", d.recommendEpisodeFormat)
	router.Post("/media/scrape/{storage}", d.scrapeMedia)
	router.Get("/message/", refusal("未启用 AI Agent，无法提供该接口"))
	router.Post("/message/", refusal("未启用 AI Agent，无法提供该接口"))
	router.Get("/message/web", refusal("未启用 AI Agent，无法提供该接口"))
	router.Post("/message/web", refusal("未启用 AI Agent，无法提供该接口"))
	router.Get("/message/agent/commands", refusal("未启用 AI Agent，无法提供该接口"))
	router.Get("/message/agent/sessions", refusal("未启用 AI Agent，无法提供该接口"))
	router.Get("/message/agent/sessions/{session_id}", refusal("未启用 AI Agent，无法提供该接口"))
	router.Delete("/message/agent/sessions/{session_id}", refusal("未启用 AI Agent，无法提供该接口"))
	router.Post("/message/agent/sessions/{session_id}/stop", refusal("未启用 AI Agent，无法提供该接口"))
	router.Put("/message/agent/sessions/{session_id}/display", refusal("未启用 AI Agent，无法提供该接口"))
	router.Get("/message/agent/file/{file_id}", refusal("未启用 AI Agent，无法提供该接口"))
	router.Post("/message/agent/callback", refusal("未启用 AI Agent，无法提供该接口"))
	router.Post("/message/agent/mcp/servers", refusal("未启用 AI Agent，无法提供该接口"))
	router.Post("/message/agent/mcp/servers/test", refusal("未启用 AI Agent，无法提供该接口"))
	router.Post("/message/agent/stream", refusal("未启用 AI Agent，无法提供该接口"))
	router.Post("/message/agent/upload", refusal("未启用 AI Agent，无法提供该接口"))
	router.Get("/workflow/actions", refusal("插件工作流未接入，无法执行该操作"))
	router.Get("/workflow/plugin/actions", refusal("插件工作流未接入，无法执行该操作"))
	router.Get("/workflow/shares", refusal("插件工作流未接入，无法执行该操作"))
	router.Post("/workflow/", refusal("插件工作流未接入，无法执行该操作"))
	router.Get("/workflow/{workflow_id}", refusal("插件工作流未接入，无法执行该操作"))
	router.Put("/workflow/{workflow_id}", refusal("插件工作流未接入，无法执行该操作"))
	router.Delete("/workflow/{workflow_id}", refusal("插件工作流未接入，无法执行该操作"))
	router.Post("/workflow/{workflow_id}/run", refusal("插件工作流未接入，无法执行该操作"))
	router.Post("/workflow/{workflow_id}/start", refusal("插件工作流未接入，无法执行该操作"))
	router.Post("/workflow/{workflow_id}/pause", refusal("插件工作流未接入，无法执行该操作"))
	router.Post("/workflow/{workflow_id}/reset", refusal("插件工作流未接入，无法执行该操作"))
	router.Post("/workflow/fork", refusal("插件工作流未接入，无法执行该操作"))
	router.Post("/workflow/share", refusal("插件工作流未接入，无法执行该操作"))
	router.Delete("/workflow/share/{share_id}", refusal("插件工作流未接入，无法执行该操作"))
	router.Get("/tmdb/collection/{collection_id}", d.tmdbCollection)
	router.Get("/tmdb/credits/{tmdbid}/{type_name}", d.tmdbCredits)
	router.Get("/tmdb/person/{person_id}", d.tmdbPerson)
	router.Get("/tmdb/person/credits/{person_id}", d.tmdbPersonCredits)
	router.Get("/tmdb/recommend/{tmdbid}/{type_name}", d.tmdbRecommend)
	router.Get("/tmdb/similar/{tmdbid}/{type_name}", d.tmdbSimilar)
	router.Get("/anilist/{anilist_id}", d.anilistSubject)
	router.Get("/anilist/credits/{anilist_id}", d.anilistCredits)
	router.Get("/anilist/discover", d.anilistDiscover)
	router.Get("/anilist/person/{person_id}", d.anilistPerson)
	router.Get("/anilist/person/credits/{person_id}", d.anilistPersonCredits)
	router.Get("/anilist/recommend/{anilist_id}", d.anilistRecommend)
	router.Get("/bangumi/{bangumiid}", d.bangumiSubject)
	router.Get("/bangumi/credits/{bangumiid}", d.bangumiCredits)
	router.Get("/bangumi/person/{person_id}", d.bangumiPerson)
	router.Get("/bangumi/person/credits/{person_id}", d.bangumiPersonCredits)
	router.Get("/bangumi/recommend/{bangumiid}", d.bangumiRecommend)
	router.Get("/douban/{doubanid}", d.doubanSubject)
	router.Get("/douban/credits/{doubanid}/{type_name}", d.doubanCredits)
	router.Get("/douban/person/{person_id}", d.doubanPerson)
	router.Get("/douban/person/credits/{person_id}", d.doubanPersonCredits)
	router.Get("/douban/recommend/{doubanid}/{type_name}", d.doubanRecommend)
	router.Get("/mfa/passkey/list", d.passkeyList)
	router.Post("/mfa/passkey/register/start", d.passkeyRegisterStart)
	router.Post("/mfa/passkey/register/finish", d.passkeyRegisterFinish)
	router.Post("/mfa/passkey/delete", d.passkeyDelete)
	router.With(d.requireAdmin).Get("/openai/v1/models", refusal(messageNoLLM))
	router.With(d.requireAdmin).Post("/openai/v1/chat/completions", refusal(messageNoLLM))
	router.With(d.requireAdmin).Post("/openai/v1/responses", refusal(messageNoLLM))
	router.With(d.requireAdmin).Post("/anthropic/v1/messages", refusal(messageNoLLM))
	router.With(d.requireAdmin).Get("/mcp/tools", refusal(messageNoMCP))
	router.With(d.requireAdmin).Get("/mcp/tools/{tool_name}", refusal(messageNoMCP))
	router.With(d.requireAdmin).Get("/mcp/tools/{tool_name}/schema", refusal(messageNoMCP))
	router.With(d.requireAdmin).Post("/mcp/tools/call", refusal(messageNoMCP))
	router.With(d.requireAdmin).Get("/torrent/cache", d.listTorrentCache)
	router.With(d.requireAdmin).Delete("/torrent/cache", d.clearTorrentCache)
	router.With(d.requireAdmin).Delete("/torrent/cache/{domain}/{torrent_hash}", d.deleteTorrentCacheItem)
	router.With(d.requireAdmin).Post("/torrent/cache/refresh", d.refreshTorrentCache)
	router.With(d.requireAdmin).Post("/torrent/cache/reidentify/{domain}/{torrent_hash}", d.reidentifyTorrentCacheItem)
	router.With(d.requireAdmin).Post("/llm/test", refusal("未配置 AI 模型"))
	router.With(d.requireAdmin).Post("/llm/provider-auth/start", refusal("未接入 LLM 提供方认证"))
	router.With(d.requireAdmin).Get("/llm/provider-auth/{session_id}", refusal("未接入 LLM 提供方认证"))
	router.With(d.requireAdmin).Post("/llm/provider-auth/{session_id}/poll", refusal("未接入 LLM 提供方认证"))
	router.With(d.requireAdmin).Delete("/llm/provider-auth/{provider_id}", refusal("未接入 LLM 提供方认证"))
	router.With(d.requireAdmin).Get("/llm/provider-auth/callback/{provider_id}", refusal("未接入 LLM 提供方认证"))
	router.Get("/notification/wechatclawbot/status", refusal("未配置企业微信机器人"))
	router.Get("/notification/wechatclawbot/test", refusal("未配置企业微信机器人"))
	router.Post("/notification/wechatclawbot/logout", refusal("未配置企业微信机器人"))
	router.Post("/notification/wechatclawbot/migrate", refusal("未配置企业微信机器人"))
	router.Post("/notification/wechatclawbot/refresh", refusal("未配置企业微信机器人"))
	router.Get("/subscribe/search", d.subscribeSearch)
	router.Post("/mfa/otp/generate", d.mfaOTPGenerate)
	router.Post("/mfa/otp/verify", d.mfaOTPVerify)
	router.Post("/mfa/otp/disable", d.mfaOTPDisable)
	router.Get("/mfa/status/{username}", d.mfaStatus)
	router.With(d.requireAdmin).Get("/tmdb/cache", tmdbCache)
	router.With(d.requireAdmin).Delete("/tmdb/cache", tmdbCacheClear)
	router.With(d.requireAdmin).Delete("/tmdb/cache/*", tmdbCacheDelete)
	router.Get("/tmdb/seasons/{tmdbid}", d.tmdbSeasons)
	router.Get("/tmdb/{tmdbid}/{season}", d.tmdbSeasonDetail)
	router.Post("/download/subtitle", d.downloadSubtitle)
	router.Post("/auth/exchange", authExchange)
	router.With(d.requireAdmin).Get("/system/logging/download/{name}", d.systemLoggingDownload)
	router.With(d.requireAdmin).Post("/history/transfer/ai-redo", transferAIRedo)
	router.With(d.requireAdmin).Post("/history/transfer/{history_id}/ai-redo", transferAIHistoryRedo)
	router.With(d.requireAdmin).Get("/history/empty/transfer", emptyTransferHistory)
	router.With(d.requireAdmin).Delete("/history/transfer", d.deleteTransferHistory)
	router.Delete("/history/download", deleteDownloadHistory)
	router.Get("/system/setting/public/{key}", d.publicSetting)
	router.Get("/system/global/user", d.systemGlobalUser)
	router.With(d.requireAdmin).Get("/system/versions", d.systemVersionsHandler)
	// 扩展接口（V2 没有）：PostgreSQL 没法"拷个文件就走"，所以补上整库导出/恢复，
	// 否则升级、迁移、出事回滚都没有退路。
	router.With(d.requireAdmin).Get("/system/backup", d.systemBackup)
	router.With(d.requireAdmin).Post("/system/restore", d.systemRestore)
	router.With(d.requireAdmin).Get("/system/progress/{process_type}", d.systemProgress)
	router.With(d.requireAdmin).Get("/system/modulelist", systemModuleList)
	router.With(d.requireAdmin).Get("/system/moduletest/{moduleid}", systemModuleTest)
	router.With(d.requireAdmin).Get("/system/restart", systemRestart)
	// 网络诊断与版本统计：目标是服务端内置的固定地址，前端只拿到 id/name/icon。
	router.Get("/system/nettest/targets", systemNettestTargets)
	router.Get("/system/nettest", systemNettest)
	router.Get("/system/usage/statistic", systemUsageStatistic)
	router.With(d.requireAdmin).Get("/system/runscheduler", d.systemRunScheduler)
	router.Get("/system/ruletest", d.systemRuleTest)
	router.With(d.requireAdmin).Post("/system/upgrade", systemUpgrade)
	router.With(d.requireAdmin).Post("/system/setting/PLUGIN_MARKET/sync-wiki", systemSyncPluginWiki)
	router.Get("/mediaserver/play/*", d.mediaServerPlay)
	router.Get("/plugin/remotes", pluginRemotes)
	router.Get("/plugin/sidebar_nav", pluginSidebarNav)
	router.Get("/message/agent/mcp/servers", agentMCPServers)
	d.registerRecommendRoutes(router)
	router.Get("/user/config/{key}", d.getUserConfig)
	router.Post("/user/config/{key}", d.setUserConfig)
}

func passkeyAuthenticationUnavailable(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, Response{
		Success: false,
		Message: "当前版本未启用通行密钥认证",
		Data:    map[string]any{},
	})
}

func emptyList(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, []any{})
}

func emptySuccess(w http.ResponseWriter, _ *http.Request) {
	writeSuccess(w, nil)
}

func (d Dependencies) systemMessage(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/event-stream; charset=utf-8")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)

	flusher, ok := w.(http.Flusher)
	if !ok {
		return
	}
	writeEmptyEvent := func() bool {
		if _, err := io.WriteString(w, "data: \n\n"); err != nil {
			return false
		}
		flusher.Flush()
		return true
	}
	if !writeEmptyEvent() {
		return
	}

	ticker := time.NewTicker(systemMessagePeriod)
	defer ticker.Stop()
	for {
		select {
		case <-r.Context().Done():
			return
		case <-ticker.C:
			if !writeEmptyEvent() {
				return
			}
		}
	}
}

func (d Dependencies) getUserConfig(w http.ResponseWriter, r *http.Request) {
	store := d.userConfigStore()
	storageKey, ok := userConfigStorageKey(r, chi.URLParam(r, "key"))
	if !ok {
		writeError(w, http.StatusBadRequest, "用户配置键无效")
		return
	}
	if store == nil {
		writeError(w, http.StatusServiceUnavailable, "用户配置存储不可用")
		return
	}

	var value any
	found, err := store.GetSetting(r.Context(), storageKey, &value)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "读取用户配置失败")
		return
	}
	if !found {
		value = nil
	}
	writeSuccess(w, map[string]any{"value": value})
}

func (d Dependencies) setUserConfig(w http.ResponseWriter, r *http.Request) {
	store := d.userConfigStore()
	storageKey, ok := userConfigStorageKey(r, chi.URLParam(r, "key"))
	if !ok {
		writeError(w, http.StatusBadRequest, "用户配置键无效")
		return
	}
	if store == nil {
		writeError(w, http.StatusServiceUnavailable, "用户配置存储不可用")
		return
	}

	if d.Config.MaxRequestBody < 1 {
		writeError(w, http.StatusServiceUnavailable, "用户配置请求限制未配置")
		return
	}
	body, err := io.ReadAll(io.LimitReader(r.Body, d.Config.MaxRequestBody+1))
	if err != nil {
		writeError(w, http.StatusBadRequest, "读取用户配置值失败")
		return
	}
	if int64(len(body)) > d.Config.MaxRequestBody {
		writeError(w, http.StatusRequestEntityTooLarge, "用户配置值超过大小限制")
		return
	}
	if len(bytes.TrimSpace(body)) == 0 {
		body = []byte("null")
	}
	decoder := json.NewDecoder(bytes.NewReader(body))
	decoder.UseNumber()
	var value any
	if err := decoder.Decode(&value); err != nil {
		writeError(w, http.StatusBadRequest, "用户配置值必须是有效 JSON")
		return
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		writeError(w, http.StatusBadRequest, "用户配置值只能包含一个 JSON 值")
		return
	}
	if err := store.SetSetting(r.Context(), storageKey, value, false); err != nil {
		writeError(w, http.StatusInternalServerError, "保存用户配置失败")
		return
	}
	writeSuccess(w, nil)
}

func (d Dependencies) userConfigStore() UserConfigStore {
	if d.UserConfigs != nil {
		return d.UserConfigs
	}
	if d.Store == nil {
		return nil
	}
	return d.Store
}

func userConfigStorageKey(r *http.Request, key string) (string, bool) {
	key = strings.TrimSpace(key)
	claims, ok := claimsFromContext(r.Context())
	if !ok || key == "" || len(key) > userConfigKeyLimit || strings.ContainsRune(key, '\x00') {
		return "", false
	}
	userID, err := strconv.ParseInt(claims.Subject, 10, 64)
	if err != nil || userID < 1 {
		return "", false
	}
	encodedKey := base64.RawURLEncoding.EncodeToString([]byte(key))
	return userConfigKeyPrefix + strconv.FormatInt(userID, 10) + ":" + encodedKey, true
}
