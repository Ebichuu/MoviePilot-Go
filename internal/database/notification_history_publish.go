package database

import (
	"context"
	"encoding/json"

	"github.com/moviepilot-go/moviepilot-go/internal/notificationhistory"
)

// PublishNotificationHistory inserts one already-published V2 business event.
func (s *Store) PublishNotificationHistory(
	ctx context.Context,
	plan notificationhistory.Plan,
) (bool, error) {
	if !plan.Valid() || plan.PublishedAt == nil {
		return false, notificationhistory.ErrInvalidPlan
	}
	if err := validateNotificationHistoryStore(s, ctx); err != nil {
		return false, err
	}
	note := plan.Note
	if len(note) == 0 {
		note = json.RawMessage(`{}`)
	}
	result, err := s.db.Pool.Exec(ctx, `
		INSERT INTO notification_events (
			batch_id, event_key, channel, source, mtype, title, text,
			image, link, userid, note, published_at
		)
		VALUES (NULL, $1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11)
		ON CONFLICT (event_key) DO NOTHING`,
		plan.EventKey, plan.Channel, plan.Source, plan.MType, plan.Title, plan.Text,
		plan.Image, plan.Link, plan.UserID, note, plan.PublishedAt.UTC(),
	)
	if err != nil {
		return false, notificationHistoryPersistenceError(ctx)
	}
	return result.RowsAffected() == 1, nil
}
