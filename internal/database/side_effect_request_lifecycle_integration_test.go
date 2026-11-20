//go:build integration

package database

import (
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/moviepilot-go/moviepilot-go/internal/notificationflow"
	"github.com/moviepilot-go/moviepilot-go/internal/scrapeflow"
)

type sideEffectRequestFixtureIntegration struct {
	scrapeID       uuid.UUID
	notificationID uuid.UUID
}

// newSideEffectRequestFixtureIntegration 用媒体索引夹具完成一次带侧效的批次收尾，
// 同时产出一条 NFO 抓取请求与一条通知投递请求。
func newSideEffectRequestFixtureIntegration(
	t *testing.T,
	store *Store,
	name string,
) sideEffectRequestFixtureIntegration {
	t.Helper()
	username := name + "-owner"
	fixture := createMediaIndexIntegrationFixture(t, store, name, 1, false)
	fixture.completion.RequiredServer = nil
	scrapePlan := scrapeflow.Plan{
		BatchID: fixture.batch.ID, IntentID: fixture.intentID, Username: username,
		Requests: []scrapeflow.RequestPlan{{
			ID:        uuid.NewSHA1(fixture.batch.ID, []byte("lifecycle-scrape")),
			Kind:      scrapeflow.KindMovie,
			MediaPath: fixture.completion.Files[0].LocalPath,
			TMDBID:    42,
		}},
	}
	_, _, scrapeIDs, notificationIDs, err := store.CompleteOrganizationBatchWithSideEffects(
		t.Context(), fixture.batch.ID, fixture.batch.LeaseToken, fixture.completion,
		scrapePlan, reliabilityNotificationPlan(t, fixture, username, "lifecycle-notification"),
	)
	if err != nil {
		t.Fatalf("CompleteOrganizationBatchWithSideEffects(%s) error = %v", name, err)
	}
	if len(scrapeIDs) != 1 || len(notificationIDs) != 1 {
		t.Fatalf("侧效请求 = (scrape %d, notification %d), want 1/1", len(scrapeIDs), len(notificationIDs))
	}
	return sideEffectRequestFixtureIntegration{scrapeID: scrapeIDs[0], notificationID: notificationIDs[0]}
}

// TestScrapeRequestLifecycleIntegration 覆盖 NFO 抓取请求的完成、重排与过期租约回收。
func TestScrapeRequestLifecycleIntegration(t *testing.T) {
	store := newSiteStatisticsIntegrationStore(t)
	ctx := t.Context()
	base := time.Now().UTC()

	// 完成：状态 completed、清空租约与错误。
	completed := newSideEffectRequestFixtureIntegration(t, store, "scrape-complete")
	lease, claimed, err := store.ClaimScrape(ctx, completed.scrapeID, base.Add(2*time.Second), time.Minute)
	if err != nil || !claimed {
		t.Fatalf("ClaimScrape(complete) = (%#v, %v, %v)", lease, claimed, err)
	}
	if lease.Token == uuid.Nil || lease.Request.ID != completed.scrapeID || lease.Request.AttemptCount != 1 {
		t.Fatalf("租约内容 = %#v", lease)
	}
	if err := store.CompleteScrape(ctx, completed.scrapeID, lease.Token, base.Add(3*time.Second)); err != nil {
		t.Fatalf("CompleteScrape() error = %v", err)
	}
	assertSideEffectRequestStateIntegration(t, store, "scrape_requests", completed.scrapeID, "completed", "")

	// 重排：写回 failed + last_error + 下次尝试时间。
	rescheduled := newSideEffectRequestFixtureIntegration(t, store, "scrape-reschedule")
	lease, claimed, err = store.ClaimScrape(ctx, rescheduled.scrapeID, base.Add(2*time.Second), time.Minute)
	if err != nil || !claimed {
		t.Fatalf("ClaimScrape(reschedule) = (%#v, %v, %v)", lease, claimed, err)
	}
	if err := store.RescheduleScrape(ctx, rescheduled.scrapeID, lease.Token,
		base.Add(time.Minute), "  抓取超时  "); err != nil {
		t.Fatalf("RescheduleScrape() error = %v", err)
	}
	assertSideEffectRequestStateIntegration(t, store, "scrape_requests", rescheduled.scrapeID, "failed", "抓取超时")

	// 过期租约回收：把 working 行按 lease_expired 放回失败队列。
	expired := newSideEffectRequestFixtureIntegration(t, store, "scrape-expire")
	lease, claimed, err = store.ClaimScrape(ctx, expired.scrapeID, base.Add(2*time.Second), time.Second)
	if err != nil || !claimed {
		t.Fatalf("ClaimScrape(expire) = (%#v, %v, %v)", lease, claimed, err)
	}
	recovered, err := store.RecoverExpiredScrapes(ctx, base.Add(10*time.Second), 50)
	if err != nil {
		t.Fatalf("RecoverExpiredScrapes() error = %v", err)
	}
	if recovered < 1 {
		t.Fatalf("回收数量 = %d, want >= 1", recovered)
	}
	assertSideEffectRequestStateIntegration(t, store, "scrape_requests", expired.scrapeID, "failed", "lease_expired")

	// 入参校验。
	if err := store.CompleteScrape(ctx, uuid.Nil, uuid.Nil, base); !errors.Is(err, scrapeflow.ErrInvalidPlan) {
		t.Fatalf("零值 CompleteScrape error = %v", err)
	}
	if err := store.RescheduleScrape(ctx, completed.scrapeID, uuid.New(), base, "  "); !errors.Is(err, scrapeflow.ErrInvalidPlan) {
		t.Fatalf("空错误原因 RescheduleScrape error = %v", err)
	}
	if _, err := store.RecoverExpiredScrapes(ctx, time.Time{}, 10); !errors.Is(err, scrapeflow.ErrInvalidPlan) {
		t.Fatalf("零值时间 RecoverExpiredScrapes error = %v", err)
	}
	if _, err := store.RecoverExpiredScrapes(ctx, base, 0); !errors.Is(err, scrapeflow.ErrInvalidPlan) {
		t.Fatalf("非法 limit RecoverExpiredScrapes error = %v", err)
	}
}

// TestNotificationRequestLifecycleIntegration 覆盖通知投递请求的重排与过期租约回收。
func TestNotificationRequestLifecycleIntegration(t *testing.T) {
	store := newSiteStatisticsIntegrationStore(t)
	ctx := t.Context()
	base := time.Now().UTC()

	rescheduled := newSideEffectRequestFixtureIntegration(t, store, "notification-reschedule")
	lease, claimed, err := store.ClaimNotification(ctx, rescheduled.notificationID, base.Add(2*time.Second), time.Minute)
	if err != nil || !claimed {
		t.Fatalf("ClaimNotification(reschedule) = (%#v, %v, %v)", lease, claimed, err)
	}
	if lease.Token == uuid.Nil || lease.Request.ID != rescheduled.notificationID {
		t.Fatalf("通知租约内容 = %#v", lease)
	}
	if err := store.RescheduleNotification(ctx, rescheduled.notificationID, lease.Token,
		base.Add(time.Minute), "  发送通道不可用  "); err != nil {
		t.Fatalf("RescheduleNotification() error = %v", err)
	}
	assertSideEffectRequestStateIntegration(t, store, "notifications", rescheduled.notificationID, "failed", "发送通道不可用")

	expired := newSideEffectRequestFixtureIntegration(t, store, "notification-expire")
	lease, claimed, err = store.ClaimNotification(ctx, expired.notificationID, base.Add(2*time.Second), time.Second)
	if err != nil || !claimed {
		t.Fatalf("ClaimNotification(expire) = (%#v, %v, %v)", lease, claimed, err)
	}
	recovered, err := store.RecoverExpiredNotifications(ctx, base.Add(10*time.Second), 50)
	if err != nil {
		t.Fatalf("RecoverExpiredNotifications() error = %v", err)
	}
	if recovered < 1 {
		t.Fatalf("回收数量 = %d, want >= 1", recovered)
	}
	assertSideEffectRequestStateIntegration(t, store, "notifications", expired.notificationID, "failed", "lease_expired")

	if err := store.RescheduleNotification(ctx, uuid.Nil, uuid.Nil, base, "x"); !errors.Is(err, notificationflow.ErrInvalidPlan) {
		t.Fatalf("零值 RescheduleNotification error = %v", err)
	}
	if _, err := store.RecoverExpiredNotifications(ctx, time.Time{}, 1); !errors.Is(err, notificationflow.ErrInvalidPlan) {
		t.Fatalf("零值时间 RecoverExpiredNotifications error = %v", err)
	}
	if _, err := store.RecoverExpiredNotifications(ctx, base, 100000); !errors.Is(err, notificationflow.ErrInvalidPlan) {
		t.Fatalf("超限 limit RecoverExpiredNotifications error = %v", err)
	}
}

func assertSideEffectRequestStateIntegration(
	t *testing.T,
	store *Store,
	table string,
	id uuid.UUID,
	wantState, wantError string,
) {
	t.Helper()
	var state, lastError string
	query := `SELECT state, last_error FROM ` + table + ` WHERE id=$1`
	if err := store.db.Pool.QueryRow(t.Context(), query, id).Scan(&state, &lastError); err != nil {
		t.Fatalf("读取 %s 状态: %v", table, err)
	}
	if state != wantState || lastError != wantError {
		t.Fatalf("%s 状态 = (%q, %q), want (%q, %q)", table, state, lastError, wantState, wantError)
	}
}
