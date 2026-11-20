package downloader

import (
	"context"
	"encoding/json"
)

const transmissionMaxSelectionFiles = 10_000

type transmissionFile struct {
	Name string `json:"name"`
}

type transmissionFilesArguments struct {
	Torrents []struct {
		Files []transmissionFile `json:"files"`
	} `json:"torrents"`
}

// SelectEpisodeFiles 实现逐集文件选择：命中目标集时禁用非目标文件，
// 随后启动任务；无命中时不修改任何优先级并直接启动（与 V2 qB 行为一致）。
func (t *Transmission) SelectEpisodeFiles(
	ctx context.Context,
	torrentHash string,
	episodes []Episode,
) error {
	if ctx == nil {
		return ErrEpisodeSelectionFailed
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	normalizedEpisodes, err := NormalizeTargetEpisodes(episodes)
	if err != nil {
		return ErrEpisodeSelectionFailed
	}
	if len(normalizedEpisodes) == 0 {
		return nil
	}
	normalizedHash, err := normalizeTorrentHash(torrentHash)
	if err != nil {
		return ErrEpisodeSelectionFailed
	}
	raw, err := t.call(ctx, "torrent-get", map[string]any{
		"ids": []string{normalizedHash}, "fields": []string{"files"},
	})
	if err != nil {
		if contextErr := ctx.Err(); contextErr != nil {
			return contextErr
		}
		return ErrEpisodeSelectionFailed
	}
	var arguments transmissionFilesArguments
	if err := json.Unmarshal(raw, &arguments); err != nil {
		return ErrEpisodeSelectionFailed
	}
	if len(arguments.Torrents) == 0 {
		// 任务或元数据尚未就绪，稍后可安全重试。
		return ErrEpisodeSelectionPending
	}
	files := arguments.Torrents[0].Files
	if len(files) == 0 || len(files) > transmissionMaxSelectionFiles {
		if len(files) > transmissionMaxSelectionFiles {
			return ErrEpisodeSelectionFailed
		}
		return ErrEpisodeSelectionPending
	}

	targets := make(map[Episode]struct{}, len(normalizedEpisodes))
	for _, episode := range normalizedEpisodes {
		targets[episode] = struct{}{}
	}
	nonTargetIDs := make([]int, 0, len(files))
	targetMatched := false
	for index, file := range files {
		if err := ctx.Err(); err != nil {
			return err
		}
		if torrentFileMatchesEpisodes(file.Name, targets) {
			targetMatched = true
			continue
		}
		nonTargetIDs = append(nonTargetIDs, index)
	}
	if targetMatched && len(nonTargetIDs) > 0 {
		if _, err := t.call(ctx, "torrent-set", map[string]any{
			"ids": []string{normalizedHash}, "files-unwanted": nonTargetIDs,
		}); err != nil {
			if contextErr := ctx.Err(); contextErr != nil {
				return contextErr
			}
			return ErrEpisodeSelectionFailed
		}
	}
	if _, err := t.call(ctx, "torrent-start", map[string]any{
		"ids": []string{normalizedHash},
	}); err != nil {
		if contextErr := ctx.Err(); contextErr != nil {
			return contextErr
		}
		return ErrEpisodeSelectionFailed
	}
	return nil
}

var _ EpisodeFileSelector = (*Transmission)(nil)
