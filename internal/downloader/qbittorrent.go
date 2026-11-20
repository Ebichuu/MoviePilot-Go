package downloader

import (
	"context"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"math"
	"net/http"
	"path"
	"regexp"
	"strings"
	"time"

	qbittorrent "github.com/autobrr/go-qbittorrent"

	"github.com/moviepilot-go/moviepilot-go/internal/torrentmeta"
)

const (
	// 与 MOVIEPILOT_MAX_RESPONSE_BODY 的硬上限保持一致，避免无界缓冲种子文件。
	maxTorrentMetainfoSize  = torrentmeta.MaxSize
	maxDownloadingTasks     = 1000
	maxDownloadHistoryTasks = 2000
	maxObservedTasks        = 50
	maxTaskContentFiles     = 10_000
	// maxTorrentIdentityScan bounds V2's "scan every torrent" duplicate lookup
	// so a pathological task list cannot force an unbounded response decode.
	maxTorrentIdentityScan = 10_000

	maxTaskContentRelativePathLength = 4096
)

var moviePilotTaskTagPattern = regexp.MustCompile(`^mp-[0-9a-f]{32}$`)

// QBittorrentConfig 保存 qBittorrent 连接信息；密码不得写入日志。
type QBittorrentConfig struct {
	URL             string
	Username        string
	Password        string
	APIKey          string
	TLSInsecure     bool
	Timeout         time.Duration
	MaxConnsPerHost int
	// ForceResume 对应 V2 qBittorrent 模块的 `force_resume`：为真时用
	// force start 恢复任务，而不是普通 start。
	ForceResume bool
	// Logger 可选；为空时使用 slog 默认 logger。V2 在底层客户端里记录
	// setForceStart 的异常，Go 侧同样需要一个日志出口。
	Logger *slog.Logger
}

// QBittorrent 使用 autobrr/go-qbittorrent 实现下载器协议。
type QBittorrent struct {
	client      *qbittorrent.Client
	transport   *http.Transport
	queryPermit chan struct{}
	forceResume bool
	logger      *slog.Logger
	// filesRetryInterval 覆盖 V2 选集文件清单的重试间隔；零值使用 1 秒。
	filesRetryInterval time.Duration
}

var _ DownloadingTaskLister = (*QBittorrent)(nil)
var _ TaskInspector = (*QBittorrent)(nil)
var _ TaskContentInspector = (*QBittorrent)(nil)
var _ TaskObserver = (*QBittorrent)(nil)

// NewQBittorrent 创建带连接和超时上限的 qBittorrent 适配器。
func NewQBittorrent(cfg QBittorrentConfig) (*QBittorrent, error) {
	if strings.TrimSpace(cfg.URL) == "" {
		return nil, errors.New("qBittorrent URL 不能为空")
	}
	if cfg.Timeout <= 0 {
		cfg.Timeout = 30 * time.Second
	}
	if cfg.MaxConnsPerHost < 1 || cfg.MaxConnsPerHost > 32 {
		cfg.MaxConnsPerHost = 4
	}
	client := qbittorrent.NewClient(qbittorrent.Config{
		Host: cfg.URL, Username: cfg.Username, Password: cfg.Password, APIKey: cfg.APIKey,
		TLSSkipVerify: cfg.TLSInsecure, Timeout: int(cfg.Timeout.Seconds()), RetryAttempts: 0,
	})
	transport := &http.Transport{
		Proxy: http.ProxyFromEnvironment, MaxIdleConns: cfg.MaxConnsPerHost * 2,
		MaxIdleConnsPerHost: cfg.MaxConnsPerHost, MaxConnsPerHost: cfg.MaxConnsPerHost,
		IdleConnTimeout: 60 * time.Second, TLSHandshakeTimeout: 10 * time.Second,
		ResponseHeaderTimeout: cfg.Timeout,
	}
	client.WithHTTPClient(&http.Client{Transport: transport, Timeout: cfg.Timeout})
	logger := cfg.Logger
	if logger == nil {
		logger = slog.Default()
	}
	return &QBittorrent{
		client: client, transport: transport,
		queryPermit: make(chan struct{}, 1), forceResume: cfg.ForceResume, logger: logger,
	}, nil
}

// CloseIdleConnections 释放 qBittorrent 客户端持有的空闲连接。
func (q *QBittorrent) CloseIdleConnections() {
	if q != nil && q.transport != nil {
		q.transport.CloseIdleConnections()
	}
}

// Connect 登录并验证 qBittorrent Web API。
func (q *QBittorrent) Connect(ctx context.Context) error {
	if err := q.client.LoginCtx(ctx); err != nil {
		return errors.New("qBittorrent 登录失败")
	}
	return nil
}

// FindByLabel 查询确定性标签对应的下载任务。
func (q *QBittorrent) FindByLabel(ctx context.Context, label string) (string, bool, error) {
	label = strings.TrimSpace(label)
	if label == "" {
		return "", false, errors.New("qBittorrent 标签无效")
	}
	task, found, err := q.InspectTask(ctx, label, "")
	return task.Hash, found, err
}

// FindByHash 按规范化的 v1 或 v2 Info Hash 查询下载任务。
func (q *QBittorrent) FindByHash(ctx context.Context, hash string) (string, bool, error) {
	normalized, err := normalizeTorrentHash(hash)
	if err != nil {
		return "", false, err
	}
	task, found, err := q.InspectTask(ctx, "", normalized)
	return task.Hash, found, err
}

// FindByNameAndSize mirrors the V2 duplicate-torrent lookup in
// `app/modules/qbittorrent/__init__.py:203–224`: a torrent whose `name` and
// `total_size` both equal the metainfo projection is treated as the same
// torrent ("名称与大小相等则认为是同一个种子"). V2 keeps the first match in
// qBittorrent's list order, so this bounded scan does the same. Torrents with
// an unusable Info Hash are skipped instead of aborting the whole scan.
func (q *QBittorrent) FindByNameAndSize(ctx context.Context, name string, totalSize int64) (string, bool, error) {
	if ctx == nil {
		return "", false, errors.New("qBittorrent 任务对账失败")
	}
	if err := ctx.Err(); err != nil {
		return "", false, err
	}
	if name == "" || totalSize < 0 {
		return "", false, errors.New("qBittorrent 任务标识无效")
	}
	torrents, err := q.getTorrents(ctx, qbittorrent.TorrentFilterOptions{
		Filter: qbittorrent.TorrentFilterAll,
		Limit:  maxTorrentIdentityScan,
	})
	if err != nil {
		return "", false, q.inspectTaskError(ctx, err)
	}
	for _, torrent := range torrents {
		if torrent.Name != name || torrent.TotalSize != totalSize {
			continue
		}
		hash, hashErr := normalizeTorrentHash(torrent.Hash)
		if hashErr != nil {
			continue
		}
		return hash, true, nil
	}
	return "", false, nil
}

// InspectTask returns one bounded, secret-free qBittorrent task observation.
// An exact deterministic label is preferred; the known hash is used as a
// fallback when a user removed the label from qBittorrent.
func (q *QBittorrent) InspectTask(ctx context.Context, label, torrentHash string) (Task, bool, error) {
	if ctx == nil {
		return Task{}, false, errors.New("qBittorrent 任务对账失败")
	}
	if err := ctx.Err(); err != nil {
		return Task{}, false, err
	}
	label = strings.TrimSpace(label)
	if label != "" && !moviePilotTaskTagPattern.MatchString(label) {
		return Task{}, false, errors.New("qBittorrent 标签无效")
	}
	normalizedHash := ""
	if strings.TrimSpace(torrentHash) != "" {
		var err error
		normalizedHash, err = normalizeTorrentHash(torrentHash)
		if err != nil {
			return Task{}, false, err
		}
	}
	if label == "" && normalizedHash == "" {
		return Task{}, false, errors.New("qBittorrent 任务标识无效")
	}

	if label != "" {
		torrents, err := q.getTorrents(ctx, qbittorrent.TorrentFilterOptions{
			Filter: qbittorrent.TorrentFilterAll, Tag: label, Limit: 20,
		})
		if err != nil {
			return Task{}, false, q.inspectTaskError(ctx, err)
		}
		for _, torrent := range torrents {
			if !torrentHasLabel(torrent, label) ||
				torrentHasConflictingMoviePilotLabel(torrent, label) ||
				(normalizedHash != "" && !torrentMatchesHash(torrent, normalizedHash)) {
				continue
			}
			if task, ok := inspectedQBTorrent(torrent, label, normalizedHash); ok {
				return task, true, nil
			}
		}
	}

	if normalizedHash == "" {
		return Task{}, false, nil
	}
	torrents, err := q.getTorrents(ctx, qbittorrent.TorrentFilterOptions{Hashes: []string{normalizedHash}})
	if err != nil {
		return Task{}, false, q.inspectTaskError(ctx, err)
	}
	for _, torrent := range torrents {
		if !torrentMatchesHash(torrent, normalizedHash) ||
			(label != "" && torrentHasConflictingMoviePilotLabel(torrent, label)) {
			continue
		}
		if task, ok := inspectedQBTorrent(torrent, label, normalizedHash); ok {
			return task, true, nil
		}
	}
	return Task{}, false, nil
}

// InspectTaskContent 返回任务及其相对于保存目录的已选择、已完成文件快照。
func (q *QBittorrent) InspectTaskContent(
	ctx context.Context,
	label string,
	torrentHash string,
) (TaskContent, bool, error) {
	task, found, err := q.InspectTask(ctx, label, torrentHash)
	if err != nil || !found {
		return TaskContent{}, found, err
	}
	if task.State != TaskStateCompleted {
		return TaskContent{}, false, q.inspectTaskContentError(ctx, nil)
	}

	normalizedHash, err := normalizeTorrentHash(task.Hash)
	if err != nil {
		return TaskContent{}, false, q.inspectTaskContentError(ctx, err)
	}
	task.Hash = normalizedHash
	files, err := q.getFilesInformation(ctx, normalizedHash)
	if err != nil {
		return TaskContent{}, false, q.inspectTaskContentError(ctx, err)
	}
	if files == nil || len(*files) > maxTaskContentFiles {
		return TaskContent{}, false, q.inspectTaskContentError(ctx, nil)
	}

	content := TaskContent{
		Task:  task,
		Files: make([]TaskContentFile, 0, len(*files)),
	}
	seenPaths := make(map[string]struct{}, len(*files))
	var totalSize int64
	for _, file := range *files {
		if err := ctx.Err(); err != nil {
			return TaskContent{}, false, err
		}
		if file.Priority <= 0 {
			continue
		}
		if file.Progress != 1 {
			return TaskContent{}, false, q.inspectTaskContentError(ctx, nil)
		}
		relativePath, ok := safeTaskContentRelativePath(file.Name)
		if !ok || file.Size < 0 || totalSize > math.MaxInt64-file.Size {
			return TaskContent{}, false, q.inspectTaskContentError(ctx, nil)
		}
		if _, duplicate := seenPaths[relativePath]; duplicate {
			return TaskContent{}, false, q.inspectTaskContentError(ctx, nil)
		}
		seenPaths[relativePath] = struct{}{}
		totalSize += file.Size
		content.Files = append(content.Files, TaskContentFile{
			RelativePath: relativePath,
			SizeBytes:    file.Size,
		})
	}
	if len(content.Files) == 0 {
		return TaskContent{}, false, q.inspectTaskContentError(ctx, nil)
	}

	verifiedTask, stillFound, err := q.InspectTask(ctx, label, normalizedHash)
	if err != nil || !stillFound {
		return TaskContent{}, false, q.inspectTaskContentError(ctx, err)
	}
	verifiedHash, err := normalizeTorrentHash(verifiedTask.Hash)
	if err != nil || verifiedHash != task.Hash ||
		verifiedTask.SavePath != task.SavePath ||
		verifiedTask.ContentPath != task.ContentPath ||
		verifiedTask.State != TaskStateCompleted {
		return TaskContent{}, false, q.inspectTaskContentError(ctx, err)
	}
	if err := ctx.Err(); err != nil {
		return TaskContent{}, false, err
	}
	return content, true, nil
}

// ObserveTasks reads up to 50 hashes with one qBittorrent request and returns
// a closed observation for every requested hash. Missing, error, and unknown
// states are never folded into completed.
func (q *QBittorrent) ObserveTasks(ctx context.Context, hashes []string) (map[string]TaskObservation, error) {
	if ctx == nil {
		return nil, errors.New("qBittorrent 批量对账失败")
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if len(hashes) == 0 || len(hashes) > maxObservedTasks {
		return nil, errors.New("qBittorrent 批量对账 Hash 数量无效")
	}

	normalized := make([]string, 0, len(hashes))
	observations := make(map[string]TaskObservation, len(hashes))
	for _, raw := range hashes {
		hash, err := normalizeTorrentHash(raw)
		if err != nil {
			return nil, err
		}
		if _, exists := observations[hash]; exists {
			continue
		}
		normalized = append(normalized, hash)
		observations[hash] = TaskObservation{Hash: hash, State: TaskObservationMissing}
	}

	torrents, err := q.getTorrents(ctx, qbittorrent.TorrentFilterOptions{
		Filter: qbittorrent.TorrentFilterAll,
		Hashes: normalized,
		Limit:  len(normalized),
	})
	if err != nil {
		return nil, q.inspectTaskError(ctx, err)
	}
	for _, torrent := range torrents {
		for _, hash := range normalized {
			if torrentMatchesHash(torrent, hash) {
				observations[hash] = TaskObservation{Hash: hash, State: observeQBTorrentState(torrent.State, torrent.Progress)}
				break
			}
		}
	}
	return observations, nil
}

func (q *QBittorrent) inspectTaskError(ctx context.Context, _ error) error {
	if ctxErr := ctx.Err(); ctxErr != nil {
		return ctxErr
	}
	return errors.New("qBittorrent 任务对账失败")
}

func (q *QBittorrent) inspectTaskContentError(ctx context.Context, _ error) error {
	if ctxErr := ctx.Err(); ctxErr != nil {
		return ctxErr
	}
	return errors.New("qBittorrent 任务内容查询失败")
}

func safeTaskContentRelativePath(value string) (string, bool) {
	if value == "" || len(value) > maxTaskContentRelativePathLength || strings.IndexByte(value, 0) >= 0 {
		return "", false
	}

	normalized := strings.ReplaceAll(value, `\`, "/")
	if strings.HasPrefix(normalized, "/") || hasWindowsDrivePrefix(normalized) {
		return "", false
	}
	for _, segment := range strings.Split(normalized, "/") {
		if segment == ".." {
			return "", false
		}
	}

	relative := path.Clean(normalized)
	if relative == "." || path.IsAbs(relative) || hasWindowsDrivePrefix(relative) ||
		len(relative) > maxTaskContentRelativePathLength {
		return "", false
	}
	return relative, true
}

func hasWindowsDrivePrefix(value string) bool {
	// Windows filepath semantics accept any single-character volume prefix,
	// including values such as "0:/file". Reject the full grammar before the
	// relative path is persisted and interpreted on another platform.
	return len(value) >= 2 && value[1] == ':'
}

func torrentHasLabel(torrent qbittorrent.Torrent, label string) bool {
	for _, tag := range strings.Split(torrent.Tags, ",") {
		if strings.TrimSpace(tag) == label {
			return true
		}
	}
	return false
}

func torrentHasConflictingMoviePilotLabel(torrent qbittorrent.Torrent, expected string) bool {
	for _, tag := range strings.Split(torrent.Tags, ",") {
		label := strings.TrimSpace(tag)
		if moviePilotTaskTagPattern.MatchString(label) && label != expected {
			return true
		}
	}
	return false
}

func torrentMatchesHash(torrent qbittorrent.Torrent, normalized string) bool {
	for _, candidate := range []string{torrent.Hash, torrent.InfohashV1, torrent.InfohashV2} {
		candidateHash, err := normalizeTorrentHash(candidate)
		if err == nil && candidateHash == normalized {
			return true
		}
	}
	return false
}

func inspectedQBTorrent(torrent qbittorrent.Torrent, label, fallbackHash string) (Task, bool) {
	hash, err := normalizeTorrentHash(torrent.Hash)
	if err != nil {
		hash, err = normalizeTorrentHash(fallbackHash)
		if err != nil {
			return Task{}, false
		}
	}
	if label == "" {
		label, _ = moviePilotTaskLabel(torrent.Tags)
	}
	return taskFromQBTorrent(torrent, hash, label), true
}

// ListDownloading returns only qBittorrent tasks owned by MoviePilot.
func (q *QBittorrent) ListDownloading(ctx context.Context) ([]Task, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if q == nil || q.client == nil {
		return nil, errors.New("qBittorrent 下载任务查询失败")
	}

	torrents, err := q.getTorrents(ctx, qbittorrent.TorrentFilterOptions{
		Filter: qbittorrent.TorrentFilterDownloading,
		Limit:  maxDownloadingTasks,
	})
	if err != nil {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return nil, ctxErr
		}
		return nil, errors.New("qBittorrent 下载任务查询失败")
	}

	tasks := make([]Task, 0, len(torrents))
	for _, torrent := range torrents {
		if len(tasks) == maxDownloadingTasks {
			break
		}
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		hash, hashErr := normalizeTorrentHash(torrent.Hash)
		label, hasLabel := moviePilotTaskLabel(torrent.Tags)
		if hashErr != nil || !hasLabel {
			continue
		}
		tasks = append(tasks, taskFromQBTorrent(torrent, hash, label))
	}
	return tasks, nil
}

// ListCompleted 列出已完成任务，供「下载历史」使用。与 ListDownloading 不同，
// 这里不要求 MoviePilot 标签：历史里应包含所有已完成的种子，标签只用来补充归属信息。
func (q *QBittorrent) ListCompleted(ctx context.Context) ([]Task, error) {
	// 上下文缺失必须先判空再取 Err()：否则会直接空指针崩溃，
	// 与同文件其它查询方法保持一致的拒绝方式。
	if ctx == nil {
		return nil, errors.New("qBittorrent 下载任务查询失败")
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if q == nil || q.client == nil {
		return nil, errors.New("qBittorrent 下载任务查询失败")
	}

	torrents, err := q.getTorrents(ctx, qbittorrent.TorrentFilterOptions{
		Filter: qbittorrent.TorrentFilterCompleted,
		Limit:  maxDownloadHistoryTasks,
	})
	if err != nil {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return nil, ctxErr
		}
		return nil, errors.New("qBittorrent 下载任务查询失败")
	}

	tasks := make([]Task, 0, len(torrents))
	for _, torrent := range torrents {
		if len(tasks) == maxDownloadHistoryTasks {
			break
		}
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		hash, hashErr := normalizeTorrentHash(torrent.Hash)
		if hashErr != nil {
			continue
		}
		label, _ := moviePilotTaskLabel(torrent.Tags)
		task := taskFromQBTorrent(torrent, hash, label)
		if torrent.CompletionOn > 0 {
			task.CompletedAt = time.Unix(torrent.CompletionOn, 0).UTC()
		}
		tasks = append(tasks, task)
	}
	return tasks, nil
}

func (q *QBittorrent) getTorrents(ctx context.Context, options qbittorrent.TorrentFilterOptions) ([]qbittorrent.Torrent, error) {
	if q == nil || q.client == nil || q.queryPermit == nil {
		return nil, errors.New("qBittorrent 查询不可用")
	}
	select {
	case q.queryPermit <- struct{}{}:
		defer func() { <-q.queryPermit }()
	case <-ctx.Done():
		return nil, ctx.Err()
	}
	return q.client.GetTorrentsCtx(ctx, options)
}

// torrentFilesRetryTimes 与 torrentFilesRetryInterval 复刻 V2 qB 模块
// `get_files(hash, retry=_TORRENT_FILES_RETRY_TIMES, interval=_TORRENT_FILES_RETRY_INTERVAL)`
// 的常量（app/modules/qbittorrent/__init__.py:40–41）。
const (
	torrentFilesRetryTimes    = 5
	torrentFilesRetryInterval = time.Second
)

// getFilesForSelection 复刻 V2 选集前的文件清单读取：元数据尚未就绪时最多重试 5 次、
// 每次间隔 1 秒；仍拿不到非空清单则返回最后一次错误或空清单。
func (q *QBittorrent) getFilesForSelection(ctx context.Context, hash string) (*qbittorrent.TorrentFiles, error) {
	interval := torrentFilesRetryInterval
	if q != nil && q.filesRetryInterval > 0 {
		interval = q.filesRetryInterval
	}
	var lastErr error
	for attempt := 0; attempt < torrentFilesRetryTimes; attempt++ {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		if attempt > 0 {
			timer := time.NewTimer(interval)
			select {
			case <-ctx.Done():
				timer.Stop()
				return nil, ctx.Err()
			case <-timer.C:
			}
		}
		files, err := q.getFilesInformation(ctx, hash)
		if err == nil && files != nil && len(*files) > 0 {
			return files, nil
		}
		lastErr = err
	}
	return nil, lastErr
}

func (q *QBittorrent) getFilesInformation(ctx context.Context, hash string) (*qbittorrent.TorrentFiles, error) {
	if ctx == nil || q == nil || q.client == nil || q.queryPermit == nil {
		return nil, errors.New("qBittorrent 文件查询不可用")
	}
	select {
	case q.queryPermit <- struct{}{}:
		defer func() { <-q.queryPermit }()
	case <-ctx.Done():
		return nil, ctx.Err()
	}
	return q.client.GetFilesInformationCtx(ctx, hash)
}

func moviePilotTaskLabel(tags string) (string, bool) {
	for _, tag := range strings.Split(tags, ",") {
		label := strings.TrimSpace(tag)
		if moviePilotTaskTagPattern.MatchString(label) {
			return label, true
		}
	}
	return "", false
}

func taskFromQBTorrent(torrent qbittorrent.Torrent, hash, label string) Task {
	return Task{
		Hash:               hash,
		Title:              torrent.Name,
		SizeBytes:          nonNegativeInt64(torrent.TotalSize),
		ProgressPercent:    progressPercent(torrent.Progress),
		State:              normalizeQBTorrentState(torrent.State, torrent.Progress),
		DownSpeedBytes:     nonNegativeInt64(torrent.DlSpeed),
		UpSpeedBytes:       nonNegativeInt64(torrent.UpSpeed),
		ETASeconds:         nonNegativeInt64(torrent.ETA),
		Label:              label,
		Tags:               torrent.Tags,
		SavePath:           torrent.SavePath,
		ContentPath:        qBContentPath(torrent.ContentPath, torrent.SavePath, torrent.Name),
		Category:           torrent.Category,
		DownloadLimitBytes: speedLimit(torrent.DlLimit),
		UploadLimitBytes:   speedLimit(torrent.UpLimit),
		RatioLimit:         shareRatioLimit(torrent.RatioLimit),
		SeedingTimeLimit:   shareTimeLimit(torrent.SeedingTimeLimit),
	}
}

func normalizeQBTorrentState(state qbittorrent.TorrentState, progress float64) string {
	if !math.IsNaN(progress) && progress >= 1 {
		return TaskStateCompleted
	}
	switch strings.ToLower(strings.TrimSpace(string(state))) {
	case "paused", "pauseddl", "pausedup", "stoppeddl", "stoppedup":
		return TaskStatePaused
	case "allocating", "checkingdl", "downloading", "forceddl", "metadl", "queueddl", "stalleddl":
		return TaskStateDownloading
	case "checkingup", "forcedup", "queuedup", "stalledup", "uploading":
		return TaskStateCompleted
	default:
		return TaskStateDownloading
	}
}

func observeQBTorrentState(state qbittorrent.TorrentState, progress float64) TaskObservationState {
	switch strings.ToLower(strings.TrimSpace(string(state))) {
	case "error":
		return TaskObservationError
	case "missingfiles":
		return TaskObservationMissing
	case "allocating", "checkingdl", "checkingresumedata", "downloading", "forceddl", "metadl", "moving", "queueddl", "stalleddl":
		if !math.IsNaN(progress) && progress >= 1 {
			return TaskObservationCompleted
		}
		return TaskObservationActive
	case "paused", "pauseddl", "stoppeddl":
		if !math.IsNaN(progress) && progress >= 1 {
			return TaskObservationCompleted
		}
		return TaskObservationPaused
	case "checkingup", "forcedup", "pausedup", "queuedup", "stalledup", "stoppedup", "uploading":
		return TaskObservationCompleted
	default:
		return TaskObservationUnknown
	}
}

func progressPercent(progress float64) float64 {
	if math.IsNaN(progress) || progress <= 0 {
		return 0
	}
	if progress >= 1 {
		return 100
	}
	return progress * 100
}

func nonNegativeInt64(value int64) int64 {
	if value < 0 {
		return 0
	}
	return value
}

func speedLimit(value int64) int64 {
	if value == -1 {
		return -1
	}
	return nonNegativeInt64(value)
}

func shareRatioLimit(value float64) float64 {
	if math.IsNaN(value) || math.IsInf(value, 0) {
		return 0
	}
	if value == -1 || value == -2 || value >= 0 {
		return value
	}
	return 0
}

func shareTimeLimit(value int64) int64 {
	if value == -1 || value == -2 || value >= 0 {
		return value
	}
	return 0
}

func qBContentPath(contentPath, savePath, name string) string {
	if contentPath != "" {
		return contentPath
	}
	if savePath == "" {
		return name
	}
	if name == "" {
		return savePath
	}
	if strings.HasSuffix(savePath, "/") || strings.HasSuffix(savePath, `\`) {
		return savePath + strings.TrimLeft(name, `/\`)
	}
	separator := "/"
	if strings.LastIndex(savePath, `\`) > strings.LastIndex(savePath, "/") {
		separator = `\`
	}
	return savePath + separator + strings.TrimLeft(name, `/\`)
}

// AddTorrentBytes 将有界的种子文件内容直接上传到 qBittorrent。
func (q *QBittorrent) AddTorrentBytes(ctx context.Context, metainfo []byte, savePath, label string, paused bool) error {
	if len(metainfo) == 0 {
		return errors.New("种子文件内容为空")
	}
	if len(metainfo) > maxTorrentMetainfoSize {
		return errors.New("种子文件超过大小上限")
	}
	options := (&qbittorrent.TorrentAddOptions{
		SavePath: savePath, Tags: label, Paused: paused, Stopped: paused,
	}).Prepare()
	response, err := q.client.AddTorrentFromMemoryCtx(ctx, metainfo, options)
	if err != nil {
		return errors.New("qBittorrent 添加种子文件失败")
	}
	if response != nil && response.FailureCount > 0 && response.SuccessCount == 0 && response.PendingCount == 0 {
		return fmt.Errorf("%w: qBittorrent 拒绝了下载任务", ErrDefinitiveAddFailure)
	}
	return nil
}

func normalizeTorrentHash(hash string) (string, error) {
	hash = strings.TrimSpace(hash)
	if len(hash) != 40 && len(hash) != 64 {
		return "", errors.New("种子 Hash 无效")
	}
	if _, err := hex.DecodeString(hash); err != nil {
		return "", errors.New("种子 Hash 无效")
	}
	return strings.ToUpper(hash), nil
}

// Add 使用确定性标签向 qBittorrent 添加 URL 或 Magnet。
func (q *QBittorrent) Add(ctx context.Context, candidate Candidate, label string) error {
	paused := candidate.Paused || len(candidate.TargetEpisodes) > 0
	options := (&qbittorrent.TorrentAddOptions{
		SavePath: candidate.SavePath, Tags: label, Paused: paused, Stopped: paused,
	}).Prepare()
	response, err := q.client.AddTorrentFromUrlCtx(ctx, candidate.DownloadURL, options)
	if err != nil {
		return errors.New("qBittorrent 添加下载任务失败")
	}
	if response != nil && response.FailureCount > 0 && response.SuccessCount == 0 && response.PendingCount == 0 {
		return fmt.Errorf("%w: qBittorrent 拒绝了下载任务", ErrDefinitiveAddFailure)
	}
	return nil
}
