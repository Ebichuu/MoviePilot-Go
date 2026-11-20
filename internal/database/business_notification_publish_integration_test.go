//go:build integration

package database

import (
	"errors"
	"strconv"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/moviepilot-go/moviepilot-go/internal/notification"
	"github.com/moviepilot-go/moviepilot-go/internal/notificationflow"
	"github.com/moviepilot-go/moviepilot-go/internal/notificationhistory"
)

// businessPlannerIntegration 是发布链路的最小规划器替身：按入参生成一条
// subscribe-added 事件，并提供「非法计划」模式用以命中校验分支。
type businessPlannerIntegration struct {
	invalid bool
	calls   int
}

func (p *businessPlannerIntegration) PlanBusiness(input notificationflow.BusinessInput) (notificationflow.BusinessPlan, error) {
	p.calls++
	source := "订阅来源"
	mtype := string(notification.TypeSubscribe)
	title := "订阅已添加"
	text := "订阅 " + input.EventKey + " 已加入队列"
	history := notificationhistory.Plan{
		EventKey: "subscribe-added:" + input.EventKey,
		Channel:  string(notification.ChannelTelegram),
		Source:   &source, MType: &mtype, Title: &title, Text: &text,
	}
	event := notificationflow.BusinessEvent{
		History: history,
		Requests: []notificationflow.BusinessRequest{{
			ID:             uuid.New(),
			Channel:        notification.ChannelTelegram,
			Source:         source,
			IdempotencyKey: history.EventKey,
			Message: notification.Message{
				Channel: notification.ChannelTelegram, Source: source,
				Type: notification.TypeSubscribe, ContentType: notification.ContentSubscribeAdded,
				Title: title, Text: text, Username: input.Username,
			},
		}},
	}
	events := []notificationflow.BusinessEvent{event}
	if p.invalid {
		// 两条事件应被 PublishNextBusinessNotification 的单事件约束拒绝。
		second := event
		second.History.EventKey = history.EventKey + ":dup"
		second.Requests[0].ID = uuid.New()
		second.Requests[0].IdempotencyKey = second.History.EventKey
		events = append(events, second)
	}
	return notificationflow.BusinessPlan{
		Namespace: uuid.NewSHA1(uuid.NameSpaceOID, []byte(input.EventKey)),
		Username:  input.Username,
		Events:    events,
	}, nil
}

// TestPublishNextBusinessNotificationIntegration 覆盖业务通知出箱的发布：
// 订阅创建写入 outbox，发布后产生事件与待投递请求并标记已发布；
// 无待发布记录时返回空。
func TestPublishNextBusinessNotificationIntegration(t *testing.T) {
	store := newSiteStatisticsIntegrationStore(t)
	const username = "business-outbox-owner"
	insertSubscriptionIntegrationUsers(t, store, username)
	ctx := t.Context()

	subscriptionID := createSubscriptionIntegrationFixture(t, store, username, "business-outbox")
	var outboxCount int
	if err := store.db.Pool.QueryRow(ctx, `
		SELECT count(*) FROM business_notification_outbox
		WHERE content_type='subscribeAdded' AND event_key=$1 AND published_at IS NULL`,
		strconv.FormatInt(subscriptionID, 10)).Scan(&outboxCount); err != nil {
		t.Fatalf("读取 outbox: %v", err)
	}
	if outboxCount != 1 {
		t.Fatalf("outbox 待发布条数 = %d, want 1", outboxCount)
	}

	planner := &businessPlannerIntegration{}
	due, err := store.PublishNextBusinessNotification(ctx, planner, time.Now().UTC())
	if err != nil {
		t.Fatalf("PublishNextBusinessNotification() error = %v", err)
	}
	if len(due) != 1 {
		t.Fatalf("待投递请求 = %d, want 1", len(due))
	}
	if planner.calls != 1 {
		t.Fatalf("规划器调用次数 = %d, want 1", planner.calls)
	}

	var eventKey string
	var published bool
	if err := store.db.Pool.QueryRow(ctx, `
		SELECT event_key, published_at IS NOT NULL
		FROM notification_events WHERE event_key LIKE 'subscribe-added:%'`).Scan(&eventKey, &published); err != nil {
		t.Fatalf("读取通知事件: %v", err)
	}
	if !published || eventKey != "subscribe-added:"+strconv.FormatInt(subscriptionID, 10) {
		t.Fatalf("通知事件 = (%q, published=%v)", eventKey, published)
	}
	assertSideEffectRequestStateIntegration(t, store, "notifications", due[0], "pending", "")

	var publishedOutbox int
	if err := store.db.Pool.QueryRow(ctx, `
		SELECT count(*) FROM business_notification_outbox
		WHERE content_type='subscribeAdded' AND event_key=$1 AND published_at IS NOT NULL`,
		strconv.FormatInt(subscriptionID, 10)).Scan(&publishedOutbox); err != nil {
		t.Fatalf("读取已发布 outbox: %v", err)
	}
	if publishedOutbox != 1 {
		t.Fatalf("已发布 outbox 条数 = %d, want 1", publishedOutbox)
	}

	// 没有待发布记录时是空操作。
	if due, err := store.PublishNextBusinessNotification(ctx, planner, time.Now().UTC()); err != nil || len(due) != 0 {
		t.Fatalf("重复发布 = (%v, %v), want 空", due, err)
	}

	// 入参校验与规划器失败分支。
	if _, err := store.PublishNextBusinessNotification(ctx, nil, time.Now().UTC()); !errors.Is(err, notificationflow.ErrInvalidPlan) {
		t.Fatalf("nil 规划器 error = %v", err)
	}
	if _, err := store.PublishNextBusinessNotification(ctx, planner, time.Time{}); !errors.Is(err, notificationflow.ErrInvalidPlan) {
		t.Fatalf("零值时间 error = %v", err)
	}

	// 多事件计划被拒绝，且 outbox 不会被标记发布。
	insertSecondOutboxIntegration(t, store, username, "999999")
	if _, err := store.PublishNextBusinessNotification(ctx, &businessPlannerIntegration{invalid: true}, time.Now().UTC()); !errors.Is(err, notificationflow.ErrInvalidPlan) {
		t.Fatalf("多事件计划 error = %v, want ErrInvalidPlan", err)
	}
}

func insertSecondOutboxIntegration(t *testing.T, store *Store, username, eventKey string) {
	t.Helper()
	if _, err := store.db.Pool.Exec(t.Context(), `
		INSERT INTO business_notification_outbox (
			event_key, content_type, username, context, image, route, userid
		) VALUES ($1, 'subscribeAdded', $2, '{}'::jsonb, '', '', '')
		ON CONFLICT (content_type, event_key) DO NOTHING`, eventKey, username); err != nil {
		t.Fatalf("插入第二条 outbox: %v", err)
	}
}
