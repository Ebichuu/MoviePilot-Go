package downloadflow

import (
	"context"

	"github.com/google/uuid"
	"github.com/moviepilot-go/moviepilot-go/internal/downloader"
)

// DownloadSearchFinalizationStore includes the outer search's N-to-R close in
// asynchronous acceptance. Serial candidate loops retain intermediate state.
type DownloadSearchFinalizationStore interface {
	AcceptDownloadIntentAndFinishSearch(context.Context, string, uuid.UUID, string) (downloader.Intent, bool, error)
}

type downloadAcceptanceMode uint8

const (
	downloadAcceptanceBatch downloadAcceptanceMode = iota
	downloadAcceptanceFinishSearch
)

func acceptDownloadIntent(
	ctx context.Context, store DownloadAcceptanceStore, mode downloadAcceptanceMode,
	username string, intentID uuid.UUID, torrentHash string,
) (downloader.Intent, bool, error) {
	if mode == downloadAcceptanceFinishSearch {
		if finalizer, ok := store.(DownloadSearchFinalizationStore); ok {
			return finalizer.AcceptDownloadIntentAndFinishSearch(ctx, username, intentID, torrentHash)
		}
	}
	return store.AcceptDownloadIntent(ctx, username, intentID, torrentHash)
}
