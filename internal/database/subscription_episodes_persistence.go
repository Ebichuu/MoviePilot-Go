package database

import "context"

func subscriptionEpisodeStoreReady(store *Store) bool {
	return store != nil && store.db != nil && store.db.Pool != nil
}

func subscriptionEpisodePersistenceError(ctx context.Context) error {
	if ctx != nil {
		if err := ctx.Err(); err != nil {
			return err
		}
	}
	return ErrSubscriptionEpisodePersistence
}
