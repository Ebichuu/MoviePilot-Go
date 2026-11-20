package database

import (
	"errors"

	"github.com/moviepilot-go/moviepilot-go/internal/subscription"
)

// SubscriptionEpisodeState 是单集从缺失到入库的领域状态。
// Deprecated: use subscription.SubscriptionEpisodeState.
type SubscriptionEpisodeState = subscription.SubscriptionEpisodeState

const (
	SubscriptionEpisodeMissing             = subscription.SubscriptionEpisodeMissing
	SubscriptionEpisodeAcquired            = subscription.SubscriptionEpisodeAcquired
	SubscriptionEpisodeDownloading         = subscription.SubscriptionEpisodeDownloading
	SubscriptionEpisodePendingOrganization = subscription.SubscriptionEpisodePendingOrganization
	SubscriptionEpisodeOrganizing          = subscription.SubscriptionEpisodeOrganizing
	SubscriptionEpisodePendingIndex        = subscription.SubscriptionEpisodePendingIndex
	SubscriptionEpisodeIndexed             = subscription.SubscriptionEpisodeIndexed
)

var (
	// ErrInvalidSubscriptionEpisode 表示逐集坐标、状态或批次参数无效。
	ErrInvalidSubscriptionEpisode = errors.New("订阅逐集状态参数无效")
	// ErrSubscriptionEpisodeNotFound 对不存在、已完成或不属于当前用户的写入目标统一返回。
	ErrSubscriptionEpisodeNotFound = errors.New("订阅逐集状态不存在")
	// ErrSubscriptionEpisodeStateConflict 表示逐集 CAS 的预期状态已经过期。
	ErrSubscriptionEpisodeStateConflict = errors.New("订阅逐集状态冲突")
	// ErrSubscriptionEpisodeBindingConflict 表示至少一集已绑定其他意图或不允许首次绑定。
	ErrSubscriptionEpisodeBindingConflict = errors.New("订阅逐集下载意图绑定冲突")
	// ErrSubscriptionEpisodePersistence 表示数据库暂时无法安全完成逐集持久化。
	ErrSubscriptionEpisodePersistence = errors.New("订阅逐集状态持久化失败")
)

const (
	maxSubscriptionEpisodes      = 10_000
	maxSubscriptionEpisodeSeason = 999
	maxSubscriptionEpisodeNumber = 5_000
)

// SubscriptionEpisode 是 owner 可见的单集进度记录。
// Deprecated: use subscription.SubscriptionEpisode.
type SubscriptionEpisode = subscription.SubscriptionEpisode

// SubscriptionEpisodeSummary 汇总一个订阅各逐集状态的数量。
// Deprecated: use subscription.SubscriptionEpisodeSummary.
type SubscriptionEpisodeSummary = subscription.SubscriptionEpisodeSummary
