package downloader

import (
	"context"
	"errors"
	"os"
	"strings"

	qbittorrent "github.com/autobrr/go-qbittorrent"
)

// TaskControlAction 标识用户明确请求的任务控制操作。
type TaskControlAction string

const (
	TaskControlStart  TaskControlAction = "start"
	TaskControlStop   TaskControlAction = "stop"
	TaskControlDelete TaskControlAction = "delete"
)

// TaskController 查询指定 Hash 的任务并应用已支持的控制操作。
type TaskController interface {
	InspectControlTask(context.Context, string) (Task, bool, error)
	ControlTask(context.Context, string, TaskControlAction) error
}

var errInvalidControlTask = errors.New("下载任务控制参数无效")

func (q *QBittorrent) InspectControlTask(ctx context.Context, torrentHash string) (Task, bool, error) {
	if ctx == nil || q == nil || q.client == nil || q.queryPermit == nil {
		return Task{}, false, errors.New("qBittorrent 下载任务查询失败")
	}
	hash, err := normalizeTorrentHash(torrentHash)
	if err != nil {
		return Task{}, false, errInvalidControlTask
	}
	torrents, err := q.getTorrents(ctx, qbittorrent.TorrentFilterOptions{
		Filter: qbittorrent.TorrentFilterAll,
		Hashes: []string{hash},
	})
	if err != nil {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return Task{}, false, ctxErr
		}
		return Task{}, false, errors.New("qBittorrent 下载任务查询失败")
	}
	for _, torrent := range torrents {
		candidate, hashErr := normalizeTorrentHash(torrent.Hash)
		label, hasLabel := moviePilotTaskLabel(torrent.Tags)
		if hashErr != nil || candidate != hash {
			continue
		}
		if !hasLabel {
			label = ""
		}
		return taskFromQBTorrent(torrent, candidate, label), true, nil
	}
	return Task{}, false, nil
}

func (q *QBittorrent) ControlTask(ctx context.Context, torrentHash string, action TaskControlAction) error {
	if ctx == nil || q == nil || q.client == nil || q.queryPermit == nil {
		return errors.New("qBittorrent 下载任务控制失败")
	}
	hash, err := normalizeTorrentHash(torrentHash)
	if err != nil {
		return errInvalidControlTask
	}
	err = q.withQueryPermit(ctx, func() error {
		switch action {
		case TaskControlStart:
			// 原版手动开始直接调用 torrents_resume；force_resume 用于其他流程。
			return q.client.ResumeCtx(ctx, []string{hash})
		case TaskControlStop:
			return q.client.PauseCtx(ctx, []string{hash})
		case TaskControlDelete:
			return q.client.DeleteTorrentsCtx(ctx, []string{hash}, true)
		default:
			return errInvalidControlTask
		}
	})
	if err != nil {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return ctxErr
		}
		return errors.New("qBittorrent 下载任务控制失败")
	}
	return nil
}

func (t *Transmission) InspectControlTask(ctx context.Context, torrentHash string) (Task, bool, error) {
	hash, err := normalizeTorrentHash(torrentHash)
	if err != nil {
		return Task{}, false, errInvalidControlTask
	}
	torrents, err := t.torrentGet(ctx, []string{hash})
	if err != nil {
		return Task{}, false, errors.New("Transmission 下载任务查询失败")
	}
	for _, torrent := range torrents {
		candidate, hashErr := normalizeTorrentHash(torrent.HashString)
		label, hasLabel := moviePilotTaskLabel(strings.Join(torrent.Labels, ","))
		if hashErr != nil || candidate != hash {
			continue
		}
		if !hasLabel {
			label = ""
		}
		return Task{Hash: candidate, Label: label, Tags: strings.Join(torrent.Labels, ","), Title: torrent.Name,
			ProgressPercent: progressPercent(torrent.PercentDone), State: normalizeTransmissionTaskState(torrent.Status, torrent.PercentDone)}, true, nil
	}
	return Task{}, false, nil
}

func (t *Transmission) ControlTask(ctx context.Context, torrentHash string, action TaskControlAction) error {
	hash, err := normalizeTorrentHash(torrentHash)
	if err != nil {
		return errInvalidControlTask
	}
	var method string
	arguments := map[string]any{"ids": []string{hash}}
	switch action {
	case TaskControlStart:
		method = "torrent-start"
	case TaskControlStop:
		method = "torrent-stop"
	case TaskControlDelete:
		method = "torrent-remove"
		arguments["delete-local-data"] = true
	default:
		return errInvalidControlTask
	}
	if _, err := t.call(ctx, method, arguments); err != nil {
		return errors.New("Transmission 下载任务控制失败")
	}
	return nil
}

func (r *RTorrent) InspectControlTask(ctx context.Context, torrentHash string) (Task, bool, error) {
	hash, err := normalizeTorrentHash(torrentHash)
	if err != nil {
		return Task{}, false, errInvalidControlTask
	}
	rows, err := r.multicall(ctx)
	if err != nil {
		return Task{}, false, errors.New("rTorrent 下载任务查询失败")
	}
	for _, row := range rows {
		candidate, hashErr := normalizeTorrentHash(row.Hash)
		label, hasLabel := moviePilotTaskLabel(row.Label)
		if hashErr != nil || candidate != hash {
			continue
		}
		if !hasLabel {
			label = ""
		}
		return Task{Hash: candidate, Label: label, Tags: row.Label, Title: row.Name,
			SizeBytes: nonNegativeInt64(row.SizeBytes), ProgressPercent: rTorrentProgress(row),
			State: normalizeRTorrentTaskState(row)}, true, nil
	}
	return Task{}, false, nil
}

func (r *RTorrent) ControlTask(ctx context.Context, torrentHash string, action TaskControlAction) error {
	hash, err := normalizeTorrentHash(torrentHash)
	if err != nil {
		return errInvalidControlTask
	}
	if action == TaskControlDelete {
		// 原版 `delete_torrents`：先取 base_path，再 `d.erase`，最后在宿主上删除该路径；
		// 文件删除失败只告警，不改变“已移除任务”的结果。
		basePath := ""
		if value, pathErr := r.call(ctx, "d.base_path", []string{hash}); pathErr == nil {
			basePath = strings.TrimSpace(xmlrpcText(value))
		}
		if _, eraseErr := r.call(ctx, "d.erase", []string{hash}); eraseErr != nil {
			return errors.New("rTorrent 下载任务控制失败")
		}
		if basePath != "" {
			if info, statErr := os.Stat(basePath); statErr == nil {
				if info.IsDir() {
					_ = os.RemoveAll(basePath)
				} else {
					_ = os.Remove(basePath)
				}
			}
		}
		return nil
	}
	var method string
	switch action {
	case TaskControlStart:
		method = "d.start"
	case TaskControlStop:
		method = "d.stop"
	default:
		return errInvalidControlTask
	}
	if _, err := r.call(ctx, method, []string{hash}); err != nil {
		return errors.New("rTorrent 下载任务控制失败")
	}
	return nil
}

var _ TaskController = (*QBittorrent)(nil)
var _ TaskController = (*Transmission)(nil)
var _ TaskController = (*RTorrent)(nil)
