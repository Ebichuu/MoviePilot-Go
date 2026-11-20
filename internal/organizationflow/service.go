package organizationflow

import (
	"context"
	"errors"
	"strings"

	"github.com/google/uuid"

	"github.com/moviepilot-go/moviepilot-go/internal/database"
	"github.com/moviepilot-go/moviepilot-go/internal/downloadconfig"
	"github.com/moviepilot-go/moviepilot-go/internal/downloader"
	"github.com/moviepilot-go/moviepilot-go/internal/downloadflow"
	"github.com/moviepilot-go/moviepilot-go/internal/jobs"
	"github.com/moviepilot-go/moviepilot-go/internal/organization"
	"github.com/moviepilot-go/moviepilot-go/internal/organizer"
)

const (
	organizationDirectoriesSetting = "Directories"
	maxOrganizationDirectories     = 1_000
)

var (
	// ErrInvalidOrganizationPlanningService indicates missing dependencies or
	// a malformed completed-download identity.
	ErrInvalidOrganizationPlanningService = errors.New("整理规划服务依赖无效")
	// ErrOrganizationPlanningUnavailable is the stable retryable boundary for
	// downloader, settings, persistence, and queue failures.
	ErrOrganizationPlanningUnavailable = errors.New("整理规划当前不可用")
	// ErrOrganizationTaskContentUnavailable indicates that the exact completed
	// downloader task snapshot could not be confirmed.
	ErrOrganizationTaskContentUnavailable = errors.New("下载任务内容当前不可用")
)

// PlanningStore is the minimal persistence contract required to turn one
// completed download into an immutable organization batch.
type PlanningStore interface {
	GetDownloadIntent(context.Context, string, uuid.UUID) (downloader.Intent, error)
	GetDownloadIntentSuccessInput(context.Context, string, uuid.UUID) (organization.SuccessInput, error)
	GetSetting(context.Context, string, any) (bool, error)
	CreateOrganizationBatch(
		context.Context,
		string,
		uuid.UUID,
		string,
		organizer.Plan,
		[]organization.SuccessSnapshot,
	) (database.OrganizationBatch, error)
	GetOrganizationBatchByIntent(context.Context, string, uuid.UUID) (database.OrganizationBatch, error)
}

// PlanningService confirms exact qBittorrent content, builds one immutable
// local plan, persists it idempotently, and requests durable execution.
type PlanningService struct {
	store             PlanningStore
	submitters        downloadflow.SubmitterProvider
	enqueuer          OrganizationEnqueuer
	namingTemplatesFn func() organization.NamingTemplates
}

// SetNamingTemplatesProvider 注入自定义命名模板读取函数（热读取系统配置）。
func (s *PlanningService) SetNamingTemplatesProvider(fn func() organization.NamingTemplates) {
	if s != nil {
		s.namingTemplatesFn = fn
	}
}

// NewPlanningService creates the completed-download organization planner.
func NewPlanningService(
	store PlanningStore,
	submitters downloadflow.SubmitterProvider,
	enqueuer OrganizationEnqueuer,
) (*PlanningService, error) {
	if store == nil || submitters == nil || enqueuer == nil {
		return nil, ErrInvalidOrganizationPlanningService
	}
	return &PlanningService{store: store, submitters: submitters, enqueuer: enqueuer}, nil
}

// HandleDownloadCompleted is idempotent across reconciliation replays. A plan
// conflict means an immutable batch already exists for the intent, so that
// batch is loaded and safely re-enqueued instead of being replaced.
func (s *PlanningService) HandleDownloadCompleted(
	ctx context.Context,
	target downloader.ReconciliationTarget,
) error {
	if err := s.validate(ctx, target); err != nil {
		return err
	}

	intent, err := s.store.GetDownloadIntent(ctx, target.Username, target.ID)
	if err != nil {
		return organizationPlanningError(ctx, err)
	}
	if err := validateCompletedIntent(target, intent); err != nil {
		return err
	}
	if existing, found, err := s.existingBatch(ctx, target); err != nil {
		return err
	} else if found {
		return s.enqueueBatch(ctx, target.ID, existing)
	}
	successInput, err := s.store.GetDownloadIntentSuccessInput(ctx, target.Username, target.ID)
	if err != nil {
		return organizationPlanningError(ctx, err)
	}
	if successInput.Legacy && successInput.Username == "" {
		successInput.Username = target.Username
	}

	submitter, err := s.submitters.ResolveSubmitter(target.DownloaderID)
	if err != nil {
		return organizationPlanningError(ctx, err)
	}
	inspector, ok := submitter.(downloader.TaskContentInspector)
	if !ok {
		return ErrOrganizationTaskContentUnavailable
	}
	content, found, err := inspector.InspectTaskContent(ctx, intent.Label, intent.TorrentHash)
	if err != nil {
		return organizationPlanningError(ctx, err)
	}
	if !found || !sameCompletedTask(content.Task, intent) {
		return ErrOrganizationTaskContentUnavailable
	}

	directories, err := s.loadDirectories(ctx)
	if err != nil {
		return err
	}
	var templates organization.NamingTemplates
	if s.namingTemplatesFn != nil {
		templates = s.namingTemplatesFn()
	}
	prepared, err := BuildPlanWithTemplates(ctx, content, directories, successInput, templates)
	if err != nil {
		return err
	}
	batch, err := s.store.CreateOrganizationBatch(
		ctx,
		target.Username,
		target.ID,
		prepared.SourceRoot,
		prepared.Plan,
		prepared.Snapshots,
	)
	if errors.Is(err, database.ErrOrganizationPlanConflict) {
		batch, err = s.store.GetOrganizationBatchByIntent(ctx, target.Username, target.ID)
	}
	if err != nil {
		return organizationPlanningError(ctx, err)
	}
	return s.enqueueBatch(ctx, target.ID, batch)
}

func (s *PlanningService) existingBatch(
	ctx context.Context,
	target downloader.ReconciliationTarget,
) (database.OrganizationBatch, bool, error) {
	batch, err := s.store.GetOrganizationBatchByIntent(ctx, target.Username, target.ID)
	if err == nil {
		return batch, true, nil
	}
	if errors.Is(err, database.ErrOrganizationBatchNotFound) {
		return database.OrganizationBatch{}, false, nil
	}
	return database.OrganizationBatch{}, false, organizationPlanningError(ctx, err)
}

func (s *PlanningService) enqueueBatch(
	ctx context.Context,
	intentID uuid.UUID,
	batch database.OrganizationBatch,
) error {
	if batch.ID == uuid.Nil || batch.IntentID != intentID {
		return ErrOrganizationPlanningUnavailable
	}
	if err := s.enqueuer.EnqueueOrganization(ctx, jobs.OrganizationArgs{BatchID: batch.ID}); err != nil {
		return organizationPlanningError(ctx, err)
	}
	return nil
}

func (s *PlanningService) validate(ctx context.Context, target downloader.ReconciliationTarget) error {
	if ctx == nil || s == nil || s.store == nil || s.submitters == nil || s.enqueuer == nil ||
		target.ID == uuid.Nil || strings.TrimSpace(target.Username) == "" ||
		strings.TrimSpace(target.DownloaderID) == "" || target.State != downloader.IntentCompleted {
		return ErrInvalidOrganizationPlanningService
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if _, err := downloader.NormalizeTorrentHash(target.TorrentHash); err != nil {
		return ErrInvalidOrganizationPlanningService
	}
	return nil
}

func (s *PlanningService) loadDirectories(ctx context.Context) ([]downloadconfig.Directory, error) {
	var directories []downloadconfig.Directory
	found, err := s.store.GetSetting(ctx, organizationDirectoriesSetting, &directories)
	if err != nil {
		return nil, organizationPlanningError(ctx, err)
	}
	if !found || directories == nil {
		directories = []downloadconfig.Directory{}
	}
	if len(directories) > maxOrganizationDirectories {
		return nil, ErrOrganizationPlanningUnavailable
	}
	return directories, nil
}

func validateCompletedIntent(target downloader.ReconciliationTarget, intent downloader.Intent) error {
	targetHash, targetErr := downloader.NormalizeTorrentHash(target.TorrentHash)
	intentHash, intentErr := downloader.NormalizeTorrentHash(intent.TorrentHash)
	if targetErr != nil || intentErr != nil || intent.ID != target.ID ||
		intent.State != downloader.IntentCompleted || strings.TrimSpace(intent.Label) == "" ||
		targetHash != intentHash {
		return ErrOrganizationTaskContentUnavailable
	}
	return nil
}

func sameCompletedTask(task downloader.Task, intent downloader.Intent) bool {
	taskHash, err := downloader.NormalizeTorrentHash(task.Hash)
	if err != nil {
		return false
	}
	intentHash, err := downloader.NormalizeTorrentHash(intent.TorrentHash)
	return err == nil && taskHash == intentHash && task.State == downloader.TaskStateCompleted &&
		task.Label == intent.Label
}

func organizationPlanningError(ctx context.Context, err error) error {
	if ctx != nil {
		if contextErr := ctx.Err(); contextErr != nil {
			return contextErr
		}
	}
	if errors.Is(err, context.Canceled) {
		return context.Canceled
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return context.DeadlineExceeded
	}
	return ErrOrganizationPlanningUnavailable
}

var _ downloadflow.DownloadCompletionHook = (*PlanningService)(nil)
var _ PlanningStore = (*database.Store)(nil)
