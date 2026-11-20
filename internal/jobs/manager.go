package jobs

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/riverqueue/river"
	"github.com/riverqueue/river/riverdriver/riverpgxv5"
	"github.com/riverqueue/river/rivermigrate"
	"github.com/riverqueue/river/rivertype"
)

const queueSystem = "system"

// ProbeArgs 是用于验证持久队列和数据库租约的轻量任务。
type ProbeArgs struct {
	Key string `json:"key" river:"unique"`
}

// Kind 返回不会随 Go 类型重命名变化的持久任务名称。
func (ProbeArgs) Kind() string { return "system_probe" }

type probeWorker struct {
	river.WorkerDefaults[ProbeArgs]
	pool *pgxpool.Pool
}

func (w *probeWorker) Work(ctx context.Context, _ *river.Job[ProbeArgs]) error {
	probeCtx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	if err := w.pool.Ping(probeCtx); err != nil {
		return fmt.Errorf("持久任务数据库探针失败: %w", err)
	}
	return nil
}

func (w *probeWorker) Timeout(*river.Job[ProbeArgs]) time.Duration { return 10 * time.Second }

type periodicJobBundle interface {
	AddSafely(*river.PeriodicJob) (rivertype.PeriodicJobHandle, error)
	Remove(rivertype.PeriodicJobHandle)
}

// Manager 管理 River 生命周期和唯一任务入队。
type Manager struct {
	client                     *river.Client[pgx.Tx]
	rssPeriodicJobs            periodicJobBundle
	rssPeriodicMu              sync.Mutex
	rssPeriodicHandle          rivertype.PeriodicJobHandle
	rssPeriodicRegistered      bool
	downloadEnabled            bool
	organizationEnabled        bool
	organizationSweepEnabled   bool
	mediaIndexEnabled          bool
	mediaIndexSweepEnabled     bool
	scrapeEnabled              bool
	scrapeSweepEnabled         bool
	notificationEnabled        bool
	notificationSweepEnabled   bool
	siteUserDataRefreshEnabled bool
	rssSweepEnabled            bool
	rssPollEnabled             bool
	rssMatchEnabled            bool
	mediaServerSyncEnabled     bool
	// siteSigninSweepEnabled / backupSweepEnabled / subscriptionSearchEnabled 记录
	// 对应 worker 是否注册成功：手工触发要先确认「有 worker 接单」，否则插入的
	// 任务会在执行期才失败。
	siteSigninSweepEnabled    bool
	backupSweepEnabled        bool
	subscriptionSearchEnabled bool

	// 签到与备份的周期任务走 bundle 注册（而非 river.Config.PeriodicJobs），
	// 这样插件配置里的间隔可以热更新：AddSafely + Remove 换一个新周期，
	// 不必重启 River 客户端。
	siteSigninPeriodicJobs   periodicJobBundle
	siteSigninPeriodicMu     sync.Mutex
	siteSigninPeriodicHandle rivertype.PeriodicJobHandle
	backupPeriodicJobs       periodicJobBundle
	backupPeriodicMu         sync.Mutex
	backupPeriodicHandle     rivertype.PeriodicJobHandle

	mediaServerPeriodicJobs    periodicJobBundle
	mediaServerPeriodicMu      sync.Mutex
	mediaServerPeriodicHandles []rivertype.PeriodicJobHandle
	// mediaServerPeriodicSchedules 记录上一次成功注册的计划，注册失败时据此回滚。
	mediaServerPeriodicSchedules []MediaServerSyncSchedule
	// mediaServerPeriodicGeneration 每次重建计划时递增，用于给周期任务换新 ID：
	// River v0.24 的 Remove 不释放 ID 唯一性，同名服务器必须换 ID 才能重建。
	mediaServerPeriodicGeneration uint64
}

// Migrate 使用运行期 pgxpool 创建或升级 River 表。
func Migrate(ctx context.Context, pool *pgxpool.Pool) error {
	migrator, err := rivermigrate.New(riverpgxv5.New(pool), nil)
	if err != nil {
		return fmt.Errorf("创建 River 迁移器: %w", err)
	}
	if _, err := migrator.Migrate(ctx, rivermigrate.DirectionUp, nil); err != nil {
		return fmt.Errorf("执行 River 迁移: %w", err)
	}
	return nil
}

// NewManager 创建使用受控队列并发的 River 客户端。
func NewManager(pool *pgxpool.Pool, logger *slog.Logger) (*Manager, error) {
	return NewManagerWithConfig(pool, logger, ManagerConfig{})
}

// NewManagerWithConfig 创建按配置启用各类 worker 的 River 客户端。
func NewManagerWithConfig(pool *pgxpool.Pool, logger *slog.Logger, config ManagerConfig) (*Manager, error) {
	if pool == nil {
		return nil, errors.New("River 需要已初始化的 pgxpool")
	}
	workers := river.NewWorkers()
	river.AddWorker(workers, &probeWorker{pool: pool})
	downloadMaxWorkers, err := registerDownloadWorker(workers, config)
	if err != nil {
		return nil, err
	}
	downloadReconcileEnabled, err := registerDownloadReconcileWorker(workers, config.DownloadReconcileHandler)
	if err != nil {
		return nil, err
	}
	subscriptionSearchEnabled, err := registerSubscriptionSearchWorker(workers, config.SubscriptionSearchHandler)
	if err != nil {
		return nil, err
	}
	rssSweepEnabled, err := registerRSSSweepWorker(workers, config.RSSSweepHandler)
	if err != nil {
		return nil, err
	}
	rssPollEnabled, err := registerRSSPollWorker(workers, config.RSSPollHandler)
	if err != nil {
		return nil, err
	}
	rssMatchEnabled, err := registerRSSMatchWorker(workers, config.RSSMatchHandler)
	if err != nil {
		return nil, err
	}
	mediaServerSyncEnabled, err := registerMediaServerSyncWorker(workers, config.MediaServerSyncHandler)
	if err != nil {
		return nil, err
	}
	organizationMaxWorkers, err := registerOrganizationWorker(workers, config)
	if err != nil {
		return nil, err
	}
	organizationSweepEnabled, err := registerOrganizationSweepWorker(workers, config.OrganizationSweepHandler)
	if err != nil {
		return nil, err
	}
	mediaIndexMaxWorkers, err := registerMediaIndexWorker(workers, config)
	if err != nil {
		return nil, err
	}
	mediaIndexSweepEnabled, err := registerMediaIndexSweepWorker(workers, config.MediaIndexSweepHandler)
	if err != nil {
		return nil, err
	}
	scrapeMaxWorkers, err := registerScrapeWorker(workers, config)
	if err != nil {
		return nil, err
	}
	scrapeSweepEnabled, err := registerScrapeSweepWorker(workers, config.ScrapeSweepHandler)
	if err != nil {
		return nil, err
	}
	notificationMaxWorkers, err := registerNotificationWorker(workers, config)
	if err != nil {
		return nil, err
	}
	notificationSweepEnabled, err := registerNotificationSweepWorker(workers, config.NotificationSweepHandler)
	if err != nil {
		return nil, err
	}
	siteUserDataRefreshEnabled, err := registerSiteUserDataRefreshWorker(workers, config.SiteUserDataRefreshHandler)
	if err != nil {
		return nil, err
	}
	cleanupSweepEnabled, err := registerCleanupSweepWorker(workers, pool, logger)
	if err != nil {
		return nil, err
	}
	backupSweepEnabled, err := registerBackupSweepWorker(workers, pool, logger)
	if err != nil {
		return nil, err
	}
	siteSigninSweepEnabled, err := registerSiteSigninSweepWorker(workers, config.SiteSigninHandler, logger)
	if err != nil {
		return nil, err
	}
	rssPollMaxWorkers := 0
	if rssPollEnabled {
		rssPollMaxWorkers, err = normalizeRSSPollMaxWorkers(config.RSSPollMaxWorkers)
		if err != nil {
			return nil, err
		}
	}
	queues := map[string]river.QueueConfig{
		queueSystem: {MaxWorkers: 1},
	}
	if downloadMaxWorkers > 0 {
		queues[queueDownload] = river.QueueConfig{MaxWorkers: downloadMaxWorkers}
	}
	if organizationMaxWorkers > 0 {
		queues[queueOrganization] = river.QueueConfig{MaxWorkers: organizationMaxWorkers}
	}
	if mediaIndexMaxWorkers > 0 {
		queues[queueMediaIndex] = river.QueueConfig{MaxWorkers: mediaIndexMaxWorkers}
	}
	if scrapeMaxWorkers > 0 {
		queues[queueScrape] = river.QueueConfig{MaxWorkers: scrapeMaxWorkers}
	}
	if notificationMaxWorkers > 0 {
		queues[queueNotification] = river.QueueConfig{MaxWorkers: notificationMaxWorkers}
	}
	periodicJobs := make([]*river.PeriodicJob, 0, 8)
	if organizationSweepEnabled {
		queues[queueOrganizationSweep] = river.QueueConfig{MaxWorkers: 1}
		periodicJobs = append(periodicJobs, organizationSweepPeriodicJob())
	}
	if mediaIndexSweepEnabled {
		queues[queueMediaIndexSweep] = river.QueueConfig{MaxWorkers: 1}
		periodicJobs = append(periodicJobs, mediaIndexSweepPeriodicJob())
	}
	if scrapeSweepEnabled {
		queues[queueScrapeSweep] = river.QueueConfig{MaxWorkers: 1}
		periodicJobs = append(periodicJobs, scrapeSweepPeriodicJob())
	}
	if notificationSweepEnabled {
		queues[queueNotificationSweep] = river.QueueConfig{MaxWorkers: 1}
		periodicJobs = append(periodicJobs, notificationSweepPeriodicJob())
	}
	if siteUserDataRefreshEnabled {
		queues[queueSiteUserDataRefresh] = river.QueueConfig{MaxWorkers: 1}
		periodicJobs = append(periodicJobs, siteUserDataRefreshPeriodicJob())
	}
	if downloadReconcileEnabled {
		queues[queueDownloadReconcile] = river.QueueConfig{MaxWorkers: 1}
		periodicJobs = append(periodicJobs, downloadReconcilePeriodicJob())
	}
	if subscriptionSearchEnabled {
		queues[queueSubscriptionSearch] = river.QueueConfig{MaxWorkers: 1}
		periodicJobs = append(periodicJobs, subscriptionSearchPeriodicJob())
	}
	if cleanupSweepEnabled {
		queues[queueCleanupSweep] = river.QueueConfig{MaxWorkers: 1}
		periodicJobs = append(periodicJobs, cleanupSweepPeriodicJob())
	}
	if backupSweepEnabled {
		queues[queueBackupSweep] = river.QueueConfig{MaxWorkers: 1}
	}
	if siteSigninSweepEnabled {
		queues[queueSiteSigninSweep] = river.QueueConfig{MaxWorkers: 1}
	}
	if mediaServerSyncEnabled {
		// Per-server schedules are registered later from the media server
		// settings so `sync_interval` changes take effect without a restart.
		queues[queueMediaServerSync] = river.QueueConfig{MaxWorkers: 1}
	}
	if rssSweepEnabled {
		queues[queueRSSSweep] = river.QueueConfig{MaxWorkers: 1}
	}
	if rssPollMaxWorkers > 0 {
		queues[queueRSSPoll] = river.QueueConfig{MaxWorkers: rssPollMaxWorkers}
	}
	if rssMatchEnabled {
		queues[queueRSSMatch] = river.QueueConfig{MaxWorkers: 1}
	}
	client, err := river.NewClient(riverpgxv5.New(pool), &river.Config{
		Logger:                      logger,
		Queues:                      queues,
		Workers:                     workers,
		PeriodicJobs:                periodicJobs,
		MaxAttempts:                 5,
		JobTimeout:                  30 * time.Second,
		RescueStuckJobsAfter:        downloadRescueAfter,
		FetchCooldown:               100 * time.Millisecond,
		FetchPollInterval:           time.Second,
		CompletedJobRetentionPeriod: 6 * time.Hour,
	})
	if err != nil {
		return nil, fmt.Errorf("创建 River 客户端: %w", err)
	}
	manager := &Manager{
		client:                     client,
		downloadEnabled:            downloadMaxWorkers > 0,
		organizationEnabled:        organizationMaxWorkers > 0,
		organizationSweepEnabled:   organizationSweepEnabled,
		mediaIndexEnabled:          mediaIndexMaxWorkers > 0,
		mediaIndexSweepEnabled:     mediaIndexSweepEnabled,
		scrapeEnabled:              scrapeMaxWorkers > 0,
		scrapeSweepEnabled:         scrapeSweepEnabled,
		notificationEnabled:        notificationMaxWorkers > 0,
		notificationSweepEnabled:   notificationSweepEnabled,
		siteUserDataRefreshEnabled: siteUserDataRefreshEnabled,
		rssSweepEnabled:            rssSweepEnabled,
		rssPollEnabled:             rssPollEnabled,
		rssMatchEnabled:            rssMatchEnabled,
		mediaServerSyncEnabled:     mediaServerSyncEnabled,
		siteSigninSweepEnabled:     siteSigninSweepEnabled,
		backupSweepEnabled:         backupSweepEnabled,
		subscriptionSearchEnabled:  subscriptionSearchEnabled,
	}
	if rssSweepEnabled {
		manager.rssPeriodicJobs = client.PeriodicJobs()
		handle, err := manager.rssPeriodicJobs.AddSafely(rssSweepPeriodicJob(config.RSSSweepInterval))
		if err != nil {
			return nil, fmt.Errorf("注册 RSS 增量扫描周期任务: %w", err)
		}
		manager.rssPeriodicHandle = handle
		manager.rssPeriodicRegistered = true
	}
	if mediaServerSyncEnabled {
		manager.mediaServerPeriodicJobs = client.PeriodicJobs()
	}
	if siteSigninSweepEnabled {
		manager.siteSigninPeriodicJobs = client.PeriodicJobs()
		handle, err := manager.siteSigninPeriodicJobs.AddSafely(siteSigninSweepPeriodicJob())
		if err != nil {
			return nil, fmt.Errorf("注册站点签到周期任务: %w", err)
		}
		manager.siteSigninPeriodicHandle = handle
	}
	if backupSweepEnabled {
		manager.backupPeriodicJobs = client.PeriodicJobs()
		handle, err := manager.backupPeriodicJobs.AddSafely(backupSweepPeriodicJob())
		if err != nil {
			return nil, fmt.Errorf("注册备份周期任务: %w", err)
		}
		manager.backupPeriodicHandle = handle
	}
	return manager, nil
}

// Start 启动任务租约、调度和 worker 循环。
func (m *Manager) Start(ctx context.Context) error {
	return m.client.Start(ctx)
}

// Stop 停止获取新任务，并等待已运行任务到达安全点。
func (m *Manager) Stop(ctx context.Context) error {
	return m.client.Stop(ctx)
}

// SetRSSSweepInterval replaces the in-memory RSS sweep schedule without
// stopping the River client. It does not enqueue an immediate sweep.
func (m *Manager) SetRSSSweepInterval(interval time.Duration) error {
	if m == nil {
		return ErrRSSSweepHandlerUnavailable
	}

	m.rssPeriodicMu.Lock()
	defer m.rssPeriodicMu.Unlock()

	if !m.rssSweepEnabled || m.rssPeriodicJobs == nil || !m.rssPeriodicRegistered {
		return ErrRSSSweepHandlerUnavailable
	}

	newHandle, err := m.rssPeriodicJobs.AddSafely(rssSweepRuntimePeriodicJob(interval))
	if err != nil {
		return fmt.Errorf("更新 RSS 增量扫描周期任务: %w", err)
	}

	m.rssPeriodicJobs.Remove(m.rssPeriodicHandle)
	m.rssPeriodicHandle = newHandle
	return nil
}

// EnsureRSSSweepSchedule 在启动期保证 RSS 增量扫描周期任务存在（幂等）：
// River 的周期任务只存在于进程内存，重启后若注册路径没走到，轮询游标会长期不动。
func (m *Manager) EnsureRSSSweepSchedule(interval time.Duration) error {
	if m == nil || !m.rssSweepEnabled {
		return ErrRSSSweepHandlerUnavailable
	}

	m.rssPeriodicMu.Lock()
	defer m.rssPeriodicMu.Unlock()

	if m.rssPeriodicJobs == nil {
		if m.client == nil {
			return ErrRSSSweepHandlerUnavailable
		}
		m.rssPeriodicJobs = m.client.PeriodicJobs()
	}

	handle, err := m.rssPeriodicJobs.AddSafely(rssSweepRuntimePeriodicJob(interval))
	if err != nil {
		return fmt.Errorf("注册 RSS 增量扫描周期任务: %w", err)
	}
	if m.rssPeriodicRegistered {
		m.rssPeriodicJobs.Remove(m.rssPeriodicHandle)
	}
	m.rssPeriodicHandle = handle
	m.rssPeriodicRegistered = true
	return nil
}

// EnqueueProbe 插入一分钟窗口内唯一的持久探针任务。
func (m *Manager) EnqueueProbe(ctx context.Context) error {
	_, err := m.client.Insert(ctx, ProbeArgs{Key: "primary"}, &river.InsertOpts{
		Queue: queueSystem, MaxAttempts: 5,
		UniqueOpts: river.UniqueOpts{ByArgs: true, ByPeriod: time.Minute},
	})
	if err != nil {
		return fmt.Errorf("插入 River 探针任务: %w", err)
	}
	return nil
}

// EnqueueOrganizationSweep requests an immediate bounded lease-recovery scan.
func (m *Manager) EnqueueOrganizationSweep(ctx context.Context) error {
	if ctx == nil {
		return ErrOrganizationSweepExecution
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if m == nil || m.client == nil || !m.organizationSweepEnabled {
		return ErrOrganizationSweepHandlerUnavailable
	}
	args := OrganizationSweepArgs{Key: organizationSweepKey}
	opts := args.InsertOpts()
	if _, err := m.client.Insert(ctx, args, &opts); err != nil {
		if contextErr := sanitizedOrganizationContextError(ctx, err); contextErr != nil {
			return contextErr
		}
		return ErrOrganizationSweepExecution
	}
	return nil
}

// EnqueueOrganization persists one organization batch job in the dedicated queue.
func (m *Manager) EnqueueOrganization(ctx context.Context, args OrganizationArgs) error {
	if err := args.Validate(); err != nil {
		return err
	}
	if ctx == nil {
		return ErrOrganizationEnqueue
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if m == nil || m.client == nil || !m.organizationEnabled {
		return ErrOrganizationHandlerUnavailable
	}
	opts := args.InsertOpts()
	if _, err := m.client.Insert(ctx, args, &opts); err != nil {
		if contextErr := sanitizedOrganizationContextError(ctx, err); contextErr != nil {
			return contextErr
		}
		return ErrOrganizationEnqueue
	}
	return nil
}

// EnqueueRSSPoll inserts one site poll. RSSPollArgs marks SiteID as the
// unique field, therefore repeated sweeps cannot enqueue concurrent polls for
// the same site even when each sweep has a different cutoff.
func (m *Manager) EnqueueRSSPoll(ctx context.Context, args RSSPollArgs) error {
	now := time.Now().UTC()
	if err := args.Validate(now); err != nil {
		return err
	}
	if ctx == nil {
		return ErrRSSPollExecution
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if m == nil || m.client == nil || !m.rssPollEnabled {
		return ErrRSSPollHandlerUnavailable
	}
	opts := args.InsertOpts()
	if _, err := m.client.Insert(ctx, args, &opts); err != nil {
		if contextErr := sanitizedRSSContextError(ctx, err); contextErr != nil {
			return contextErr
		}
		return ErrRSSPollExecution
	}
	return nil
}

// EnqueueRSSSweep requests a bounded due-cursor snapshot. The periodic job
// normally calls the same constructor automatically; this method is useful
// for startup recovery and operator-triggered sweeps.
func (m *Manager) EnqueueRSSSweep(ctx context.Context, cutoff time.Time) error {
	args := RSSSweepArgs{Key: rssSweepKey, Cutoff: cutoff.UTC()}
	if err := args.Validate(time.Now().UTC()); err != nil {
		return err
	}
	if ctx == nil {
		return ErrRSSSweepExecution
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if m == nil || m.client == nil || !m.rssSweepEnabled {
		return ErrRSSSweepHandlerUnavailable
	}
	opts := args.InsertOpts()
	if _, err := m.client.Insert(ctx, args, &opts); err != nil {
		if contextErr := sanitizedRSSContextError(ctx, err); contextErr != nil {
			return contextErr
		}
		return ErrRSSSweepExecution
	}
	return nil
}

// EnqueueRSSMatch inserts one bounded global cache-matching snapshot. Key and
// cutoff make repeated insertion of one round idempotent without suppressing a
// later round; the single-worker queue serializes execution. Expected site IDs
// form the poll-completion barrier for this cutoff.
func (m *Manager) EnqueueRSSMatch(ctx context.Context, cutoff time.Time, expectedSiteIDs []int) error {
	args := RSSMatchArgs{
		Key:             rssMatchKey,
		Cutoff:          cutoff.UTC(),
		ExpectedSiteIDs: append([]int(nil), expectedSiteIDs...),
	}
	if err := args.Validate(time.Now().UTC()); err != nil {
		return err
	}
	if ctx == nil {
		return ErrRSSMatchExecution
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if m == nil || m.client == nil || !m.rssMatchEnabled {
		return ErrRSSMatchHandlerUnavailable
	}
	opts := args.InsertOpts()
	if _, err := m.client.Insert(ctx, args, &opts); err != nil {
		if contextErr := sanitizedRSSContextError(ctx, err); contextErr != nil {
			return contextErr
		}
		return ErrRSSMatchExecution
	}
	return nil
}
