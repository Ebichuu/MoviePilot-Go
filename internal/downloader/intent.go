package downloader

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"sort"
	"strings"

	"github.com/google/uuid"
)

const (
	maxTargetEpisodes = 10_000
	maxTargetSeason   = 999
	maxTargetEpisode  = 5_000
)

var (
	// ErrEpisodeSelectionUnavailable 表示下载器不支持按目标集选择文件。
	ErrEpisodeSelectionUnavailable = errors.New("下载器不支持逐集文件选择")
	// ErrEpisodeSelectionPending 表示种子元数据尚未准备好，稍后可安全重试。
	ErrEpisodeSelectionPending = errors.New("下载器种子文件清单尚未准备好")
	// ErrEpisodeSelectionNoMatch 表示种子中没有可确认的目标集文件。
	ErrEpisodeSelectionNoMatch = errors.New("种子中没有匹配的目标集文件")
	// ErrEpisodeSelectionFailed 表示文件选择或恢复动作未能安全完成。
	ErrEpisodeSelectionFailed = errors.New("下载器逐集文件选择失败")
	// ErrTorrentResumeFailed 表示下载器已确认新增任务，但恢复/强制继续动作失败。
	ErrTorrentResumeFailed = errors.New("下载器恢复新添加任务失败")
)

// Episode 是下载器文件选择使用的有限季集坐标。
type Episode struct {
	Season  int
	Episode int
}

// NormalizeTargetEpisodes 校验、去重并稳定排序目标集坐标。
func NormalizeTargetEpisodes(episodes []Episode) ([]Episode, error) {
	if len(episodes) > maxTargetEpisodes {
		return nil, ErrEpisodeSelectionFailed
	}
	if len(episodes) == 0 {
		return nil, nil
	}
	seen := make(map[Episode]struct{}, len(episodes))
	result := make([]Episode, 0, len(episodes))
	for _, episode := range episodes {
		if episode.Season < 0 || episode.Season > maxTargetSeason ||
			episode.Episode < 1 || episode.Episode > maxTargetEpisode {
			return nil, ErrEpisodeSelectionFailed
		}
		if _, exists := seen[episode]; exists {
			continue
		}
		seen[episode] = struct{}{}
		result = append(result, episode)
	}
	sort.Slice(result, func(i, j int) bool {
		if result[i].Season != result[j].Season {
			return result[i].Season < result[j].Season
		}
		return result[i].Episode < result[j].Episode
	})
	return result, nil
}

// IntentState 是下载意图的持久状态。
type IntentState string

const (
	IntentPlanned     IntentState = "planned"
	IntentSubmitting  IntentState = "submitting"
	IntentDownloading IntentState = "downloading"
	IntentCompleted   IntentState = "completed"
	IntentFailed      IntentState = "failed"
)

// ReconcileDecision 描述下一步允许执行的幂等动作。
type ReconcileDecision string

const (
	ReconcileReuseExisting ReconcileDecision = "reuse_existing"
	ReconcileSubmitNew     ReconcileDecision = "submit_new"
	ReconcileCheckHash     ReconcileDecision = "check_hash"
	ReconcileRetryFailed   ReconcileDecision = "retry_failed"
)

var (
	// ErrReconciliationPending 表示外部结果未知，不得直接重复加种。
	ErrReconciliationPending = errors.New("下载结果尚未确认，必须继续按 Hash 对账")
	// ErrReconciliationUnavailable 表示当前无法安全完成标签或 Hash 对账。
	ErrReconciliationUnavailable = errors.New("下载器对账暂不可用")
	// ErrDefinitiveAddFailure 由下载器适配器用于声明已经得到明确失败结果。
	ErrDefinitiveAddFailure = errors.New("下载器明确拒绝了下载任务")
)

// Candidate 是已完成过滤和排序的下载候选。
type Candidate struct {
	Key         string
	DownloadURL string
	SavePath    string
	Paused      bool
	// TargetEpisodes contains the explicit file selection from the immutable
	// download plan/input policy. It is excluded from CandidateAssociationKey.
	TargetEpisodes []Episode
}

// Intent 在调用下载器之前持久化，Label 可用于外部对账。
type Intent struct {
	ID           uuid.UUID
	CandidateKey string
	Label        string
	State        IntentState
	TorrentHash  string
	// AdoptedExisting 表示本次提交没有新增任务，而是接管了一个已经存在的种子
	// （V2 `app/modules/qbittorrent/__init__.py:198–225` 的「名称与大小相同即同一
	// 个种子」分支）。它只作为本次调用的结果标记，不写入数据库：V2 在该分支直接
	// 返回「下载任务已存在」，因此外层不得再对该种子执行暂停/选集/恢复。
	AdoptedExisting bool
}

// NewIntent 从用户和候选键生成确定性意图 ID、安全关联键与 qB 标签。
func NewIntent(username string, candidate Candidate) (Intent, error) {
	username = strings.TrimSpace(username)
	if username == "" {
		return Intent{}, errors.New("下载意图缺少用户或候选关联键")
	}
	associationKey, err := CandidateAssociationKey(candidate)
	if err != nil {
		return Intent{}, err
	}
	digest := sha256.Sum256([]byte("moviepilot:intent:v1\x00" + username + "\x00" + associationKey))
	id, err := uuid.FromBytes(digest[:16])
	if err != nil {
		return Intent{}, err
	}
	label := "mp-" + hex.EncodeToString(digest[:16])
	return Intent{ID: id, CandidateKey: associationKey, Label: label, State: IntentPlanned}, nil
}

// CandidateAssociationKey 返回不暴露候选原始定位信息的确定性关联键。
func CandidateAssociationKey(candidate Candidate) (string, error) {
	key := strings.TrimSpace(candidate.Key)
	if key == "" {
		return "", errors.New("下载意图缺少用户或候选关联键")
	}
	digest := sha256.Sum256([]byte("moviepilot:candidate:v1\x00" + key))
	return "sha256:" + hex.EncodeToString(digest[:]), nil
}

// NormalizeTorrentHash 校验并规范化 40 位 v1 或 64 位 v2 Torrent Hash。
func NormalizeTorrentHash(hash string) (string, error) {
	hash = strings.TrimSpace(hash)
	if len(hash) != 40 && len(hash) != 64 {
		return "", errors.New("种子 Hash 无效")
	}
	if _, err := hex.DecodeString(hash); err != nil {
		return "", errors.New("种子 Hash 无效")
	}
	return strings.ToUpper(hash), nil
}

// WithTorrentHash 返回写入规范化 Hash 后的意图副本。
func (i Intent) WithTorrentHash(hash string) (Intent, error) {
	normalized, err := NormalizeTorrentHash(hash)
	if err != nil {
		return i, err
	}
	i.TorrentHash = normalized
	return i, nil
}

// Transition 执行受控状态转换；相同状态可幂等重放。
func (i Intent) Transition(next IntentState) (Intent, error) {
	if !validIntentState(i.State) || !validIntentState(next) {
		return i, errors.New("下载意图状态无效")
	}
	if i.State == next {
		return normalizeStateHash(i, next)
	}
	allowed := false
	switch i.State {
	case IntentPlanned:
		allowed = next == IntentSubmitting
	case IntentSubmitting:
		allowed = next == IntentDownloading || next == IntentFailed
	case IntentDownloading:
		allowed = next == IntentCompleted || next == IntentFailed
	case IntentFailed:
		allowed = next == IntentSubmitting
	case IntentCompleted:
		allowed = false
	}
	if !allowed {
		return i, errors.New("下载意图状态转换非法")
	}
	updated, err := normalizeStateHash(i, next)
	if err != nil {
		return i, err
	}
	updated.State = next
	return updated, nil
}

// DecideReconciliation 根据当前状态和外部发现结果决定唯一安全动作。
func DecideReconciliation(state IntentState, found bool) (ReconcileDecision, error) {
	if !validIntentState(state) {
		return "", errors.New("下载意图状态无效")
	}
	if found {
		return ReconcileReuseExisting, nil
	}
	switch state {
	case IntentPlanned:
		return ReconcileSubmitNew, nil
	case IntentFailed:
		return ReconcileRetryFailed, nil
	case IntentSubmitting, IntentDownloading, IntentCompleted:
		return ReconcileCheckHash, nil
	default:
		return "", errors.New("下载意图状态无效")
	}
}

// Client 是 qBittorrent 适配器需要提供的最小幂等接口。
type Client interface {
	FindByLabel(ctx context.Context, label string) (torrentHash string, found bool, err error)
	Add(ctx context.Context, candidate Candidate, label string) error
}

// HashClient 是支持按 Torrent Hash 对账的可选下载器能力。
type HashClient interface {
	FindByHash(ctx context.Context, hash string) (torrentHash string, found bool, err error)
}

// EpisodeFileSelector is an optional downloader capability used by TV
// subscriptions. Implementations must leave the task paused until all
// non-target files have been disabled and then resume it.
type EpisodeFileSelector interface {
	SelectEpisodeFiles(ctx context.Context, torrentHash string, episodes []Episode) error
}

// Submitter 实现“先对账、再加种、错误后再对账”的副作用协议。
type Submitter struct {
	client Client
}

// NewSubmitter 创建下载提交协调器。
func NewSubmitter(client Client) *Submitter { return &Submitter{client: client} }

// Submit 提交下载意图；未知结果只允许继续标签和 Hash 对账。
func (s *Submitter) Submit(ctx context.Context, intent Intent, candidate Candidate) (Intent, error) {
	if s == nil || s.client == nil || intent.Label == "" || intent.ID == uuid.Nil {
		return intent, errors.New("下载意图尚未持久化或缺少确定性标签")
	}
	if !validIntentState(intent.State) {
		return intent, errors.New("下载意图状态无效")
	}
	associationKey, err := CandidateAssociationKey(candidate)
	if err != nil || associationKey != intent.CandidateKey {
		return intent, errors.New("下载候选与意图不匹配")
	}
	if intent.TorrentHash != "" {
		normalized, err := NormalizeTorrentHash(intent.TorrentHash)
		if err != nil {
			return intent, err
		}
		intent.TorrentHash = normalized
	}
	reconciled, found, err := s.reconcileExisting(ctx, intent)
	if err != nil {
		return intent, err
	}
	decision, err := DecideReconciliation(intent.State, found)
	if err != nil {
		return intent, err
	}
	if decision == ReconcileReuseExisting {
		return reconciled, nil
	}
	if decision == ReconcileCheckHash {
		return intent, ErrReconciliationPending
	}

	submitting, err := intent.Transition(IntentSubmitting)
	if err != nil {
		return intent, err
	}
	addErr := s.client.Add(ctx, candidate, submitting.Label)
	reconciled, found, reconcileErr := s.reconcileExisting(ctx, submitting)
	if reconcileErr != nil {
		return submitting, reconcileErr
	}
	if found {
		return reconciled, nil
	}
	if errors.Is(addErr, ErrDefinitiveAddFailure) {
		failed, transitionErr := submitting.Transition(IntentFailed)
		if transitionErr != nil {
			return submitting, transitionErr
		}
		return failed, ErrDefinitiveAddFailure
	}
	return submitting, ErrReconciliationPending
}

func (s *Submitter) reconcileExisting(ctx context.Context, intent Intent) (Intent, bool, error) {
	hash, found, err := s.client.FindByLabel(ctx, intent.Label)
	if err != nil {
		return intent, false, ErrReconciliationUnavailable
	}
	if found {
		updated, updateErr := markIntentDownloading(intent, hash)
		if updateErr != nil {
			return intent, false, ErrReconciliationUnavailable
		}
		return updated, true, nil
	}
	if intent.TorrentHash == "" {
		return intent, false, nil
	}
	hashClient, ok := s.client.(HashClient)
	if !ok {
		return intent, false, ErrReconciliationUnavailable
	}
	hash, found, err = hashClient.FindByHash(ctx, intent.TorrentHash)
	if err != nil {
		return intent, false, ErrReconciliationUnavailable
	}
	if !found {
		return intent, false, nil
	}
	updated, updateErr := markIntentDownloading(intent, hash)
	if updateErr != nil {
		return intent, false, ErrReconciliationUnavailable
	}
	return updated, true, nil
}

func markIntentDownloading(intent Intent, hash string) (Intent, error) {
	updated, err := intent.WithTorrentHash(hash)
	if err != nil {
		return intent, err
	}
	switch updated.State {
	case IntentPlanned, IntentFailed:
		updated, err = updated.Transition(IntentSubmitting)
		if err != nil {
			return intent, err
		}
	case IntentCompleted:
		return updated, nil
	}
	return updated.Transition(IntentDownloading)
}

func normalizeStateHash(intent Intent, state IntentState) (Intent, error) {
	if state != IntentDownloading && state != IntentCompleted {
		return intent, nil
	}
	return intent.WithTorrentHash(intent.TorrentHash)
}

func validIntentState(state IntentState) bool {
	switch state {
	case IntentPlanned, IntentSubmitting, IntentDownloading, IntentCompleted, IntentFailed:
		return true
	default:
		return false
	}
}
