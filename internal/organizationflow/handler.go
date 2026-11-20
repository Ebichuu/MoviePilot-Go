package organizationflow

import (
	"context"
	"errors"
	"time"

	"github.com/google/uuid"

	"github.com/moviepilot-go/moviepilot-go/internal/database"
	"github.com/moviepilot-go/moviepilot-go/internal/events"
	"github.com/moviepilot-go/moviepilot-go/internal/jobs"
	"github.com/moviepilot-go/moviepilot-go/internal/mediaindex"
	"github.com/moviepilot-go/moviepilot-go/internal/notificationflow"
	"github.com/moviepilot-go/moviepilot-go/internal/organization"
	"github.com/moviepilot-go/moviepilot-go/internal/organizer"
	"github.com/moviepilot-go/moviepilot-go/internal/scrapeflow"
)

// The bounded worker files that implement batch execution, lease handling,
// completion planning and durable side effects must not import the persistence
// or task adapters themselves (internal/architecture/dependencies_test.go
// rejects new flow-to-database and flow-to-jobs edges). This legacy file is
// where that migration debt is recorded, so the shared domain and River
// argument types are re-exported here as aliases. Aliases keep the runtime
// types identical, so existing adapters, tests and interface assertions keep
// working without runtime behaviour changes.
type (
	// OrganizationArgs is the River argument payload for one organization batch.
	OrganizationArgs = jobs.OrganizationArgs
	// MediaIndexArgs is the River argument payload for one media index request.
	MediaIndexArgs = jobs.MediaIndexArgs
	// OrganizationBatch is the persisted immutable plan and its lease state.
	OrganizationBatch = database.OrganizationBatch
	// OrganizationFile is one file of the persisted immutable plan.
	OrganizationFile = database.OrganizationFile
)

const (
	// OrganizationBatchOrganizing marks a batch currently owned by a worker.
	OrganizationBatchOrganizing = database.OrganizationBatchOrganizing
	// OrganizationFileOrganizing marks a file currently claimed by a worker.
	OrganizationFileOrganizing = database.OrganizationFileOrganizing
)

var (
	// ErrOrganizationBatchNotFound hides missing, unready and foreign batches.
	ErrOrganizationBatchNotFound = database.ErrOrganizationBatchNotFound
	// ErrOrganizationLeaseConflict reports an expired or stolen lease.
	ErrOrganizationLeaseConflict = database.ErrOrganizationLeaseConflict
	// ErrOrganizationSuccessSnapshotNotFound reports a missing notification snapshot.
	ErrOrganizationSuccessSnapshotNotFound = database.ErrOrganizationSuccessSnapshotNotFound
)

const (
	organizationLeaseDuration    = 5 * time.Minute
	organizationHeartbeat        = time.Minute
	organizationFileClaimSize    = 16
	organizationSweepPageSize    = 100
	organizationReleaseTimeout   = 5 * time.Second
	mediaIndexEnqueueTimeout     = 5 * time.Second
	organizationStableFileError  = "organization operation failed"
	organizationStableIndexError = "media index completion plan failed"
	// organizationStableServerUnavailableError 单独区分「媒体服务器暂时不可达」，
	// 恢复扫描据此不消耗批次重试预算，等服务器恢复后继续确认。
	organizationStableServerUnavailableError = "media server unavailable, index confirmation deferred"
	// organizationStablePathMappingError 表示媒体服务器缺少目标路径映射：
	// 补上映射后同一批次应能继续完成确认，因此同样不受重试预算限制。
	organizationStablePathMappingError = "media server path mapping missing"
)

var (
	// ErrInvalidOrganizationHandler indicates missing dependencies or invalid
	// bounded execution configuration.
	ErrInvalidOrganizationHandler = errors.New("整理执行处理器依赖无效")
	// ErrOrganizationExecutionStorage is the stable persistence failure exposed
	// to River. Database and path details are deliberately not included.
	ErrOrganizationExecutionStorage = errors.New("整理执行状态当前不可用")
	// ErrOrganizationFileOperation is the stable retryable filesystem failure.
	ErrOrganizationFileOperation = errors.New("整理文件操作失败")
	// ErrOrganizationIndexPlanning is the stable retryable completion-planning failure.
	ErrOrganizationIndexPlanning = errors.New("媒体入库确认规划失败")
	// ErrOrganizationIndexServerUnavailable 表示媒体服务器暂时不可达：
	// 整理本身已完成，只是入库确认要等服务器恢复，因此不消耗批次重试预算。
	ErrOrganizationIndexServerUnavailable = errors.New("媒体服务器暂不可用，整理确认待重试")
	// ErrOrganizationIndexPathMapping 表示媒体服务器缺少目标路径映射。
	ErrOrganizationIndexPathMapping = errors.New("媒体服务器缺少目标路径映射，配置后自动重试")
)

// ExecutionStore is the minimal lease and file-CAS contract used by one River
// organization job.
type ExecutionStore interface {
	ClaimOrganizationBatch(
		ctx context.Context,
		batchID uuid.UUID,
		now time.Time,
		leaseDuration time.Duration,
	) (database.OrganizationBatch, bool, error)
	RenewOrganizationBatchLease(
		ctx context.Context,
		batchID uuid.UUID,
		token uuid.UUID,
		now time.Time,
		leaseDuration time.Duration,
	) error
	ClaimOrganizationFiles(
		ctx context.Context,
		batchID uuid.UUID,
		token uuid.UUID,
		limit int,
	) ([]database.OrganizationFile, error)
	CompleteOrganizationFile(ctx context.Context, batchID, token uuid.UUID, fileID int64) error
	FailOrganizationFile(ctx context.Context, batchID, token uuid.UUID, fileID int64, stableErr string) error
	FailOrganizationBatch(
		ctx context.Context,
		batchID uuid.UUID,
		token uuid.UUID,
		retryAt time.Time,
		stableErr string,
	) error
	CompleteOrganizationBatchForIndexing(
		ctx context.Context,
		batchID uuid.UUID,
		token uuid.UUID,
		plan mediaindex.CompletionPlan,
	) (uuid.UUID, bool, error)
	RecoverExpiredOrganizationLeases(ctx context.Context, now time.Time, limit int) (int64, error)
	ListDueOrganizationBatchIDs(ctx context.Context, now time.Time, limit int) ([]uuid.UUID, error)
}

// TerminalOrganizationFileStore is implemented by the PostgreSQL adapter for
// V2's non-retryable per-file organization outcome. It is optional so focused
// legacy test doubles can continue to exercise the retry path.
type TerminalOrganizationFileStore interface {
	FinalizeOrganizationFileFailure(context.Context, uuid.UUID, uuid.UUID, int64, string) error
}

// OrganizationOutcomeStore projects persistence states into organization
// domain outcomes without leaking database enums into this package.
type OrganizationOutcomeStore interface {
	ListOrganizationFileOutcomes(context.Context, string, uuid.UUID) ([]organization.FileOutcome, error)
}

// TerminalNotificationPlanBuilder builds the complete V2 event timeline for
// terminal file results. It is kept separate from the legacy planner contract
// so existing focused test doubles remain source-compatible.
type TerminalNotificationPlanBuilder interface {
	PlanResults(uuid.UUID, string, []organization.ResultSnapshot, []organization.FileFailure, string) (notificationflow.Plan, error)
}

// OrganizationEnqueuer persists one batch job. River's active-state unique key
// makes repeated recovery sweeps idempotent.
type OrganizationEnqueuer interface {
	EnqueueOrganization(context.Context, jobs.OrganizationArgs) error
}

// MediaIndexCompletionPlanner derives the immutable confirmation snapshot from
// the already-persisted organization plan without exposing Emby to this package.
type MediaIndexCompletionPlanner interface {
	MediaIndexCompletionPlan(context.Context, organizer.Plan) (mediaindex.CompletionPlan, error)
}

// MediaIndexEnqueuer performs only the best-effort River notification after
// the durable request has committed. PostgreSQL recovery remains authoritative.
type MediaIndexEnqueuer interface {
	EnqueueMediaIndex(context.Context, jobs.MediaIndexArgs) error
}

// SideEffectCompletionStore atomically records NFO and notification intents
// together with organization completion.
type SideEffectCompletionStore interface {
	CompleteOrganizationBatchWithSideEffects(context.Context, uuid.UUID, uuid.UUID, mediaindex.CompletionPlan, scrapeflow.Plan, notificationflow.Plan) (uuid.UUID, bool, []uuid.UUID, []uuid.UUID, error)
	ListOrganizationSuccessSnapshots(context.Context, string, uuid.UUID) ([]organization.SuccessSnapshot, error)
	GetDownloadIntentSuccessInput(context.Context, string, uuid.UUID) (organization.SuccessInput, error)
}

// ScrapePlanBuilder derives a durable NFO snapshot from persisted identity.
type ScrapePlanBuilder interface {
	Plan(context.Context, uuid.UUID, uuid.UUID, organizer.Plan) (scrapeflow.Plan, error)
}

// NotificationPlanBuilder derives a durable per-channel completion snapshot.
type NotificationPlanBuilder interface {
	Plan(uuid.UUID, string, organizer.Plan) (notificationflow.Plan, error)
	PlanSuccess(uuid.UUID, string, []organization.SuccessSnapshot, string) (notificationflow.Plan, error)
}

// ScrapeEnqueuer enqueues persisted NFO requests after commit.
type ScrapeEnqueuer interface {
	EnqueueScrape(context.Context, uuid.UUID) error
}

// NotificationEnqueuer enqueues persisted notification requests after commit.
type NotificationEnqueuer interface {
	EnqueueNotification(context.Context, uuid.UUID) error
}

// NotificationSender delivers notifications after organization completion.
type NotificationSender interface {
	Send(ctx context.Context, msg NotificationMessage) error
}

// NotificationMessage contains fields needed for organization notifications.
type NotificationMessage struct {
	Type        string
	ContentType string
	Title       string
	Text        string
	Image       string
	Link        string
}

// NFOScraper generates NFO files for organized media.
type NFOScraper interface {
	ScrapeMovieNFO(ctx context.Context, tmdbID int, mediaPath string) error
	ScrapeTVShowNFO(ctx context.Context, tmdbID int, mediaPath string) error
	ScrapeEpisodeNFO(ctx context.Context, tmdbID int, season, episode int, mediaPath string) error
}

// Handler executes one persisted immutable plan under a renewable database
// lease. It never derives target paths inside the worker.
type Handler struct {
	store                ExecutionStore
	enqueuer             OrganizationEnqueuer
	completionPlanner    MediaIndexCompletionPlanner
	mediaIndexEnqueuer   MediaIndexEnqueuer
	sideEffectStore      SideEffectCompletionStore
	scrapePlanner        ScrapePlanBuilder
	notificationPlanner  NotificationPlanBuilder
	scrapeEnqueuer       ScrapeEnqueuer
	notificationEnqueuer NotificationEnqueuer
	notificationSender   NotificationSender
	appDomain            string
	nfoScraper           NFOScraper
	now                  func() time.Time
	leaseDuration        time.Duration
	heartbeatInterval    time.Duration
	fileClaimSize        int
	execute              func(context.Context, string, organizer.Plan) error
	// eventBus 是旁路事件总线；为空表示不广播整理完成事件。
	eventBus *events.Bus
}

// SetEventBus 注入域事件总线，用于广播 organization.completed。
func (h *Handler) SetEventBus(bus *events.Bus) {
	if h == nil {
		return
	}
	h.eventBus = bus
}

// publishOrganizationCompleted 广播整理完成事实。事件是旁路，不影响批次结果。
func (h *Handler) publishOrganizationCompleted(batch database.OrganizationBatch, files int) {
	if h == nil || h.eventBus == nil {
		return
	}
	payload := map[string]any{
		"batch_id": batch.ID.String(),
		"files":    files,
	}
	if root := batch.SourceRoot; root != "" {
		payload["source_root"] = root
	}
	h.eventBus.Publish(events.Event{
		Type:    events.TypeOrganizationCompleted,
		Payload: payload,
	})
}

// SetDurableSideEffects enables the production completion path. Dependencies
// are kept optional for compatibility with existing focused unit tests.
func (h *Handler) SetDurableSideEffects(
	store SideEffectCompletionStore,
	scrapePlanner ScrapePlanBuilder,
	notificationPlanner NotificationPlanBuilder,
	scrapeEnqueuer ScrapeEnqueuer,
	notificationEnqueuer NotificationEnqueuer,
) {
	if h == nil {
		return
	}
	h.sideEffectStore = store
	h.scrapePlanner = scrapePlanner
	h.notificationPlanner = notificationPlanner
	h.scrapeEnqueuer = scrapeEnqueuer
	h.notificationEnqueuer = notificationEnqueuer
}

// NewHandler creates the bounded organization worker used by River.
func NewHandler(
	store ExecutionStore,
	enqueuer OrganizationEnqueuer,
	completionPlanner MediaIndexCompletionPlanner,
	mediaIndexEnqueuer MediaIndexEnqueuer,
) (*Handler, error) {
	if store == nil || enqueuer == nil || completionPlanner == nil || mediaIndexEnqueuer == nil {
		return nil, ErrInvalidOrganizationHandler
	}
	return &Handler{
		store:              store,
		enqueuer:           enqueuer,
		completionPlanner:  completionPlanner,
		mediaIndexEnqueuer: mediaIndexEnqueuer,
		notificationSender: nil, // optional, can be set via SetNotificationSender
		nfoScraper:         nil, // optional, can be set via SetNFOScraper
		now:                time.Now,
		leaseDuration:      organizationLeaseDuration,
		heartbeatInterval:  organizationHeartbeat,
		fileClaimSize:      organizationFileClaimSize,
		execute:            organizer.ExecuteWithinSourceRoot,
	}, nil
}

// SetNotificationSender attaches an optional notification sender.
// This method is safe to call after construction.
func (h *Handler) SetNotificationSender(sender NotificationSender) {
	if h != nil {
		h.notificationSender = sender
	}
}

// SetAppDomain sets the V2 base URL used for organization history links.
func (h *Handler) SetAppDomain(appDomain string) {
	if h != nil {
		h.appDomain = appDomain
	}
}

// SetNFOScraper attaches an optional NFO scraper.
// This method is safe to call after construction.
func (h *Handler) SetNFOScraper(scraper NFOScraper) {
	if h != nil {
		h.nfoScraper = scraper
	}
}

var _ jobs.OrganizationHandler = (*Handler)(nil)
var _ jobs.OrganizationSweepHandler = (*Handler)(nil)
var _ OrganizationEnqueuer = (*jobs.Manager)(nil)
var _ MediaIndexEnqueuer = (*jobs.Manager)(nil)
