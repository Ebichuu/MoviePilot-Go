package database

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"math"
	"strconv"
	"strings"

	"github.com/jackc/pgx/v5"

	"github.com/moviepilot-go/moviepilot-go/internal/notificationhistory"
)

const notificationClearBeforeSetting = "NotificationClearBefore"

// ListNotificationHistory returns one globally visible V2 history page.
func (s *Store) ListNotificationHistory(
	ctx context.Context,
	page, count int,
) ([]notificationhistory.Event, error) {
	if err := notificationhistory.ValidatePage(page, count); err != nil {
		return nil, err
	}
	if err := validateNotificationHistoryStore(s, ctx); err != nil {
		return nil, err
	}
	clearBefore, err := s.LoadNotificationClearBefore(ctx)
	if err != nil {
		return nil, err
	}
	systemBoundary := max(clearBefore.All, clearBefore.System)
	mediaBoundary := max(clearBefore.All, clearBefore.Media)
	offset := int64(page-1) * int64(count)
	rows, err := s.db.Pool.Query(ctx, `
		SELECT id, event_key, channel, source, mtype, title, text, image,
			link, userid, note, published_at, created_at
		FROM notification_events
		WHERE published_at IS NOT NULL
		  AND (
			((image IS NULL OR image='') AND
			 ($1::bigint=0 OR published_at > to_timestamp($1::double precision / 1000.0)))
			OR
			(image IS NOT NULL AND image<>'' AND
			 ($2::bigint=0 OR published_at > to_timestamp($2::double precision / 1000.0)))
		  )
		ORDER BY published_at DESC, id DESC
		LIMIT $3 OFFSET $4`, systemBoundary, mediaBoundary, count, offset)
	if err != nil {
		return nil, notificationHistoryPersistenceError(ctx)
	}
	defer rows.Close()

	events := make([]notificationhistory.Event, 0, min(count, 100))
	for rows.Next() {
		event, scanErr := scanNotificationHistoryEvent(rows)
		if scanErr != nil {
			return nil, notificationHistoryPersistenceError(ctx)
		}
		events = append(events, event)
	}
	if err := rows.Err(); err != nil {
		return nil, notificationHistoryPersistenceError(ctx)
	}
	return events, nil
}

// LoadNotificationClearBefore reads the process-global V2 clear markers.
func (s *Store) LoadNotificationClearBefore(ctx context.Context) (notificationhistory.ClearBefore, error) {
	if err := validateNotificationHistoryStore(s, ctx); err != nil {
		return notificationhistory.ClearBefore{}, err
	}
	var raw []byte
	var encrypted bool
	err := s.db.Pool.QueryRow(ctx, `
		SELECT value, encrypted
		FROM settings
		WHERE key=$1`, notificationClearBeforeSetting).Scan(&raw, &encrypted)
	if errors.Is(err, pgx.ErrNoRows) {
		return notificationhistory.ClearBefore{}, nil
	}
	if err != nil || encrypted {
		return notificationhistory.ClearBefore{}, notificationHistoryPersistenceError(ctx)
	}
	return decodeNotificationClearBefore(raw)
}

// UpdateNotificationClearBefore atomically replaces one process-global V2 clear marker.
func (s *Store) UpdateNotificationClearBefore(
	ctx context.Context,
	scope notificationhistory.Scope,
	beforeMillis int64,
) (notificationhistory.ClearBefore, error) {
	if !scope.Valid() {
		return notificationhistory.ClearBefore{}, notificationhistory.ErrInvalidScope
	}
	if beforeMillis <= 0 {
		return notificationhistory.ClearBefore{}, notificationhistory.ErrInvalidClearBefore
	}
	if err := validateNotificationHistoryStore(s, ctx); err != nil {
		return notificationhistory.ClearBefore{}, err
	}
	var raw []byte
	err := s.db.Pool.QueryRow(ctx, `
		INSERT INTO settings (key, value, encrypted, updated_at)
		VALUES (
			$3,
			jsonb_set('{"all":0,"system":0,"media":0}'::jsonb,
				ARRAY[$1::text], to_jsonb($2::bigint), true),
			false,
			now()
		)
		ON CONFLICT (key) DO UPDATE
		SET value=jsonb_set(
			CASE
				WHEN settings.encrypted THEN '{"all":0,"system":0,"media":0}'::jsonb
				WHEN jsonb_typeof(settings.value)='object' THEN settings.value
				WHEN settings.value='null'::jsonb THEN '{"all":0,"system":0,"media":0}'::jsonb
				ELSE jsonb_build_object('all', settings.value, 'system', 0, 'media', 0)
			END,
			ARRAY[$1::text], to_jsonb($2::bigint), true
		), encrypted=false, updated_at=now()
		RETURNING value`, string(scope), beforeMillis, notificationClearBeforeSetting).Scan(&raw)
	if err != nil {
		return notificationhistory.ClearBefore{}, notificationHistoryPersistenceError(ctx)
	}
	return decodeNotificationClearBefore(raw)
}

func scanNotificationHistoryEvent(row interface{ Scan(...any) error }) (notificationhistory.Event, error) {
	var event notificationhistory.Event
	var note []byte
	if err := row.Scan(
		&event.ID, &event.EventKey, &event.Channel, &event.Source, &event.MType,
		&event.Title, &event.Text, &event.Image, &event.Link, &event.UserID,
		&note, &event.PublishedAt, &event.CreatedAt,
	); err != nil {
		return notificationhistory.Event{}, notificationhistory.ErrPersistence
	}
	event.Note = append(event.Note, note...)
	if !event.Valid() || event.PublishedAt == nil {
		return notificationhistory.Event{}, notificationhistory.ErrPersistence
	}
	return event, nil
}

func decodeNotificationClearBefore(raw []byte) (notificationhistory.ClearBefore, error) {
	if len(raw) == 0 {
		return notificationhistory.ClearBefore{}, nil
	}
	decoder := json.NewDecoder(strings.NewReader(string(raw)))
	decoder.UseNumber()
	var value any
	if err := decoder.Decode(&value); err != nil {
		return notificationhistory.ClearBefore{}, notificationhistory.ErrPersistence
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		return notificationhistory.ClearBefore{}, notificationhistory.ErrPersistence
	}

	var result notificationhistory.ClearBefore
	if fields, ok := value.(map[string]any); ok {
		result.All = normalizeNotificationClearTimestamp(fields["all"])
		result.System = normalizeNotificationClearTimestamp(fields["system"])
		result.Media = normalizeNotificationClearTimestamp(fields["media"])
	} else {
		result.All = normalizeNotificationClearTimestamp(value)
	}
	if !result.Valid() {
		return notificationhistory.ClearBefore{}, notificationhistory.ErrPersistence
	}
	return result, nil
}

func normalizeNotificationClearTimestamp(value any) int64 {
	var normalized int64
	switch typed := value.(type) {
	case json.Number:
		if integer, err := typed.Int64(); err == nil {
			normalized = integer
			break
		}
		decimal, err := typed.Float64()
		if err == nil && !math.IsInf(decimal, 0) && !math.IsNaN(decimal) && decimal <= math.MaxInt64 {
			normalized = int64(decimal)
		}
	case string:
		normalized, _ = strconv.ParseInt(strings.TrimSpace(typed), 10, 64)
	case bool:
		if typed {
			normalized = 1
		}
	case float64:
		if !math.IsInf(typed, 0) && !math.IsNaN(typed) && typed <= math.MaxInt64 {
			normalized = int64(typed)
		}
	case int64:
		normalized = typed
	}
	if normalized <= 0 {
		return 0
	}
	return normalized
}

func validateNotificationHistoryStore(s *Store, ctx context.Context) error {
	if ctx == nil || s == nil || s.db == nil || s.db.Pool == nil {
		return notificationhistory.ErrPersistence
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	return nil
}

func notificationHistoryPersistenceError(ctx context.Context) error {
	if ctx != nil && ctx.Err() != nil {
		return ctx.Err()
	}
	return notificationhistory.ErrPersistence
}
