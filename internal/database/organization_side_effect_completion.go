package database

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/moviepilot-go/moviepilot-go/internal/mediaindex"
	"github.com/moviepilot-go/moviepilot-go/internal/notificationflow"
	"github.com/moviepilot-go/moviepilot-go/internal/scrapeflow"
)

// CompleteOrganizationBatchWithSideEffects atomically completes one batch,
// persists its NFO and notification intents, and optionally creates the
// required media-server confirmation request.
func (s *Store) CompleteOrganizationBatchWithSideEffects(
	ctx context.Context,
	batchID uuid.UUID,
	token uuid.UUID,
	indexPlan mediaindex.CompletionPlan,
	scrapePlan scrapeflow.Plan,
	notificationPlan notificationflow.Plan,
) (uuid.UUID, bool, []uuid.UUID, []uuid.UUID, error) {
	if batchID == uuid.Nil || token == uuid.Nil {
		return uuid.Nil, false, nil, nil, ErrInvalidOrganizationInput
	}
	if err := validateOrganizationStore(s, ctx); err != nil {
		return uuid.Nil, false, nil, nil, err
	}

	var (
		normalizedIndex mediaindex.CompletionPlan
		serverID        uuid.UUID
		err             error
	)
	if indexPlan.RequiredServer != nil {
		normalizedIndex, serverID, err = normalizeMediaIndexCompletionPlan(indexPlan)
		if err != nil {
			return uuid.Nil, false, nil, nil, err
		}
	}

	tx, err := s.db.Pool.Begin(ctx)
	if err != nil {
		return uuid.Nil, false, nil, nil, organizationPersistenceError(ctx)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	var (
		batchState  OrganizationBatchState
		intentID    uuid.UUID
		storedToken *uuid.UUID
		leaseEnd    *time.Time
		leaseValid  bool
	)
	err = tx.QueryRow(ctx, `
		SELECT state, intent_id, lease_token, lease_expires_at,
			COALESCE(lease_expires_at > now(), FALSE)
		FROM organization_batches
		WHERE id=$1
		FOR UPDATE`, batchID).Scan(&batchState, &intentID, &storedToken, &leaseEnd, &leaseValid)
	if errors.Is(err, pgx.ErrNoRows) {
		return uuid.Nil, false, nil, nil, ErrOrganizationBatchNotFound
	}
	if err != nil {
		return uuid.Nil, false, nil, nil, organizationPersistenceError(ctx)
	}

	var username string
	var subscriptionID *int64
	if err := tx.QueryRow(ctx, `
		SELECT username, subscription_id
		FROM download_intents
		WHERE id=$1`, intentID).Scan(&username, &subscriptionID); err != nil {
		return uuid.Nil, false, nil, nil, organizationPersistenceError(ctx)
	}
	if err := validateOrganizationSideEffectPlans(
		batchID, intentID, username, scrapePlan, notificationPlan,
	); err != nil {
		return uuid.Nil, false, nil, nil, err
	}

	requestID := uuid.Nil
	if indexPlan.RequiredServer != nil {
		requestID = mediaIndexRequestID(batchID, serverID)
	}
	if batchState == OrganizationBatchCompleted {
		if err := verifyCompletedMediaIndexPlan(
			ctx, tx, batchID, requestID, serverID, normalizedIndex,
		); err != nil {
			return uuid.Nil, false, nil, nil, err
		}
		if err := verifyOrganizationSideEffectPlans(ctx, tx, batchID, scrapePlan, notificationPlan); err != nil {
			return uuid.Nil, false, nil, nil, err
		}
		if err := tx.Commit(ctx); err != nil {
			return uuid.Nil, false, nil, nil, organizationPersistenceError(ctx)
		}
		return requestID, false, nil, nil, nil
	}
	if batchState != OrganizationBatchOrganizing || storedToken == nil || *storedToken != token ||
		leaseEnd == nil || !leaseValid {
		return uuid.Nil, false, nil, nil, ErrOrganizationLeaseConflict
	}

	organizationFiles, err := loadCompletedOrganizationFilesForIndex(ctx, tx, batchID)
	if err != nil {
		return uuid.Nil, false, nil, nil, err
	}
	if requestID != uuid.Nil {
		if err := matchMediaIndexFiles(organizationFiles, normalizedIndex.Files); err != nil {
			return uuid.Nil, false, nil, nil, err
		}
	}

	scrapeIDs, err := insertScrapeRequests(ctx, tx, scrapePlan)
	if err != nil {
		return uuid.Nil, false, nil, nil, err
	}
	notificationIDs, err := insertNotificationEvents(ctx, tx, notificationPlan)
	if err != nil {
		return uuid.Nil, false, nil, nil, err
	}

	mediaCreated := false
	if requestID != uuid.Nil {
		if err := insertMediaIndexRequest(ctx, tx, requestID, batchID, serverID, normalizedIndex); err != nil {
			return uuid.Nil, false, nil, nil, err
		}
		mediaCreated = true
	}
	if subscriptionID != nil {
		if err := completeOrganizationSubscription(ctx, tx, *subscriptionID, intentID); err != nil {
			return uuid.Nil, false, nil, nil, err
		}
	}

	result, err := tx.Exec(ctx, `
		UPDATE organization_batches
		SET state='completed', last_error='', completed_at=COALESCE(completed_at, now()),
			lease_token=NULL, lease_expires_at=NULL, updated_at=now()
		WHERE id=$1 AND state='organizing' AND lease_token=$2
		  AND lease_expires_at > now()`, batchID, token)
	if err != nil {
		return uuid.Nil, false, nil, nil, organizationPersistenceError(ctx)
	}
	if result.RowsAffected() != 1 {
		return uuid.Nil, false, nil, nil, ErrOrganizationLeaseConflict
	}
	if err := tx.Commit(ctx); err != nil {
		return uuid.Nil, false, nil, nil, organizationPersistenceError(ctx)
	}
	return requestID, mediaCreated, scrapeIDs, notificationIDs, nil
}

func verifyCompletedMediaIndexPlan(
	ctx context.Context,
	tx pgx.Tx,
	batchID uuid.UUID,
	requestID uuid.UUID,
	serverID uuid.UUID,
	plan mediaindex.CompletionPlan,
) error {
	var count int
	if err := tx.QueryRow(ctx, `SELECT count(*) FROM media_index_requests WHERE batch_id=$1`, batchID).Scan(&count); err != nil {
		return organizationPersistenceError(ctx)
	}
	if requestID == uuid.Nil {
		if count != 0 {
			return ErrOrganizationStateConflict
		}
		return nil
	}
	if count != 1 {
		return ErrOrganizationStateConflict
	}
	return verifyMediaIndexRequestPlan(ctx, tx, requestID, batchID, serverID, plan)
}

func validateOrganizationSideEffectPlans(
	batchID uuid.UUID,
	intentID uuid.UUID,
	username string,
	scrapePlan scrapeflow.Plan,
	notificationPlan notificationflow.Plan,
) error {
	if scrapePlan.BatchID != uuid.Nil && (!scrapePlan.Valid() || scrapePlan.BatchID != batchID ||
		scrapePlan.IntentID != intentID || scrapePlan.Username != username) {
		return ErrOrganizationStateConflict
	}
	if notificationPlan.BatchID != uuid.Nil && (!notificationPlan.Valid() ||
		notificationPlan.BatchID != batchID || notificationPlan.Username != username) {
		return ErrOrganizationStateConflict
	}
	return nil
}

func insertScrapeRequests(ctx context.Context, tx pgx.Tx, plan scrapeflow.Plan) ([]uuid.UUID, error) {
	if plan.BatchID == uuid.Nil {
		return nil, nil
	}
	due := make([]uuid.UUID, 0, len(plan.Requests))
	for _, request := range plan.Requests {
		state := scrapeflow.StatePending
		var completedAt *time.Time
		if request.SkipReason != "" {
			state = scrapeflow.StateSkipped
			now := time.Now().UTC()
			completedAt = &now
		} else {
			due = append(due, request.ID)
		}
		if _, err := tx.Exec(ctx, `
			INSERT INTO scrape_requests (
				id, batch_id, intent_id, username, kind, media_path, tmdb_id,
				season, episode, skip_reason, state, next_attempt_at,
				last_error, completed_at
			) VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,now(),'',$12)`,
			request.ID, plan.BatchID, plan.IntentID, plan.Username, request.Kind,
			request.MediaPath, request.TMDBID, request.Season, request.Episode,
			request.SkipReason, state, completedAt); err != nil {
			return nil, organizationPersistenceError(ctx)
		}
	}
	return due, nil
}

func insertNotificationEvents(
	ctx context.Context,
	tx pgx.Tx,
	plan notificationflow.Plan,
) ([]uuid.UUID, error) {
	if plan.BatchID == uuid.Nil {
		return nil, nil
	}
	due := make([]uuid.UUID, 0)
	for _, eventPlan := range plan.Events {
		eventID, err := insertNotificationEvent(ctx, tx, plan.BatchID, eventPlan)
		if err != nil {
			return nil, err
		}
		requestIDs, err := insertNotificationRequests(
			ctx, tx, plan.BatchID, plan.Username, eventPlan, eventID,
		)
		if err != nil {
			return nil, err
		}
		due = append(due, requestIDs...)
	}
	return due, nil
}

func insertNotificationEvent(
	ctx context.Context,
	tx pgx.Tx,
	batchID uuid.UUID,
	plan notificationflow.EventPlan,
) (int64, error) {
	event := plan.Event.Clone()
	if !event.Valid() {
		return 0, ErrOrganizationStateConflict
	}
	if event.PublishedAt == nil {
		now := time.Now().UTC()
		event.PublishedAt = &now
	}
	note := event.Note
	if len(note) == 0 {
		note = []byte(`{}`)
	}
	var eventID int64
	err := tx.QueryRow(ctx, `
		INSERT INTO notification_events (
			batch_id, event_key, channel, source, mtype, title, text, image,
			link, userid, note, published_at
		) VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11::jsonb,$12)
		ON CONFLICT (event_key) DO UPDATE
		SET event_key=notification_events.event_key
		RETURNING id`,
		batchID, event.EventKey, event.Channel, event.Source, event.MType,
		event.Title, event.Text, event.Image, event.Link, event.UserID, note,
		event.PublishedAt,
	).Scan(&eventID)
	if err != nil || eventID <= 0 {
		return 0, organizationPersistenceError(ctx)
	}
	if err := verifyNotificationEvent(ctx, tx, batchID, plan); err != nil {
		return 0, err
	}
	return eventID, nil
}

func insertNotificationRequests(
	ctx context.Context,
	tx pgx.Tx,
	batchID uuid.UUID,
	username string,
	plan notificationflow.EventPlan,
	eventID int64,
) ([]uuid.UUID, error) {
	due := make([]uuid.UUID, 0, len(plan.Requests))
	for _, request := range plan.Requests {
		payload, err := json.Marshal(request.Message)
		if err != nil {
			return nil, ErrOrganizationStateConflict
		}
		if _, err := tx.Exec(ctx, `
			INSERT INTO notifications (
				id, batch_id, username, channel, source, event_id, idempotency_key, payload,
				state, last_error, next_attempt_at
			) VALUES ($1,$2,$3,$4,$5,NULLIF($6,0),$7,$8::jsonb,$9,'',now())`,
			request.ID, batchID, username, request.Channel, request.Source,
			eventID, request.IdempotencyKey, payload, notificationflow.StatePending); err != nil {
			return nil, organizationPersistenceError(ctx)
		}
		due = append(due, request.ID)
	}
	return due, nil
}

func verifyOrganizationSideEffectPlans(
	ctx context.Context,
	tx pgx.Tx,
	batchID uuid.UUID,
	scrapePlan scrapeflow.Plan,
	notificationPlan notificationflow.Plan,
) error {
	var scrapeCount int
	if err := tx.QueryRow(ctx, `SELECT count(*) FROM scrape_requests WHERE batch_id=$1`, batchID).Scan(&scrapeCount); err != nil {
		return organizationPersistenceError(ctx)
	}
	expectedScrapeCount := 0
	if scrapePlan.BatchID != uuid.Nil {
		expectedScrapeCount = len(scrapePlan.Requests)
	}
	if scrapeCount != expectedScrapeCount {
		return ErrOrganizationStateConflict
	}
	if scrapePlan.BatchID != uuid.Nil {
		for _, request := range scrapePlan.Requests {
			var stored scrapeflow.RequestPlan
			if err := tx.QueryRow(ctx, `
				SELECT id, kind, media_path, tmdb_id, season, episode, skip_reason
				FROM scrape_requests WHERE id=$1 AND batch_id=$2`, request.ID, scrapePlan.BatchID).Scan(
				&stored.ID, &stored.Kind, &stored.MediaPath, &stored.TMDBID,
				&stored.Season, &stored.Episode, &stored.SkipReason,
			); err != nil || stored != request {
				return ErrOrganizationStateConflict
			}
		}
	}
	var eventCount int
	if err := tx.QueryRow(ctx, `SELECT count(*) FROM notification_events WHERE batch_id=$1`, batchID).Scan(&eventCount); err != nil {
		return organizationPersistenceError(ctx)
	}
	expectedEventCount := 0
	if notificationPlan.BatchID != uuid.Nil {
		expectedEventCount = len(notificationPlan.Events)
	}
	if eventCount != expectedEventCount {
		return ErrOrganizationStateConflict
	}
	var requestCount int
	if err := tx.QueryRow(ctx, `SELECT count(*) FROM notifications WHERE batch_id=$1`, batchID).Scan(&requestCount); err != nil {
		return organizationPersistenceError(ctx)
	}
	expectedRequestCount := 0
	if notificationPlan.BatchID != uuid.Nil {
		for _, eventPlan := range notificationPlan.Events {
			expectedRequestCount += len(eventPlan.Requests)
			if err := verifyNotificationEvent(ctx, tx, notificationPlan.BatchID, eventPlan); err != nil {
				return err
			}
		}
	}
	if requestCount != expectedRequestCount {
		return ErrOrganizationStateConflict
	}
	if notificationPlan.BatchID != uuid.Nil {
		for _, eventPlan := range notificationPlan.Events {
			for _, request := range eventPlan.Requests {
				var channel string
				var source string
				var eventID *int64
				var key string
				var payload []byte
				if err := tx.QueryRow(ctx, `
					SELECT channel, source, event_id, idempotency_key, payload
					FROM notifications WHERE id=$1 AND batch_id=$2`, request.ID, notificationPlan.BatchID).Scan(
					&channel, &source, &eventID, &key, &payload,
				); err != nil {
					return ErrOrganizationStateConflict
				}
				var message any
				var expected any
				expectedPayload, err := json.Marshal(request.Message)
				if err != nil || json.Unmarshal(payload, &message) != nil || json.Unmarshal(expectedPayload, &expected) != nil ||
					channel != string(request.Channel) || source != request.Source || key != request.IdempotencyKey ||
					!notificationEventIDMatches(eventID, eventPlan.Event.EventKey, ctx, tx) || !jsonValuesEqual(message, expected) {
					return ErrOrganizationStateConflict
				}
			}
		}
	}
	return nil
}

func verifyNotificationEvent(
	ctx context.Context,
	tx pgx.Tx,
	expectedBatchID uuid.UUID,
	plan notificationflow.EventPlan,
) error {
	event := plan.Event
	var (
		storedBatchID uuid.UUID
		key           string
		channel       string
		source        *string
		mtype         *string
		title         *string
		text          *string
		image         *string
		link          *string
		userID        *string
		note          []byte
	)
	if err := tx.QueryRow(ctx, `
		SELECT batch_id, event_key, channel, source, mtype, title, text, image,
			link, userid, note
		FROM notification_events
		WHERE batch_id=$1 AND event_key=$2`, expectedBatchID, event.EventKey).Scan(
		&storedBatchID, &key, &channel, &source, &mtype, &title, &text, &image,
		&link, &userID, &note,
	); err != nil {
		return ErrOrganizationStateConflict
	}
	expectedNote := event.Note
	if len(expectedNote) == 0 {
		expectedNote = []byte(`{}`)
	}
	if storedBatchID != expectedBatchID || key != event.EventKey || channel != event.Channel ||
		!nullableStringEqual(source, event.Source) || !nullableStringEqual(mtype, event.MType) ||
		!nullableStringEqual(title, event.Title) || !nullableStringEqual(text, event.Text) ||
		!nullableStringEqual(image, event.Image) || !nullableStringEqual(link, event.Link) ||
		!nullableStringEqual(userID, event.UserID) || !jsonValuesEqualBytes(note, expectedNote) {
		return ErrOrganizationStateConflict
	}
	return nil
}

func notificationEventIDMatches(
	eventID *int64,
	eventKey string,
	ctx context.Context,
	tx pgx.Tx,
) bool {
	if eventKey == "" {
		return eventID == nil
	}
	if eventID == nil || *eventID <= 0 {
		return false
	}
	var storedKey string
	return tx.QueryRow(ctx, `SELECT event_key FROM notification_events WHERE id=$1`, *eventID).Scan(&storedKey) == nil &&
		storedKey == eventKey
}

func nullableStringEqual(left, right *string) bool {
	if left == nil || right == nil {
		return left == nil && right == nil
	}
	return *left == *right
}

func jsonValuesEqual(left, right any) bool {
	leftJSON, leftErr := json.Marshal(left)
	rightJSON, rightErr := json.Marshal(right)
	return leftErr == nil && rightErr == nil && string(leftJSON) == string(rightJSON)
}

func jsonValuesEqualBytes(left, right []byte) bool {
	var leftValue any
	var rightValue any
	return json.Unmarshal(left, &leftValue) == nil &&
		json.Unmarshal(right, &rightValue) == nil &&
		jsonValuesEqual(leftValue, rightValue)
}
