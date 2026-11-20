package database

import (
	"context"
	"errors"
	"strings"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/moviepilot-go/moviepilot-go/internal/downloader"
)

const getDownloadIntentFileSelectionPlanSQL = `
	SELECT p.paused, p.tv_coverage IS NOT NULL
	FROM download_intent_plans AS p
	JOIN download_intents AS di ON di.id=p.intent_id
	WHERE p.intent_id=$1 AND di.username=$2`

// GetDownloadIntentFileSelection 按不可变计划返回文件选择目标，不改写成功快照的集数事实。
func (s *Store) GetDownloadIntentFileSelection(
	ctx context.Context,
	username string,
	intentID uuid.UUID,
) ([]downloader.Episode, error) {
	username, err := normalizeDownloadIntentSuccessInputOwner(username, intentID)
	if err != nil {
		return nil, err
	}
	if s == nil || s.db == nil || s.db.Pool == nil || ctx == nil {
		return nil, ErrDownloadIntentSuccessInputPersistence
	}
	return getDownloadIntentFileSelection(ctx, s.db.Pool, username, intentID)
}

func getDownloadIntentFileSelection(
	ctx context.Context,
	querier pgxQueryRower,
	username string,
	intentID uuid.UUID,
) ([]downloader.Episode, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	input, err := getDownloadIntentSuccessInput(ctx, querier, username, intentID)
	if err != nil {
		return nil, err
	}
	if input.IntentID != intentID || strings.TrimSpace(input.Username) != username {
		return nil, ErrDownloadIntentSuccessInputPersistence
	}

	var paused, knownTV bool
	if err := querier.QueryRow(ctx, getDownloadIntentFileSelectionPlanSQL, intentID, username).Scan(
		&paused, &knownTV,
	); err != nil {
		if contextErr := ctx.Err(); contextErr != nil {
			return nil, contextErr
		}
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, ErrDownloadIntentPlanNotFound
		}
		return nil, ErrDownloadIntentPlanPersistence
	}
	if knownTV && !paused {
		return nil, nil
	}

	episodes := make([]downloader.Episode, len(input.Episodes))
	for index, episode := range input.Episodes {
		episodes[index] = downloader.Episode{Season: episode.Season, Episode: episode.Episode}
	}
	return downloader.NormalizeTargetEpisodes(episodes)
}
