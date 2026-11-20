//go:build integration

package database

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/pressly/goose/v3"

	"github.com/moviepilot-go/moviepilot-go/internal/notification"
	"github.com/moviepilot-go/moviepilot-go/internal/notificationflow"
	"github.com/moviepilot-go/moviepilot-go/internal/scrapeflow"
	"github.com/moviepilot-go/moviepilot-go/internal/subscription"
)

func TestMediaIndexPostgresWaitsForScrapeTerminalState(t *testing.T) {
	tests := []struct {
		name       string
		skipped    bool
		transition string
		wantDue    bool
	}{
		{name: "pending blocks indexing", transition: "pending"},
		{name: "working blocks indexing", transition: "working"},
		{name: "retryable failure blocks indexing", transition: "failed_retryable"},
		{name: "completed allows indexing", transition: "completed", wantDue: true},
		{name: "skipped allows indexing", skipped: true, transition: "skipped", wantDue: true},
		{name: "exhausted failure allows indexing", transition: "failed_terminal", wantDue: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			store := newSiteStatisticsIntegrationStore(t)
			fixtureName := "scrape-gate-" + uuid.NewString()
			username := fixtureName + "-owner"
			fixture := createMediaIndexIntegrationFixture(t, store, fixtureName, 1, false)
			scrapePlan := reliabilityScrapePlan(fixture, username, tt.skipped)

			requestID, created, scrapeIDs, _, err := store.CompleteOrganizationBatchWithSideEffects(
				t.Context(), fixture.batch.ID, fixture.batch.LeaseToken, fixture.completion,
				scrapePlan, notificationflow.Plan{},
			)
			if err != nil || !created || requestID == uuid.Nil {
				t.Fatalf("CompleteOrganizationBatchWithSideEffects() = %s, %v, %v", requestID, created, err)
			}
			if tt.skipped {
				if len(scrapeIDs) != 0 {
					t.Fatalf("terminal skipped IDs = %v, want none", scrapeIDs)
				}
			} else if len(scrapeIDs) != 1 || scrapeIDs[0] != scrapePlan.Requests[0].ID {
				t.Fatalf("due scrape IDs = %v, want [%s]", scrapeIDs, scrapePlan.Requests[0].ID)
			}

			now := time.Now().UTC().Add(time.Second)
			transitionScrapeRequestForGate(t, store, scrapePlan.Requests[0].ID, tt.transition, now)
			due, err := store.ListDue(t.Context(), now, 10)
			if err != nil {
				t.Fatalf("ListDue(): %v", err)
			}
			if got := len(due) == 1 && due[0] == requestID; got != tt.wantDue {
				t.Fatalf("ListDue() = %v, want request due=%v", due, tt.wantDue)
			}
			lease, claimed, err := store.Claim(t.Context(), requestID, now, time.Minute)
			if err != nil {
				t.Fatalf("Claim(): %v", err)
			}
			if claimed != tt.wantDue {
				t.Fatalf("Claim() claimed=%v lease=%#v, want %v", claimed, lease, tt.wantDue)
			}
		})
	}
}

func TestMediaIndexPostgresRequiredConfirmationDoesNotBlockNewNotifications(t *testing.T) {
	store := newSiteStatisticsIntegrationStore(t)
	fixtureName := "notification-unblock-" + uuid.NewString()
	username := fixtureName + "-owner"
	fixture := createMediaIndexIntegrationFixture(t, store, fixtureName, 1, false)
	scrapePlan := reliabilityScrapePlan(fixture, username, true)
	notificationPlan := reliabilityNotificationPlan(t, fixture, username, "家庭通知")

	requestID, created, _, notificationIDs, err := store.CompleteOrganizationBatchWithSideEffects(
		t.Context(), fixture.batch.ID, fixture.batch.LeaseToken, fixture.completion,
		scrapePlan, notificationPlan,
	)
	if err != nil || !created || requestID == uuid.Nil {
		t.Fatalf("CompleteOrganizationBatchWithSideEffects() = %s, %v, %v", requestID, created, err)
	}
	notificationID := notificationPlan.Events[0].Requests[0].ID
	if len(notificationIDs) != 1 || notificationIDs[0] != notificationID {
		t.Fatalf("required Emby notification IDs = %v, want [%s]", notificationIDs, notificationID)
	}
	assertNotificationState(t, store, notificationID, notificationflow.StatePending)
	eventID, publishedAt := organizationNotificationEventState(t, store, fixture.batch.ID)
	if publishedAt == nil {
		t.Fatal("required Emby notification event was not published at local completion")
	}
	assertNotificationEventLink(t, store, notificationID, eventID)
	dueAt := time.Now().UTC().Add(time.Second)
	if due, err := store.ListDueNotifications(t.Context(), dueAt, 10); err != nil || len(due) != 1 || due[0] != notificationID {
		t.Fatalf("required Emby ListDueNotifications() = %v, %v; want [%s]", due, err, notificationID)
	}
	assertMediaIndexSubscriptionState(
		t, store, fixture.subscriptionID, fixture.intentID,
		subscription.Completed, "indexed", false, true,
	)

	confirmedAt := confirmMediaIndexIntegrationRequest(t, store, requestID)
	assertNotificationState(t, store, notificationID, notificationflow.StatePending)
	due, err := store.ListDueNotifications(t.Context(), confirmedAt.Add(time.Second), 10)
	if err != nil || len(due) != 1 || due[0] != notificationID {
		t.Fatalf("confirmed ListDueNotifications() = %v, %v; want [%s]", due, err, notificationID)
	}
	_, afterConfirmation := organizationNotificationEventState(t, store, fixture.batch.ID)
	if afterConfirmation == nil || !afterConfirmation.Equal(*publishedAt) || !publishedAt.Before(confirmedAt) {
		t.Fatalf("published_at changed across confirmation: before=%v after=%v confirmed=%s", publishedAt, afterConfirmation, confirmedAt)
	}
	assertMediaIndexSubscriptionState(
		t, store, fixture.subscriptionID, fixture.intentID,
		subscription.Completed, "indexed", false, true,
	)
}

func TestMediaIndexPostgresConfirmationUpgradesLegacyBlockedNotifications(t *testing.T) {
	store := newSiteStatisticsIntegrationStore(t)
	fixtureName := "legacy-notification-unblock-" + uuid.NewString()
	username := fixtureName + "-owner"
	fixture := createMediaIndexIntegrationFixture(t, store, fixtureName, 1, false)
	requestID, created, _, _, err := store.CompleteOrganizationBatchWithSideEffects(
		t.Context(), fixture.batch.ID, fixture.batch.LeaseToken, fixture.completion,
		reliabilityScrapePlan(fixture, username, true), notificationflow.Plan{},
	)
	if err != nil || !created || requestID == uuid.Nil {
		t.Fatalf("CompleteOrganizationBatchWithSideEffects() = %s, %v, %v", requestID, created, err)
	}

	legacyNotificationID, legacyEventID := insertLegacyBlockedOrganizationNotification(
		t, store, fixture.batch.ID, username,
	)
	assertNotificationState(t, store, legacyNotificationID, notificationflow.StateBlocked)
	if due, err := store.ListDueNotifications(t.Context(), time.Now().UTC().Add(time.Second), 10); err != nil || len(due) != 0 {
		t.Fatalf("legacy blocked ListDueNotifications() = %v, %v", due, err)
	}

	confirmedAt := confirmMediaIndexIntegrationRequest(t, store, requestID)
	assertNotificationState(t, store, legacyNotificationID, notificationflow.StatePending)
	assertNotificationEventLink(t, store, legacyNotificationID, legacyEventID)
	due, err := store.ListDueNotifications(t.Context(), confirmedAt.Add(time.Second), 10)
	if err != nil || len(due) != 1 || due[0] != legacyNotificationID {
		t.Fatalf("upgraded legacy ListDueNotifications() = %v, %v; want [%s]", due, err, legacyNotificationID)
	}
	var publishedAt *time.Time
	if err := store.db.Pool.QueryRow(t.Context(), `
		SELECT published_at FROM notification_events WHERE id=$1`, legacyEventID).Scan(&publishedAt); err != nil {
		t.Fatal(err)
	}
	if publishedAt == nil || publishedAt.After(confirmedAt) {
		t.Fatalf("legacy event published_at = %v, confirmed_at=%s", publishedAt, confirmedAt)
	}
}

func TestOrganizationPostgresLocalCompletionMakesNotificationDue(t *testing.T) {
	store := newSiteStatisticsIntegrationStore(t)
	fixtureName := "notification-local-" + uuid.NewString()
	username := fixtureName + "-owner"
	fixture := createMediaIndexIntegrationFixture(t, store, fixtureName, 1, false)
	fixture.completion.RequiredServer = nil
	scrapePlan := reliabilityScrapePlan(fixture, username, true)
	notificationPlan := reliabilityNotificationPlan(t, fixture, username, "家庭通知")

	requestID, created, _, notificationIDs, err := store.CompleteOrganizationBatchWithSideEffects(
		t.Context(), fixture.batch.ID, fixture.batch.LeaseToken, fixture.completion,
		scrapePlan, notificationPlan,
	)
	if err != nil || created || requestID != uuid.Nil {
		t.Fatalf("local CompleteOrganizationBatchWithSideEffects() = %s, %v, %v", requestID, created, err)
	}
	notificationID := notificationPlan.Events[0].Requests[0].ID
	if len(notificationIDs) != 1 || notificationIDs[0] != notificationID {
		t.Fatalf("local notification IDs = %v, want [%s]", notificationIDs, notificationID)
	}
	assertNotificationState(t, store, notificationID, notificationflow.StatePending)
	eventID, publishedAt := organizationNotificationEventState(t, store, fixture.batch.ID)
	if publishedAt == nil {
		t.Fatal("local notification event was not published")
	}
	assertNotificationEventLink(t, store, notificationID, eventID)
	due, err := store.ListDueNotifications(t.Context(), time.Now().UTC().Add(time.Second), 10)
	if err != nil || len(due) != 1 || due[0] != notificationID {
		t.Fatalf("local ListDueNotifications() = %v, %v; want [%s]", due, err, notificationID)
	}
	lease, claimed, err := store.ClaimNotification(
		t.Context(), notificationID, time.Now().UTC().Add(time.Second), time.Minute,
	)
	if err != nil || !claimed || lease.Request.Source != "家庭通知" ||
		lease.Request.Message.Source != "家庭通知" || !lease.Valid(notificationID) {
		t.Fatalf("ClaimNotification() = %#v, %v, %v", lease, claimed, err)
	}
}

func TestNotificationPostgresRejectsCrossBatchEventLinkOnClaim(t *testing.T) {
	store := newSiteStatisticsIntegrationStore(t)
	requestFixtureName := "notification-request-batch-" + uuid.NewString()
	eventFixtureName := "notification-event-batch-" + uuid.NewString()
	requestFixture := createMediaIndexIntegrationFixture(t, store, requestFixtureName, 1, false)
	eventFixture := createMediaIndexIntegrationFixture(t, store, eventFixtureName, 1, false)

	var eventID int64
	if err := store.db.Pool.QueryRow(t.Context(), `
		INSERT INTO notification_events (batch_id, event_key, note, published_at)
		VALUES ($1, $2, '{}'::jsonb, now())
		RETURNING id`, eventFixture.batch.ID, "cross-batch-event-"+uuid.NewString()).Scan(&eventID); err != nil {
		t.Fatal(err)
	}
	requestID := uuid.New()
	message := notification.Message{
		Channel: notification.ChannelTelegram,
		Source:  "家庭通知",
		Title:   "整理完成",
		Text:    "文件已成功整理到媒体库",
	}
	payload, err := json.Marshal(message)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.db.Pool.Exec(t.Context(), `
		INSERT INTO notifications (
			id, batch_id, username, channel, source, event_id,
			idempotency_key, payload, state, next_attempt_at
		) VALUES ($1,$2,$3,'telegram',$4,$5,$6,$7::jsonb,'pending',now())`,
		requestID, requestFixture.batch.ID, requestFixtureName+"-owner",
		message.Source, eventID, "cross-batch-request-"+uuid.NewString(), payload,
	); err != nil {
		t.Fatal(err)
	}

	lease, claimed, err := store.ClaimNotification(
		t.Context(), requestID, time.Now().UTC().Add(time.Second), time.Minute,
	)
	if err == nil || claimed || lease.Token != uuid.Nil {
		t.Fatalf("ClaimNotification() = %#v, %v, %v; want rejected corrupt relation", lease, claimed, err)
	}
	assertNotificationClaimRolledBack(t, store, requestID)
}

func TestNotificationPostgresRejectsCorruptPayloadOnClaim(t *testing.T) {
	store := newSiteStatisticsIntegrationStore(t)
	fixtureName := "notification-corrupt-payload-" + uuid.NewString()
	fixture := createMediaIndexIntegrationFixture(t, store, fixtureName, 1, false)
	requestID := uuid.New()
	if _, err := store.db.Pool.Exec(t.Context(), `
		INSERT INTO notifications (
			id, batch_id, username, channel, source,
			idempotency_key, payload, state, next_attempt_at
		) VALUES ($1,$2,$3,'telegram',$4,$5,$6::jsonb,'pending',now())`,
		requestID, fixture.batch.ID, fixtureName+"-owner", "家庭通知",
		"corrupt-payload-request-"+uuid.NewString(), []byte(`"corrupt"`),
	); err != nil {
		t.Fatal(err)
	}

	lease, claimed, err := store.ClaimNotification(
		t.Context(), requestID, time.Now().UTC().Add(time.Second), time.Minute,
	)
	if err == nil || claimed || lease.Token != uuid.Nil {
		t.Fatalf("ClaimNotification() = %#v, %v, %v; want rejected corrupt payload", lease, claimed, err)
	}
	assertNotificationClaimRolledBack(t, store, requestID)
}

func TestOrganizationPostgresPublishesHistoryWithoutDeliveryTargets(t *testing.T) {
	store := newSiteStatisticsIntegrationStore(t)
	fixtureName := "notification-history-only-" + uuid.NewString()
	username := fixtureName + "-owner"
	fixture := createMediaIndexIntegrationFixture(t, store, fixtureName, 1, false)
	fixture.completion.RequiredServer = nil
	plan, err := notificationflow.BuildPlan(
		fixture.batch.ID, username, len(fixture.batch.Plan.Operations), nil,
	)
	if err != nil || len(plan.Events) != 1 || len(plan.Events[0].Requests) != 0 ||
		plan.Events[0].Event.EventKey == "" {
		t.Fatalf("history-only plan = %#v, %v", plan, err)
	}

	_, created, _, notificationIDs, err := store.CompleteOrganizationBatchWithSideEffects(
		t.Context(), fixture.batch.ID, fixture.batch.LeaseToken, fixture.completion,
		reliabilityScrapePlan(fixture, username, true), plan,
	)
	if err != nil || created || len(notificationIDs) != 0 {
		t.Fatalf("history-only completion created=%v notification_ids=%v error=%v", created, notificationIDs, err)
	}
	eventID, publishedAt := organizationNotificationEventState(t, store, fixture.batch.ID)
	if eventID <= 0 || publishedAt == nil {
		t.Fatalf("history-only event = id:%d published:%v", eventID, publishedAt)
	}
	var deliveries int
	if err := store.db.Pool.QueryRow(t.Context(), `
		SELECT count(*) FROM notifications WHERE batch_id=$1`, fixture.batch.ID).Scan(&deliveries); err != nil {
		t.Fatal(err)
	}
	if deliveries != 0 {
		t.Fatalf("history-only completion created %d deliveries", deliveries)
	}
}

func TestOrganizationPostgresPersistsSkippedTVRequests(t *testing.T) {
	store := newSiteStatisticsIntegrationStore(t)
	fixtureName := "skipped-tv-" + uuid.NewString()
	username := fixtureName + "-owner"
	fixture := createMediaIndexIntegrationFixture(t, store, fixtureName, 1, false)
	fixture.completion.RequiredServer = nil
	videoPath := fixture.completion.Files[0].LocalPath
	scrapePlan := scrapeflow.Plan{
		BatchID: fixture.batch.ID, IntentID: fixture.intentID, Username: username,
		Requests: []scrapeflow.RequestPlan{
			{
				ID:   uuid.NewSHA1(fixture.batch.ID, []byte("tvshow\x00"+videoPath)),
				Kind: scrapeflow.KindTVShow, MediaPath: videoPath + ".tvshow.nfo",
				SkipReason: "scraping_disabled",
			},
			{
				ID:   uuid.NewSHA1(fixture.batch.ID, []byte("episode\x00"+videoPath)),
				Kind: scrapeflow.KindEpisode, MediaPath: videoPath,
				SkipReason: "scraping_disabled",
			},
		},
	}
	if !scrapePlan.Valid() {
		t.Fatalf("skipped TV plan is invalid: %#v", scrapePlan)
	}
	_, _, scrapeIDs, _, err := store.CompleteOrganizationBatchWithSideEffects(
		t.Context(), fixture.batch.ID, fixture.batch.LeaseToken, fixture.completion,
		scrapePlan, notificationflow.Plan{},
	)
	if err != nil {
		t.Fatalf("CompleteOrganizationBatchWithSideEffects(): %v", err)
	}
	if len(scrapeIDs) != 0 {
		t.Fatalf("skipped scrape IDs = %v, want none", scrapeIDs)
	}
	var terminal int
	if err := store.db.Pool.QueryRow(t.Context(), `
		SELECT count(*) FROM scrape_requests
		WHERE batch_id=$1 AND state='skipped' AND completed_at IS NOT NULL`, fixture.batch.ID).Scan(&terminal); err != nil {
		t.Fatal(err)
	}
	if terminal != 2 {
		t.Fatalf("terminal skipped requests = %d, want 2", terminal)
	}
}

func TestScrapeNotificationMigrationUpgradesLegacyNotifications(t *testing.T) {
	store := newSiteStatisticsIntegrationStore(t)
	var schema string
	if err := store.db.Pool.QueryRow(t.Context(), "SELECT current_schema()").Scan(&schema); err != nil {
		t.Fatal(err)
	}
	migrationDB, _ := openSubscriptionV2StateMigrationDB(t, schema)
	ctx, cancel := context.WithTimeout(t.Context(), 45*time.Second)
	defer cancel()
	goose.SetBaseFS(migrationFS)
	if err := goose.SetDialect("postgres"); err != nil {
		t.Fatal(err)
	}
	if err := goose.DownToContext(ctx, migrationDB, "migrations", 22); err != nil {
		t.Fatalf("回退到迁移 022: %v", err)
	}

	username := "legacy-notification-" + uuid.NewString()
	insertSubscriptionIntegrationUsers(t, store, username)
	notificationID := uuid.New()
	if _, err := store.db.Pool.Exec(ctx, `
		INSERT INTO notifications (
			id, username, channel, idempotency_key, payload, state, last_error, sent_at
		) VALUES ($1,$2,'legacy-email',$3,'{}'::jsonb,'legacy-sent',NULL,now())`,
		notificationID, username, "legacy:"+notificationID.String()); err != nil {
		t.Fatalf("创建旧通知 fixture: %v", err)
	}

	if err := goose.UpToContext(ctx, migrationDB, "migrations", 23); err != nil {
		t.Fatalf("升级到迁移 023: %v", err)
	}
	if err := goose.UpToContext(ctx, migrationDB, "migrations", 23); err != nil {
		t.Fatalf("重复执行升级流程: %v", err)
	}
	var (
		state          notificationflow.State
		batchID        *uuid.UUID
		lastError      string
		attemptCount   int
		leaseToken     *uuid.UUID
		leaseExpiresAt *time.Time
		sentAt         *time.Time
		updatedAt      time.Time
		legacyState    string
		legacyError    *string
		legacySentAt   *time.Time
	)
	if err := store.db.Pool.QueryRow(ctx, `
		SELECT state, batch_id, last_error, attempt_count, lease_token,
			lease_expires_at, sent_at, updated_at,
			legacy_state, legacy_last_error, legacy_sent_at
		FROM notifications WHERE id=$1`, notificationID).Scan(
		&state, &batchID, &lastError, &attemptCount, &leaseToken,
		&leaseExpiresAt, &sentAt, &updatedAt,
		&legacyState, &legacyError, &legacySentAt,
	); err != nil {
		t.Fatalf("读取升级后的旧通知: %v", err)
	}
	if state != notificationflow.StateSkipped || batchID != nil || lastError != "legacy_notification" ||
		attemptCount != 0 || leaseToken != nil || leaseExpiresAt != nil || sentAt != nil || updatedAt.IsZero() ||
		legacyState != "legacy-sent" || legacyError != nil || legacySentAt == nil {
		t.Fatalf("升级后的旧通知状态异常: state=%q batch=%v error=%q attempts=%d lease=%v/%v sent=%v updated=%s",
			state, batchID, lastError, attemptCount, leaseToken, leaseExpiresAt, sentAt, updatedAt)
	}

	if err := goose.DownToContext(ctx, migrationDB, "migrations", 22); err != nil {
		t.Fatalf("回退迁移 023: %v", err)
	}
	var (
		restoredState string
		restoredError *string
		restoredSent  *time.Time
	)
	if err := store.db.Pool.QueryRow(ctx, `
		SELECT state, last_error, sent_at FROM notifications WHERE id=$1`, notificationID).Scan(
		&restoredState, &restoredError, &restoredSent,
	); err != nil {
		t.Fatalf("读取回退后的旧通知: %v", err)
	}
	if restoredState != "legacy-sent" || restoredError != nil || restoredSent == nil {
		t.Fatalf("回退未恢复旧通知: state=%q error=%v sent=%v", restoredState, restoredError, restoredSent)
	}
}

func TestScrapeSkippedEpisodeIdentityMigrationDownAndUp(t *testing.T) {
	store := newSiteStatisticsIntegrationStore(t)
	fixtureName := "skipped-tv-migration-" + uuid.NewString()
	username := fixtureName + "-owner"
	fixture := createMediaIndexIntegrationFixture(t, store, fixtureName, 1, false)
	fixture.completion.RequiredServer = nil
	videoPath := fixture.completion.Files[0].LocalPath
	requestID := uuid.NewSHA1(fixture.batch.ID, []byte("episode\x00"+videoPath))
	scrapePlan := scrapeflow.Plan{
		BatchID: fixture.batch.ID, IntentID: fixture.intentID, Username: username,
		Requests: []scrapeflow.RequestPlan{{
			ID: requestID, Kind: scrapeflow.KindEpisode, MediaPath: videoPath,
			SkipReason: "scraping_disabled",
		}},
	}
	if _, _, _, _, err := store.CompleteOrganizationBatchWithSideEffects(
		t.Context(), fixture.batch.ID, fixture.batch.LeaseToken, fixture.completion,
		scrapePlan, notificationflow.Plan{},
	); err != nil {
		t.Fatal(err)
	}

	var schema string
	if err := store.db.Pool.QueryRow(t.Context(), "SELECT current_schema()").Scan(&schema); err != nil {
		t.Fatal(err)
	}
	migrationDB, _ := openSubscriptionV2StateMigrationDB(t, schema)
	ctx, cancel := context.WithTimeout(t.Context(), 45*time.Second)
	defer cancel()
	goose.SetBaseFS(migrationFS)
	if err := goose.SetDialect("postgres"); err != nil {
		t.Fatal(err)
	}
	if err := goose.DownToContext(ctx, migrationDB, "migrations", 23); err != nil {
		t.Fatalf("回退 migration 024: %v", err)
	}
	var episode int
	if err := store.db.Pool.QueryRow(ctx, `
		SELECT episode FROM scrape_requests WHERE id=$1`, requestID).Scan(&episode); err != nil {
		t.Fatal(err)
	}
	if episode != 1 {
		t.Fatalf("down migration episode = %d, want compatibility sentinel 1", episode)
	}
	if err := goose.UpToContext(ctx, migrationDB, "migrations", 24); err != nil {
		t.Fatalf("重新应用 migration 024: %v", err)
	}
}

type reliabilityNotificationTargetProvider []notification.Target

func (p reliabilityNotificationTargetProvider) Targets(notification.Message) []notification.Target {
	return append([]notification.Target(nil), p...)
}

func reliabilityNotificationPlan(
	t *testing.T,
	fixture mediaIndexIntegrationFixture,
	username string,
	source string,
) notificationflow.Plan {
	t.Helper()
	planner, err := notificationflow.NewPlanner(reliabilityNotificationTargetProvider{{
		Channel: notification.ChannelTelegram,
		Source:  source,
	}})
	if err != nil {
		t.Fatal(err)
	}
	plan, err := planner.Plan(fixture.batch.ID, username, fixture.batch.Plan)
	if err != nil {
		t.Fatal(err)
	}
	return plan
}

func insertLegacyBlockedOrganizationNotification(
	t *testing.T,
	store *Store,
	batchID uuid.UUID,
	username string,
) (uuid.UUID, int64) {
	t.Helper()
	eventKey := "legacy-blocked:" + uuid.NewString()
	source := "旧版家庭通知"
	message := notification.Message{
		Channel:     notification.ChannelTelegram,
		Source:      source,
		Type:        notification.TypeOrganize,
		ContentType: notification.ContentOrganizeSuccess,
		Title:       "整理完成",
		Text:        "文件已成功整理到媒体库",
		Username:    username,
	}
	payload, err := json.Marshal(message)
	if err != nil {
		t.Fatal(err)
	}
	var eventID int64
	if err := store.db.Pool.QueryRow(t.Context(), `
		INSERT INTO notification_events (
			batch_id, event_key, channel, source, mtype, title, text, note, published_at
		) VALUES ($1,$2,$3,$4,$5,$6,$7,'{}'::jsonb,NULL)
		RETURNING id`,
		batchID, eventKey, message.Channel, source, message.Type, message.Title, message.Text,
	).Scan(&eventID); err != nil {
		t.Fatal(err)
	}
	notificationID := uuid.New()
	if _, err := store.db.Pool.Exec(t.Context(), `
		INSERT INTO notifications (
			id, batch_id, username, channel, source, event_id,
			idempotency_key, payload, state, next_attempt_at
		) VALUES ($1,$2,$3,$4,$5,$6,$7,$8::jsonb,'blocked',now())`,
		notificationID, batchID, username, message.Channel, source, eventID,
		"legacy-blocked:"+notificationID.String(), payload,
	); err != nil {
		t.Fatal(err)
	}
	return notificationID, eventID
}

func reliabilityScrapePlan(
	fixture mediaIndexIntegrationFixture,
	username string,
	skipped bool,
) scrapeflow.Plan {
	request := scrapeflow.RequestPlan{
		ID:        uuid.NewSHA1(fixture.batch.ID, []byte("reliability-scrape")),
		Kind:      scrapeflow.KindMovie,
		MediaPath: fixture.completion.Files[0].LocalPath,
		TMDBID:    42,
	}
	if skipped {
		request.Kind = scrapeflow.KindUnknown
		request.TMDBID = 0
		request.SkipReason = "scraping_disabled"
	}
	return scrapeflow.Plan{
		BatchID: fixture.batch.ID, IntentID: fixture.intentID,
		Username: username, Requests: []scrapeflow.RequestPlan{request},
	}
}

func transitionScrapeRequestForGate(
	t *testing.T,
	store *Store,
	requestID uuid.UUID,
	transition string,
	now time.Time,
) {
	t.Helper()
	var err error
	switch transition {
	case "pending", "skipped":
		return
	case "working":
		_, err = store.db.Pool.Exec(t.Context(), `
			UPDATE scrape_requests
			SET state='working', attempt_count=1, lease_token=$2,
				lease_expires_at=$3, updated_at=$4
			WHERE id=$1`, requestID, uuid.New(), now.Add(time.Minute), now)
	case "failed_retryable":
		_, err = store.db.Pool.Exec(t.Context(), `
			UPDATE scrape_requests
			SET state='failed', attempt_count=4, next_attempt_at=$2,
				last_error='scrape_failed', updated_at=$2
			WHERE id=$1`, requestID, now)
	case "completed":
		_, err = store.db.Pool.Exec(t.Context(), `
			UPDATE scrape_requests
			SET state='completed', attempt_count=1, completed_at=$2, updated_at=$2
			WHERE id=$1`, requestID, now)
	case "failed_terminal":
		_, err = store.db.Pool.Exec(t.Context(), `
			UPDATE scrape_requests
			SET state='failed', attempt_count=5, next_attempt_at=$2,
				last_error='scrape_failed', updated_at=$2
			WHERE id=$1`, requestID, now)
	default:
		t.Fatalf("unknown scrape transition %q", transition)
	}
	if err != nil {
		t.Fatalf("transition scrape request to %s: %v", transition, err)
	}
}

func confirmMediaIndexIntegrationRequest(t *testing.T, store *Store, requestID uuid.UUID) time.Time {
	t.Helper()
	now := time.Now().UTC()
	lease, found, err := store.Claim(t.Context(), requestID, now, time.Minute)
	if err != nil || !found {
		t.Fatalf("initial Claim() = %#v, %v, %v", lease, found, err)
	}
	if err := store.RecordRefresh(t.Context(), requestID, lease.LeaseToken, now, now); err != nil {
		t.Fatalf("RecordRefresh(): %v", err)
	}
	lease, found, err = store.Claim(t.Context(), requestID, now, time.Minute)
	if err != nil || !found {
		t.Fatalf("confirmation Claim() = %#v, %v, %v", lease, found, err)
	}
	files, err := store.ListPendingFiles(t.Context(), requestID, lease.LeaseToken, 16)
	if err != nil || len(files) == 0 {
		t.Fatalf("ListPendingFiles() = %v, %v", files, err)
	}
	for _, file := range files {
		if err := store.ConfirmFile(t.Context(), requestID, lease.LeaseToken, file.ID, "emby-"+uuid.NewString()); err != nil {
			t.Fatalf("ConfirmFile(%d): %v", file.ID, err)
		}
	}
	confirmedAt := now.Add(time.Second)
	confirmed, err := store.ConfirmRequest(t.Context(), requestID, lease.LeaseToken, confirmedAt)
	if err != nil || !confirmed {
		t.Fatalf("ConfirmRequest() = %v, %v", confirmed, err)
	}
	return confirmedAt
}

func assertNotificationState(
	t *testing.T,
	store *Store,
	requestID uuid.UUID,
	want notificationflow.State,
) {
	t.Helper()
	var state notificationflow.State
	if err := store.db.Pool.QueryRow(t.Context(), `
		SELECT state FROM notifications WHERE id=$1`, requestID).Scan(&state); err != nil {
		t.Fatal(err)
	}
	if state != want {
		t.Fatalf("notification state = %q, want %q", state, want)
	}
}

func assertNotificationClaimRolledBack(t *testing.T, store *Store, requestID uuid.UUID) {
	t.Helper()
	var (
		state          notificationflow.State
		attemptCount   int
		leaseToken     *uuid.UUID
		leaseExpiresAt *time.Time
	)
	if err := store.db.Pool.QueryRow(t.Context(), `
		SELECT state, attempt_count, lease_token, lease_expires_at
		FROM notifications WHERE id=$1`, requestID).Scan(
		&state, &attemptCount, &leaseToken, &leaseExpiresAt,
	); err != nil {
		t.Fatal(err)
	}
	if state != notificationflow.StatePending || attemptCount != 0 || leaseToken != nil || leaseExpiresAt != nil {
		t.Fatalf("notification claim was not rolled back: state=%q attempts=%d lease=%v/%v",
			state, attemptCount, leaseToken, leaseExpiresAt)
	}
}

func organizationNotificationEventState(
	t *testing.T,
	store *Store,
	batchID uuid.UUID,
) (int64, *time.Time) {
	t.Helper()
	var eventID int64
	var publishedAt *time.Time
	if err := store.db.Pool.QueryRow(t.Context(), `
		SELECT id, published_at
		FROM notification_events WHERE batch_id=$1`, batchID).Scan(&eventID, &publishedAt); err != nil {
		t.Fatal(err)
	}
	return eventID, publishedAt
}

func assertNotificationEventLink(
	t *testing.T,
	store *Store,
	requestID uuid.UUID,
	wantEventID int64,
) {
	t.Helper()
	var eventID *int64
	if err := store.db.Pool.QueryRow(t.Context(), `
		SELECT event_id FROM notifications WHERE id=$1`, requestID).Scan(&eventID); err != nil {
		t.Fatal(err)
	}
	if eventID == nil || *eventID != wantEventID {
		t.Fatalf("notification event_id = %v, want %d", eventID, wantEventID)
	}
}
