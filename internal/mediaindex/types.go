// Package mediaindex contains the durable, media-server-independent part of
// the local-file-to-media-library confirmation workflow.
package mediaindex

import (
	"context"
	"errors"
	"strings"
	"time"

	"github.com/google/uuid"
)

const (
	RequestPending   RequestState = "pending"
	RequestWorking   RequestState = "working"
	RequestFailed    RequestState = "failed"
	RequestConfirmed RequestState = "confirmed"
	maxFilePage                   = 16
)

// ErrRequiredServerUnavailable 表示必需媒体服务器当前不可达。
// 这是可重试的临时状态：整理本身没有失败，也不应消耗整理批次的重试预算。
var ErrRequiredServerUnavailable = errors.New("必需媒体服务器当前不可用")

// ErrRequiredServerPathMapping 表示必需媒体服务器没有配置目标路径映射。
// 这是配置缺口：补上映射后同一批次可以继续完成确认。
var ErrRequiredServerPathMapping = errors.New("必需媒体服务器缺少目标路径映射")

type RequestState string

// ServerRef is the immutable server identity captured when a request is made.
type ServerRef struct {
	ID          string
	Name        string
	Type        string
	Fingerprint string
}

func (s ServerRef) Valid() bool {
	return strings.TrimSpace(s.ID) != "" && strings.TrimSpace(s.Name) != "" && strings.TrimSpace(s.Type) != "" &&
		validServerFingerprint(s.Fingerprint)
}

func validServerFingerprint(value string) bool {
	if len(value) != 64 || value != strings.TrimSpace(value) {
		return false
	}
	for index := range value {
		if (value[index] < '0' || value[index] > '9') && (value[index] < 'a' || value[index] > 'f') {
			return false
		}
	}
	return true
}

// Request is the minimal durable request projection needed by the worker.
type Request struct {
	ID                 uuid.UUID
	Server             ServerRef
	State              RequestState
	AttemptCount       int
	RefreshRequestedAt *time.Time
	NextAttemptAt      time.Time
	LastError          string
}

// Lease is a token-CAS lease returned by a store claim.
type Lease struct {
	Request    Request
	LeaseToken uuid.UUID
}

func (l Lease) Valid(requestID uuid.UUID) bool {
	return l.Request.ID == requestID && l.LeaseToken != uuid.Nil && l.Request.Server.Valid()
}

// File is an immutable target snapshot and its confirmation projection.
type File struct {
	ID              int64
	RequestID       uuid.UUID
	LocalPath       string
	RemotePath      string
	PathStyle       string
	CaseSensitive   bool
	ExpectedSize    int64
	ExpectedSHA256  string
	MediaType       string
	Season          int
	Episode         int
	ProviderID      string
	ConfirmedItemID string
}

func (f File) Valid(requestID uuid.UUID) bool {
	return f.ID > 0 && f.RequestID == requestID && strings.TrimSpace(f.LocalPath) != "" &&
		strings.TrimSpace(f.RemotePath) != "" &&
		f.ExpectedSize >= 0 && len(strings.TrimSpace(f.ExpectedSHA256)) == 64
}

// CompletionPlan is supplied by the organization flow in the same logical
// operation that completes the immutable local organization batch.
type CompletionPlan struct {
	RequiredServer *ServerRef
	Files          []TargetFile
}

type TargetFile struct {
	LocalPath      string
	RemotePath     string
	PathStyle      string
	CaseSensitive  bool
	ExpectedSize   int64
	ExpectedSHA256 string
	MediaType      string
	Season         int
	Episode        int
	ProviderID     string
}

func (p CompletionPlan) HasRequiredServer() bool {
	return p.RequiredServer != nil && p.RequiredServer.Valid()
}

func (p CompletionPlan) Clone() CompletionPlan {
	copyPlan := CompletionPlan{Files: append([]TargetFile(nil), p.Files...)}
	if p.RequiredServer != nil {
		server := *p.RequiredServer
		copyPlan.RequiredServer = &server
	}
	return copyPlan
}

// Item is the media-server-independent subset used for exact confirmation.
type Item struct {
	ID          string
	Type        string
	Path        string
	ProviderIDs map[string]string
}

type Query struct {
	Path       string
	ProviderID string
}

// Server is deliberately narrow: the mediaindex package does not import a
// concrete HTTP client or the River/database adapters.
type Server interface {
	RefreshAll(context.Context) error
	FindItems(context.Context, Query) ([]Item, error)
}

type Registry interface {
	Resolve(ServerRef) (Server, bool)
}

// LocalVerifier checks the immutable local file snapshot before a remote
// lookup can confirm it in a media server.
type LocalVerifier interface {
	Verify(context.Context, string, int64, string) (bool, error)
}

// LeaseStore is the request state/lease seam owned by the consumer.
type LeaseStore interface {
	Claim(context.Context, uuid.UUID, time.Time, time.Duration) (Lease, bool, error)
	Renew(context.Context, uuid.UUID, uuid.UUID, time.Time, time.Time) error
	Reschedule(context.Context, uuid.UUID, uuid.UUID, time.Time, string) error
	RecordRefresh(context.Context, uuid.UUID, uuid.UUID, time.Time, time.Time) error
}

// FileStore is the bounded page and per-file CAS seam.
type FileStore interface {
	ListPendingFiles(context.Context, uuid.UUID, uuid.UUID, int) ([]File, error)
	ConfirmFile(context.Context, uuid.UUID, uuid.UUID, int64, string) error
}

// CompletionStore atomically completes the request after every target file has
// been confirmed.
type CompletionStore interface {
	ConfirmRequest(context.Context, uuid.UUID, uuid.UUID, time.Time) (bool, error)
}

// SweepStore is kept separate so the normal worker does not need a large
// persistence interface.
type SweepStore interface {
	RecoverExpired(context.Context, time.Time, int) (int64, error)
	ListDue(context.Context, time.Time, int) ([]uuid.UUID, error)
}

type Enqueuer interface {
	Enqueue(context.Context, uuid.UUID) error
}

var (
	ErrInvalidRequest    = errors.New("媒体入库确认请求无效")
	ErrServerUnavailable = errors.New("媒体服务器当前不可用")
	ErrOutcomeUnknown    = errors.New("媒体服务器刷新结果未知")
	ErrAmbiguousMatch    = errors.New("媒体服务器匹配不唯一")
	ErrPathMismatch      = errors.New("媒体服务器返回路径不匹配")
	ErrProviderMismatch  = errors.New("媒体服务器返回 Provider ID 不匹配")
	ErrLeaseLost         = errors.New("媒体入库确认租约已失效")
	ErrPersistence       = errors.New("媒体入库确认状态持久化失败")
)

// ServerError allows adapters to expose only a stable retry classification.
type ServerError struct {
	Cause     error
	Unknown   bool
	Retryable bool
}

func (e *ServerError) Error() string {
	if e == nil {
		return "媒体服务器错误"
	}
	if e.Unknown {
		return ErrOutcomeUnknown.Error()
	}
	return ErrServerUnavailable.Error()
}

func (e *ServerError) Unwrap() error {
	if e == nil {
		return nil
	}
	return e.Cause
}

// Is exposes only the stable media-server error class. The adapter cause stays
// available for context cancellation without becoming part of user-facing text.
func (e *ServerError) Is(target error) bool {
	if e == nil {
		return false
	}
	if e.Unknown {
		return target == ErrOutcomeUnknown
	}
	return target == ErrServerUnavailable
}
