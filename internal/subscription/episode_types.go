package subscription

import (
	"time"

	"github.com/google/uuid"
)

// SubscriptionEpisodeState 是单集从缺失到入库的领域状态。
type SubscriptionEpisodeState string

const (
	SubscriptionEpisodeMissing             SubscriptionEpisodeState = "missing"
	SubscriptionEpisodeAcquired            SubscriptionEpisodeState = "acquired"
	SubscriptionEpisodeDownloading         SubscriptionEpisodeState = "downloading"
	SubscriptionEpisodePendingOrganization SubscriptionEpisodeState = "pending_organization"
	SubscriptionEpisodeOrganizing          SubscriptionEpisodeState = "organizing"
	SubscriptionEpisodePendingIndex        SubscriptionEpisodeState = "pending_index"
	SubscriptionEpisodeIndexed             SubscriptionEpisodeState = "indexed"
)

// SubscriptionEpisode 是订阅领域中的单集进度记录。
type SubscriptionEpisode struct {
	ID               int64
	SubscriptionID   int64
	Season           int
	Episode          int
	State            SubscriptionEpisodeState
	DownloadIntentID *uuid.UUID
	UpdatedAt        time.Time
}

// SubscriptionEpisodeSummary 汇总一个订阅各逐集状态的数量。
type SubscriptionEpisodeSummary struct {
	Total               int
	Missing             int
	Acquired            int
	Downloading         int
	PendingOrganization int
	Organizing          int
	PendingIndex        int
	Indexed             int
}
