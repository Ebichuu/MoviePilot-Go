package downloader

import (
	"context"
	"errors"
	"strings"
)

// FindByLabel 查询带指定标签的下载任务（rTorrent `d.custom1`）。
func (r *RTorrent) FindByLabel(ctx context.Context, label string) (string, bool, error) {
	label = strings.TrimSpace(label)
	if label == "" {
		return "", false, errors.New("rTorrent 标签无效")
	}
	rows, err := r.multicall(ctx)
	if err != nil {
		return "", false, err
	}
	for _, row := range rows {
		if strings.TrimSpace(row.Label) != label {
			continue
		}
		hash, hashErr := normalizeTorrentHash(row.Hash)
		if hashErr != nil {
			continue
		}
		return hash, true, nil
	}
	return "", false, nil
}

// FindByHash 按 Info Hash 查询下载任务。
func (r *RTorrent) FindByHash(ctx context.Context, hash string) (string, bool, error) {
	normalized, err := normalizeTorrentHash(hash)
	if err != nil {
		return "", false, err
	}
	rows, err := r.multicall(ctx)
	if err != nil {
		return "", false, err
	}
	for _, row := range rows {
		candidate, hashErr := normalizeTorrentHash(row.Hash)
		if hashErr == nil && candidate == normalized {
			return candidate, true, nil
		}
	}
	return "", false, nil
}

// AddTorrentBytes 通过 `d.load.raw[_start]` 提交内存种子并设置标签与目录。
func (r *RTorrent) AddTorrentBytes(
	ctx context.Context,
	metainfo []byte,
	savePath, label string,
	paused bool,
) error {
	if len(metainfo) == 0 {
		return errors.New("种子文件内容为空")
	}
	if len(metainfo) > maxTorrentMetainfoSize {
		return errors.New("种子文件超过大小上限")
	}
	method := "load.raw_start"
	if paused {
		method = "load.raw"
	}
	params := []string{"", xmlrpcEncodeBase64(metainfo)}
	if savePath = strings.TrimSpace(savePath); savePath != "" {
		params = append(params, `d.directory.set="`+savePath+`"`)
	}
	if label = strings.TrimSpace(label); label != "" {
		params = append(params, `d.custom1.set="`+label+`"`)
	}
	_, err := r.call(ctx, method, params)
	return err
}

// ObserveTasks 读取一批 Hash 的受限状态快照。
func (r *RTorrent) ObserveTasks(
	ctx context.Context,
	hashes []string,
) (map[string]TaskObservation, error) {
	if ctx == nil {
		return nil, errors.New("rTorrent 批量对账上下文无效")
	}
	if len(hashes) == 0 || len(hashes) > maxObservedTasks {
		return nil, errors.New("rTorrent 批量对账 Hash 数量无效")
	}
	observations := make(map[string]TaskObservation, len(hashes))
	for _, raw := range hashes {
		hash, err := normalizeTorrentHash(raw)
		if err != nil {
			return nil, err
		}
		observations[hash] = TaskObservation{Hash: hash, State: TaskObservationMissing}
	}
	rows, err := r.multicall(ctx)
	if err != nil {
		return nil, err
	}
	for _, row := range rows {
		hash, hashErr := normalizeTorrentHash(row.Hash)
		if hashErr != nil {
			continue
		}
		if _, tracked := observations[hash]; !tracked {
			continue
		}
		observations[hash] = TaskObservation{Hash: hash, State: observeRTorrentState(row)}
	}
	return observations, nil
}

// ListDownloading 返回有界的下载任务列表。
func (r *RTorrent) ListDownloading(ctx context.Context) ([]Task, error) {
	rows, err := r.multicall(ctx)
	if err != nil {
		return nil, err
	}
	tasks := make([]Task, 0, len(rows))
	for _, row := range rows {
		if len(tasks) >= maxDownloadingTasks {
			break
		}
		hash, hashErr := normalizeTorrentHash(row.Hash)
		if hashErr != nil {
			continue
		}
		label, _ := moviePilotTaskLabel(row.Label)
		tasks = append(tasks, Task{
			Hash: hash, Title: row.Name, SizeBytes: nonNegativeInt64(row.SizeBytes),
			ProgressPercent: rTorrentProgress(row), State: normalizeRTorrentTaskState(row),
			DownSpeedBytes: nonNegativeInt64(row.DownRate), UpSpeedBytes: nonNegativeInt64(row.UpRate),
			Label: label, Tags: row.Label, SavePath: row.Directory, ContentPath: row.Directory,
		})
	}
	return tasks, nil
}

func observeRTorrentState(row rTorrentTorrentRow) TaskObservationState {
	switch normalizeRTorrentTaskState(row) {
	case TaskStateCompleted:
		return TaskObservationCompleted
	case TaskStatePaused:
		return TaskObservationPaused
	default:
		return TaskObservationActive
	}
}

func normalizeRTorrentTaskState(row rTorrentTorrentRow) string {
	if row.Complete == 1 {
		return TaskStateCompleted
	}
	if row.State == 0 {
		return TaskStatePaused
	}
	return TaskStateDownloading
}

func rTorrentProgress(row rTorrentTorrentRow) float64 {
	if row.SizeBytes <= 0 {
		return 0
	}
	if row.BytesDone >= row.SizeBytes {
		return 100
	}
	return float64(row.BytesDone) / float64(row.SizeBytes) * 100
}

var _ TorrentBytesClient = (*RTorrent)(nil)
var _ DownloadingTaskLister = (*RTorrent)(nil)

// ListCompleted 返回已完成任务，供「下载历史」使用。rTorrent 的任务列表本身
// 就包含全部种子，这里只按归一化状态过滤。
func (r *RTorrent) ListCompleted(ctx context.Context) ([]Task, error) {
	tasks, err := r.ListDownloading(ctx)
	if err != nil {
		return nil, err
	}
	completed := make([]Task, 0, len(tasks))
	for _, task := range tasks {
		if task.State == TaskStateCompleted {
			completed = append(completed, task)
		}
	}
	return completed, nil
}
