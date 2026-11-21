package httpapi

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"math"
	"mime"
	"net/http"
	"net/http/pprof"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/go-chi/chi/v5/middleware"
	"github.com/jackc/pgx/v5"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"

	"github.com/moviepilot-go/moviepilot-go/internal/auth"
	"github.com/moviepilot-go/moviepilot-go/internal/cache"
	"github.com/moviepilot-go/moviepilot-go/internal/config"
	"github.com/moviepilot-go/moviepilot-go/internal/database"
	"github.com/moviepilot-go/moviepilot-go/internal/doctor"
	"github.com/moviepilot-go/moviepilot-go/internal/indexer"
	"github.com/moviepilot-go/moviepilot-go/internal/media"
	"github.com/moviepilot-go/moviepilot-go/internal/metadata"
	"github.com/moviepilot-go/moviepilot-go/internal/pluginengine"
	"github.com/moviepilot-go/moviepilot-go/internal/storage"
	"github.com/moviepilot-go/moviepilot-go/internal/subscription"
	"github.com/moviepilot-go/moviepilot-go/internal/subtitle"
)

const (
	backendVersion  = "v0.1.0-dev"
	frontendVersion = "v2.15.6"
)

// Dependencies 汇总 HTTP 边界所需的显式依赖。
type Dependencies struct {
	Config config.Config
	DB     *database.DB
	Cache  *cache.Redis
	Store  *database.Store
	Doctor *doctor.Runner
	Users  UserLookup
	Auth   *auth.Service
	// LoginThrottle 限制连续登录失败（防爆破）。nil 表示不限制（仅用于测试）。
	LoginThrottle  *loginThrottle
	Logger         *slog.Logger
	Search         *indexer.Coordinator
	SearchAdapters []indexer.Adapter
	SearchRegistry *indexer.Registry
	Metadata       *metadata.Coordinator
	Discover       *metadata.Discover
	// ReloadMetadata 在系统设置里的媒体源密钥（TMDB_API_KEY 等）变化后重建客户端，
	// 对齐 V2 的 CONFIG_WATCH：不重启就生效。为 nil 表示本部署不支持热加载。
	ReloadMetadata              func(context.Context) error
	Categories                  CategoryConfigService
	SubscriptionHistory         SubscriptionHistoryStore
	DownloaderSettings          DownloaderSettingStore
	DownloadSubmissions         DownloadSubmission
	DownloadRequests            DownloadRequestStore
	DownloadTasks               DownloadTaskSource
	DownloadOwners              DownloadIntentOwnerStore
	Sites                       SiteStore
	SiteStatistics              SiteStatisticStore
	SiteValidator               func(database.Site) error
	BuildSiteAdapter            func(database.Site) (indexer.Adapter, error)
	ReloadSites                 func(context.Context) error
	ReloadDownloaders           func(context.Context) error
	MediaServerSettings         MediaServerSettingStore
	ReloadMediaServers          func(context.Context) error
	MediaServerClients          MediaServerClientSource
	MediaServerPlayback         MediaServerPlaybackSource
	MediaServerLibraries        MediaServerLibrarySource
	LocalMediaIndex             LocalMediaIndexSource
	Dashboard                   DashboardSource
	MediaServerRemote           any
	UserConfigs                 UserConfigStore
	NotificationSettings        NotificationSettingStore
	ReloadNotifications         func(context.Context) error
	ReloadNotificationTemplates func(context.Context) error
	ReloadNotificationSendTime  func(context.Context) error
	NotificationHistory         NotificationHistoryStore
	SiteUserDataEffects         SiteUserDataEffects
	EnvironmentSettings         EnvironmentSettings
	SiteCatalog                 SiteCategoryCatalog
	SiteIcons                   SiteIconStore
	CookieCloud                 CookieCloudSyncer
	SiteCookieUpdater           SiteCookieUpdater
	WebPush                     WebPushDispatcher
	Storage                     storage.Provider
	Scheduler                   SchedulerRunner
	LogBuffer                   LogReader
	WebhookNotifier             WebhookNotifier
	Scraper                     ManualScraper
	MediaServerRefresher        MediaServerRefresher
	TorrentCache                torrentCacheStore
	Subtitle                    *subtitle.Coordinator
	// Plugins 是原生插件注册表；为 nil 表示本部署不启用插件体系，
	// 此时插件路由返回空列表/明确失败，而不是 panic。
	Plugins *pluginengine.Registry
}

// ScheduleInfo 对应 V2 `schemas.ScheduleInfo`（仪表盘后台服务列表项）。
type ScheduleInfo struct {
	ID       string `json:"id"`
	Name     string `json:"name"`
	Provider string `json:"provider"`
	Status   string `json:"status"`
	NextRun  string `json:"next_run"`
}

// ScheduleLister 返回当前所有已注册的周期后台任务投影。
type ScheduleLister interface {
	ListSchedules(context.Context) []ScheduleInfo
}

// LogReader 是 Web 端实时日志查看与下载消费的内存日志缓冲契约。
type LogReader interface {
	Tail(n int) []string
	All() []string
	Subscribe(bufSize int) (<-chan string, func())
}

// SchedulerRunner 按 V2 的 jobid 立即触发一次后台服务；未注册的 jobid 返回 false。
type SchedulerRunner interface {
	RunScheduler(context.Context, string) (bool, error)
}

// SubscriptionHistoryStore 是 HTTP 历史接口消费的最小完成记录契约。
type SubscriptionHistoryStore interface {
	ListCompletedSubscriptionsByType(context.Context, string, bool, string, int, int) ([]database.Subscription, error)
	DeleteCompletedSubscription(context.Context, int64, string, bool) (bool, error)
}

// NewRouter 构建 V2 兼容 API、监控端点和静态前端路由。
func NewRouter(deps Dependencies) http.Handler {
	registerer := prometheus.NewRegistry()
	registerer.MustRegister(prometheus.NewGoCollector(), prometheus.NewProcessCollector(prometheus.ProcessCollectorOpts{}))
	registerPostgresPoolMetrics(registerer, deps.DB)
	m := newMetrics(registerer)
	router := chi.NewRouter()
	router.Use(middleware.RealIP)
	router.Use(requestContext)
	router.Use(recoverer(deps.Logger))
	router.Use(limitBody(deps.Config.MaxRequestBody))
	router.Use(observe(m))

	router.Get("/healthz", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, http.StatusOK, map[string]any{"status": "ok", "version": backendVersion})
	})
	router.Get("/readyz", deps.ready)
	router.Handle("/metrics", promhttp.HandlerFor(registerer, promhttp.HandlerOpts{}))

	debugRouter := chi.NewRouter()
	debugRouter.Use(authenticate(deps.Auth), deps.requireAdmin)
	debugRouter.HandleFunc("/", pprof.Index)
	debugRouter.HandleFunc("/cmdline", pprof.Cmdline)
	debugRouter.HandleFunc("/profile", pprof.Profile)
	debugRouter.HandleFunc("/symbol", pprof.Symbol)
	debugRouter.HandleFunc("/trace", pprof.Trace)
	debugRouter.Handle("/allocs", pprof.Handler("allocs"))
	debugRouter.Handle("/goroutine", pprof.Handler("goroutine"))
	debugRouter.Handle("/heap", pprof.Handler("heap"))
	debugRouter.Handle("/mutex", pprof.Handler("mutex"))
	debugRouter.Handle("/threadcreate", pprof.Handler("threadcreate"))
	router.Mount("/debug/pprof", debugRouter)

	router.Mount("/api/v1", deps.v1Router())
	router.Mount("/api/v2", deps.v2Router())
	router.NotFound(notFoundHandler(deps.Config.FrontendDir).ServeHTTP)
	router.MethodNotAllowed(func(w http.ResponseWriter, r *http.Request) {
		if isAPIPath(r.URL.Path) {
			writeError(w, http.StatusMethodNotAllowed, "请求方法不受支持")
			return
		}
		http.Error(w, http.StatusText(http.StatusMethodNotAllowed), http.StatusMethodNotAllowed)
	})
	return router
}

func (d Dependencies) v1Router() http.Handler {
	router := chi.NewRouter()
	router.Post("/login/access-token", d.login)
	router.Get("/login/wallpaper", func(w http.ResponseWriter, _ *http.Request) { writeSuccess(w, nil) })
	router.Get("/login/wallpapers", func(w http.ResponseWriter, _ *http.Request) { writeJSON(w, http.StatusOK, []string{}) })
	router.Get("/auth/providers", func(w http.ResponseWriter, _ *http.Request) { writeJSON(w, http.StatusOK, []any{}) })
	d.registerPublicCompatibility(router)
	router.With(authenticateResource(d.Auth)).Get("/search/title/stream", d.searchTitleStream)
	router.With(authenticateResource(d.Auth)).Get("/search/media/{mediaid}/stream", d.searchMediaStream)
	router.With(authenticateResource(d.Auth)).Get("/resource/download/{siteid}/{resourceid}", d.resolveResourceDownload)

	router.Group(func(protected chi.Router) {
		protected.Use(authenticate(d.Auth))
		d.registerProtected(protected)
	})
	registerAPIFallback(router)
	return router
}

func (d Dependencies) v2Router() http.Handler {
	router := chi.NewRouter()
	router.Post("/login/access-token", func(w http.ResponseWriter, r *http.Request) {
		capture := newCaptureWriter()
		d.login(capture, r)
		for key, values := range capture.header {
			for _, value := range values {
				w.Header().Add(key, value)
			}
		}
		if capture.status >= http.StatusBadRequest {
			w.WriteHeader(capture.status)
			_, _ = w.Write(capture.body.Bytes())
			return
		}
		var token any
		if err := json.Unmarshal(capture.body.Bytes(), &token); err != nil {
			writeError(w, http.StatusInternalServerError, "登录响应编码失败")
			return
		}
		writeSuccess(w, token)
	})
	router.Get("/login/wallpaper", func(w http.ResponseWriter, _ *http.Request) { writeSuccess(w, nil) })
	router.Get("/login/wallpapers", func(w http.ResponseWriter, _ *http.Request) { writeSuccess(w, []string{}) })
	router.Get("/auth/providers", func(w http.ResponseWriter, _ *http.Request) { writeSuccess(w, []any{}) })
	d.registerPublicCompatibility(router)
	router.With(authenticateResource(d.Auth)).Get("/search/title/stream", d.searchTitleStream)
	router.With(authenticateResource(d.Auth)).Get("/search/media/{mediaid}/stream", d.searchMediaStream)
	router.With(authenticateResource(d.Auth)).Get("/resource/download/{siteid}/{resourceid}", d.resolveResourceDownload)
	router.Group(func(protected chi.Router) {
		protected.Use(authenticate(d.Auth))
		d.registerProtected(protected)
	})
	registerAPIFallback(router)
	return router
}

func (d Dependencies) registerProtected(router chi.Router) {
	d.registerProtectedCompatibility(router)
	router.Get("/user/current", d.currentUser)
	router.With(d.requireAdmin).Get("/user/", d.listUsers)
	router.With(d.requireAdmin).Post("/user/", d.createUser)
	router.With(d.requireAdmin).Put("/user/", d.updateUser)
	router.With(d.requireAdmin).Delete("/user/id/{user_id}", d.deleteUserByID)
	router.With(d.requireAdmin).Delete("/user/name/{user_name}", d.deleteUserByName)
	router.With(d.requireAdmin).Get("/user/{username}", d.getUser)
	router.Post("/user/avatar/{user_id}", d.uploadUserAvatar)
	router.Get("/system/ping", func(w http.ResponseWriter, _ *http.Request) { writeSuccess(w, "pong") })
	router.Get("/system/global", d.systemGlobal)
	router.With(d.requireAdmin).Get("/system/doctor", d.systemDoctor)
	router.With(d.requireAdmin).Get("/monitor/runtime", d.monitorRuntime)
	commandRegistry := newCommandRegistry(d)
	router.Get("/command/list", d.listCommands(commandRegistry))
	router.Post("/command", d.executeCommand(commandRegistry))
	router.Post("/command/{name}", d.executeCommandName(commandRegistry))
	router.With(d.requireSystemEnvSuperuser).Get("/system/env", d.systemEnv)
	router.With(d.requireSystemEnvSuperuser).Post("/system/env", d.setSystemEnv)
	router.With(d.requireAdmin).Get("/system/setting/{key}", d.getSetting)
	router.With(d.requireAdmin).Post("/system/setting/{key}", d.setSetting)
	router.Get("/plugin/", d.listPlugins)
	router.Get("/plugin/installed", d.listInstalledPlugins)
	router.Get("/workflow/", func(w http.ResponseWriter, _ *http.Request) { writeJSON(w, http.StatusOK, []any{}) })
	// V2 的工作流尚未迁移：事件类型返回空列表，页面显示空状态而不是 404。
	router.Get("/workflow/event_types", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, http.StatusOK, []any{})
	})
	router.Get("/llm/models", func(w http.ResponseWriter, _ *http.Request) { writeSuccess(w, []any{}) })
	router.Get("/llm/providers", func(w http.ResponseWriter, _ *http.Request) { writeSuccess(w, []any{}) })
	router.Get("/download/clients", d.downloadClients)
	router.Get("/download/paths", d.downloadPaths)
	router.Get("/download", d.listDownloads)
	router.Get("/download/", d.listDownloads)
	router.Post("/download", d.submitDownload)
	router.Post("/download/", d.submitDownload)
	router.Post("/download/add", d.submitDownload)
	// V2 的任务控制入口：暂停 / 续传 / 删除，只对带 MoviePilot 标签的任务生效。
	router.Get("/download/start/{hashString}", d.startDownload)
	router.Get("/download/stop/{hashString}", d.stopDownload)
	router.Delete("/download/{hashString}", d.deleteDownload)
	router.Get("/mediaserver/clients", d.mediaServerClients)
	router.Get("/mediaserver/exists", d.mediaServerExists)
	router.Post("/mediaserver/exists_remote", d.mediaServerExistsRemote)
	router.Get("/mediaserver/latest", d.mediaServerLatest)
	router.Get("/mediaserver/library", d.mediaServerLibrary)
	router.Post("/mediaserver/notexists", d.mediaServerNotExists)
	router.Get("/mediaserver/playing", d.mediaServerPlaying)
	router.Get("/search/last", d.lastSearch)
	router.Get("/search/last/context", d.lastSearchContext)
	router.Get("/search/title", d.searchTitle)
	router.Get("/search/media/{mediaid}", d.searchMedia)
	d.registerSiteCompatibility(router)
	router.With(d.requireManage).Get("/site", d.listSites)
	router.With(d.requireManage).Get("/site/", d.listSites)
	router.With(d.requireManage).Get("/site/{siteid}", d.getSite)
	router.With(d.requireManage).Post("/site", d.createSite)
	router.With(d.requireManage).Post("/site/", d.createSite)
	router.With(d.requireManage).Put("/site", d.updateSite)
	router.With(d.requireManage).Put("/site/", d.updateSite)
	router.With(d.requireManage).Put("/site/{siteid}", d.updateSite)
	router.With(d.requireManage).Delete("/site/{siteid}", d.deleteSite)
	router.Get("/media/recognize", d.recognizeMedia)
	router.Get("/media/category/config", d.getCategoryConfig)
	router.With(d.requireAdmin).Post("/media/category/config", d.saveCategoryConfig)
	router.Get("/media/category", d.listMediaCategories)
	router.Get("/media/search", d.mediaSearch)
	router.Get("/media/recognize_file", d.mediaRecognizeFile)
	router.Get("/media/seasons", d.mediaSeasons)
	router.Get("/media/groups/{tmdbid}", d.mediaGroups)
	router.Get("/media/group/seasons/{episode_group}", d.mediaGroupSeasons)
	router.Get("/media/{mediaid}", d.mediaDetail)
	router.Get("/subscribe/", d.listSubscriptions)
	router.Get("/subscribe", d.listSubscriptions)
	router.Get("/subscribe/history/{mtype}", d.listSubscriptionHistory)
	router.Get("/subscribe/media/{mediaid}", d.subscriptionByMedia)
	router.Get("/subscribe/user/{username}", d.listUserSubscriptions)
	router.Post("/subscribe/", d.createSubscription)
	router.Post("/subscribe", d.createSubscription)
	router.Put("/subscribe/", d.updateSubscription)
	router.Put("/subscribe", d.updateSubscription)
	router.Get("/subscribe/reset/{subid}", d.resetSubscription)
	router.Put("/subscribe/status/{subid}", d.updateSubscriptionStatus)
	router.Delete("/subscribe/history/{history_id}", d.deleteSubscriptionHistory)
	router.Delete("/subscribe/media/{mediaid}", d.deleteSubscriptionByMedia)
	router.Delete("/subscribe/{subid}", d.deleteSubscription)
	router.Get("/subscribe/{subscribe_id}", d.getSubscription)
}

func (d Dependencies) login(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseMultipartForm(1 << 20); err != nil {
		if err := r.ParseForm(); err != nil {
			writeError(w, http.StatusBadRequest, "登录参数无效")
			return
		}
	}
	username := strings.TrimSpace(r.FormValue("username"))
	clientIP := clientIPFromRequest(r)
	// 先看锁定状态：锁定期间连 bcrypt 都不做，既省 CPU 也让爆破变得不划算。
	if wait, locked := d.LoginThrottle.blocked(username, clientIP); locked {
		w.Header().Set("Retry-After", strconv.Itoa(int(math.Ceil(wait.Seconds()))))
		writeError(w, http.StatusTooManyRequests, "登录尝试过于频繁，请 "+humanShortDuration(wait)+"后再试")
		return
	}
	user, err := d.Auth.Authenticate(r.Context(), username, r.FormValue("password"))
	if err != nil {
		if lockFor, locked := d.LoginThrottle.fail(username, clientIP); locked && d.Logger != nil {
			d.Logger.Warn("登录失败次数过多，已临时锁定",
				"username", username, "ip", clientIP, "lock", lockFor.String())
		}
		writeError(w, http.StatusUnauthorized, "用户名或密码错误")
		return
	}
	d.LoginThrottle.reset(username, clientIP)
	token, err := d.Auth.AccessToken(user)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "生成访问令牌失败")
		return
	}
	resourceToken, err := d.Auth.ResourceToken(user, resourceTokenLifetime)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "生成资源令牌失败")
		return
	}
	secure := r.TLS != nil || strings.EqualFold(r.Header.Get("X-Forwarded-Proto"), "https")
	http.SetCookie(w, &http.Cookie{
		Name: resourceCookieName, Value: resourceToken, Path: "/", HttpOnly: true,
		Secure: secure, SameSite: http.SameSiteLaxMode,
	})
	writeJSON(w, http.StatusOK, map[string]any{
		"access_token": token, "token_type": "bearer", "super_user": user.SuperUser,
		"user_id": user.ID, "user_name": user.Username, "avatar": user.Avatar,
		"level": 1, "permissions": user.Permissions, "wizard": false,
	})
}

func (d Dependencies) currentUser(w http.ResponseWriter, r *http.Request) {
	claims, _ := claimsFromContext(r.Context())
	id, err := strconv.ParseInt(claims.Subject, 10, 64)
	if err != nil {
		writeError(w, http.StatusForbidden, "token校验不通过")
		return
	}
	user, err := d.Store.UserByID(r.Context(), id)
	if errors.Is(err, pgx.ErrNoRows) {
		writeError(w, http.StatusNotFound, "用户不存在")
		return
	}
	if err != nil {
		writeError(w, http.StatusInternalServerError, "查询用户失败")
		return
	}
	writeJSON(w, http.StatusOK, user)
}

func (d Dependencies) systemGlobal(w http.ResponseWriter, _ *http.Request) {
	writeSuccess(w, map[string]any{
		"APP_VERSION": backendVersion, "FRONTEND_VERSION": frontendVersion,
		"PROJECT_NAME": "MoviePilot", "ENV": d.Config.Environment,
	})
}

func (d Dependencies) getSetting(w http.ResponseWriter, r *http.Request) {
	if chi.URLParam(r, "key") == notificationSettingsKey {
		d.getNotificationSettings(w, r)
		return
	}
	if chi.URLParam(r, "key") == database.DownloadersSettingKey {
		d.getDownloadersSetting(w, r)
		return
	}
	if chi.URLParam(r, "key") == database.MediaServersSettingKey {
		d.getMediaServersSetting(w, r)
		return
	}
	var value any
	found, err := d.Store.GetSetting(r.Context(), chi.URLParam(r, "key"), &value)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "读取系统设置失败")
		return
	}
	if !found {
		value = nil
	}
	writeSuccess(w, value)
}

func (d Dependencies) setSetting(w http.ResponseWriter, r *http.Request) {
	if chi.URLParam(r, "key") == notificationSettingsKey {
		d.setNotificationSettings(w, r)
		return
	}
	if chi.URLParam(r, "key") == database.DownloadersSettingKey {
		d.setDownloadersSetting(w, r)
		return
	}
	if chi.URLParam(r, "key") == database.MediaServersSettingKey {
		d.setMediaServersSetting(w, r)
		return
	}
	reader := io.LimitReader(r.Body, d.Config.MaxRequestBody)
	decoder := json.NewDecoder(reader)
	decoder.UseNumber()
	var value any
	if err := decoder.Decode(&value); err != nil {
		writeError(w, http.StatusBadRequest, "设置值必须是有效 JSON")
		return
	}
	if err := d.Store.SetSetting(r.Context(), chi.URLParam(r, "key"), value, false); err != nil {
		writeError(w, http.StatusInternalServerError, "保存系统设置失败")
		return
	}
	if chi.URLParam(r, "key") == database.NotificationTemplatesSettingKey && d.ReloadNotificationTemplates != nil {
		reloadCtx, cancel := context.WithTimeout(context.WithoutCancel(r.Context()), notificationReloadLimit)
		if err := d.ReloadNotificationTemplates(reloadCtx); err != nil && d.Logger != nil {
			d.Logger.Warn("通知模板运行态重载失败，继续使用旧快照")
		}
		cancel()
	}
	if chi.URLParam(r, "key") == database.NotificationSendTimeSettingKey && d.ReloadNotificationSendTime != nil {
		reloadCtx, cancel := context.WithTimeout(context.WithoutCancel(r.Context()), notificationReloadLimit)
		if err := d.ReloadNotificationSendTime(reloadCtx); err != nil && d.Logger != nil {
			d.Logger.Warn("通知发送时段运行态重载失败，继续使用旧快照")
		}
		cancel()
	}
	// V2 的 CONFIG_WATCH = {PROXY_HOST, TMDB_API_DOMAIN, TMDB_API_KEY, TMDB_LOCALE}：
	// 这几个键改完都要重建客户端，保存后立刻生效。
	if key := chi.URLParam(r, "key"); key == database.TMDBAPIKeySettingKey ||
		key == database.TMDBAPIDomainSettingKey || key == database.ProxyHostSettingKey {
		// V2 的 CONFIG_WATCH 在 TMDB 密钥/域名变化时重建客户端；保存后立刻热加载，
		// 用户不用重启就能看到海报、演职员和推荐列表。热加载失败不吞错：写日志提示需重启。
		if d.ReloadMetadata != nil {
			reloadCtx, cancel := context.WithTimeout(context.WithoutCancel(r.Context()), notificationReloadLimit)
			err := d.ReloadMetadata(reloadCtx)
			cancel()
			if d.Logger != nil {
				if err != nil {
					d.Logger.Warn("媒体源设置已保存，但热加载失败，需重启后生效", "key", key, "error", err)
				} else {
					d.Logger.Info("媒体源设置已热加载", "key", key)
				}
			}
		}
	}
	writeSuccess(w, nil)
}

func (d Dependencies) ready(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 2*time.Second)
	defer cancel()
	status := map[string]any{"database": false, "redis": false}
	if d.DB == nil || d.DB.Ping(ctx) != nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]any{"status": "not_ready", "checks": status})
		return
	}
	status["database"] = true
	if d.Cache != nil && d.Cache.Ping(ctx) == nil {
		status["redis"] = true
	}
	writeJSON(w, http.StatusOK, map[string]any{"status": "ready", "checks": status})
}

func (d Dependencies) recognizeMedia(w http.ResponseWriter, r *http.Request) {
	title := strings.TrimSpace(r.URL.Query().Get("title"))
	if title == "" {
		writeError(w, http.StatusBadRequest, "title不能为空")
		return
	}
	customWords := strings.Split(strings.TrimSpace(r.URL.Query().Get("custom_words")), "\n")
	if len(customWords) == 1 && customWords[0] == "" {
		customWords = nil
	}
	parsed := media.Parse(title+" "+r.URL.Query().Get("subtitle"), media.Options{CustomWords: customWords})
	var mediaInfo any
	matchSource := "unknown"
	candidateRecognized := parsed.MediaID != ""
	if d.Metadata != nil {
		item, found, err := d.identifyMedia(r.Context(), parsed, r.URL.Query().Get("source"), title)
		if err != nil {
			if d.Logger != nil {
				d.Logger.Warn("媒体元数据识别失败", "request_id", requestID(r.Context()), "error", err)
			}
		} else if found {
			mediaInfo = v2MediaInfo(item, parsed.BeginSeason)
			candidateRecognized = true
			if parsed.MediaID != "" {
				matchSource = metadataMatchSource(item.Source)
			} else {
				matchSource = "title"
			}
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"meta_info": parsed, "torrent_info": nil, "media_info": mediaInfo,
		"media_recognize_fail_count": 0, "resource_source": "unknown", "match_source": matchSource,
		"candidate_recognized": candidateRecognized, "media_info_is_target": false,
		"allowed_episodes": nil, "confirmed_full_coverage": false,
	})
}

type subscriptionRequest struct {
	Name            string   `json:"name"`
	Year            string   `json:"year"`
	Type            string   `json:"type"`
	TMDBID          int64    `json:"tmdbid"`
	DoubanID        string   `json:"doubanid"`
	BangumiID       int64    `json:"bangumiid"`
	AniListID       int64    `json:"anilistid"`
	MediaKey        string   `json:"mediaid"`
	MediaSource     string   `json:"media_source"`
	MediaID         string   `json:"media_id"`
	Season          *int     `json:"season"`
	TotalEpisode    int      `json:"total_episode"`
	StartEpisode    int      `json:"start_episode"`
	Keyword         string   `json:"keyword"`
	Filter          string   `json:"filter"`
	Include         string   `json:"include"`
	Exclude         string   `json:"exclude"`
	Quality         string   `json:"quality"`
	Resolution      string   `json:"resolution"`
	Effect          string   `json:"effect"`
	Sites           []int    `json:"sites"`
	Downloader      string   `json:"downloader"`
	SavePath        string   `json:"save_path"`
	BestVersion     int      `json:"best_version"`
	BestVersionFull int      `json:"best_version_full"`
	SearchIMDBID    int      `json:"search_imdbid"`
	CustomWords     string   `json:"custom_words"`
	MediaCategory   string   `json:"media_category"`
	FilterGroups    []string `json:"filter_groups"`
	EpisodeGroup    string   `json:"episode_group"`
}

type subscriptionUpdateRequest struct {
	ID              *int64   `json:"id"`
	Name            *string  `json:"name"`
	Year            *string  `json:"year"`
	Type            *string  `json:"type"`
	Keyword         *string  `json:"keyword"`
	TMDBID          *int64   `json:"tmdbid"`
	DoubanID        *string  `json:"doubanid"`
	BangumiID       *int64   `json:"bangumiid"`
	AniListID       *int64   `json:"anilistid"`
	MediaKey        *string  `json:"mediaid"`
	MediaSource     *string  `json:"media_source"`
	MediaID         *string  `json:"media_id"`
	Season          *int     `json:"season"`
	Filter          *string  `json:"filter"`
	Include         *string  `json:"include"`
	Exclude         *string  `json:"exclude"`
	Quality         *string  `json:"quality"`
	Resolution      *string  `json:"resolution"`
	Effect          *string  `json:"effect"`
	TotalEpisode    *int     `json:"total_episode"`
	StartEpisode    *int     `json:"start_episode"`
	Sites           []int    `json:"sites"`
	Downloader      *string  `json:"downloader"`
	BestVersion     *int     `json:"best_version"`
	BestVersionFull *int     `json:"best_version_full"`
	SavePath        *string  `json:"save_path"`
	SearchIMDBID    *int     `json:"search_imdbid"`
	CustomWords     *string  `json:"custom_words"`
	MediaCategory   *string  `json:"media_category"`
	FilterGroups    []string `json:"filter_groups"`
	EpisodeGroup    *string  `json:"episode_group"`
}

func newSubscriptionUpdateRequest() subscriptionUpdateRequest {
	zeroTotal, zeroStart, zeroSearch := 0, 0, 0
	return subscriptionUpdateRequest{
		TotalEpisode: &zeroTotal, StartEpisode: &zeroStart, SearchIMDBID: &zeroSearch,
		Sites: []int{}, FilterGroups: []string{},
	}
}

func (d Dependencies) updateSubscription(w http.ResponseWriter, r *http.Request) {
	claims, _ := claimsFromContext(r.Context())
	input := newSubscriptionUpdateRequest()
	decoder := json.NewDecoder(io.LimitReader(r.Body, d.Config.MaxRequestBody))
	if err := decoder.Decode(&input); err != nil {
		writeError(w, http.StatusUnprocessableEntity, "订阅参数无效")
		return
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		writeError(w, http.StatusUnprocessableEntity, "订阅参数无效")
		return
	}
	if input.ID == nil || *input.ID <= 0 {
		writeError(w, http.StatusOK, database.ErrSubscriptionNotFound.Error())
		return
	}
	if !validSubscriptionUpdateRequest(input) {
		writeError(w, http.StatusOK, "订阅参数无效")
		return
	}
	if d.Store == nil {
		writeError(w, http.StatusInternalServerError, "更新订阅失败")
		return
	}
	observed, err := d.Store.GetAccessibleActiveSubscription(r.Context(), claims.Username, *input.ID, claims.SuperUser)
	if errors.Is(err, database.ErrSubscriptionNotFound) {
		writeError(w, http.StatusOK, database.ErrSubscriptionNotFound.Error())
		return
	}
	if err != nil {
		writeError(w, http.StatusInternalServerError, "更新订阅失败")
		return
	}
	// 局部更新（只带 id + 少数字段）时，title 是 NOT NULL 列，必须回落到原值，
	// 否则整条更新会因 NULL 违反约束而 500。
	if input.Name == nil || strings.TrimSpace(*input.Name) == "" {
		name := observed.Name
		input.Name = &name
	}
	configPatch := subscriptionUpdateConfig(input)
	mutation := database.SubscriptionPublicMutation{
		Username: observed.Username, SubscriptionID: observed.ID, ExpectedUpdatedAt: observed.UpdatedAt,
		Name: input.Name, MediaType: input.Type, Year: input.Year, Season: input.Season,
		MediaSource: input.MediaSource, MediaID: input.MediaID,
		Config: configPatch, ReplacePublic: true,
	}
	if input.Type != nil && isTVSubscriptionType(*input.Type) {
		season := 1
		if input.Season != nil {
			season = *input.Season
		}
		target, targetErr := database.ResolveSubscriptionTVPublicTarget(
			observed, season, subscriptionUpdateInteger(input.StartEpisode), subscriptionUpdateInteger(input.TotalEpisode),
		)
		if targetErr != nil {
			writeError(w, http.StatusOK, "订阅参数无效")
			return
		}
		if subscriptionUpdateTotalNullnessChanged(observed, input.TotalEpisode) {
			target.Snapshot.ManualTotalEpisode = true
			target.Changed = true
		}
		mutation.Target = &target
	} else {
		progress, progressErr := database.ResolveSubscriptionPublicProgress(
			observed, subscriptionUpdateInteger(input.TotalEpisode),
		)
		if progressErr != nil {
			writeError(w, http.StatusOK, "订阅参数无效")
			return
		}
		if subscriptionUpdateTotalNullnessChanged(observed, input.TotalEpisode) {
			progress.ManualTotalEpisode = true
		}
		mutation.Progress = &progress
	}
	_, err = d.Store.ApplySubscriptionPublicMutation(r.Context(), mutation)
	if errors.Is(err, database.ErrSubscriptionNotFound) {
		writeError(w, http.StatusOK, database.ErrSubscriptionNotFound.Error())
		return
	}
	if errors.Is(err, database.ErrSubscriptionPublicMutationConflict) {
		writeError(w, http.StatusOK, "订阅已更新，请重试")
		return
	}
	if errors.Is(err, database.ErrInvalidSubscriptionPublicMutation) {
		writeError(w, http.StatusOK, "订阅参数无效")
		return
	}
	if err != nil {
		if d.Logger != nil {
			d.Logger.Error("更新订阅失败", "request_id", requestID(r.Context()), "subscription_id", observed.ID, "error", err)
		}
		writeError(w, http.StatusInternalServerError, "更新订阅失败")
		return
	}
	writeSuccess(w, nil)
}

func subscriptionUpdateTotalNullnessChanged(observed database.Subscription, total *int) bool {
	old, exists := observed.Config["total_episode"]
	oldNull := !exists || old == nil
	newNull := total == nil
	return oldNull != newNull
}

func validSubscriptionUpdateRequest(input subscriptionUpdateRequest) bool {
	for _, value := range []*int{input.TotalEpisode, input.StartEpisode} {
		if value != nil && (*value < 0 || *value > 5000) {
			return false
		}
	}
	if input.Season != nil && (*input.Season < 0 || *input.Season > 999) {
		return false
	}
	for _, value := range []*int64{input.TMDBID, input.BangumiID, input.AniListID} {
		if value != nil && *value < 0 {
			return false
		}
	}
	return len(input.Sites) <= 1024 && len(input.FilterGroups) <= 1024
}

func subscriptionUpdateConfig(input subscriptionUpdateRequest) map[string]any {
	return map[string]any{
		"keyword": input.Keyword, "tmdbid": input.TMDBID, "doubanid": input.DoubanID,
		"bangumiid": input.BangumiID, "anilistid": input.AniListID, "mediaid": input.MediaKey,
		"filter": input.Filter, "include": input.Include, "exclude": input.Exclude,
		"quality": input.Quality, "resolution": input.Resolution, "effect": input.Effect,
		"total_episode": input.TotalEpisode, "start_episode": input.StartEpisode,
		"sites": input.Sites, "downloader": input.Downloader,
		"best_version": input.BestVersion, "best_version_full": input.BestVersionFull,
		"save_path": input.SavePath, "search_imdbid": input.SearchIMDBID,
		"custom_words": input.CustomWords, "media_category": input.MediaCategory,
		"filter_groups": input.FilterGroups, "episode_group": input.EpisodeGroup,
	}
}

func subscriptionUpdateInteger(value *int) int {
	if value == nil {
		return 0
	}
	return *value
}

func isTVSubscriptionType(value string) bool {
	switch strings.ToLower(strings.TrimSpace(value)) {
	case "tv", "电视剧":
		return true
	default:
		return false
	}
}

func (d Dependencies) createSubscription(w http.ResponseWriter, r *http.Request) {
	claims, _ := claimsFromContext(r.Context())
	var input subscriptionRequest
	decoder := json.NewDecoder(io.LimitReader(r.Body, d.Config.MaxRequestBody))
	if err := decoder.Decode(&input); err != nil {
		writeError(w, http.StatusBadRequest, "订阅参数无效")
		return
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		writeError(w, http.StatusBadRequest, "订阅参数无效")
		return
	}
	if err := validateSubscriptionRequest(input); err != nil {
		// V2 的订阅业务错误使用 HTTP 200 + success=false；前端据此展示 message。
		writeError(w, http.StatusOK, err.Error())
		return
	}
	mediaSource, mediaID := normalizeSubscriptionIdentity(input)
	state := subscription.PendingIdentification
	if mediaID != "" {
		state = subscription.PendingSearch
	}
	configValue := map[string]any{
		"keyword": input.Keyword, "filter": input.Filter, "include": input.Include, "exclude": input.Exclude,
		"quality": input.Quality, "resolution": input.Resolution, "effect": input.Effect,
		"sites": input.Sites, "downloader": input.Downloader, "save_path": input.SavePath,
		"best_version": input.BestVersion, "best_version_full": input.BestVersionFull,
		"total_episode": input.TotalEpisode, "start_episode": input.StartEpisode,
		"search_imdbid": input.SearchIMDBID, "custom_words": input.CustomWords,
		"media_category": input.MediaCategory, "filter_groups": input.FilterGroups,
		"episode_group": input.EpisodeGroup,
	}
	id, err := d.Store.CreateSubscription(r.Context(), database.NewSubscription{
		Username: claims.Username, Name: input.Name, MediaType: input.Type, Year: input.Year,
		Season: input.Season, MediaSource: mediaSource, MediaID: mediaID, State: state, Config: configValue,
	})
	if err != nil {
		writeError(w, http.StatusInternalServerError, "创建订阅失败")
		return
	}
	writeSuccess(w, map[string]any{"id": id})
}

func validateSubscriptionRequest(input subscriptionRequest) error {
	if strings.TrimSpace(input.Name) == "" || (input.Type != "电影" && input.Type != "电视剧") {
		return errors.New("订阅名称或类型无效")
	}
	if input.Season != nil && *input.Season < 0 {
		return errors.New("订阅季号不能为负数")
	}
	if input.TotalEpisode < 0 || input.TotalEpisode > 5000 || input.StartEpisode < 0 || input.StartEpisode > 5000 {
		return errors.New("订阅集数超出有效范围")
	}
	if input.TMDBID < 0 || input.BangumiID < 0 || input.AniListID < 0 {
		return errors.New("订阅媒体 ID 无效")
	}
	mediaSource := strings.TrimSpace(input.MediaSource)
	mediaID := strings.TrimSpace(input.MediaID)
	if (mediaSource == "") != (mediaID == "") {
		return errors.New("订阅媒体来源和媒体 ID 必须同时提供")
	}
	if len(input.Sites) > 1024 {
		return errors.New("订阅站点数量超出上限")
	}
	for _, siteID := range input.Sites {
		if siteID <= 0 {
			return errors.New("订阅站点 ID 无效")
		}
	}
	return nil
}

func (d Dependencies) listSubscriptions(w http.ResponseWriter, r *http.Request) {
	claims, _ := claimsFromContext(r.Context())
	items, err := d.Store.ListSubscriptions(r.Context(), claims.Username, claims.SuperUser)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "查询订阅失败")
		return
	}
	result := make([]map[string]any, 0, len(items))
	for _, item := range items {
		result = append(result, item.V2())
	}
	writeJSON(w, http.StatusOK, result)
}

func (d Dependencies) listSubscriptionHistory(w http.ResponseWriter, r *http.Request) {
	page, count, err := subscriptionHistoryPage(r)
	if err != nil {
		writeError(w, http.StatusBadRequest, database.ErrInvalidSubscriptionHistoryPage.Error())
		return
	}
	store := d.subscriptionHistoryStore()
	if store == nil {
		writeError(w, http.StatusInternalServerError, "查询订阅历史失败")
		return
	}
	claims, _ := claimsFromContext(r.Context())
	items, err := store.ListCompletedSubscriptionsByType(
		r.Context(),
		claims.Username,
		claims.SuperUser,
		strings.TrimSpace(chi.URLParam(r, "mtype")),
		page,
		count,
	)
	if err != nil {
		if errors.Is(err, database.ErrInvalidSubscriptionHistoryPage) {
			writeError(w, http.StatusBadRequest, err.Error())
			return
		}
		if d.Logger != nil {
			d.Logger.Error("查询订阅历史失败", "request_id", requestID(r.Context()))
		}
		writeError(w, http.StatusInternalServerError, "查询订阅历史失败")
		return
	}
	result := make([]map[string]any, 0, len(items))
	for _, item := range items {
		result = append(result, item.V2History())
	}
	writeJSON(w, http.StatusOK, result)
}

func subscriptionHistoryPage(r *http.Request) (int, int, error) {
	page, err := positiveQueryInt(r, "page", 1)
	if err != nil {
		return 0, 0, database.ErrInvalidSubscriptionHistoryPage
	}
	count, err := positiveQueryInt(r, "count", 30)
	if err != nil || count > 100 {
		return 0, 0, database.ErrInvalidSubscriptionHistoryPage
	}
	return page, count, nil
}

func positiveQueryInt(r *http.Request, key string, fallback int) (int, error) {
	value := strings.TrimSpace(r.URL.Query().Get(key))
	if value == "" {
		return fallback, nil
	}
	parsed, err := strconv.Atoi(value)
	if err != nil || parsed < 1 {
		return 0, database.ErrInvalidSubscriptionHistoryPage
	}
	return parsed, nil
}

func (d Dependencies) subscriptionHistoryStore() SubscriptionHistoryStore {
	if d.SubscriptionHistory != nil {
		return d.SubscriptionHistory
	}
	if d.Store != nil {
		return d.Store
	}
	return nil
}

func (d Dependencies) updateSubscriptionStatus(w http.ResponseWriter, r *http.Request) {
	claims, _ := claimsFromContext(r.Context())
	id, err := strconv.ParseInt(chi.URLParam(r, "subid"), 10, 64)
	if err != nil || id <= 0 {
		writeError(w, http.StatusBadRequest, "订阅ID无效")
		return
	}
	state := strings.TrimSpace(r.URL.Query().Get("state"))
	if _, err := subscription.ParseV2ControlState(state); err != nil {
		writeError(w, http.StatusOK, database.ErrInvalidSubscriptionState.Error())
		return
	}
	if err := d.Store.SetSubscriptionV2State(r.Context(), id, claims.Username, claims.SuperUser, state); err != nil {
		if errors.Is(err, database.ErrInvalidSubscriptionState) || errors.Is(err, database.ErrSubscriptionNotFound) {
			writeError(w, http.StatusOK, err.Error())
			return
		}
		if d.Logger != nil {
			d.Logger.Error("更新订阅状态失败", "request_id", requestID(r.Context()), "subscription_id", id)
		}
		writeError(w, http.StatusInternalServerError, "更新订阅状态失败")
		return
	}
	writeSuccess(w, nil)
}

func (d Dependencies) resetSubscription(w http.ResponseWriter, r *http.Request) {
	claims, _ := claimsFromContext(r.Context())
	id, err := strconv.ParseInt(chi.URLParam(r, "subid"), 10, 64)
	if err != nil || id <= 0 {
		writeError(w, http.StatusBadRequest, "订阅ID无效")
		return
	}
	if d.Store == nil {
		writeError(w, http.StatusInternalServerError, "重置订阅失败")
		return
	}
	observed, err := d.Store.GetAccessibleActiveSubscription(
		r.Context(), claims.Username, id, claims.SuperUser,
	)
	if errors.Is(err, database.ErrSubscriptionNotFound) {
		writeError(w, http.StatusOK, database.ErrSubscriptionNotFound.Error())
		return
	}
	if err != nil {
		if d.Logger != nil {
			d.Logger.Error("读取重置订阅失败", "request_id", requestID(r.Context()), "subscription_id", id)
		}
		writeError(w, http.StatusInternalServerError, "重置订阅失败")
		return
	}
	_, err = d.Store.ResetActiveSubscription(r.Context(), observed)
	if errors.Is(err, database.ErrSubscriptionNotFound) {
		writeError(w, http.StatusOK, database.ErrSubscriptionNotFound.Error())
		return
	}
	if errors.Is(err, database.ErrSubscriptionPublicMutationConflict) {
		writeError(w, http.StatusOK, "订阅已更新，请重试")
		return
	}
	if err != nil {
		if d.Logger != nil {
			d.Logger.Error("重置订阅失败", "request_id", requestID(r.Context()), "subscription_id", id)
		}
		writeError(w, http.StatusInternalServerError, "重置订阅失败")
		return
	}
	writeSuccess(w, nil)
}

func (d Dependencies) deleteSubscription(w http.ResponseWriter, r *http.Request) {
	claims, _ := claimsFromContext(r.Context())
	id, err := strconv.ParseInt(chi.URLParam(r, "subid"), 10, 64)
	if err != nil {
		writeError(w, http.StatusBadRequest, "订阅ID无效")
		return
	}
	deleted, err := d.Store.DeleteSubscription(r.Context(), id, claims.Username, claims.SuperUser)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "删除订阅失败")
		return
	}
	if !deleted {
		writeSuccess(w, map[string]any{})
		return
	}
	writeSuccess(w, nil)
}

func (d Dependencies) deleteSubscriptionHistory(w http.ResponseWriter, r *http.Request) {
	claims, _ := claimsFromContext(r.Context())
	id, err := strconv.ParseInt(chi.URLParam(r, "history_id"), 10, 64)
	if err != nil || id <= 0 {
		writeError(w, http.StatusBadRequest, "订阅历史ID无效")
		return
	}
	store := d.subscriptionHistoryStore()
	if store == nil {
		writeError(w, http.StatusInternalServerError, "删除订阅历史失败")
		return
	}
	if _, err := store.DeleteCompletedSubscription(r.Context(), id, claims.Username, claims.SuperUser); err != nil {
		if d.Logger != nil {
			d.Logger.Error("删除订阅历史失败", "request_id", requestID(r.Context()), "history_id", id)
		}
		writeError(w, http.StatusInternalServerError, "删除订阅历史失败")
		return
	}
	// 与官方 V2 一致：不存在或无权访问都按幂等成功处理，避免枚举记录。
	writeSuccess(w, nil)
}

func normalizeSubscriptionIdentity(input subscriptionRequest) (string, string) {
	mediaSource := normalizeSubscriptionMediaSource(input.MediaSource)
	mediaID := strings.TrimSpace(input.MediaID)
	if mediaSource != "" && mediaID != "" {
		return mediaSource, mediaID
	}
	switch {
	case input.TMDBID > 0:
		return "themoviedb", strconv.FormatInt(input.TMDBID, 10)
	case strings.TrimSpace(input.DoubanID) != "":
		return "douban", strings.TrimSpace(input.DoubanID)
	case input.BangumiID > 0:
		return "bangumi", strconv.FormatInt(input.BangumiID, 10)
	case input.AniListID > 0:
		return "anilist", strconv.FormatInt(input.AniListID, 10)
	}
	parts := strings.SplitN(strings.TrimSpace(input.MediaKey), ":", 2)
	if len(parts) != 2 || strings.TrimSpace(parts[1]) == "" {
		return "", ""
	}
	source := normalizeSubscriptionMediaSource(parts[0])
	switch source {
	case "themoviedb", "douban", "bangumi", "anilist":
	default:
		return "", ""
	}
	return source, strings.TrimSpace(parts[1])
}

func normalizeSubscriptionMediaSource(source string) string {
	source = strings.ToLower(strings.TrimSpace(source))
	if source == "tmdb" {
		return "themoviedb"
	}
	return source
}

func staticHandler(frontendDir string) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			http.NotFound(w, r)
			return
		}
		root, err := filepath.Abs(frontendDir)
		if err != nil {
			http.NotFound(w, r)
			return
		}
		clean := filepath.Clean(strings.TrimPrefix(r.URL.Path, "/"))
		candidate := filepath.Join(root, clean)
		relative, err := filepath.Rel(root, candidate)
		if err != nil || strings.HasPrefix(relative, "..") {
			http.NotFound(w, r)
			return
		}
		info, err := os.Stat(candidate)
		if err != nil || info.IsDir() {
			candidate = filepath.Join(root, "index.html")
		}
		if extension := filepath.Ext(candidate); extension != "" {
			if contentType := mime.TypeByExtension(extension); contentType != "" {
				w.Header().Set("Content-Type", contentType)
			}
		}
		if strings.Contains(filepath.Base(candidate), ".") && filepath.Base(candidate) != "index.html" {
			w.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
		} else {
			w.Header().Set("Cache-Control", "no-cache")
		}
		http.ServeFile(w, r, candidate)
	})
}

func notFoundHandler(frontendDir string) http.Handler {
	static := staticHandler(frontendDir)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if isAPIPath(r.URL.Path) {
			writeError(w, http.StatusNotFound, "接口不存在")
			return
		}
		static.ServeHTTP(w, r)
	})
}

func isAPIPath(path string) bool {
	return path == "/api" || strings.HasPrefix(path, "/api/")
}

func registerAPIFallback(router *chi.Mux) {
	router.NotFound(func(w http.ResponseWriter, _ *http.Request) {
		writeError(w, http.StatusNotFound, "接口不存在")
	})
	router.MethodNotAllowed(func(w http.ResponseWriter, _ *http.Request) {
		writeError(w, http.StatusMethodNotAllowed, "请求方法不受支持")
	})
}
