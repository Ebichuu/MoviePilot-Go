package database

import (
	"encoding/json"
	"errors"
	"time"

	"github.com/google/uuid"
)

const (
	maxRSSCursorEndpoint         = 2048
	maxRSSItemsLimit             = 200
	maxRSSItemKey                = 256
	maxRSSGUID                   = 512
	maxRSSLocator                = 2048
	maxRSSTitle                  = 1024
	maxRSSLastError              = 512
	maxRSSResourceBytes          = 1 << 20
	maxRSSAttempts               = 8
	maxRSSCacheItems             = 100
	maxRSSRoundSites             = 128
	maxRSSMediaRecognizeFailures = 3
)

var (
	// ErrInvalidRSSInput 表示 RSS 持久化输入不满足边界或身份约束。
	ErrInvalidRSSInput = errors.New("RSS 输入无效")
	// ErrRSSCursorNotFound 表示站点 RSS 游标不存在。
	ErrRSSCursorNotFound = errors.New("RSS 游标不存在")
	// ErrRSSItemNotFound 表示 RSS 条目不存在。
	ErrRSSItemNotFound = errors.New("RSS 条目不存在")
	// ErrRSSLeaseConflict 表示租约已被其他 worker 持有，或提交 token 已过期。
	ErrRSSLeaseConflict = errors.New("RSS 租约冲突")
	// ErrRSSPersistence 表示 RSS 持久化暂时失败。
	ErrRSSPersistence = errors.New("RSS 持久化失败")
)

// RSSCursor 是站点 RSS 增量轮询的持久游标。LeaseToken 仅在当前 worker
// 成功取得租约时返回，调用方不得将其写入日志或 API 响应。
type RSSCursor struct {
	SiteID               int64
	Endpoint             string
	ETag                 string
	LastModified         string
	HighWaterPublishedAt *time.Time
	HighWaterItemKey     string
	NextPollAt           time.Time
	LastAttemptAt        *time.Time
	LastSuccessAt        *time.Time
	FailureCount         int
	LastError            string
	LeaseToken           uuid.UUID
	LeaseExpiresAt       *time.Time
	UpdatedAt            time.Time
}

// RSSItemInput 是一次 RSS 响应中待幂等写入的条目。Resource 必须是 JSON
// object，且只应包含公开的候选字段，不应包含 Cookie、passkey 或授权信息。
type RSSItemInput struct {
	ItemKey     string
	GUID        string
	DownloadID  string
	DetailURL   string
	DownloadURL string
	// ResourceSecret is an optional sensitive locator. It is encrypted before
	// persistence and never returned through JSON/API layers.
	ResourceSecret string `json:"-"`
	Title          string
	// RecognizedMediaTitle is the private canonical media title used by V2
	// candidate ordering. It is never exposed through the public resource JSON.
	RecognizedMediaTitle string
	// MediaRecognizeFailCount records attempts made by the RSS media recognizer.
	MediaRecognizeFailCount int
	PublishedAt             *time.Time
	Resource                json.RawMessage
}

// RSSItem 是从数据库读取的 RSS 条目。Resource 受 1 MiB 上限保护，且不保存
// RSS 原文或认证信息。
type RSSItem struct {
	ID                      int64
	SiteID                  int64
	ItemKey                 string
	GUID                    string
	DownloadID              string
	DetailURL               string
	DownloadURL             string
	ResourceSecret          string `json:"-"`
	Title                   string
	RecognizedMediaTitle    string
	MediaRecognizeFailCount int
	PublishedAt             *time.Time
	Resource                json.RawMessage
	FirstSeenAt             time.Time
	LastSeenAt              time.Time
	State                   string
	AttemptCount            int
	LastError               string
	MatchedAt               *time.Time
	LeaseToken              uuid.UUID
	LeaseExpires            *time.Time
	UpdatedAt               time.Time
}

// RSSPollResult 是一次有界 RSS 请求的结果。ObservedAt 是本轮稳定的逻辑
// 观察时间；NotModified 为 true 时不会改写条目，只推进校验器和成功时间。
type RSSPollResult struct {
	NotModified bool
	// ReplaceCache mirrors V2's NO_CACHE_SITE_KEY behavior for a successful
	// non-empty refresh. It is internal persistence metadata, not API data.
	ReplaceCache bool
	ETag         string
	LastModified string
	ObservedAt   time.Time
	Items        []RSSItemInput
}
