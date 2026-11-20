package database

import (
	"errors"
	"time"

	"github.com/google/uuid"
	"github.com/moviepilot-go/moviepilot-go/internal/organizer"
)

// OrganizationBatchState 是一个不可变整理计划的持久执行状态。
type OrganizationBatchState string

const (
	OrganizationBatchPlanned    OrganizationBatchState = "planned"
	OrganizationBatchOrganizing OrganizationBatchState = "organizing"
	OrganizationBatchFailed     OrganizationBatchState = "failed"
	OrganizationBatchCompleted  OrganizationBatchState = "completed"
)

// OrganizationFileState 是单个整理文件的持久执行状态。
type OrganizationFileState string

const (
	OrganizationFilePlanned        OrganizationFileState = "planned"
	OrganizationFileOrganizing     OrganizationFileState = "organizing"
	OrganizationFileFailed         OrganizationFileState = "failed"
	OrganizationFileTerminalFailed OrganizationFileState = "terminal_failed"
	OrganizationFileCompleted      OrganizationFileState = "completed"
)

var (
	// ErrInvalidOrganizationInput 表示计划、分页、租期或失败信息越过受控边界。
	ErrInvalidOrganizationInput = errors.New("整理持久化输入无效")
	// ErrOrganizationBatchNotFound 隐藏不存在、未就绪和不属于当前 owner 的批次。
	ErrOrganizationBatchNotFound = errors.New("整理批次不存在")
	// ErrOrganizationPlanConflict 表示下载意图已绑定不同的不可变整理计划。
	ErrOrganizationPlanConflict = errors.New("整理计划冲突")
	// ErrOrganizationLeaseConflict 表示租约已过期或已被其他 worker 取得。
	ErrOrganizationLeaseConflict = errors.New("整理批次租约冲突")
	// ErrOrganizationFileStateConflict 表示文件已离开本次 CAS 的预期状态。
	ErrOrganizationFileStateConflict = errors.New("整理文件状态冲突")
	// ErrOrganizationFilesIncomplete 表示批次仍有未进入终态的文件，不能完成。
	ErrOrganizationFilesIncomplete = errors.New("整理批次文件尚未全部完成")
	// ErrOrganizationStateConflict 表示关联订阅或逐集状态不允许完成闭环。
	ErrOrganizationStateConflict = errors.New("整理关联状态冲突")
	// ErrOrganizationPersistence 表示数据库暂时无法安全完成整理持久化。
	ErrOrganizationPersistence = errors.New("整理持久化失败")
)

const (
	maxOrganizationOwnerLength = 255
	maxOrganizationPathLength  = 4096
	maxOrganizationPlanFiles   = 10_000
	maxOrganizationPlanBytes   = 16 << 20
	maxOrganizationPageSize    = 500
	maxOrganizationClaimSize   = 100
	maxOrganizationErrorLength = 2048
	maxOrganizationAttempts    = 8
	maxOrganizationLease       = time.Hour
	minOrganizationLease       = time.Second
)

// OrganizationBatch 是 worker 可领取的不可变整理计划及其租约状态。
// LeaseToken 仅供内部 worker 执行 CAS，不应写入日志或 API 响应。
type OrganizationBatch struct {
	ID             uuid.UUID
	IntentID       uuid.UUID
	State          OrganizationBatchState
	SourceRoot     string
	Plan           organizer.Plan
	AttemptCount   int
	NextAttemptAt  time.Time
	LastError      string
	LeaseToken     uuid.UUID
	LeaseExpiresAt *time.Time
	CompletedAt    *time.Time
	CreatedAt      time.Time
	UpdatedAt      time.Time
}

// OrganizationFile 是不可变计划中的单个文件及其幂等执行状态。
type OrganizationFile struct {
	ID             int64
	BatchID        uuid.UUID
	Ordinal        int
	SourcePath     string
	TargetPath     string
	Mode           organizer.Mode
	ExpectedSize   int64
	ExpectedSHA256 string
	State          OrganizationFileState
	AttemptCount   int
	LastError      string
	CompletedAt    *time.Time
	UpdatedAt      time.Time
}
