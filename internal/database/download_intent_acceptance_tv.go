package database

import (
	"context"
	"encoding/json"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/moviepilot-go/moviepilot-go/internal/subscription"
)

type downloadIntentTVFacts struct {
	priority     *int
	coverage     []subscription.Episode
	fullCoverage bool
	invalid      bool
	mediaSource  string
	mediaID      string
	mediaType    string
	mediaKnown   bool
	recovered    *subscription.TVRecoveredTarget
}

// V2's TV writer skips a download only when both sides have a comparable
// TMDB/Douban identity and it differs, or the captured download is not TV.
// Legacy plans without a captured media identity retain the missing-ID rule.
func downloadIntentTVWriterAcceptsIdentity(parent downloadIntentAcceptanceSubscription, tv downloadIntentTVFacts) bool {
	if !isDownloadIntentAcceptanceTV(parent.mediaType) || tv.mediaKnown && !isDownloadIntentAcceptanceTV(tv.mediaType) {
		return false
	}
	provider := downloadIntentTVWriterProvider(parent.mediaSource)
	if provider == "" || provider != downloadIntentTVWriterProvider(tv.mediaSource) {
		return true
	}
	parentID, downloadID := strings.TrimSpace(parent.mediaID), strings.TrimSpace(tv.mediaID)
	return parentID == "" || downloadID == "" || parentID == downloadID
}

func downloadIntentTVWriterProvider(source string) string {
	switch strings.ToLower(strings.TrimSpace(source)) {
	case "tmdb", "themoviedb":
		return "themoviedb"
	case "douban":
		return "douban"
	default:
		return ""
	}
}

// Malformed legacy data must not roll back an irreversible downloader fact.
// Parent facts remain attached to the current intent for a later repair.
func downloadIntentAcceptanceFactsReadable(parent downloadIntentAcceptanceSubscription, tv downloadIntentTVFacts, moviePriority *int) bool {
	if isDownloadIntentAcceptanceMovie(parent.mediaType) {
		_, err := downloadIntentAcceptanceMovieState(parent.config, moviePriority)
		return err == nil
	}
	if !isDownloadIntentAcceptanceTV(parent.mediaType) {
		return false
	}
	resolution, _, err := downloadIntentAcceptanceTVTarget(parent.season, parent.config)
	if err != nil {
		return false
	}
	if tv.coverage == nil || !downloadIntentTVWriterAcceptsIdentity(parent, tv) {
		return true
	}
	_, err = subscription.RecordTVDownloadPriority(resolution.Snapshot, tv.priority, tv.coverage, tv.fullCoverage)
	return err == nil
}

// All quality writers run before progress and completion, under the parent lock.
func buildDownloadIntentAcceptanceSettlement(
	ctx context.Context, tx pgx.Tx, subscriptionID int64,
	parent downloadIntentAcceptanceSubscription, consumed []subscription.Episode,
	moviePriority *int, tv downloadIntentTVFacts,
) (subscription.State, []byte, error) {
	if isDownloadIntentAcceptanceTV(parent.mediaType) && tv.recovered != nil && !downloadIntentAcceptanceBestVersion(parent.config) {
		resolution, _, err := downloadIntentAcceptanceTVTarget(parent.season, parent.config)
		if err != nil {
			return "", nil, err
		}
		if resolution.Snapshot.TotalEpisode == 0 {
			recovered, err := subscription.NormalizeTVRecoveredTarget(tv.recovered)
			if err != nil {
				return "", nil, ErrDownloadIntentPersistence
			}
			remaining := len(recovered.MissingEpisodes) - len(recovered.ExpectedCoverage)
			missing := 0
			if recovered.MissingEpisodes[0].Season == resolution.Snapshot.Season {
				missing = remaining
			}
			next := subscription.PendingSearch
			if remaining == 0 {
				next = subscription.Completed
			}
			patch, err := json.Marshal(map[string]any{"lack_episode": missing})
			return next, patch, err
		}
	}
	if isDownloadIntentAcceptanceTV(parent.mediaType) && downloadIntentAcceptanceBestVersion(parent.config) {
		return buildTVQualityAcceptancePatch(parent, consumed, 0, tv)
	}
	var coverageArgs [][]subscription.Episode
	if isDownloadIntentAcceptanceTV(parent.mediaType) && tv.coverage != nil {
		actual := tv.coverage
		if tv.recovered != nil {
			season := 1
			if parent.season != nil {
				season = *parent.season
			}
			actual = make([]subscription.Episode, 0, len(tv.recovered.ExpectedCoverage))
			for _, episode := range tv.recovered.ExpectedCoverage {
				if episode.Season == season {
					actual = append(actual, episode)
				}
			}
		}
		if len(actual) == 0 && tv.fullCoverage && tv.recovered == nil {
			resolution, _, err := downloadIntentAcceptanceTVTarget(parent.season, parent.config)
			if err != nil {
				return "", nil, err
			}
			actual = resolution.TargetEpisodes
		}
		coverageArgs = append(coverageArgs, actual)
	}
	consumed, missing, known, err := downloadIntentAcceptanceCurrentTargetProgress(ctx, tx, subscriptionID, parent, consumed, coverageArgs...)
	if err != nil {
		return "", nil, err
	}
	next, err := downloadIntentAcceptanceParentState(parent.mediaType, missing, known)
	if err != nil {
		return "", nil, err
	}
	if isDownloadIntentAcceptanceTV(parent.mediaType) {
		_, patch, err := buildTVQualityAcceptancePatch(parent, consumed, missing, tv)
		return next, patch, err
	}
	next, err = downloadIntentAcceptanceMovieState(parent.config, moviePriority)
	if err != nil {
		return "", nil, err
	}
	patch, err := buildDownloadIntentAcceptanceProgressPatch(parent.mediaType, parent.config, consumed, missing)
	if err != nil {
		return "", nil, err
	}
	patch, err = addDownloadIntentAcceptanceMoviePriority(parent.mediaType, patch, moviePriority)
	return next, patch, err
}

func decodeDownloadIntentTVRecoveredTarget(raw []byte) (*subscription.TVRecoveredTarget, error) {
	var target *subscription.TVRecoveredTarget
	if json.Unmarshal(raw, &target) != nil || target == nil {
		return nil, ErrDownloadIntentPersistence
	}
	normalized, err := subscription.NormalizeTVRecoveredTarget(target)
	if err != nil {
		return nil, ErrDownloadIntentPersistence
	}
	return normalized, nil
}

func isDownloadIntentAcceptanceTV(mediaType string) bool {
	mediaType = strings.ToLower(strings.TrimSpace(mediaType))
	return mediaType == "tv" || mediaType == "电视剧"
}

func buildTVQualityAcceptancePatch(
	parent downloadIntentAcceptanceSubscription, consumed []subscription.Episode,
	missing int64, tv downloadIntentTVFacts,
) (subscription.State, []byte, error) {
	resolution, legacy, err := downloadIntentAcceptanceTVTarget(parent.season, parent.config)
	if err != nil {
		return "", nil, err
	}
	snapshot := resolution.Snapshot
	writerAllowed := downloadIntentTVWriterAcceptsIdentity(parent, tv)
	if !snapshot.BestVersion && (legacy || resolution.Knowledge != subscription.TVTargetKnown || len(resolution.TargetEpisodes) == 0) {
		if !writerAllowed {
			if missing < 0 || missing > maxSubscriptionEpisodes {
				return "", nil, ErrDownloadIntentPersistence
			}
			patch, err := json.Marshal(map[string]any{"lack_episode": int(missing)})
			return subscription.PendingSearch, patch, err
		}
		patch, err := buildDownloadIntentAcceptanceProgressPatch(parent.mediaType, parent.config, consumed, missing)
		return subscription.PendingSearch, patch, err
	}
	actual := tv.coverage
	priority := tv.priority
	if actual == nil {
		// Old plans can contribute proven bound episodes to note, never quality.
		actual, priority = consumed, nil
	}
	written := snapshot
	writerHasCoverage := writerAllowed && downloadIntentTVWriterHasCoverage(resolution.TargetEpisodes, actual, tv.fullCoverage)
	if writerAllowed {
		written, err = subscription.RecordTVDownloadPriority(snapshot, priority, actual, tv.fullCoverage)
		if err != nil {
			return "", nil, ErrDownloadIntentPersistence
		}
	}
	patch := make(map[string]any)
	if writerHasCoverage {
		patch["note"] = written.Note
		if tv.coverage != nil && tv.priority != nil {
			patch["episode_priority"] = written.EpisodePriority
		}
	}
	next := subscription.PendingSearch
	if written.BestVersion {
		facts, err := subscription.DescribeTVBestVersion(written)
		if err != nil {
			return "", nil, ErrDownloadIntentPersistence
		}
		patch["lack_episode"] = len(resolution.TargetEpisodes) - len(facts.DownloadedEpisodes)
		if !written.BestVersionFull {
			patch["current_priority"] = facts.CurrentPriority
		} else if writerHasCoverage && written.CurrentPriority != nil {
			patch["current_priority"] = *written.CurrentPriority
		}
		next = subscription.Active
		if facts.Complete {
			next = subscription.Completed
		}
	} else {
		if missing < 0 || missing > maxSubscriptionEpisodes {
			return "", nil, ErrDownloadIntentPersistence
		}
		patch["lack_episode"] = int(missing)
		if missing == 0 {
			next = subscription.Completed
		}
	}
	encoded, err := json.Marshal(patch)
	if err != nil {
		return "", nil, ErrDownloadIntentPersistence
	}
	return next, encoded, nil
}

// V2 persists note and episode_priority only when a download covers a current
// target episode. The writer interprets file episodes as numbers in this season.
func downloadIntentTVWriterHasCoverage(targets, actual []subscription.Episode, fullCoverage bool) bool {
	if len(actual) == 0 {
		return fullCoverage && len(targets) > 0
	}
	allowed := make(map[int]struct{}, len(targets))
	for _, episode := range targets {
		allowed[episode.Episode] = struct{}{}
	}
	for _, episode := range actual {
		if _, exists := allowed[episode.Episode]; exists {
			return true
		}
	}
	return false
}
