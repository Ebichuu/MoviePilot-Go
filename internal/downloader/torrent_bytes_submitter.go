package downloader

import (
	"context"
	"errors"
	"strings"

	"github.com/google/uuid"

	"github.com/moviepilot-go/moviepilot-go/internal/torrentmeta"
)

// TorrentBytesClient 是种子字节提交闭环所需的最小下载器接口。
type TorrentBytesClient interface {
	TaskObserver
	FindByLabel(ctx context.Context, label string) (torrentHash string, found bool, err error)
	FindByHash(ctx context.Context, hash string) (torrentHash string, found bool, err error)
	AddTorrentBytes(ctx context.Context, metainfo []byte, savePath, label string, paused bool) error
}

// SelectEpisodeFiles 将种子中非目标集文件的优先级设为 0；这是可选能力，
// 不把该方法加入 TorrentBytesClient，以免破坏其他下载器测试替身。
func (s *TorrentBytesSubmitter) SelectEpisodeFiles(ctx context.Context, intent Intent, episodes []Episode) error {
	if ctx == nil {
		return errors.New("下载选择上下文无效")
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	normalized, err := NormalizeTargetEpisodes(episodes)
	if err != nil {
		return err
	}
	if len(normalized) == 0 {
		return nil
	}
	if s == nil || s.client == nil {
		return ErrEpisodeSelectionUnavailable
	}
	if strings.TrimSpace(intent.TorrentHash) == "" {
		return ErrEpisodeSelectionUnavailable
	}
	selector, ok := s.client.(EpisodeFileSelector)
	if !ok {
		return ErrEpisodeSelectionUnavailable
	}
	hash, err := NormalizeTorrentHash(intent.TorrentHash)
	if err != nil {
		return err
	}
	if err := selector.SelectEpisodeFiles(ctx, hash, normalized); err != nil {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return ctxErr
		}
		for _, known := range []error{
			ErrEpisodeSelectionPending,
			ErrEpisodeSelectionNoMatch,
			ErrEpisodeSelectionUnavailable,
			ErrEpisodeSelectionFailed,
		} {
			if errors.Is(err, known) {
				return known
			}
		}
		return ErrEpisodeSelectionFailed
	}
	return nil
}

var _ TorrentBytesClient = (*QBittorrent)(nil)

// TorrentBytesSubmitter 协调内存种子字节的幂等提交与后续对账。
type TorrentBytesSubmitter struct {
	client TorrentBytesClient
}

// NewTorrentBytesSubmitter 创建种子字节提交协调器。
func NewTorrentBytesSubmitter(client TorrentBytesClient) *TorrentBytesSubmitter {
	return &TorrentBytesSubmitter{client: client}
}

// InspectTask exposes the downloader's bounded, secret-free task projection
// when the configured client supports task inspection. This keeps the normal
// submission contract small while allowing the organizer to recover a
// completed task's local content path by deterministic label or hash.
func (s *TorrentBytesSubmitter) InspectTask(
	ctx context.Context,
	label string,
	torrentHash string,
) (Task, bool, error) {
	if ctx == nil {
		return Task{}, false, ErrReconciliationUnavailable
	}
	if err := ctx.Err(); err != nil {
		return Task{}, false, err
	}
	if s == nil || s.client == nil {
		return Task{}, false, ErrReconciliationUnavailable
	}
	inspector, ok := s.client.(TaskInspector)
	if !ok {
		return Task{}, false, ErrReconciliationUnavailable
	}
	task, found, err := inspector.InspectTask(ctx, label, torrentHash)
	if err != nil {
		if contextErr := ctx.Err(); contextErr != nil {
			return Task{}, false, contextErr
		}
		if errors.Is(err, context.Canceled) {
			return Task{}, false, context.Canceled
		}
		if errors.Is(err, context.DeadlineExceeded) {
			return Task{}, false, context.DeadlineExceeded
		}
		return Task{}, false, ErrReconciliationUnavailable
	}
	return task, found, nil
}

// InspectTaskContent 在底层客户端支持安全内容快照时委托查询。
func (s *TorrentBytesSubmitter) InspectTaskContent(
	ctx context.Context,
	label string,
	torrentHash string,
) (TaskContent, bool, error) {
	if ctx == nil {
		return TaskContent{}, false, ErrReconciliationUnavailable
	}
	if err := ctx.Err(); err != nil {
		return TaskContent{}, false, err
	}
	if s == nil || s.client == nil {
		return TaskContent{}, false, ErrReconciliationUnavailable
	}
	inspector, ok := s.client.(TaskContentInspector)
	if !ok {
		return TaskContent{}, false, ErrReconciliationUnavailable
	}
	content, found, err := inspector.InspectTaskContent(ctx, label, torrentHash)
	if err != nil {
		if contextErr := ctx.Err(); contextErr != nil {
			return TaskContent{}, false, contextErr
		}
		if errors.Is(err, context.Canceled) {
			return TaskContent{}, false, context.Canceled
		}
		if errors.Is(err, context.DeadlineExceeded) {
			return TaskContent{}, false, context.DeadlineExceeded
		}
		return TaskContent{}, false, ErrReconciliationUnavailable
	}
	return content, found, nil
}

// ObserveTasks delegates one bounded, read-only status batch to the downloader.
func (s *TorrentBytesSubmitter) ObserveTasks(
	ctx context.Context,
	hashes []string,
) (map[string]TaskObservation, error) {
	if ctx == nil {
		return nil, errors.New("下载任务批量对账上下文无效")
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if s == nil || s.client == nil {
		return nil, ErrReconciliationUnavailable
	}
	observations, err := s.client.ObserveTasks(ctx, hashes)
	if err != nil {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return nil, ctxErr
		}
		return nil, ErrReconciliationUnavailable
	}
	return observations, nil
}

var _ TaskInspector = (*TorrentBytesSubmitter)(nil)
var _ TaskContentInspector = (*TorrentBytesSubmitter)(nil)

// Reconcile 只按确定性标签和已知 Hash 对账，绝不会调用 AddTorrentBytes。
// found 为 false 且 err 为 nil 表示下载器当前确实没有可见任务。
func (s *TorrentBytesSubmitter) Reconcile(
	ctx context.Context,
	intent Intent,
) (reconciled Intent, found bool, err error) {
	intent, err = s.validateIntent(ctx, intent)
	if err != nil {
		return intent, false, err
	}
	reconciled, found, err = s.reconcileExisting(ctx, intent)
	if err != nil {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return intent, false, ctxErr
		}
		return intent, false, ErrReconciliationUnavailable
	}
	return reconciled, found, nil
}

// AddOnce 在调用方已经持有 planned|failed -> submitting 的持久 CAS 许可后，
// 最多调用一次 AddTorrentBytes。未知结果始终保持 submitting，交给后续任务对账。
func (s *TorrentBytesSubmitter) AddOnce(
	ctx context.Context,
	intent Intent,
	candidate Candidate,
	metainfo []byte,
) (Intent, error) {
	validated, err := s.validateIntent(ctx, intent)
	if err != nil {
		return intent, err
	}
	intent = validated
	if intent.State != IntentSubmitting {
		return intent, errors.New("下载意图尚未取得单次提交许可")
	}
	if err := validateTorrentBytesCandidate(intent, candidate); err != nil {
		return intent, err
	}
	if err := validateTorrentMetainfo(metainfo); err != nil {
		return intent, err
	}

	addErr := s.client.AddTorrentBytes(
		ctx,
		metainfo,
		candidate.SavePath,
		intent.Label,
		candidate.Paused || len(candidate.TargetEpisodes) > 0,
	)
	if addResultUnknown(ctx, addErr) {
		return intent, ErrReconciliationPending
	}
	definitiveFailure := errors.Is(addErr, ErrDefinitiveAddFailure)

	reconciled, found, reconcileErr := s.Reconcile(ctx, intent)
	if reconcileErr != nil {
		return intent, ErrReconciliationPending
	}
	if found {
		if resumeErr := s.resumeAfterFreshAdd(ctx, candidate, reconciled, addErr); resumeErr != nil {
			return reconciled, resumeErr
		}
		return reconciled, nil
	}
	if addErr != nil && !definitiveFailure {
		// V2 raises on a transport/unknown add failure instead of scanning the
		// torrent list, so keep the conservative unknown result.
		return intent, ErrReconciliationPending
	}
	if addErr != nil {
		// V2 只在「本次没有新增任务」的分支（`__init__.py:198–225`）按名称与
		// 大小查重；本次新增成功却取不到 Hash 时走 233–234 的失败路径，不能
		// 退化成对既有种子的接管。
		if identity, identityFound, identityErr := s.reconcileExistingIdentity(ctx, intent, metainfo); identityErr != nil {
			return intent, ErrReconciliationPending
		} else if identityFound {
			// V2 `__init__.py:198–225` adopts the existing torrent and returns
			// "下载任务已存在" without resuming, force starting or selecting files
			// again. AdoptedExisting carries that fact to the caller.
			identity.AdoptedExisting = true
			return identity, nil
		}
	}
	if definitiveFailure {
		failed, transitionErr := intent.Transition(IntentFailed)
		if transitionErr != nil {
			return intent, transitionErr
		}
		return failed, ErrDefinitiveAddFailure
	}
	return intent, ErrReconciliationPending
}

// torrentAddedResumer is the optional V2 `force_resume` capability of a
// downloader client for the freshly-added, non-selection branch. Clients that do
// not implement it keep the previous behavior, which matches V2 where only the
// qBittorrent module owns the switch.
type torrentAddedResumer interface {
	ResumeAddedTorrent(ctx context.Context, torrentHash string) error
}

// torrentIdentityMatcher is the optional V2 duplicate-lookup capability. It
// implements the frozen `app/modules/qbittorrent/__init__.py:203–224` rule that
// a torrent with an identical name and total_size is the same torrent.
type torrentIdentityMatcher interface {
	FindByNameAndSize(ctx context.Context, name string, totalSize int64) (string, bool, error)
}

// resumeAfterFreshAdd mirrors V2 `app/modules/qbittorrent/__init__.py:275–278`:
// a freshly added torrent that is not paused for episode selection is force
// started when the downloader exposes `force_resume`. An add that merely matched
// an existing torrent (`addErr != nil`) returns early in V2 and must not force
// start here, and the paused path force starts when the episode selection
// resumes the torrent instead.
//
// V2 在底层客户端 `app/modules/qbittorrent/qbittorrent.py:354–365` 捕获并记录
// setForceStart 的异常后继续，QBittorrent 适配器同样吞掉该错误并记录，因此正常
// 情况下这里不会看到恢复失败。保留的稳定错误出口只用于其它下载器适配器真的返回
// 恢复失败时让 Worker 重试并对账已确认的任务。
func (s *TorrentBytesSubmitter) resumeAfterFreshAdd(
	ctx context.Context,
	candidate Candidate,
	reconciled Intent,
	addErr error,
) error {
	if addErr != nil || candidate.Paused || len(candidate.TargetEpisodes) > 0 {
		return nil
	}
	resumer, ok := s.client.(torrentAddedResumer)
	if !ok {
		return nil
	}
	hash := reconciled.TorrentHash
	if hash == "" {
		return nil
	}
	if err := resumer.ResumeAddedTorrent(ctx, hash); err != nil {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return ctxErr
		}
		return ErrTorrentResumeFailed
	}
	return nil
}

// reconcileExistingIdentity mirrors the V2 duplicate-torrent lookup in
// `app/modules/qbittorrent/__init__.py:198–225`: when the add did not yield a new
// task, V2 scans every torrent and treats an exact name + total_size match as the
// already-existing task. Clients that expose the bounded lookup adopt that hash
// so the confirmed task is tracked instead of being reported as a failure.
func (s *TorrentBytesSubmitter) reconcileExistingIdentity(
	ctx context.Context,
	intent Intent,
	metainfo []byte,
) (Intent, bool, error) {
	matcher, ok := s.client.(torrentIdentityMatcher)
	if !ok {
		return intent, false, nil
	}
	inspection, err := torrentmeta.Inspect(metainfo)
	if err != nil || inspection.Name == "" {
		return intent, false, nil
	}
	hash, found, err := matcher.FindByNameAndSize(ctx, inspection.Name, inspection.TotalSize)
	if err != nil {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return intent, false, ctxErr
		}
		// A dependency failure after a confirmed add is an unknown result; V2
		// would have raised inside the same branch.
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

// Submit 提交仅在本次调用内存中存在的 metainfo。
// 任何不能确认的 Add 结果都会保持 submitting，后续重放只允许继续对账。
func (s *TorrentBytesSubmitter) Submit(
	ctx context.Context,
	intent Intent,
	candidate Candidate,
	metainfo []byte,
) (Intent, error) {
	validated, err := s.validateIntent(ctx, intent)
	if err != nil {
		return intent, err
	}
	intent = validated
	if err := validateTorrentBytesCandidate(intent, candidate); err != nil {
		return intent, err
	}

	reconciled, found, reconcileErr := s.Reconcile(ctx, intent)
	if reconcileErr != nil {
		if errors.Is(reconcileErr, context.Canceled) || errors.Is(reconcileErr, context.DeadlineExceeded) {
			return intent, reconcileErr
		}
		if intent.State == IntentSubmitting {
			return intent, ErrReconciliationPending
		}
		return intent, ErrReconciliationUnavailable
	}
	decision, err := DecideReconciliation(intent.State, found)
	if err != nil {
		return intent, err
	}
	switch decision {
	case ReconcileReuseExisting:
		return reconciled, nil
	case ReconcileCheckHash:
		return intent, ErrReconciliationPending
	}
	if err := validateTorrentMetainfo(metainfo); err != nil {
		return intent, err
	}
	submitting, err := intent.Transition(IntentSubmitting)
	if err != nil {
		return intent, err
	}
	return s.AddOnce(ctx, submitting, candidate, metainfo)
}

func (s *TorrentBytesSubmitter) validateIntent(ctx context.Context, intent Intent) (Intent, error) {
	if ctx == nil {
		return intent, errors.New("下载提交上下文无效")
	}
	if err := ctx.Err(); err != nil {
		return intent, err
	}
	if s == nil || s.client == nil || intent.Label == "" || intent.ID == uuid.Nil {
		return intent, errors.New("下载意图尚未持久化或缺少确定性标签")
	}
	if !validIntentState(intent.State) {
		return intent, errors.New("下载意图状态无效")
	}
	if intent.TorrentHash == "" {
		return intent, nil
	}
	normalized, err := NormalizeTorrentHash(intent.TorrentHash)
	if err != nil {
		return intent, err
	}
	intent.TorrentHash = normalized
	return intent, nil
}

func validateTorrentBytesCandidate(intent Intent, candidate Candidate) error {
	associationKey, err := CandidateAssociationKey(candidate)
	if err != nil || associationKey != intent.CandidateKey {
		return errors.New("下载候选与意图不匹配")
	}
	return nil
}

func validateTorrentMetainfo(metainfo []byte) error {
	if len(metainfo) == 0 {
		return errors.New("种子文件内容为空")
	}
	if len(metainfo) > maxTorrentMetainfoSize {
		return errors.New("种子文件超过大小上限")
	}
	return nil
}

func (s *TorrentBytesSubmitter) reconcileExisting(ctx context.Context, intent Intent) (Intent, bool, error) {
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
	hash, found, err = s.client.FindByHash(ctx, intent.TorrentHash)
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

func addResultUnknown(ctx context.Context, addErr error) bool {
	if ctx != nil && ctx.Err() != nil {
		return true
	}
	return errors.Is(addErr, context.Canceled) || errors.Is(addErr, context.DeadlineExceeded)
}
