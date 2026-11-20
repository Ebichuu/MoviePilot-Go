package downloader

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"strings"
)

const transmissionMaxObserve = 50

type transmissionTorrent struct {
	HashString  string   `json:"hashString"`
	Name        string   `json:"name"`
	Labels      []string `json:"labels"`
	Status      int      `json:"status"`
	PercentDone float64  `json:"percentDone"`
}

type transmissionTorrentGetArguments struct {
	Torrents []transmissionTorrent `json:"torrents"`
}

func (t *Transmission) torrentGet(ctx context.Context, ids []string) ([]transmissionTorrent, error) {
	arguments := map[string]any{
		"fields": []string{"hashString", "labels", "name", "status", "percentDone"},
	}
	if ids != nil {
		arguments["ids"] = ids
	}
	raw, err := t.call(ctx, "torrent-get", arguments)
	if err != nil {
		return nil, err
	}
	var decoded transmissionTorrentGetArguments
	if err := json.Unmarshal(raw, &decoded); err != nil {
		return nil, errors.New("Transmission 任务解析失败")
	}
	return decoded.Torrents, nil
}

// FindByLabel 查询带指定标签的下载任务（Transmission RPC 16+ 的 labels）。
func (t *Transmission) FindByLabel(ctx context.Context, label string) (string, bool, error) {
	label = strings.TrimSpace(label)
	if label == "" {
		return "", false, errors.New("Transmission 标签无效")
	}
	torrents, err := t.torrentGet(ctx, nil)
	if err != nil {
		return "", false, err
	}
	for _, torrent := range torrents {
		for _, candidate := range torrent.Labels {
			if strings.TrimSpace(candidate) != label {
				continue
			}
			hash, hashErr := normalizeTorrentHash(torrent.HashString)
			if hashErr != nil {
				continue
			}
			return hash, true, nil
		}
	}
	return "", false, nil
}

// FindByHash 按 Info Hash 查询下载任务。
func (t *Transmission) FindByHash(ctx context.Context, hash string) (string, bool, error) {
	normalized, err := normalizeTorrentHash(hash)
	if err != nil {
		return "", false, err
	}
	torrents, err := t.torrentGet(ctx, []string{normalized})
	if err != nil {
		return "", false, err
	}
	for _, torrent := range torrents {
		candidate, hashErr := normalizeTorrentHash(torrent.HashString)
		if hashErr == nil && candidate == normalized {
			return candidate, true, nil
		}
	}
	return "", false, nil
}

// AddTorrentBytes 把内存种子内容提交给 Transmission。
func (t *Transmission) AddTorrentBytes(
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
	arguments := map[string]any{
		"metainfo": base64.StdEncoding.EncodeToString(metainfo),
		"paused":   paused,
	}
	if savePath = strings.TrimSpace(savePath); savePath != "" {
		arguments["download-dir"] = savePath
	}
	if label = strings.TrimSpace(label); label != "" {
		arguments["labels"] = []string{label}
	}
	_, err := t.call(ctx, "torrent-add", arguments)
	return err
}

// ObserveTasks 读取一批 Hash 的受限状态快照。
func (t *Transmission) ObserveTasks(
	ctx context.Context,
	hashes []string,
) (map[string]TaskObservation, error) {
	if ctx == nil {
		return nil, errors.New("Transmission 批量对账上下文无效")
	}
	if len(hashes) == 0 || len(hashes) > transmissionMaxObserve {
		return nil, errors.New("Transmission 批量对账 Hash 数量无效")
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
	torrents, err := t.torrentGet(ctx, normalized)
	if err != nil {
		return nil, err
	}
	for _, torrent := range torrents {
		hash, hashErr := normalizeTorrentHash(torrent.HashString)
		if hashErr != nil {
			continue
		}
		if _, tracked := observations[hash]; !tracked {
			continue
		}
		observations[hash] = TaskObservation{
			Hash: hash, State: observeTransmissionState(torrent.Status, torrent.PercentDone),
		}
	}
	return observations, nil
}

// observeTransmissionState 映射 Transmission 状态码：
// 0 停止、1/2 校验、3/4 下载/等待下载、5/6 做种/等待做种。
func observeTransmissionState(status int, percentDone float64) TaskObservationState {
	switch status {
	case 0:
		return TaskObservationPaused
	case 3, 4:
		if percentDone >= 1 {
			return TaskObservationCompleted
		}
		return TaskObservationActive
	case 5, 6:
		return TaskObservationCompleted
	default:
		return TaskObservationUnknown
	}
}

type transmissionTorrentDetail struct {
	HashString   string   `json:"hashString"`
	Name         string   `json:"name"`
	Labels       []string `json:"labels"`
	Status       int      `json:"status"`
	PercentDone  float64  `json:"percentDone"`
	TotalSize    int64    `json:"totalSize"`
	RateDownload int64    `json:"rateDownload"`
	RateUpload   int64    `json:"rateUpload"`
	ETA          int64    `json:"eta"`
	DownloadDir  string   `json:"downloadDir"`
}

type transmissionTorrentDetailArguments struct {
	Torrents []transmissionTorrentDetail `json:"torrents"`
}

// ListDownloading 返回有界的下载任务列表（V2 下载器任务投影）。
func (t *Transmission) ListDownloading(ctx context.Context) ([]Task, error) {
	raw, err := t.call(ctx, "torrent-get", map[string]any{
		"fields": []string{
			"id", "name", "status", "labels", "hashString", "totalSize", "percentDone", "addedDate",
			"leftUntilDone", "rateDownload", "rateUpload", "recheckProgress",
			"peersGettingFromUs", "peersSendingToUs", "uploadRatio", "uploadedEver", "downloadedEver", "downloadDir",
		},
	})
	if err != nil {
		return nil, err
	}
	var decoded transmissionTorrentDetailArguments
	if err := json.Unmarshal(raw, &decoded); err != nil {
		return nil, errors.New("Transmission 任务解析失败")
	}
	tasks := make([]Task, 0, len(decoded.Torrents))
	for _, torrent := range decoded.Torrents {
		if len(tasks) >= maxDownloadingTasks {
			break
		}
		hash, hashErr := normalizeTorrentHash(torrent.HashString)
		if hashErr != nil {
			continue
		}
		tags := strings.Join(torrent.Labels, ",")
		label, _ := moviePilotTaskLabel(tags)
		tasks = append(tasks, Task{
			Hash: hash, Title: torrent.Name, SizeBytes: nonNegativeInt64(torrent.TotalSize),
			ProgressPercent: progressPercent(torrent.PercentDone),
			State:           normalizeTransmissionTaskState(torrent.Status, torrent.PercentDone),
			DownSpeedBytes:  nonNegativeInt64(torrent.RateDownload),
			UpSpeedBytes:    nonNegativeInt64(torrent.RateUpload),
			ETASeconds:      nonNegativeInt64(torrent.ETA),
			Label:           label, Tags: tags,
			SavePath: torrent.DownloadDir, ContentPath: torrent.DownloadDir,
		})
	}
	return tasks, nil
}

func normalizeTransmissionTaskState(status int, percentDone float64) string {
	switch status {
	case 0:
		return TaskStatePaused
	case 5, 6:
		return TaskStateCompleted
	case 3, 4:
		if percentDone >= 1 {
			return TaskStateCompleted
		}
		return TaskStateDownloading
	default:
		return TaskStateDownloading
	}
}

var _ TorrentBytesClient = (*Transmission)(nil)
var _ DownloadingTaskLister = (*Transmission)(nil)

// ListCompleted 返回已完成任务，供「下载历史」使用。Transmission 的任务列表本身
// 就包含全部种子，这里只按归一化状态过滤。
func (t *Transmission) ListCompleted(ctx context.Context) ([]Task, error) {
	tasks, err := t.ListDownloading(ctx)
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
