package downloadflow

import (
	"context"

	"github.com/google/uuid"

	"github.com/moviepilot-go/moviepilot-go/internal/downloader"
)

// DownloadFileSelectionStore exposes the immutable file-selection policy,
// independently of the episode coverage retained in the success input.
// Stores without this capability retain the legacy success-input behavior.
type DownloadFileSelectionStore interface {
	GetDownloadIntentFileSelection(
		ctx context.Context,
		username string,
		intentID uuid.UUID,
	) ([]downloader.Episode, error)
}

func loadDownloadFileSelection(
	ctx context.Context,
	store any,
	username string,
	intentID uuid.UUID,
	stableError error,
) ([]downloader.Episode, error) {
	var (
		episodes []downloader.Episode
		err      error
	)
	if selectionStore, supported := store.(DownloadFileSelectionStore); supported {
		episodes, err = selectionStore.GetDownloadIntentFileSelection(ctx, username, intentID)
	} else if inputStore, supported := store.(DownloadSuccessInputStore); supported {
		input, inputErr := inputStore.GetDownloadIntentSuccessInput(ctx, username, intentID)
		err = inputErr
		if err == nil {
			if input.IntentID != intentID || normalizeUsername(input.Username) != username {
				return nil, stableError
			}
			episodes = make([]downloader.Episode, 0, len(input.Episodes))
			for _, episode := range input.Episodes {
				episodes = append(episodes, downloader.Episode{
					Season: episode.Season, Episode: episode.Episode,
				})
			}
		}
	} else {
		return nil, nil
	}
	if contextErr := contextError(ctx, err); contextErr != nil {
		return nil, contextErr
	}
	if isDownloadSuccessInputNotFound(err) {
		return nil, nil
	}
	if err != nil {
		return nil, stableError
	}
	normalized, err := downloader.NormalizeTargetEpisodes(episodes)
	if err != nil {
		return nil, stableError
	}
	return normalized, nil
}
