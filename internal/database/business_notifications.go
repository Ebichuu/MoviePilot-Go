package database

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/moviepilot-go/moviepilot-go/internal/notificationflow"
	"github.com/moviepilot-go/moviepilot-go/internal/organization"
)

// enqueueSubscribeBusinessNotification 写入订阅类业务通知（subscribeAdded /
// subscribeComplete）。与下载通知一样只登记事实，真实投递交给 notification_sweep，
// 因此这里必须与订阅状态变更在同一个事务里提交。
func enqueueSubscribeBusinessNotification(
	ctx context.Context,
	tx pgx.Tx,
	contentType string,
	username string,
	subscriptionID int64,
	contextValues map[string]any,
	route string,
) error {
	if contentType != "subscribeAdded" && contentType != "subscribeComplete" {
		return notificationflow.ErrInvalidPlan
	}
	if subscriptionID <= 0 || username == "" {
		return notificationflow.ErrInvalidPlan
	}
	encoded, err := json.Marshal(contextValues)
	if err != nil {
		return notificationflow.ErrPersistence
	}
	_, err = tx.Exec(ctx, `
		INSERT INTO business_notification_outbox (
			event_key, content_type, username, context, image, route, userid
		) VALUES ($1, $2, $3, $4::jsonb, '', $5, '')
		ON CONFLICT (content_type, event_key) DO NOTHING`,
		strconv.FormatInt(subscriptionID, 10), contentType, username, encoded, route)
	if err != nil {
		return notificationflow.ErrPersistence
	}
	return nil
}

// subscribeTitleYear 复刻 V2 的 title_year：标题带上年份显示，已含年份时不重复拼接。
func subscribeTitleYear(title, year string) string {
	title = strings.TrimSpace(title)
	year = strings.TrimSpace(year)
	if title == "" || year == "" || strings.Contains(title, year) {
		return title
	}
	return title + " (" + year + ")"
}

// subscribeSeasonFmt 复刻 V2 的 season_fmt：S01。
func subscribeSeasonFmt(season *int) string {
	if season == nil || *season <= 0 {
		return ""
	}
	return fmt.Sprintf("S%02d", *season)
}

// subscribeNotificationRoute 与 V2 一致：跳转到对应类型的「我的订阅」页签。
func subscribeNotificationRoute(mediaType string) string {
	if strings.EqualFold(strings.TrimSpace(mediaType), "movie") {
		return "#/subscribe/movie?tab=mysub"
	}
	return "#/subscribe/tv?tab=mysub"
}

func enqueueAcceptedDownloadNotification(ctx context.Context, tx pgx.Tx, username string, intentID uuid.UUID) error {
	input, err := scanDownloadIntentSuccessInput(tx.QueryRow(ctx, getDownloadIntentSuccessInputSQL, intentID, username))
	if errors.Is(err, pgx.ErrNoRows) {
		// Intents created before the success-input snapshot was introduced do
		// not contain enough frozen facts to reproduce the V2 downloadAdded
		// message. Keep the accepted download, but do not fabricate a sparse
		// business notification for it.
		return nil
	} else if err != nil {
		if ctx != nil && ctx.Err() != nil {
			return ctx.Err()
		}
		return ErrDownloadIntentSuccessInputPersistence
	}
	if input.Legacy || input.Username != username || input.IntentID != intentID {
		return nil
	}

	contextValues := downloadAddedTemplateContext(input)
	encoded, err := json.Marshal(contextValues)
	if err != nil {
		return notificationflow.ErrPersistence
	}
	_, err = tx.Exec(ctx, `
		INSERT INTO business_notification_outbox (
			event_key, content_type, username, context, image, route, userid
		) VALUES ($1, 'downloadAdded', $2, $3::jsonb, $4, $5, '')
		ON CONFLICT (content_type, event_key) DO NOTHING`,
		intentID.String(), username, encoded, organization.V2MessageImage(input.Media), "/#/downloading")
	if err != nil {
		return notificationflow.ErrPersistence
	}
	return nil
}

func downloadAddedTemplateContext(input organization.SuccessInput) map[string]any {
	contextValues := map[string]any{
		"title_year":        input.Media.TitleYear,
		"season_fmt":        "",
		"season_episode":    "",
		"download_episodes": "",
		"resource_term":     input.ResourceTerm,
		"username":          input.Username,
		"site_name":         "",
		"size":              "",
		"torrent_title":     "",
		"pubdate":           "",
		"freedate":          "",
		"seeders":           "",
		"volume_factor":     "",
		"hit_and_run":       "",
		"labels":            "",
		"description":       "",
	}
	seasons := make(map[int][]int)
	for _, episode := range input.Episodes {
		seasons[episode.Season] = append(seasons[episode.Season], episode.Episode)
	}
	if len(seasons) == 1 {
		for season, episodes := range seasons {
			contextValues["season_fmt"] = fmt.Sprintf("S%02d", season)
			contextValues["download_episodes"] = organization.FormatV2Episodes(episodes)
			contextValues["season_episode"] = organization.FormatV2SeasonEpisodes(season, episodes)
		}
	} else if len(seasons) > 1 {
		seasonNumbers := make([]int, 0, len(seasons))
		for season := range seasons {
			seasonNumbers = append(seasonNumbers, season)
		}
		sort.Ints(seasonNumbers)
		formatted := make([]string, 0, len(seasonNumbers))
		for _, season := range seasonNumbers {
			formatted = append(formatted, organization.FormatV2SeasonEpisodes(season, seasons[season]))
		}
		contextValues["season_episode"] = joinNotificationContext(formatted)
	}
	return contextValues
}

func joinNotificationContext(values []string) string {
	if len(values) == 0 {
		return ""
	}
	result := values[0]
	for _, value := range values[1:] {
		result += ", " + value
	}
	return result
}

// PublishNextBusinessNotification publishes at most one frozen business fact.
// History, per-channel delivery rows, and the outbox transition commit together.
func (s *Store) PublishNextBusinessNotification(
	ctx context.Context,
	planner notificationflow.BusinessPlanner,
	now time.Time,
) ([]uuid.UUID, error) {
	if planner == nil || now.IsZero() || ctx == nil || s == nil || s.db == nil || s.db.Pool == nil {
		return nil, notificationflow.ErrInvalidPlan
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	tx, err := s.db.Pool.Begin(ctx)
	if err != nil {
		return nil, notificationflow.ErrPersistence
	}
	defer func() { _ = tx.Rollback(context.Background()) }()

	var input notificationflow.BusinessInput
	var contextJSON []byte
	err = tx.QueryRow(ctx, `
		SELECT event_key, username, content_type, context, image, route, userid
		FROM business_notification_outbox
		WHERE published_at IS NULL
		ORDER BY id
		FOR UPDATE SKIP LOCKED
		LIMIT 1`).Scan(
		&input.EventKey, &input.Username, &input.ContentType, &contextJSON,
		&input.Image, &input.LinkRoute, &input.UserID,
	)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil || json.Unmarshal(contextJSON, &input.Context) != nil || input.Context == nil {
		return nil, notificationflow.ErrPersistence
	}

	plan, err := planner.PlanBusiness(input)
	if err != nil || !plan.Valid() || len(plan.Events) != 1 {
		return nil, notificationflow.ErrInvalidPlan
	}
	event := plan.Events[0]
	if err := validateBusinessNotificationPlan(input, event); err != nil {
		return nil, err
	}
	event.History.PublishedAt = timePointer(now.UTC())
	note := event.History.Note
	if len(note) == 0 {
		note = json.RawMessage(`{}`)
	}
	var eventID int64
	err = tx.QueryRow(ctx, `
		INSERT INTO notification_events (
			batch_id, event_key, channel, source, mtype, title, text,
			image, link, userid, note, published_at
		) VALUES (NULL,$1,$2,$3,$4,$5,$6,$7,$8,$9,$10::jsonb,$11)
		RETURNING id`,
		event.History.EventKey, event.History.Channel, event.History.Source,
		event.History.MType, event.History.Title, event.History.Text,
		event.History.Image, event.History.Link, event.History.UserID, note,
		event.History.PublishedAt,
	).Scan(&eventID)
	if err != nil || eventID <= 0 {
		return nil, notificationflow.ErrPersistence
	}
	due := make([]uuid.UUID, 0, len(event.Requests))
	for _, request := range event.Requests {
		payload, marshalErr := json.Marshal(request.Message)
		if marshalErr != nil {
			return nil, notificationflow.ErrInvalidPlan
		}
		if _, err := tx.Exec(ctx, `
			INSERT INTO notifications (
				id, batch_id, username, channel, source, event_id, idempotency_key,
				payload, state, last_error, next_attempt_at
			) VALUES ($1,NULL,$2,$3,$4,$5,$6,$7::jsonb,'pending','', $8)`,
			request.ID, plan.Username, request.Channel, request.Source, eventID,
			request.IdempotencyKey, payload, now.UTC()); err != nil {
			return nil, notificationflow.ErrPersistence
		}
		due = append(due, request.ID)
	}
	result, err := tx.Exec(ctx, `
		UPDATE business_notification_outbox
		SET event_id=$2, published_at=$3
		WHERE content_type=$1 AND event_key=$4 AND published_at IS NULL`,
		input.ContentType, eventID, now.UTC(), input.EventKey)
	if err != nil || result.RowsAffected() != 1 {
		return nil, notificationflow.ErrPersistence
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, notificationflow.ErrPersistence
	}
	return due, nil
}

func validateBusinessNotificationPlan(input notificationflow.BusinessInput, event notificationflow.BusinessEvent) error {
	wantPrefix := map[string]string{
		"downloadAdded":     "download-added:",
		"subscribeAdded":    "subscribe-added:",
		"subscribeComplete": "subscribe-complete:",
	}[string(input.ContentType)]
	if wantPrefix == "" || event.History.EventKey != wantPrefix+input.EventKey ||
		!event.History.Valid() || event.History.PublishedAt != nil ||
		len(event.Requests) > notificationflow.MaxRequestsPerEvent {
		return notificationflow.ErrInvalidPlan
	}
	return nil
}

func timePointer(value time.Time) *time.Time {
	return &value
}

var _ interface {
	PublishNextBusinessNotification(context.Context, notificationflow.BusinessPlanner, time.Time) ([]uuid.UUID, error)
} = (*Store)(nil)
