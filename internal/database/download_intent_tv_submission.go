package database

import (
	"context"
	"encoding/json"
	"errors"
	"slices"
	"strings"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/moviepilot-go/moviepilot-go/internal/subscription"
)

const verifyTVBestVersionSubmissionSQL = `
	SELECT s.media_type, s.config, s.internal_state, s.v2_state,
		p.tv_priority, p.tv_coverage, success.episodes, success.media_type, p.tv_recovered_target
	FROM subscriptions AS s
	JOIN download_intents AS di
		ON di.subscription_id=s.id AND di.id=$3 AND di.username=$2
	JOIN download_intent_plans AS p ON p.intent_id=di.id
	JOIN download_intent_success_inputs AS success
		ON success.intent_id=di.id AND success.username=$2
		AND success.version=1 AND NOT success.legacy
	WHERE s.id=$1 AND s.username=$2 AND s.completed_at IS NULL
		AND s.current_download_intent_id=di.id
	FOR UPDATE OF s, di`

// VerifyTVBestVersionSubmissionTx 验证可独立于历史逐集行执行的 TV 下载事实。
// 包括洗版与总数补认计划；其余普通 TV 保留现有逐集绑定路径。
func (s *Store) VerifyTVBestVersionSubmissionTx(
	ctx context.Context, tx pgx.Tx, username string, subscriptionID int64,
	intentID uuid.UUID, episodes []subscription.Episode,
) (bool, error) {
	if ctx == nil || s == nil || tx == nil {
		return false, ErrSubscriptionEpisodePersistence
	}
	return verifyTVBestVersionSubmission(ctx, tx, username, subscriptionID, intentID, episodes, nil)
}

// VerifyTVBestVersionSubmissionReplayTx also verifies the post-claim parent
// state. P is replayable only for an original RSS P submission.
func (s *Store) VerifyTVBestVersionSubmissionReplayTx(
	ctx context.Context, tx pgx.Tx, username string, subscriptionID int64,
	intentID uuid.UUID, episodes []subscription.Episode, expectedState subscription.State,
) error {
	if ctx == nil || s == nil || tx == nil {
		return ErrSubscriptionEpisodePersistence
	}
	if expectedState != subscription.PendingSearch && expectedState != subscription.Active &&
		expectedState != subscription.PendingIdentification {
		return ErrInvalidSubscriptionEpisode
	}
	best, err := verifyTVBestVersionSubmission(ctx, tx, username, subscriptionID, intentID, episodes, &expectedState)
	if err != nil {
		return err
	}
	if !best {
		return ErrSubscriptionEpisodeBindingConflict
	}
	return nil
}

func verifyTVBestVersionSubmission(
	ctx context.Context, querier pgxQueryRower, username string, subscriptionID int64,
	intentID uuid.UUID, episodes []subscription.Episode, replayExpectedState *subscription.State,
) (bool, error) {
	username = strings.TrimSpace(username)
	if username == "" || subscriptionID <= 0 || intentID == uuid.Nil {
		return false, ErrInvalidSubscriptionEpisode
	}
	requested, err := normalizeTVSubmissionCoverage(episodes)
	if err != nil {
		return false, err
	}
	if err := ctx.Err(); err != nil {
		return false, err
	}
	var (
		mediaType, successMediaType, v2State             string
		configJSON, planJSON, successJSON, recoveredJSON []byte
		state                                            subscription.State
		priority                                         *int
	)
	err = querier.QueryRow(ctx, verifyTVBestVersionSubmissionSQL, subscriptionID, username, intentID).Scan(
		&mediaType, &configJSON, &state, &v2State, &priority, &planJSON, &successJSON, &successMediaType, &recoveredJSON,
	)
	if err != nil {
		if contextErr := ctx.Err(); contextErr != nil {
			return false, contextErr
		}
		if errors.Is(err, pgx.ErrNoRows) {
			return false, ErrSubscriptionEpisodeBindingConflict
		}
		return false, ErrSubscriptionEpisodePersistence
	}
	if !isDownloadIntentAcceptanceTV(mediaType) || !isDownloadIntentAcceptanceTV(successMediaType) ||
		priority == nil || planJSON == nil {
		return false, ErrSubscriptionEpisodeBindingConflict
	}
	var config map[string]any
	if err := json.Unmarshal(configJSON, &config); err != nil || config == nil {
		return false, ErrSubscriptionEpisodePersistence
	}
	best := downloadIntentAcceptanceBestVersion(config)
	var recovered *subscription.TVRecoveredTarget
	if recoveredJSON != nil {
		var recoveredErr error
		recovered, recoveredErr = decodeDownloadIntentTVRecoveredTarget(recoveredJSON)
		if recoveredErr != nil {
			return false, ErrSubscriptionEpisodePersistence
		}
		resolution, _, targetErr := downloadIntentAcceptanceTVTarget(nil, config)
		if targetErr != nil || best || resolution.Snapshot.TotalEpisode != 0 {
			return false, ErrSubscriptionEpisodeBindingConflict
		}
	}
	independent := best || recovered != nil
	if !independent && replayExpectedState == nil {
		// Ordinary TV retains its existing binding contract. Its download
		// metadata facts and full-season progress need not be the same set.
		return false, nil
	}
	var planCoverage, successCoverage []subscription.Episode
	if json.Unmarshal(planJSON, &planCoverage) != nil || planCoverage == nil ||
		json.Unmarshal(successJSON, &successCoverage) != nil || successCoverage == nil {
		return false, ErrSubscriptionEpisodePersistence
	}
	planCoverage, err = normalizeTVSubmissionCoverage(planCoverage)
	if err != nil {
		return false, ErrSubscriptionEpisodePersistence
	}
	successCoverage, err = normalizeTVSubmissionCoverage(successCoverage)
	if err != nil {
		return false, ErrSubscriptionEpisodePersistence
	}
	if !slices.Equal(planCoverage, successCoverage) {
		return false, ErrSubscriptionEpisodeBindingConflict
	}
	if recovered != nil {
		if !slices.Equal(requested, recovered.ExpectedCoverage) {
			return false, ErrSubscriptionEpisodeBindingConflict
		}
	} else {
		for _, episode := range requested {
			if !slices.Contains(planCoverage, episode) {
				return false, ErrSubscriptionEpisodeBindingConflict
			}
		}
	}
	projected, err := subscription.V2State(state)
	if err != nil || projected != v2State || state == subscription.Paused || state == subscription.Completed {
		return false, ErrSubscriptionEpisodeBindingConflict
	}
	if replayExpectedState != nil && (!independent || !tvSubmissionReplayState(state, *replayExpectedState)) {
		return false, ErrSubscriptionEpisodeBindingConflict
	}
	return independent, nil
}

func normalizeTVSubmissionCoverage(episodes []subscription.Episode) ([]subscription.Episode, error) {
	if len(episodes) == 0 {
		return []subscription.Episode{}, nil
	}
	_, normalized, err := normalizeSubscriptionEpisodeInput("tv-submission", 1, episodes)
	return normalized, err
}

func tvSubmissionReplayState(current, expected subscription.State) bool {
	if expected == subscription.PendingIdentification {
		return current == subscription.PendingIdentification
	}
	if expected != subscription.PendingSearch && expected != subscription.Active {
		return false
	}
	switch current {
	case subscription.Acquired, subscription.Downloading, subscription.PendingOrganization,
		subscription.Organizing, subscription.PendingIndex, subscription.Indexed:
		return true
	default:
		return false
	}
}
