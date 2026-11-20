package downloader

import (
	"context"
	"errors"
	"fmt"
	"path"
	"regexp"
	"sort"
	"strconv"
	"strings"

	qbittorrent "github.com/autobrr/go-qbittorrent"

	"github.com/moviepilot-go/moviepilot-go/internal/media"
)

var compactEpisodeRangeRE = regexp.MustCompile(`(?i)\bS(\d{1,3})E(\d{1,4})\s*-\s*E?(\d{1,4})\b`)

// SelectEpisodeFiles pauses a torrent and disables non-target files when at
// least one target file matches. V2 resumes a valid nonempty list even without
// a match, leaving all file priorities unchanged in that case.
func (q *QBittorrent) SelectEpisodeFiles(
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
		return err
	}
	if len(normalizedEpisodes) == 0 {
		return nil
	}
	normalizedHash, err := normalizeTorrentHash(torrentHash)
	if err != nil {
		return ErrEpisodeSelectionFailed
	}
	if q == nil || q.client == nil || q.queryPermit == nil {
		return ErrEpisodeSelectionFailed
	}

	if err := q.pauseTorrent(ctx, normalizedHash); err != nil {
		return mapEpisodeSelectionDependencyError(ctx, err)
	}
	files, err := q.getFilesForSelection(ctx, normalizedHash)
	if err != nil {
		return mapEpisodeSelectionDependencyError(ctx, err)
	}
	if files == nil || len(*files) == 0 || len(*files) > maxTaskContentFiles {
		return ErrEpisodeSelectionFailed
	}

	targets := make(map[Episode]struct{}, len(normalizedEpisodes))
	for _, episode := range normalizedEpisodes {
		targets[episode] = struct{}{}
	}
	nonTargetIDs := make([]int, 0, len(*files))
	seenIDs := make(map[int]struct{}, len(*files))
	targetMatched := false
	for _, file := range *files {
		if err := ctx.Err(); err != nil {
			return err
		}
		if file.Index < 0 {
			return ErrEpisodeSelectionFailed
		}
		if _, duplicate := seenIDs[file.Index]; duplicate {
			return ErrEpisodeSelectionFailed
		}
		seenIDs[file.Index] = struct{}{}
		if torrentFileMatchesEpisodes(file.Name, targets) {
			targetMatched = true
			continue
		}
		nonTargetIDs = append(nonTargetIDs, file.Index)
	}
	sort.Ints(nonTargetIDs)
	if targetMatched && len(nonTargetIDs) > 0 {
		if err := q.setFilePriority(ctx, normalizedHash, nonTargetIDs, 0); err != nil {
			return mapEpisodeSelectionDependencyError(ctx, err)
		}
	}
	if err := q.resumeTorrent(ctx, normalizedHash); err != nil {
		return mapEpisodeSelectionDependencyError(ctx, err)
	}
	return nil
}

func torrentFileMatchesEpisodes(name string, targets map[Episode]struct{}) bool {
	name = strings.TrimSpace(strings.ReplaceAll(name, `\`, "/"))
	if name == "" || len(targets) == 0 {
		return false
	}
	parsed := media.Parse(expandCompactEpisodeRange(path.Base(name)), media.Options{})
	if parsed.Type != media.TypeTV {
		return false
	}
	episodeNumbers, err := parsed.EpisodeList()
	if err != nil || len(episodeNumbers) == 0 {
		return false
	}
	seasons := parsed.SeasonList()
	// media.Parse uses zero as the internal "season omitted" sentinel and
	// therefore defaults SeasonList to S01 for an explicit S00 token. V2 keeps
	// an explicit special season as season 0, so restore that distinction at
	// this boundary without changing the shared parser's legacy default.
	if parsed.BeginSeason == 0 && torrentFileHasExplicitSeasonZero(path.Base(name)) {
		seasons = []int{0}
	}
	if len(seasons) == 0 {
		seasons = []int{1}
	}
	if len(seasons) > maxTargetEpisodes/len(episodeNumbers) {
		return false
	}
	for _, season := range seasons {
		for _, episode := range episodeNumbers {
			coordinate := Episode{Season: season, Episode: episode}
			if _, ok := targets[coordinate]; !ok {
				return false
			}
		}
	}
	// Match the V2 selector: a multi-episode file is retained only when all
	// episodes represented by that file are requested.
	return true
}

func torrentFileHasExplicitSeasonZero(name string) bool {
	upper := strings.ToUpper(name)
	for index := 0; index+3 <= len(upper); index++ {
		if upper[index] != 'S' || upper[index+1] != '0' || upper[index+2] != '0' {
			continue
		}
		beforeOK := index == 0 || !isASCIIAlphaNumeric(upper[index-1])
		after := index + 3
		afterOK := after == len(upper) || !isASCIIAlphaNumeric(upper[after]) || upper[after] == 'E'
		if beforeOK && afterOK {
			return true
		}
	}
	return false
}

func isASCIIAlphaNumeric(value byte) bool {
	return value >= 'A' && value <= 'Z' || value >= '0' && value <= '9'
}

func expandCompactEpisodeRange(name string) string {
	return compactEpisodeRangeRE.ReplaceAllStringFunc(name, func(value string) string {
		match := compactEpisodeRangeRE.FindStringSubmatch(value)
		if len(match) != 4 {
			return value
		}
		season, seasonErr := strconv.Atoi(match[1])
		begin, beginErr := strconv.Atoi(match[2])
		end, endErr := strconv.Atoi(match[3])
		if seasonErr != nil || beginErr != nil || endErr != nil ||
			season < 0 || begin < 1 || end < begin {
			return value
		}
		return fmt.Sprintf("S%02dE%d-S%02dE%d", season, begin, season, end)
	})
}

// ResumeAddedTorrent mirrors V2 `app/modules/qbittorrent/__init__.py:275–278`:
// a freshly added torrent is force started only when the downloader has
// `force_resume` enabled; otherwise V2 performs no extra action at all.
// It is the optional capability behind the `force_resume` switch.
func (q *QBittorrent) ResumeAddedTorrent(ctx context.Context, torrentHash string) error {
	if q == nil || !q.forceResume {
		return nil
	}
	normalized, err := normalizeTorrentHash(torrentHash)
	if err != nil {
		return ErrEpisodeSelectionFailed
	}
	if err := q.forceStartTorrent(ctx, normalized); err != nil {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return ctxErr
		}
		// V2 `app/modules/qbittorrent/qbittorrent.py:354–365` 在底层客户端里捕获
		// setForceStart 的异常并记录，不把失败传播给下载链路：任务已经确认新增，
		// 强制开始失败只影响恢复动作本身。
		if q.logger != nil {
			q.logger.WarnContext(ctx, "强制开始新添加的种子失败", "hash", normalized, "error", err)
		}
	}
	return nil
}

func (q *QBittorrent) pauseTorrent(ctx context.Context, hash string) error {
	return q.withQueryPermit(ctx, func() error {
		return q.client.PauseCtx(ctx, []string{hash})
	})
}

// resumeTorrent mirrors V2's resume step: `force_resume` uses force start and
// otherwise the torrent is started normally.
func (q *QBittorrent) resumeTorrent(ctx context.Context, hash string) error {
	if q != nil && q.forceResume {
		return q.forceStartTorrent(ctx, hash)
	}
	return q.withQueryPermit(ctx, func() error {
		return q.client.ResumeCtx(ctx, []string{hash})
	})
}

func (q *QBittorrent) forceStartTorrent(ctx context.Context, hash string) error {
	return q.withQueryPermit(ctx, func() error {
		return q.client.SetForceStartCtx(ctx, []string{hash}, true)
	})
}

func (q *QBittorrent) setFilePriority(ctx context.Context, hash string, ids []int, priority int) error {
	if len(ids) == 0 {
		return nil
	}
	values := make([]string, 0, len(ids))
	for _, id := range ids {
		if id < 0 {
			return ErrEpisodeSelectionFailed
		}
		values = append(values, strconv.Itoa(id))
	}
	return q.withQueryPermit(ctx, func() error {
		return q.client.SetFilePriorityCtx(ctx, hash, strings.Join(values, "|"), priority)
	})
}

func (q *QBittorrent) withQueryPermit(ctx context.Context, fn func() error) error {
	if ctx == nil || q == nil || q.client == nil || q.queryPermit == nil {
		return ErrEpisodeSelectionFailed
	}
	select {
	case q.queryPermit <- struct{}{}:
		defer func() { <-q.queryPermit }()
	case <-ctx.Done():
		return ctx.Err()
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	return fn()
}

func mapEpisodeSelectionDependencyError(ctx context.Context, err error) error {
	if ctx != nil {
		if contextErr := ctx.Err(); contextErr != nil {
			return contextErr
		}
	}
	if errors.Is(err, qbittorrent.ErrTorrentMetadataNotDownloadedYet) ||
		strings.Contains(strings.ToLower(err.Error()), "status code: 409") {
		return ErrEpisodeSelectionPending
	}
	return ErrEpisodeSelectionFailed
}

var _ EpisodeFileSelector = (*QBittorrent)(nil)
