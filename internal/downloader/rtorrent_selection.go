package downloader

import (
	"context"
	"strconv"
)

const rTorrentMaxSelectionFiles = 10_000

// SelectEpisodeFiles 实现 rTorrent 逐集文件选择：命中目标集时把非目标文件
// 优先级设为 0（`f.priority.set`），随后启动任务；无命中不改优先级。
func (r *RTorrent) SelectEpisodeFiles(
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
	value, err := r.call(ctx, "f.multicall", []string{normalizedHash, "", "f.path="})
	if err != nil {
		if contextErr := ctx.Err(); contextErr != nil {
			return contextErr
		}
		return ErrEpisodeSelectionFailed
	}
	if len(value.Array) == 0 {
		// 文件清单尚未就绪，稍后可安全重试。
		return ErrEpisodeSelectionPending
	}
	if len(value.Array) > rTorrentMaxSelectionFiles {
		return ErrEpisodeSelectionFailed
	}

	targets := make(map[Episode]struct{}, len(normalizedEpisodes))
	for _, episode := range normalizedEpisodes {
		targets[episode] = struct{}{}
	}
	nonTargetIDs := make([]int, 0, len(value.Array))
	targetMatched := false
	for index, entry := range value.Array {
		if err := ctx.Err(); err != nil {
			return err
		}
		filePath := ""
		if len(entry.Array) > 0 {
			filePath = xmlrpcText(entry.Array[0])
		}
		if torrentFileMatchesEpisodes(filePath, targets) {
			targetMatched = true
			continue
		}
		nonTargetIDs = append(nonTargetIDs, index)
	}
	if targetMatched {
		for _, index := range nonTargetIDs {
			if _, err := r.call(
				ctx,
				"f.priority.set",
				[]string{normalizedHash + ":f" + strconv.Itoa(index), "0"},
			); err != nil {
				if contextErr := ctx.Err(); contextErr != nil {
					return contextErr
				}
				return ErrEpisodeSelectionFailed
			}
		}
		if _, err := r.call(ctx, "d.update_priorities", []string{normalizedHash}); err != nil {
			if contextErr := ctx.Err(); contextErr != nil {
				return contextErr
			}
			return ErrEpisodeSelectionFailed
		}
	}
	if _, err := r.call(ctx, "d.start", []string{normalizedHash}); err != nil {
		if contextErr := ctx.Err(); contextErr != nil {
			return contextErr
		}
		return ErrEpisodeSelectionFailed
	}
	return nil
}

var _ EpisodeFileSelector = (*RTorrent)(nil)
