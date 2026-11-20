//go:build integration

package database

import (
	"bytes"
	"encoding/json"
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/moviepilot-go/moviepilot-go/internal/mediaindex"
	"github.com/moviepilot-go/moviepilot-go/internal/notification"
	"github.com/moviepilot-go/moviepilot-go/internal/notificationflow"
	"github.com/moviepilot-go/moviepilot-go/internal/organization"
	"github.com/moviepilot-go/moviepilot-go/internal/organizer"
	"github.com/moviepilot-go/moviepilot-go/internal/scrapeflow"
)

func TestOrganizationPostgresPersistsCrossSeasonSuccessNotifications(t *testing.T) {
	store := newDownloadIntentPlanIntegrationStore(t)
	fixtureName := "cross-season-success-" + uuid.NewString()
	username := fixtureName + "-owner"
	insertDownloadIntentIntegrationUsers(t, store, username)

	intent := mustDatabaseDownloadIntent(t, username, fixtureName)
	if _, err := store.CreateDownloadIntent(t.Context(), username, intent); err != nil {
		t.Fatalf("create download intent: %v", err)
	}
	siteID := insertDownloadIntentPlanIntegrationSite(t, store, fixtureName)
	if _, err := store.CreateDownloadIntentPlan(
		t.Context(), username, downloadIntentPlanIntegrationFixture(intent.ID, siteID, fixtureName),
	); err != nil {
		t.Fatalf("create download intent plan: %v", err)
	}
	if _, err := store.db.Pool.Exec(t.Context(), `
		UPDATE download_intents SET state='completed', torrent_hash=$2 WHERE id=$1`,
		intent.ID, "0123456789abcdef0123456789abcdef01234567"); err != nil {
		t.Fatalf("complete download intent fixture: %v", err)
	}

	media := organization.MediaContext{
		Source: "themoviedb", MediaID: "8080", TitleYear: "Cross Season (2026)",
		MediaType: "电视剧", Category: "Drama", VoteAverage: 8.8,
		PosterURL: "https://image.example/original/cross-season.jpg",
	}
	input := organization.SuccessInput{
		IntentID: intent.ID, Version: organization.CurrentSuccessVersion,
		Username: username, Background: true, Media: media, ResourceTerm: "2160p WEB-DL",
		Episodes: []organization.Episode{{Season: 1, Episode: 1}, {Season: 2, Episode: 3}},
	}
	createDownloadSuccessInputIntegration(t, store, username, input)

	root := t.TempDir()
	sourceRoot := filepath.Join(root, "downloads")
	organizationPlan, err := organizer.BuildPlan(filepath.Join(root, "library"), []organizer.Operation{
		organizationTestOperation(
			filepath.Join(sourceRoot, "Cross.Season.S01E01.mkv"),
			filepath.Join(root, "library", "Season 01", "Cross.Season.S01E01.mkv"),
			organizer.Copy,
		),
		organizationTestOperation(
			filepath.Join(sourceRoot, "Cross.Season.S02E03.mkv"),
			filepath.Join(root, "library", "Season 02", "Cross.Season.S02E03.mkv"),
			organizer.Copy,
		),
	})
	if err != nil {
		t.Fatalf("build organization plan: %v", err)
	}
	batchID := uuid.NewSHA1(intent.ID, []byte(organizationPlan.ID))
	seasonOneGroup, err := organization.BuildGroupKey(media, 1)
	if err != nil {
		t.Fatalf("build S01 group key: %v", err)
	}
	seasonTwoGroup, err := organization.BuildGroupKey(media, 2)
	if err != nil {
		t.Fatalf("build S02 group key: %v", err)
	}
	snapshots := []organization.SuccessSnapshot{
		{
			BatchID: batchID, IntentID: intent.ID, GroupKey: seasonOneGroup,
			Version: organization.CurrentSuccessVersion, Username: username,
			NeedNotify: true, Background: true, Media: media,
			Files: []organization.FileFact{{
				Ordinal: 0, Role: "video", SizeBytes: organizationPlan.Operations[0].ExpectedSize,
				Season: 1, Episodes: []int{1}, ResourceTerm: input.ResourceTerm,
			}},
		},
		{
			BatchID: batchID, IntentID: intent.ID, GroupKey: seasonTwoGroup,
			Version: organization.CurrentSuccessVersion, Username: username,
			NeedNotify: true, Background: true, Media: media,
			Files: []organization.FileFact{{
				Ordinal: 1, Role: "video", SizeBytes: organizationPlan.Operations[1].ExpectedSize,
				Season: 2, Episodes: []int{3}, ResourceTerm: input.ResourceTerm,
			}},
		},
	}
	batch, err := store.CreateOrganizationBatch(
		t.Context(), username, intent.ID, sourceRoot, organizationPlan, snapshots,
	)
	if err != nil {
		t.Fatalf("create organization batch: %v", err)
	}
	if batch.ID != batchID {
		t.Fatalf("organization batch ID = %s, want %s", batch.ID, batchID)
	}
	storedSnapshots, err := store.ListOrganizationSuccessSnapshots(t.Context(), username, batch.ID)
	if err != nil || len(storedSnapshots) != 2 {
		t.Fatalf("ListOrganizationSuccessSnapshots() = %#v, %v", storedSnapshots, err)
	}
	for _, snapshot := range storedSnapshots {
		if snapshot.Legacy || snapshot.Version != organization.CurrentSuccessVersion {
			t.Fatalf("stored snapshot is not V1: %#v", snapshot)
		}
	}

	claimed, found, err := store.ClaimOrganizationBatch(t.Context(), batch.ID, time.Now().UTC(), time.Minute)
	if err != nil || !found {
		t.Fatalf("ClaimOrganizationBatch() = %#v, %v, %v", claimed, found, err)
	}
	files, err := store.ClaimOrganizationFiles(t.Context(), batch.ID, claimed.LeaseToken, len(organizationPlan.Operations))
	if err != nil || len(files) != len(organizationPlan.Operations) {
		t.Fatalf("ClaimOrganizationFiles() = %#v, %v", files, err)
	}
	for _, file := range files {
		if err := store.CompleteOrganizationFile(t.Context(), batch.ID, claimed.LeaseToken, file.ID); err != nil {
			t.Fatalf("CompleteOrganizationFile(%d): %v", file.ID, err)
		}
	}

	planner, err := notificationflow.NewPlanner(crossSeasonNotificationTargetProvider{{
		Channel: notification.ChannelTelegram,
		Source:  "season-telegram",
	}})
	if err != nil {
		t.Fatalf("NewPlanner(): %v", err)
	}
	notificationPlan, err := planner.PlanSuccess(batch.ID, username, storedSnapshots, "https://mp.example")
	if err != nil || !notificationPlan.Valid() || len(notificationPlan.Events) != 2 ||
		len(notificationPlan.Events[0].Requests) != 1 || len(notificationPlan.Events[1].Requests) != 1 {
		t.Fatalf("PlanSuccess() = %#v, %v", notificationPlan, err)
	}

	requestID, mediaCreated, scrapeDue, notificationDue, err := store.CompleteOrganizationBatchWithSideEffects(
		t.Context(), batch.ID, claimed.LeaseToken, mediaindex.CompletionPlan{},
		scrapeflow.Plan{}, notificationPlan,
	)
	if err != nil || requestID != uuid.Nil || mediaCreated || len(scrapeDue) != 0 {
		t.Fatalf("CompleteOrganizationBatchWithSideEffects() = %s, %v, %v, %v, %v",
			requestID, mediaCreated, scrapeDue, notificationDue, err)
	}
	wantDue := []uuid.UUID{
		notificationPlan.Events[0].Requests[0].ID,
		notificationPlan.Events[1].Requests[0].ID,
	}
	if !sameCrossSeasonNotificationIDs(notificationDue, wantDue) {
		t.Fatalf("notification due IDs = %v, want %v", notificationDue, wantDue)
	}

	expectedGroups := map[string]struct {
		title         string
		seasonEpisode string
	}{
		seasonOneGroup: {title: "Cross Season (2026) S01 E01 已入库", seasonEpisode: "S01 E01"},
		seasonTwoGroup: {title: "Cross Season (2026) S02 E03 已入库", seasonEpisode: "S02 E03"},
	}
	var eventCount, requestCount int
	if err := store.db.Pool.QueryRow(t.Context(), `
		SELECT count(*) FROM notification_events WHERE batch_id=$1`, batch.ID).Scan(&eventCount); err != nil {
		t.Fatal(err)
	}
	if err := store.db.Pool.QueryRow(t.Context(), `
		SELECT count(*) FROM notifications WHERE batch_id=$1`, batch.ID).Scan(&requestCount); err != nil {
		t.Fatal(err)
	}
	if eventCount != 2 || requestCount != 2 {
		t.Fatalf("persisted event/request counts = %d/%d, want 2/2", eventCount, requestCount)
	}

	for _, eventPlan := range notificationPlan.Events {
		eventPrefix := "organization-completed:" + batch.ID.String() + ":"
		if !strings.HasPrefix(eventPlan.Event.EventKey, eventPrefix) {
			t.Fatalf("unexpected planned event key %q", eventPlan.Event.EventKey)
		}
		groupKey := strings.TrimPrefix(eventPlan.Event.EventKey, eventPrefix)
		want, exists := expectedGroups[groupKey]
		if !exists || len(eventPlan.Requests) != 1 {
			t.Fatalf("unexpected planned event = %#v", eventPlan)
		}
		var (
			eventID    int64
			eventBatch uuid.UUID
			eventKey   string
			title      string
			note       []byte
			published  *time.Time
		)
		if err := store.db.Pool.QueryRow(t.Context(), `
			SELECT id, batch_id, event_key, title, note, published_at
			FROM notification_events WHERE batch_id=$1 AND event_key=$2`,
			batch.ID, eventPlan.Event.EventKey,
		).Scan(&eventID, &eventBatch, &eventKey, &title, &note, &published); err != nil {
			t.Fatalf("query notification event %q: %v", eventPlan.Event.EventKey, err)
		}
		if eventBatch != batch.ID || eventKey != eventPrefix+groupKey ||
			title != want.title || published == nil {
			t.Fatalf("persisted event = batch %s key %q title %q published %v",
				eventBatch, eventKey, title, published)
		}
		var contextNote organization.V2OrganizeSuccessContext
		if err := json.Unmarshal(note, &contextNote); err != nil {
			t.Fatalf("decode event note for %s: %v", groupKey, err)
		}
		if contextNote.SeasonEpisode != want.seasonEpisode || contextNote.TitleYear != media.TitleYear ||
			contextNote.Username != username || contextNote.FileCount != 1 {
			t.Fatalf("event note for %s = %#v", groupKey, contextNote)
		}

		plannedRequest := eventPlan.Requests[0]
		var (
			storedRequestID uuid.UUID
			storedEventID   int64
			storedBatchID   uuid.UUID
			storedUsername  string
			storedChannel   notification.Channel
			storedSource    string
			storedState     notificationflow.State
			payload         []byte
		)
		if err := store.db.Pool.QueryRow(t.Context(), `
			SELECT id, event_id, batch_id, username, channel, source, state, payload
			FROM notifications WHERE id=$1`, plannedRequest.ID).Scan(
			&storedRequestID, &storedEventID, &storedBatchID, &storedUsername,
			&storedChannel, &storedSource, &storedState, &payload,
		); err != nil {
			t.Fatalf("query notification request %s: %v", plannedRequest.ID, err)
		}
		if storedRequestID != plannedRequest.ID || storedEventID != eventID || storedBatchID != batch.ID ||
			storedUsername != username || storedChannel != notification.ChannelTelegram ||
			storedSource != "season-telegram" || storedState != notificationflow.StatePending {
			t.Fatalf("persisted request = id %s event %d batch %s username %q target %q/%q state %q",
				storedRequestID, storedEventID, storedBatchID, storedUsername,
				storedChannel, storedSource, storedState)
		}
		var message notification.Message
		if err := json.Unmarshal(payload, &message); err != nil {
			t.Fatalf("decode request payload %s: %v", storedRequestID, err)
		}
		if message.Title != want.title || message.Username != username ||
			message.Channel != notification.ChannelTelegram || message.Source != "season-telegram" {
			t.Fatalf("request payload for %s = %#v", groupKey, message)
		}
	}

	listedDue, err := store.ListDueNotifications(t.Context(), time.Now().UTC().Add(time.Second), 10)
	if err != nil || !sameCrossSeasonNotificationIDs(listedDue, wantDue) {
		t.Fatalf("ListDueNotifications() = %v, %v; want %v", listedDue, err, wantDue)
	}

	sentRequestID := notificationPlan.Events[0].Requests[0].ID
	claimAt := time.Now().UTC().Add(time.Second)
	lease, found, err := store.ClaimNotification(t.Context(), sentRequestID, claimAt, time.Minute)
	if err != nil || !found || !lease.Valid(sentRequestID) {
		t.Fatalf("ClaimNotification() = %#v, %v, %v", lease, found, err)
	}
	if err := store.CompleteNotification(t.Context(), sentRequestID, lease.Token, claimAt); err != nil {
		t.Fatalf("CompleteNotification(): %v", err)
	}

	replayedRequestID, replayedCreated, replayedScrapeDue, replayedNotificationDue, err :=
		store.CompleteOrganizationBatchWithSideEffects(
			t.Context(), batch.ID, claimed.LeaseToken, mediaindex.CompletionPlan{},
			scrapeflow.Plan{}, notificationPlan,
		)
	if err != nil || replayedRequestID != uuid.Nil || replayedCreated ||
		len(replayedScrapeDue) != 0 || len(replayedNotificationDue) != 0 {
		t.Fatalf("replayed CompleteOrganizationBatchWithSideEffects() = %s, %v, %v, %v, %v",
			replayedRequestID, replayedCreated, replayedScrapeDue, replayedNotificationDue, err)
	}

	var sentState, pendingState notificationflow.State
	if err := store.db.Pool.QueryRow(t.Context(), `
		SELECT state FROM notifications WHERE id=$1`, sentRequestID).Scan(&sentState); err != nil {
		t.Fatal(err)
	}
	pendingRequestID := notificationPlan.Events[1].Requests[0].ID
	if err := store.db.Pool.QueryRow(t.Context(), `
		SELECT state FROM notifications WHERE id=$1`, pendingRequestID).Scan(&pendingState); err != nil {
		t.Fatal(err)
	}
	if sentState != notificationflow.StateSent || pendingState != notificationflow.StatePending {
		t.Fatalf("states after replay = %q/%q, want sent/pending", sentState, pendingState)
	}
	if err := store.db.Pool.QueryRow(t.Context(), `
		SELECT count(*) FROM notification_events WHERE batch_id=$1`, batch.ID).Scan(&eventCount); err != nil {
		t.Fatal(err)
	}
	if err := store.db.Pool.QueryRow(t.Context(), `
		SELECT count(*) FROM notifications WHERE batch_id=$1`, batch.ID).Scan(&requestCount); err != nil {
		t.Fatal(err)
	}
	if eventCount != 2 || requestCount != 2 {
		t.Fatalf("replayed event/request counts = %d/%d, want 2/2", eventCount, requestCount)
	}

	conflictingPlan := notificationPlan.Clone()
	conflictingPreview := true
	if preview := conflictingPlan.Events[1].Requests[0].Message.DisableWebPagePreview; preview != nil {
		conflictingPreview = !*preview
	}
	conflictingPlan.Events[1].Requests[0].Message.DisableWebPagePreview = &conflictingPreview
	if !conflictingPlan.Valid() {
		t.Fatal("conflicting notification plan is invalid")
	}
	var titleBefore string
	var payloadBefore []byte
	if err := store.db.Pool.QueryRow(t.Context(), `
		SELECT title FROM notification_events WHERE batch_id=$1 AND event_key=$2`,
		batch.ID, notificationPlan.Events[1].Event.EventKey).Scan(&titleBefore); err != nil {
		t.Fatal(err)
	}
	if err := store.db.Pool.QueryRow(t.Context(), `
		SELECT payload FROM notifications WHERE id=$1`, pendingRequestID).Scan(&payloadBefore); err != nil {
		t.Fatal(err)
	}

	conflictRequestID, conflictCreated, conflictScrapeDue, conflictNotificationDue, conflictErr :=
		store.CompleteOrganizationBatchWithSideEffects(
			t.Context(), batch.ID, claimed.LeaseToken, mediaindex.CompletionPlan{},
			scrapeflow.Plan{}, conflictingPlan,
		)
	if !errors.Is(conflictErr, ErrOrganizationStateConflict) || conflictRequestID != uuid.Nil ||
		conflictCreated || len(conflictScrapeDue) != 0 || len(conflictNotificationDue) != 0 {
		t.Fatalf("conflicting replay = %s, %v, %v, %v, %v; want state conflict",
			conflictRequestID, conflictCreated, conflictScrapeDue, conflictNotificationDue, conflictErr)
	}

	var titleAfter string
	var payloadAfter []byte
	if err := store.db.Pool.QueryRow(t.Context(), `
		SELECT title FROM notification_events WHERE batch_id=$1 AND event_key=$2`,
		batch.ID, notificationPlan.Events[1].Event.EventKey).Scan(&titleAfter); err != nil {
		t.Fatal(err)
	}
	if err := store.db.Pool.QueryRow(t.Context(), `
		SELECT payload FROM notifications WHERE id=$1`, pendingRequestID).Scan(&payloadAfter); err != nil {
		t.Fatal(err)
	}
	if titleAfter != titleBefore || !bytes.Equal(payloadAfter, payloadBefore) {
		t.Fatalf("conflicting replay rewrote persisted projection: title %q -> %q, payload %s -> %s",
			titleBefore, titleAfter, payloadBefore, payloadAfter)
	}
}

func TestOrganizationPostgresCompletedReplayRequiresPersistedSideEffectPlans(t *testing.T) {
	tests := []struct {
		name             string
		initialEmpty     bool
		omitMediaIndex   bool
		omitScrape       bool
		omitNotification bool
		wantConflict     bool
	}{
		{name: "完整计划重放成功"},
		{name: "省略媒体索引计划冲突", omitMediaIndex: true, wantConflict: true},
		{name: "省略刮削计划冲突", omitScrape: true, wantConflict: true},
		{name: "省略通知计划冲突", omitNotification: true, wantConflict: true},
		{name: "全空计划重放成功", initialEmpty: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			store := newSiteStatisticsIntegrationStore(t)
			fixtureName := "completed-replay-" + uuid.NewString()
			username := fixtureName + "-owner"
			fixture := createMediaIndexIntegrationFixture(t, store, fixtureName, 1, false)

			indexPlan := fixture.completion
			scrapePlan := reliabilityScrapePlan(fixture, username, true)
			notificationPlan := reliabilityNotificationPlan(t, fixture, username, "replay-telegram")
			if tt.initialEmpty {
				indexPlan = mediaindex.CompletionPlan{}
				scrapePlan = scrapeflow.Plan{}
				notificationPlan = notificationflow.Plan{}
			}
			if _, _, _, _, err := store.CompleteOrganizationBatchWithSideEffects(
				t.Context(), fixture.batch.ID, fixture.batch.LeaseToken,
				indexPlan, scrapePlan, notificationPlan,
			); err != nil {
				t.Fatalf("initial completion: %v", err)
			}

			replayIndex := indexPlan
			replayScrape := scrapePlan
			replayNotification := notificationPlan
			if tt.omitMediaIndex {
				replayIndex = mediaindex.CompletionPlan{}
			}
			if tt.omitScrape {
				replayScrape = scrapeflow.Plan{}
			}
			if tt.omitNotification {
				replayNotification = notificationflow.Plan{}
			}

			requestID, created, scrapeDue, notificationDue, err :=
				store.CompleteOrganizationBatchWithSideEffects(
					t.Context(), fixture.batch.ID, fixture.batch.LeaseToken,
					replayIndex, replayScrape, replayNotification,
				)
			if tt.wantConflict {
				if !errors.Is(err, ErrOrganizationStateConflict) || requestID != uuid.Nil || created ||
					len(scrapeDue) != 0 || len(notificationDue) != 0 {
					t.Fatalf("conflicting replay = %s, %v, %v, %v, %v",
						requestID, created, scrapeDue, notificationDue, err)
				}
				return
			}
			wantRequestID := uuid.Nil
			if indexPlan.RequiredServer != nil {
				wantRequestID = mediaIndexRequestID(fixture.batch.ID, uuid.MustParse(indexPlan.RequiredServer.ID))
			}
			if err != nil || requestID != wantRequestID || created ||
				len(scrapeDue) != 0 || len(notificationDue) != 0 {
				t.Fatalf("successful replay = %s, %v, %v, %v, %v; want request %s",
					requestID, created, scrapeDue, notificationDue, err, wantRequestID)
			}
		})
	}
}

type crossSeasonNotificationTargetProvider []notification.Target

func (p crossSeasonNotificationTargetProvider) Targets(_ notification.Message) []notification.Target {
	return append([]notification.Target(nil), p...)
}

func sameCrossSeasonNotificationIDs(left, right []uuid.UUID) bool {
	if len(left) != len(right) {
		return false
	}
	want := make(map[uuid.UUID]int, len(right))
	for _, id := range right {
		want[id]++
	}
	for _, id := range left {
		want[id]--
		if want[id] < 0 {
			return false
		}
	}
	return true
}
