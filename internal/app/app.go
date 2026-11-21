package app

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"sync/atomic"
	"time"

	"github.com/moviepilot-go/moviepilot-go/internal/auth"
	"github.com/moviepilot-go/moviepilot-go/internal/cache"
	"github.com/moviepilot-go/moviepilot-go/internal/config"
	"github.com/moviepilot-go/moviepilot-go/internal/database"
	"github.com/moviepilot-go/moviepilot-go/internal/downloadflow"
	"github.com/moviepilot-go/moviepilot-go/internal/events"
	"github.com/moviepilot-go/moviepilot-go/internal/httpapi"
	"github.com/moviepilot-go/moviepilot-go/internal/indexer"
	"github.com/moviepilot-go/moviepilot-go/internal/jobs"
	"github.com/moviepilot-go/moviepilot-go/internal/logbuf"
	"github.com/moviepilot-go/moviepilot-go/internal/mediaindex"
	"github.com/moviepilot-go/moviepilot-go/internal/metadata"
	"github.com/moviepilot-go/moviepilot-go/internal/notification"
	"github.com/moviepilot-go/moviepilot-go/internal/notificationflow"
	"github.com/moviepilot-go/moviepilot-go/internal/organization"
	"github.com/moviepilot-go/moviepilot-go/internal/organizationflow"
	"github.com/moviepilot-go/moviepilot-go/internal/scrapeflow"
	"github.com/moviepilot-go/moviepilot-go/internal/scraper"
	"github.com/moviepilot-go/moviepilot-go/internal/siteflow"
	"github.com/moviepilot-go/moviepilot-go/internal/storage"
	"github.com/moviepilot-go/moviepilot-go/internal/subscriptionflow"
)

var defaultLogBuffer atomic.Pointer[logbuf.Buffer]

// SetDefaultLogBuffer 注入进程级内存日志缓冲，供 /api/v1/system/logging 实时查看与下载。
func SetDefaultLogBuffer(buf *logbuf.Buffer) {
	defaultLogBuffer.Store(buf)
}

// Run 执行迁移、初始化依赖并运行单进程服务，直到上下文取消。
func Run(ctx context.Context, cfg config.Config, logger *slog.Logger) error {
	if err := Migrate(ctx, cfg); err != nil {
		return err
	}

	db, err := database.Open(ctx, cfg)
	if err != nil {
		return err
	}
	defer db.Close()
	secretBox, err := auth.NewSecretBox(cfg.MasterKey)
	if err != nil {
		return fmt.Errorf("初始化敏感配置加密: %w", err)
	}
	store := database.NewStore(db, secretBox)
	if err := store.UpgradeLegacySiteSecrets(ctx); err != nil {
		return fmt.Errorf("升级历史站点凭据: %w", err)
	}
	if err := store.EnsureBootstrapUser(ctx, cfg.BootstrapUsername, cfg.BootstrapPassword); err != nil {
		return err
	}
	// V2 把媒体源密钥放在系统设置里（TMDB_API_KEY / TMDB_API_DOMAIN 都在 CONFIG_WATCH
	// 里，改动会立刻重建 TMDB 客户端）。环境变量没给时先读一次设置库，这样在
	// 「设置 → 系统」里填的密钥重启后照样生效；运行中的改动由 ReloadMetadata 接上。
	cfg = applyMetadataSettings(ctx, store, cfg, logger)

	redisCache, err := cache.Open(ctx, cfg)
	if err != nil {
		logger.Warn("Redis 当前不可用，服务将以无缓存模式启动", "error", err)
		redisCache = nil
	} else {
		defer redisCache.Close()
	}

	authService := auth.NewService(store, cfg.JWTSecret, cfg.ResourceSecret, cfg.AccessTokenLifetime)
	searchCoordinator, err := indexer.NewCoordinator(cfg.SearchWorkers)
	if err != nil {
		return fmt.Errorf("初始化搜索协调器: %w", err)
	}
	defer searchCoordinator.Close()
	searchHTTP, err := indexer.NewHTTPClient(cfg.SearchWorkers, cfg.RequestTimeout, cfg.MaxResponseBody)
	if err != nil {
		return fmt.Errorf("初始化站点 HTTP 客户端: %w", err)
	}
	defer searchHTTP.CloseIdleConnections()
	// 内网自建站点需要放开私网地址限制；公网部署默认关闭。
	searchHTTP.SetAllowPrivateHosts(cfg.AllowPrivateSiteHosts)
	siteAdapters, err := loadSiteAdapters(ctx, store, searchHTTP, logger)
	if err != nil {
		return err
	}
	siteRegistry := indexer.NewRegistry(siteAdapters)
	siteReloader := newSiteReloader(siteRegistry, func(reloadCtx context.Context) ([]indexer.Adapter, error) {
		adapters, loadErr := loadSiteAdapters(reloadCtx, store, searchHTTP, logger)
		if loadErr != nil {
			return nil, loadErr
		}
		return adapters, nil
	})
	buildSiteAdapter := func(site database.Site) (indexer.Adapter, error) {
		return indexer.NewAdapterFromConfig(searchHTTP, site.ID, site.Name, site.URL, site.Adapter, site.Config)
	}
	validateSite := func(site database.Site) error {
		_, buildErr := buildSiteAdapter(site)
		return buildErr
	}
	metadataHTTP, err := metadata.NewHTTPClient(cfg.MetadataWorkers, cfg.MetadataTimeout, cfg.MaxResponseBody)
	if err != nil {
		return fmt.Errorf("初始化元数据 HTTP 客户端: %w", err)
	}
	defer metadataHTTP.CloseIdleConnections()
	// V2 的 PROXY_HOST：没配就沿用 HTTP(S)_PROXY 环境变量（与 Go 默认一致）。
	if err := metadataHTTP.SetProxy(cfg.ProxyHost); err != nil {
		return fmt.Errorf("初始化元数据代理: %w", err)
	}
	metadataCoordinator, err := newMetadataCoordinator(cfg, metadataHTTP, redisCache)
	if err != nil {
		return fmt.Errorf("初始化元数据协调器: %w", err)
	}

	// Build NFO scraper service if TMDB is configured.
	var scraperService *scraper.ScraperService
	if cfg.TMDBToken != "" {
		tmdbClient := metadata.NewTMDB(metadataHTTP, cfg.TMDBBaseURL, cfg.TMDBToken)
		tmdbAdapter := metadata.NewTMDBScraperAdapter(tmdbClient)
		nfoWriter := scraper.NewNFOWriter()
		scraperService = scraper.NewScraperService(tmdbAdapter, nfoWriter, logger)
	}
	notificationRuntime, err := loadNotificationRuntime(ctx, store, logger)
	if err != nil {
		return fmt.Errorf("初始化通知运行时: %w", err)
	}
	notificationRegistry := notificationRuntime.Registry()
	// Telegram 入站：按通知配置里的机器人 Token 起 getUpdates 轮询，
	// 命令与按钮回调由 telegrambot.Router 处理；无 Token 时什么都不做。
	telegramBots := startTelegramBots(ctx, logger, notificationRuntime, searchCoordinator, siteRegistry, store)
	if telegramBots != nil {
		defer telegramBots.Stop()
	}
	notificationSchedule, err := newNotificationScheduleRuntime(
		ctx, store, database.NotificationSendTimeSettingKey,
	)
	if err != nil {
		return fmt.Errorf("读取通知发送时段: %w", err)
	}
	webPushSender := newWebPushSender(cfg, store, logger)
	notificationRegistry.SetWebPushAvailable(webPushSender != nil)
	templateSetting := defaultNotificationTemplates()
	var configuredTemplates json.RawMessage
	templatesFound, err := store.GetSetting(ctx, database.NotificationTemplatesSettingKey, &configuredTemplates)
	if err != nil {
		return fmt.Errorf("读取通知模板: %w", err)
	}
	if templatesFound && len(configuredTemplates) != 0 {
		templateSetting = configuredTemplates
	}
	templateRuntime, err := newNotificationTemplateRuntime(templateSetting)
	if err != nil {
		logger.Warn("通知模板设置无效，使用内置模板")
		templateRuntime, err = newNotificationTemplateRuntime(defaultNotificationTemplates())
		if err != nil {
			return fmt.Errorf("初始化内置通知模板: %w", err)
		}
	}
	// V2 `MessageQueueManager` gates every notification by the
	// `NotificationSendTime` allow-window; wrap the registry so all consumers
	// below share the same in-process delay queue. EnvironmentSettings is
	// published after its job dependencies are ready and read on every check.
	var currentEnvironmentSettings atomic.Pointer[config.EnvironmentSettings]
	sendTimeProvider := func() any {
		var fallback func() any
		if settings := currentEnvironmentSettings.Load(); settings != nil {
			fallback = settings.NotificationSendTime
		}
		return notificationSchedule.Schedule(fallback)
	}
	notificationDispatcher := notificationChannelDispatcher{registry: notificationRegistry, webPush: webPushSender}
	notificationSender := notification.NewSendTimeGateWithContext(
		logger, notificationDispatcher, sendTimeProvider, 0, cfg.Location, ctx,
	)
	defer notificationSender.Stop()
	siteUserDataEffects, err := siteflow.NewService(store, notificationSender, nil, logger)
	if err != nil {
		return fmt.Errorf("初始化站点刷新副作用: %w", err)
	}

	downloadRuntime, err := loadDownloadRuntime(ctx, store, cfg.RequestTimeout, logger)
	if err != nil {
		return err
	}
	defer downloadRuntime.CloseIdleConnections()
	mediaServerRuntime, err := loadMediaServerRuntime(ctx, store, cfg.RequestTimeout, logger)
	if err != nil {
		return err
	}
	defer mediaServerRuntime.CloseIdleConnections()
	organizationBinder := &organizationHandlerBinder{}
	downloadCompletionBinder := &downloadCompletionBinder{}
	mediaIndexBinder := &mediaIndexHandlerBinder{}
	mediaIndexSweepBinder := &mediaIndexSweepHandlerBinder{}
	scrapeBinder := &scrapeHandlerBinder{}
	scrapeSweepBinder := &scrapeSweepHandlerBinder{}
	notificationBinder := &notificationHandlerBinder{}
	notificationSweepBinder := &notificationSweepHandlerBinder{}
	siteUserDataRefreshBinder := &siteUserDataRefreshBinder{}
	if err := siteUserDataRefreshBinder.Bind(&siteUserDataRefresher{
		store:         store,
		search:        searchCoordinator,
		buildAdapter:  buildSiteAdapter,
		effects:       siteUserDataEffects,
		fetchMessages: cfg.SiteMessage,
		logger:        logger,
	}); err != nil {
		return fmt.Errorf("绑定站点用户数据批量刷新处理器: %w", err)
	}
	mediaServerSyncBinder := &mediaServerSyncHandlerBinder{}
	if err := mediaServerRuntime.SetItemCache(store, store, cfg.BootstrapUsername); err != nil {
		return fmt.Errorf("装配媒体服务器条目缓存: %w", err)
	}
	if err := mediaServerSyncBinder.Bind(mediaServerRuntime); err != nil {
		return fmt.Errorf("绑定媒体服务器条目同步处理器: %w", err)
	}
	var scrapeJobHandler jobs.ScrapeHandler
	var scrapeSweepJobHandler jobs.ScrapeSweepHandler
	if scraperService != nil {
		scrapeJobHandler = scrapeBinder
		scrapeSweepJobHandler = scrapeSweepBinder
	}
	var notificationJobHandler jobs.NotificationHandler = notificationBinder
	var notificationSweepJobHandler jobs.NotificationSweepHandler = notificationSweepBinder
	var siteUserDataRefreshJobHandler jobs.SiteUserDataRefreshHandler = siteUserDataRefreshBinder
	// 域事件总线：把「下载完成 / 整理完成」这类业务事实广播给旁路消费者。
	// 目前消费者是统计落库；后续插件或外部集成按 event.Type 订阅即可，
	// 不需要在完成路径上再加依赖。
	eventBus := events.New(logger, 256)
	defer eventBus.Close()
	eventBus.Subscribe("statistics", func(ctx context.Context, event events.Event) {
		if err := store.RecordEventStatistic(ctx, event.At, event.Type, event.Username); err != nil {
			logger.Warn("事件统计写入失败", "type", event.Type, "error", err)
		}
	})
	downloadHandler, err := downloadflow.NewHandler(
		store,
		siteRegistry,
		downloadRuntime.submitters,
		downloadCompletionBinder,
	)
	if err != nil {
		return fmt.Errorf("初始化下载任务处理器: %w", err)
	}
	downloadHandler.SetEventBus(eventBus)
	downloadReconcileHandler, err := downloadflow.NewReconciliationHandler(
		store,
		downloadRuntime.submitters,
		downloadCompletionBinder,
	)
	if err != nil {
		return fmt.Errorf("初始化下载对账处理器: %w", err)
	}
	downloadReconcileHandler.SetEventBus(eventBus)
	// 对账的逐目标失败不再让整轮任务失败，因此底层原因必须写进日志与自检报告。
	downloadReconcileHandler.SetLogger(logger)
	subscriptionSearchBinder := &subscriptionSearchBinder{}
	rssBinder := &rssHandlerBinder{}
	// 刷流开关只做如实说明：开启也不会注册周期任务（见 brush_switch.go）。
	logBrushSwitch(logger)
	// 站点签到：依赖站点清单 + 登录能力；任一缺失时保持 nil，jobs 层就不会注册周期任务。
	var siteSigninHandler jobs.SiteSigninHandler
	if sweep := newSiteSigninSweep(store, siteflow.NewCookieUpdater(store, buildSiteAdapter), logger); sweep != nil {
		siteSigninHandler = sweep
	}
	jobManager, err := jobs.NewManagerWithConfig(db.Pool, logger, jobs.ManagerConfig{
		DownloadHandler:            downloadHandler,
		DownloadReconcileHandler:   downloadReconcileHandler,
		OrganizationHandler:        organizationBinder,
		OrganizationSweepHandler:   organizationBinder,
		MediaIndexHandler:          mediaIndexBinder,
		MediaIndexSweepHandler:     mediaIndexSweepBinder,
		ScrapeHandler:              scrapeJobHandler,
		ScrapeSweepHandler:         scrapeSweepJobHandler,
		NotificationHandler:        notificationJobHandler,
		NotificationSweepHandler:   notificationSweepJobHandler,
		SiteSigninHandler:          siteSigninHandler,
		SiteUserDataRefreshHandler: siteUserDataRefreshJobHandler,
		SubscriptionSearchHandler:  subscriptionSearchBinder,
		MediaServerSyncHandler:     mediaServerSyncBinder,
		RSSSweepHandler:            rssBinder,
		RSSPollHandler:             rssBinder,
		RSSMatchHandler:            rssBinder,
		RSSSweepInterval:           cfg.SubscribeRSSInterval,
		OrganizationMaxWorkers:     cfg.OrganizeWorkers,
		ScrapeMaxWorkers:           workerCountIfEnabled(scraperService != nil, cfg.ScrapeWorkers),
	})
	if err != nil {
		return err
	}
	if err := applyMediaServerSyncSchedules(ctx, mediaServerRuntime, jobManager); err != nil {
		return fmt.Errorf("注册媒体服务器同步周期任务: %w", err)
	}
	mediaIndexService, err := mediaindex.NewService(
		store,
		store,
		store,
		mediaServerRuntime,
		localMediaFingerprintVerifier{},
	)
	if err != nil {
		return fmt.Errorf("初始化媒体入库确认处理器: %w", err)
	}
	if err := mediaIndexBinder.Bind(mediaIndexService); err != nil {
		return fmt.Errorf("绑定媒体入库确认处理器: %w", err)
	}
	mediaIndexSweep, err := mediaindex.NewSweepHandler(store, mediaIndexEnqueuer{manager: jobManager})
	if err != nil {
		return fmt.Errorf("初始化媒体入库确认恢复处理器: %w", err)
	}
	if err := mediaIndexSweepBinder.Bind(mediaIndexSweep); err != nil {
		return fmt.Errorf("绑定媒体入库确认恢复处理器: %w", err)
	}
	organizationHandler, err := organizationflow.NewHandler(
		store,
		jobManager,
		mediaServerRuntime,
		jobManager,
	)
	if err != nil {
		return fmt.Errorf("初始化整理任务处理器: %w", err)
	}
	organizationHandler.SetEventBus(eventBus)
	configureOrganizationAppDomain(organizationHandler, cfg.AppDomain)
	if err := organizationBinder.Bind(organizationHandler); err != nil {
		return fmt.Errorf("绑定整理任务处理器: %w", err)
	}

	scrapePlanner, err := scrapeflow.NewPlanner(store, scraperService != nil)
	if err != nil {
		return fmt.Errorf("初始化 NFO 计划器: %w", err)
	}
	notificationPlanner, err := notificationflow.NewPlanner(notificationRegistry)
	if err != nil {
		return fmt.Errorf("初始化通知计划器: %w", err)
	}
	notificationPlanner.SetLogger(logger)
	businessNotificationPlanner, err := notificationflow.NewBusinessPlanner(
		notificationRegistry, templateRuntime, cfg.AppDomain,
	)
	if err != nil {
		return fmt.Errorf("初始化业务通知计划器: %w", err)
	}
	// 没有渠道被选中时计划器仍然产出合法计划（0 条投递），
	// 这条日志是排查「outbox 已发布但 notifications 0 行」的关键线索。
	businessNotificationPlanner.SetLogger(logger)
	organizationHandler.SetDurableSideEffects(
		store,
		scrapePlanner,
		notificationPlanner,
		jobManager,
		jobManager,
	)

	if scraperService != nil {
		scrapeService, serviceErr := scrapeflow.NewService(store, scraperService)
		if serviceErr != nil {
			return fmt.Errorf("初始化 NFO 执行器: %w", serviceErr)
		}
		if err := scrapeBinder.Bind(scrapeService); err != nil {
			return fmt.Errorf("绑定 NFO 执行器: %w", err)
		}
		scrapeSweep, sweepErr := scrapeflow.NewSweepHandler(store, jobManager)
		if sweepErr != nil {
			return fmt.Errorf("初始化 NFO 恢复器: %w", sweepErr)
		}
		if err := scrapeSweepBinder.Bind(scrapeSweep); err != nil {
			return fmt.Errorf("绑定 NFO 恢复器: %w", err)
		}
	}
	notificationService, serviceErr := notificationflow.NewService(store, notificationSender)
	if serviceErr != nil {
		return fmt.Errorf("初始化通知执行器: %w", serviceErr)
	}
	if err := notificationBinder.Bind(notificationService); err != nil {
		return fmt.Errorf("绑定通知执行器: %w", err)
	}
	notificationSweep, sweepErr := notificationflow.NewSweepHandler(store, jobManager)
	if sweepErr != nil {
		return fmt.Errorf("初始化通知恢复器: %w", sweepErr)
	}
	notificationSweep.SetBusinessOutbox(store, businessNotificationPlanner)
	if err := notificationSweepBinder.Bind(notificationSweep); err != nil {
		return fmt.Errorf("绑定通知恢复器: %w", err)
	}

	organizationPlanner, err := organizationflow.NewPlanningService(
		store,
		downloadRuntime.submitters,
		jobManager,
	)
	if err != nil {
		return fmt.Errorf("初始化整理规划服务: %w", err)
	}
	namingTemplatesFn := func() organization.NamingTemplates {
		templates := organization.NamingTemplates{
			Movie: cfg.MovieRenameFormat,
			TV:    cfg.TVRenameFormat,
		}
		if settings := currentEnvironmentSettings.Load(); settings != nil {
			movieFmt, tvFmt := settings.RenameFormats()
			if movieFmt != "" {
				templates.Movie = movieFmt
			}
			if tvFmt != "" {
				templates.TV = tvFmt
			}
		}
		return templates
	}
	organizationPlanner.SetNamingTemplatesProvider(namingTemplatesFn)
	if err := downloadCompletionBinder.Bind(organizationPlanner); err != nil {
		return fmt.Errorf("绑定下载完成处理器: %w", err)
	}
	submissionService, err := downloadflow.NewSubmissionService(db.Pool, store, jobManager)
	if err != nil {
		return fmt.Errorf("初始化下载提交服务: %w", err)
	}
	subscriptionSearchHandler, err := subscriptionflow.NewHandler(
		store,
		searchCoordinator,
		siteRegistry,
		submissionService,
		downloadHandler,
	)
	if err != nil {
		return fmt.Errorf("初始化订阅精准搜索处理器: %w", err)
	}
	categoryService := metadata.NewCategoryService(cfg.ConfigDir)
	subscriptionSearchHandler.SetMediaDetailResolver(metadataCoordinator)
	subscriptionSearchHandler.SetLogger(logger)
	if cfg.TMDBToken != "" {
		supplementer, supplementErr := metadata.NewTMDBSupplementer(metadataCoordinator)
		if supplementErr != nil {
			return fmt.Errorf("初始化订阅 TMDB 辅助事实: %w", supplementErr)
		}
		subscriptionSearchHandler.SetMediaSupplementer(&loggingMediaSupplementer{
			delegate: supplementer,
			logger:   logger,
		})
	}
	subscriptionSearchHandler.SetCategoryResolver(categoryService)
	subscriptionSearchHandler.SetExistingEpisodesSource(mediaServerRuntime)
	subscriptionSearchHandler.SetExistingMovieSource(mediaServerRuntime)
	if err := subscriptionSearchBinder.Bind(subscriptionSearchHandler); err != nil {
		return fmt.Errorf("绑定订阅精准搜索处理器: %w", err)
	}
	rssHandler, err := subscriptionflow.NewRSSHandler(
		store,
		subscriptionSearchHandler,
		siteRegistry,
		jobManager,
		cfg.SubscribeRSSInterval,
	)
	if err != nil {
		return fmt.Errorf("RSS handler init: %w", err)
	}
	rssHandler.SetMediaRecognizer(metadataCoordinator)
	if err := rssBinder.Bind(rssHandler); err != nil {
		return fmt.Errorf("RSS handler bind: %w", err)
	}
	// River 周期任务在进程重启后可能丢失；启动期显式补注册一次，
	// 保证 RSS 增量扫描有稳定节拍（否则轮询游标会长期不动）。
	if err := jobManager.EnsureRSSSweepSchedule(cfg.SubscribeRSSInterval); err != nil {
		logger.Warn("注册 RSS 增量扫描周期任务失败", "error", err)
	}
	environmentSettings := config.NewEnvironmentSettings(
		cfg,
		func(ctx context.Context, interval time.Duration) error {
			if err := jobManager.SetRSSSweepInterval(interval); err != nil {
				return err
			}
			return rssHandler.SetPollInterval(interval)
		},
	)
	currentEnvironmentSettings.Store(environmentSettings)
	if err := jobManager.Start(ctx); err != nil {
		return fmt.Errorf("启动 River: %w", err)
	}
	defer func() {
		stopCtx, cancel := context.WithTimeout(context.Background(), cfg.ShutdownTimeout)
		defer cancel()
		if stopErr := jobManager.Stop(stopCtx); stopErr != nil {
			logger.Error("停止 River 失败", "error", stopErr)
		}
	}()
	if err := jobManager.EnqueueProbe(ctx); err != nil {
		return err
	}
	if err := jobManager.EnqueueMediaIndexSweep(ctx); err != nil {
		return err
	}
	var cookieCloud httpapi.CookieCloudSyncer
	if syncer := newCookieCloudSyncer(
		cfg, store, nil, searchCoordinator, searchHTTP, buildSiteAdapter, siteReloader.Reload,
	); syncer != nil {
		cookieCloud = syncer
	}
	var storageProvider storage.Provider
	if storageRoot := strings.TrimSpace(cfg.StorageLocalPath); storageRoot != "" {
		storageProvider = storage.NewLocalProvider(map[string]string{"local": storageRoot})
	}
	if rcloneURL := strings.TrimSpace(cfg.StorageRcloneURL); rcloneURL != "" {
		if rclone, rcloneErr := storage.NewRcloneProvider(storage.RcloneConfig{
			URL: rcloneURL, FileSystem: cfg.StorageRcloneFS,
			Username: cfg.StorageRcloneUsername, Password: cfg.StorageRclonePassword,
		}); rcloneErr == nil {
			registry, _ := storageProvider.(*storage.ProviderRegistry)
			if registry == nil {
				registry = storage.NewProviderRegistry()
				if storageProvider != nil {
					registry.Register("local", storageProvider)
				}
			}
			registry.Register(rclone.Name(), rclone)
			storageProvider = registry
		}
	}
	if alistURL := strings.TrimSpace(cfg.StorageAlistURL); alistURL != "" {
		if alist, alistErr := storage.NewAlistProvider(storage.AlistConfig{
			URL: alistURL, Username: cfg.StorageAlistUsername,
			Password: cfg.StorageAlistPassword, Root: cfg.StorageAlistRoot,
		}); alistErr == nil {
			registry, _ := storageProvider.(*storage.ProviderRegistry)
			if registry == nil {
				registry = storage.NewProviderRegistry()
				if storageProvider != nil {
					registry.Register("local", storageProvider)
				}
			}
			registry.Register(alist.Name(), alist)
			storageProvider = registry
		}
	}
	mediaDiscover := newMediaDiscover(cfg, metadataHTTP)
	doctorRunner := newDoctorRunner(cfg, db, redisCache, notificationRegistry, downloadReconcileHandler, mediaDiscover)
	pluginState, err := pluginStateStore(store)
	if err != nil {
		return err
	}
	pluginRegistry, err := newPluginRegistry(ctx, pluginState, jobManager, logger)
	if err != nil {
		return err
	}
	dirMonitor := newDirectoryMonitor(store, namingTemplatesFn, logger)
	if dirMonitor != nil {
		dirMonitor.Start(ctx)
	}
	var logReader httpapi.LogReader
	if buf := defaultLogBuffer.Load(); buf != nil {
		logReader = buf
	}
	var manualScraper httpapi.ManualScraper
	if scraperService != nil {
		manualScraper = scraperService
	}
	handler := httpapi.NewRouter(httpapi.Dependencies{
		Config: cfg, DB: db, Cache: redisCache, Store: store, Auth: authService, Logger: logger,
		LogBuffer:            logReader,
		WebhookNotifier:      webhookNotificationBridge{sender: notificationSender},
		Scraper:              manualScraper,
		MediaServerRefresher: mediaServerRuntime,
		// 登录防爆破：账号与 IP 双维度锁定，阈值来自 MOVIEPILOT_LOGIN_* 配置。
		LoginThrottle: httpapi.NewLoginThrottle(httpapi.LoginThrottleConfig{
			MaxUserFailures: cfg.LoginMaxFailures,
			MaxIPFailures:   cfg.LoginIPMaxFailures,
			IPWindow:        cfg.LoginIPWindow,
			LockBase:        cfg.LoginLockBase,
			LockMax:         cfg.LoginLockMax,
		}, time.Now),
		Doctor: doctorRunner,
		Search: searchCoordinator, SearchRegistry: siteRegistry, Metadata: metadataCoordinator,
		Discover: mediaDiscover, Categories: categoryService,
		ReloadMetadata: func(reloadCtx context.Context) error {
			// V2 的 CONFIG_WATCH 在 TMDB_API_KEY / TMDB_API_DOMAIN 变化时重建 TMDB 客户端；
			// 这里同样重建并热替换，保存设置后不重启就能刷出海报、演职员和推荐列表。
			refreshed := applyMetadataSettings(reloadCtx, store, cfg, logger)
			// PROXY_HOST 变了也要换连接池，否则改了代理仍旧走老通道。
			if err := metadataHTTP.SetProxy(refreshed.ProxyHost); err != nil {
				return err
			}
			if err := metadataCoordinator.SetProviders(metadataProviders(refreshed, metadataHTTP)); err != nil {
				return err
			}
			mediaDiscover.SetTMDB(newTMDBClient(refreshed, metadataHTTP))
			return nil
		},
		SubscriptionHistory: store,
		DownloaderSettings:  store, DownloadRequests: store, DownloadSubmissions: submissionService,
		DownloadTasks: downloadRuntime, DownloadOwners: store,
		Dashboard: newDashboardRuntime(store, downloadRuntime, time.Now()),
		Scheduler: newSchedulerRuntime(jobManager, cookieCloud, mediaServerRuntime, dirMonitor, cfg.Location),
		Plugins:   pluginRegistry,
		Sites:     store, SiteStatistics: store, SiteValidator: validateSite, BuildSiteAdapter: buildSiteAdapter, ReloadSites: siteReloader.Reload,
		SiteCookieUpdater: siteflow.NewCookieUpdater(store, buildSiteAdapter),
		SiteCatalog:       newCookieCloudSiteDirectory(store), SiteIcons: store,
		ReloadDownloaders: func(ctx context.Context) error {
			return downloadRuntime.Reload(ctx, store, cfg.RequestTimeout, logger)
		},
		MediaServerSettings:  store,
		MediaServerClients:   mediaServerRuntime,
		MediaServerPlayback:  mediaServerRuntime,
		MediaServerLibraries: mediaServerRuntime,
		LocalMediaIndex:      mediaServerRuntime,
		MediaServerRemote:    mediaServerRuntime,
		ReloadMediaServers: func(ctx context.Context) error {
			if err := mediaServerRuntime.Reload(ctx, store, cfg.RequestTimeout, logger); err != nil {
				return err
			}
			// V2 rebuilds the per-server schedules when the media server
			// configuration changes, so a `sync_interval` edit applies without
			// a restart.
			return applyMediaServerSyncSchedules(ctx, mediaServerRuntime, jobManager)
		},
		NotificationSettings: store,
		NotificationHistory:  store,
		ReloadNotificationTemplates: func(reloadCtx context.Context) error {
			var raw json.RawMessage
			found, loadErr := store.GetSetting(reloadCtx, database.NotificationTemplatesSettingKey, &raw)
			if loadErr != nil {
				return loadErr
			}
			if !found || len(raw) == 0 {
				raw = defaultNotificationTemplates()
			}
			return templateRuntime.Reload(raw)
		},
		ReloadNotificationSendTime: notificationSchedule.Reload,
		SiteUserDataEffects:        siteUserDataEffects,
		ReloadNotifications:        notificationRuntime.Reload,
		EnvironmentSettings:        environmentSettings,
		CookieCloud:                cookieCloud,
		WebPush:                    webPushDispatcherFrom(webPushSender),
		Storage:                    storageProvider,
	})
	server := &http.Server{
		Addr:              cfg.HTTPAddr,
		Handler:           handler,
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       30 * time.Second,
		IdleTimeout:       60 * time.Second,
		MaxHeaderBytes:    1 << 20,
	}

	serveErrors := make(chan error, 1)
	go func() {
		logger.Info("MoviePilot Go 已启动", "address", cfg.HTTPAddr)
		serveErrors <- server.ListenAndServe()
	}()

	select {
	case <-ctx.Done():
		shutdownCtx, cancel := context.WithTimeout(context.Background(), cfg.ShutdownTimeout)
		defer cancel()
		if err := server.Shutdown(shutdownCtx); err != nil {
			return fmt.Errorf("关闭 HTTP 服务: %w", err)
		}
		return nil
	case err := <-serveErrors:
		if errors.Is(err, http.ErrServerClosed) {
			return nil
		}
		return fmt.Errorf("运行 HTTP 服务: %w", err)
	}
}

func loadSiteAdapters(
	ctx context.Context,
	store *database.Store,
	client *indexer.HTTPClient,
	logger *slog.Logger,
) ([]indexer.Adapter, error) {
	sites, err := store.ListEnabledSites(ctx)
	if err != nil {
		return nil, err
	}
	adapters := make([]indexer.Adapter, 0, len(sites))
	for _, site := range sites {
		// Cookie/passkey 等敏感字段在库中单独加密存放，运行时必须取解密后的配置，
		// 否则适配器拿不到 Cookie，搜索会静默变成「未搜索到任何资源」。
		runtimeSite, runtimeErr := store.RuntimeSiteByID(ctx, site.ID)
		if runtimeErr != nil {
			logger.Warn("读取站点解密配置失败，改用公开配置", "site_id", site.ID, "error", runtimeErr)
			runtimeSite = site
		}
		adapter, buildErr := indexer.NewAdapterFromConfig(
			client, runtimeSite.ID, runtimeSite.Name, runtimeSite.URL, runtimeSite.Adapter, runtimeSite.Config,
		)
		if buildErr != nil {
			logger.Warn("跳过不支持的站点适配器", "site_id", runtimeSite.ID, "site", runtimeSite.Name, "adapter", runtimeSite.Adapter, "error", buildErr)
			continue
		}
		adapters = append(adapters, adapter)
	}
	return adapters, nil
}

// newTMDBClient 按当前配置构造 TMDB 客户端；没配置密钥时返回 nil。
// V2 未配置 TMDB_API_KEY 时同样拿不到任何数据，我们也不假装能取到。
func newTMDBClient(cfg config.Config, client *metadata.HTTPClient) *metadata.TMDB {
	if cfg.TMDBToken == "" {
		return nil
	}
	return metadata.NewTMDB(client, cfg.TMDBBaseURL, cfg.TMDBToken)
}

// newDoubanClient 按当前配置构造豆瓣客户端；API Key/Secret 缺一不可。
func newDoubanClient(cfg config.Config, client *metadata.HTTPClient) *metadata.Douban {
	if cfg.DoubanAPIKey == "" || cfg.DoubanSecretKey == "" {
		return nil
	}
	return metadata.NewDouban(client, cfg.DoubanBaseURL, cfg.DoubanAPIKey, cfg.DoubanSecretKey)
}

// metadataProviders 组装识别层的数据源顺序：TMDB、豆瓣在前，Bangumi / AniList 兜底。
// 建协调器与运行期热替换（ReloadMetadata）共用，避免两条路径的排序/取舍漂移。
func metadataProviders(cfg config.Config, client *metadata.HTTPClient) []metadata.Provider {
	providers := make([]metadata.Provider, 0, 4)
	if tmdb := newTMDBClient(cfg, client); tmdb != nil {
		providers = append(providers, tmdb)
	}
	if douban := newDoubanClient(cfg, client); douban != nil {
		providers = append(providers, douban)
	}
	return append(providers,
		metadata.NewBangumi(client, cfg.BangumiBaseURL, cfg.BangumiToken),
		metadata.NewAniList(client, cfg.AniListEndpoint),
	)
}

// newMediaDiscover 构造「发现 / 推荐」列表门面，复用与识别层相同的元数据客户端。
// 未配置的来源保持为空客户端：对应接口返回空数组，不影响页面其余部分。
func newMediaDiscover(cfg config.Config, client *metadata.HTTPClient) *metadata.Discover {
	return metadata.NewDiscover(
		newTMDBClient(cfg, client),
		newDoubanClient(cfg, client),
		metadata.NewBangumi(client, cfg.BangumiBaseURL, cfg.BangumiToken),
		metadata.NewAniList(client, cfg.AniListEndpoint),
	)
}

// applyMetadataSettings 用系统设置里的媒体源配置补齐环境变量没给的部分。
// 返回补齐后的配置；设置库读不到时原样返回（环境变量仍然是权威来源）。
func applyMetadataSettings(ctx context.Context, store *database.Store, cfg config.Config, logger *slog.Logger) config.Config {
	// PROXY_HOST 先加载：它和 TMDB 密钥没有依赖关系，且 bangumi 这类源
	// 直连不通时（实测 api.bgm.tv 直连超时、经代理 1.4s 返回）必须有它才出数据。
	if proxy, err := loadSettingString(ctx, store, database.ProxyHostSettingKey); err == nil {
		if proxy != "" {
			cfg.ProxyHost = proxy
			logger.Info("已从系统设置加载出口代理", "proxy", proxy)
		}
	} else {
		logger.Warn("读取出口代理设置失败，沿用环境变量配置", "error", err)
	}
	if cfg.TMDBToken != "" {
		return cfg
	}
	token, err := loadSettingString(ctx, store, database.TMDBAPIKeySettingKey)
	if err != nil {
		logger.Warn("读取 TMDB 密钥设置失败，TMDB 数据源保持未启用", "error", err)
		return cfg
	}
	if token == "" {
		return cfg
	}
	cfg.TMDBToken = token
	if domain, domainErr := loadSettingString(ctx, store, database.TMDBAPIDomainSettingKey); domainErr == nil && domain != "" {
		if base := tmdbBaseURLFromDomain(domain); base != "" {
			cfg.TMDBBaseURL = base
		}
	}
	logger.Info("已从系统设置加载 TMDB 密钥", "base_url", cfg.TMDBBaseURL)
	return cfg
}

// loadSettingString 读一个字符串型系统设置；不存在或类型不对都按空串处理。
func loadSettingString(ctx context.Context, store *database.Store, key string) (string, error) {
	var value any
	found, err := store.GetSetting(ctx, key, &value)
	if err != nil {
		return "", err
	}
	if !found {
		return "", nil
	}
	text, _ := value.(string)
	return strings.TrimSpace(text), nil
}

// tmdbBaseURLFromDomain 把 V2 的 TMDB_API_DOMAIN（默认 api.themoviedb.org）
// 规范成我们客户端要的完整 base URL。
func tmdbBaseURLFromDomain(domain string) string {
	domain = strings.TrimSpace(domain)
	if domain == "" {
		return ""
	}
	if strings.HasPrefix(domain, "http://") || strings.HasPrefix(domain, "https://") {
		return strings.TrimRight(domain, "/")
	}
	return "https://" + strings.TrimRight(domain, "/")
}

func newMetadataCoordinator(
	cfg config.Config,
	client *metadata.HTTPClient,
	jsonCache metadata.JSONCache,
) (*metadata.Coordinator, error) {
	providers := metadataProviders(cfg, client)
	return metadata.NewCoordinator(providers, metadata.Options{
		Cache: jsonCache, CacheTTL: cfg.MetadataCacheTTL,
		RequestTimeout: cfg.MetadataTimeout, MaxResults: cfg.MetadataMaxResults,
		ProviderMinInterval: cfg.MetadataMinInterval,
		ProviderMaxBackoff:  cfg.MetadataMaxBackoff,
	})
}

func workerCountIfEnabled(enabled bool, count int) int {
	if !enabled {
		return 0
	}
	return count
}
