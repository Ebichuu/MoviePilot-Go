package notificationflow

import (
	"fmt"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/moviepilot-go/moviepilot-go/internal/notification"
	"github.com/moviepilot-go/moviepilot-go/internal/notificationhistory"
)

func TestPlanCloneDetachesEventsAndRequests(t *testing.T) {
	t.Parallel()

	disablePreview := false
	batchID := uuid.New()
	original := Plan{
		BatchID:  batchID,
		Username: "alice",
		Events: []EventPlan{{
			Event: notificationHistoryPlan("organization-completed:" + batchID.String()),
			Requests: []RequestPlan{{
				ID:             uuid.New(),
				Channel:        notification.ChannelTelegram,
				Source:         "Primary",
				IdempotencyKey: "notification-key",
				Message: notification.Message{
					Channel:               notification.ChannelTelegram,
					Source:                "Primary",
					Title:                 "title",
					Text:                  "text",
					DisableWebPagePreview: &disablePreview,
				},
			}},
		}},
	}

	cloned := original.Clone()
	if &original.Events[0] == &cloned.Events[0] ||
		&original.Events[0].Requests[0] == &cloned.Events[0].Requests[0] {
		t.Fatal("cloned event or request shares slice storage with the original")
	}
	if original.Events[0].Event.Title == cloned.Events[0].Event.Title ||
		&original.Events[0].Event.Note[0] == &cloned.Events[0].Event.Note[0] {
		t.Fatal("cloned history event shares pointer or note storage with the original")
	}
	if original.Events[0].Requests[0].Message.DisableWebPagePreview ==
		cloned.Events[0].Requests[0].Message.DisableWebPagePreview {
		t.Fatal("cloned preview option shares its pointer with the original")
	}

	*original.Events[0].Event.Title = "changed"
	original.Events[0].Event.Note[0] = '['
	*original.Events[0].Requests[0].Message.DisableWebPagePreview = true
	if got := *cloned.Events[0].Event.Title; got != "title" {
		t.Fatalf("cloned history title = %q, want title", got)
	}
	if got := string(cloned.Events[0].Event.Note); got != `{}` {
		t.Fatalf("cloned history note = %q, want {}", got)
	}
	if got := *cloned.Events[0].Requests[0].Message.DisableWebPagePreview; got {
		t.Fatalf("cloned preview option changed with original: got %v, want false", got)
	}
}

func TestPlanValidMultiEventOwnershipAndGlobalUniqueness(t *testing.T) {
	t.Parallel()

	batchID := uuid.New()
	firstEventKey := groupedEventKey(batchID, 1)
	secondEventKey := groupedEventKey(batchID, 2)
	firstRequest := validRequestPlan(batchID, firstEventKey, "Primary", "alice")
	secondRequest := validRequestPlan(batchID, secondEventKey, "Primary", "alice")
	base := Plan{
		BatchID:  batchID,
		Username: "alice",
		Events: []EventPlan{
			{
				Event:    notificationHistoryPlan(firstEventKey),
				Requests: []RequestPlan{firstRequest},
			},
			{
				Event:    notificationHistoryPlan(secondEventKey),
				Requests: []RequestPlan{secondRequest},
			},
		},
	}
	tests := []struct {
		name   string
		mutate func(*Plan)
		want   bool
	}{
		{name: "跨事件可复用渠道来源", want: true},
		{name: "拒绝 grouped 消息空用户名", mutate: func(plan *Plan) {
			plan.Events[0].Requests[0].Message.Username = ""
		}},
		{name: "拒绝不属于批次的事件键", mutate: func(plan *Plan) {
			plan.Events[0].Event.EventKey = groupedEventKey(uuid.New(), 1)
		}},
		{name: "拒绝批次 ID 只是键后缀的一部分", mutate: func(plan *Plan) {
			plan.Events[0].Event.EventKey = "organization-completed:other:" + batchID.String()
		}},
		{name: "拒绝跨事件交换请求", mutate: func(plan *Plan) {
			plan.Events[0].Requests[0], plan.Events[1].Requests[0] =
				plan.Events[1].Requests[0], plan.Events[0].Requests[0]
		}},
		{name: "拒绝请求标题与事件投影不一致", mutate: func(plan *Plan) {
			plan.Events[0].Requests[0].Message.Title = "other title"
		}},
		{name: "拒绝请求正文与事件投影不一致", mutate: func(plan *Plan) {
			plan.Events[0].Requests[0].Message.Text = "other text"
		}},
		{name: "拒绝请求图片与事件投影不一致", mutate: func(plan *Plan) {
			image := "https://example.com/history.jpg"
			plan.Events[0].Event.Image = &image
			plan.Events[0].Requests[0].Message.Image = "https://example.com/delivery.jpg"
		}},
		{name: "拒绝请求链接与事件投影不一致", mutate: func(plan *Plan) {
			link := "https://example.com/history"
			plan.Events[0].Event.Link = &link
			plan.Events[0].Requests[0].Message.Link = "https://example.com/delivery"
		}},
		{name: "拒绝请求 mtype 与事件投影不一致", mutate: func(plan *Plan) {
			plan.Events[0].Requests[0].Message.Type = notification.TypeDownload
		}},
		{name: "拒绝请求 userid 与事件投影不一致", mutate: func(plan *Plan) {
			plan.Events[0].Requests[0].Message.UserID = "alice"
		}},
		{name: "拒绝重复事件键", mutate: func(plan *Plan) {
			plan.Events[1].Event.EventKey = plan.Events[0].Event.EventKey
		}},
		{name: "拒绝跨事件重复请求 ID", mutate: func(plan *Plan) {
			plan.Events[1].Requests[0].ID = plan.Events[0].Requests[0].ID
		}},
		{name: "拒绝跨事件重复幂等键", mutate: func(plan *Plan) {
			plan.Events[1].Requests[0].IdempotencyKey = plan.Events[0].Requests[0].IdempotencyKey
		}},
		{name: "拒绝同事件重复渠道来源", mutate: func(plan *Plan) {
			duplicate := plan.Events[0].Requests[0]
			plan.Events[0].Requests = append(plan.Events[0].Requests, duplicate)
		}},
		{name: "拒绝其他消息用户名", mutate: func(plan *Plan) {
			plan.Events[0].Requests[0].Message.Username = "bob"
		}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			plan := base.Clone()
			if tt.mutate != nil {
				tt.mutate(&plan)
			}
			if got := plan.Valid(); got != tt.want {
				t.Fatalf("Plan.Valid() = %v, want %v; plan=%#v", got, tt.want, plan)
			}
		})
	}
}

func TestClassifyOrganizationEventKeyStrictGrammar(t *testing.T) {
	t.Parallel()

	batchID := uuid.MustParse("11111111-2222-3333-8444-555555555555")
	legacyKey := organizationEventKeyPrefix + batchID.String()
	groupedKey := groupedEventKey(batchID, 1)
	tests := []struct {
		name       string
		key        string
		wantLegacy bool
		wantValid  bool
	}{
		{name: "legacy 精确键", key: legacyKey, wantLegacy: true, wantValid: true},
		{name: "grouped 精确键", key: groupedKey, wantValid: true},
		{name: "拒绝其他前缀", key: "organize-success:" + batchID.String()},
		{name: "拒绝空分组", key: legacyKey + groupedEventKeyMarker},
		{name: "拒绝旧式任意分组", key: legacyKey + ":group-a"},
		{name: "拒绝大写摘要", key: legacyKey + groupedEventKeyMarker + strings.Repeat("A", sha256HexLength)},
		{name: "拒绝短摘要", key: legacyKey + groupedEventKeyMarker + strings.Repeat("a", sha256HexLength-1)},
		{name: "拒绝非十六进制摘要", key: legacyKey + groupedEventKeyMarker + strings.Repeat("g", sha256HexLength)},
		{name: "拒绝其他批次", key: groupedEventKey(uuid.New(), 1)},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			legacy, valid := classifyOrganizationEventKey(tt.key, batchID)
			if legacy != tt.wantLegacy || valid != tt.wantValid {
				t.Fatalf("classifyOrganizationEventKey(%q) = (%v, %v), want (%v, %v)",
					tt.key, legacy, valid, tt.wantLegacy, tt.wantValid)
			}
		})
	}
}

func TestPlanValidRequiresCompleteHistoryWithoutDeliveryTargets(t *testing.T) {
	t.Parallel()

	batchID := uuid.New()
	base := Plan{
		BatchID: batchID, Username: "alice",
		Events: []EventPlan{{Event: notificationHistoryPlan(groupedEventKey(batchID, 1))}},
	}
	tests := []struct {
		name   string
		mutate func(*Plan)
		want   bool
	}{
		{name: "完整历史无需 delivery 仍有效", want: true},
		{name: "拒绝缺少 mtype", mutate: func(plan *Plan) { plan.Events[0].Event.MType = nil }},
		{name: "拒绝缺少标题", mutate: func(plan *Plan) { plan.Events[0].Event.Title = nil }},
		{name: "拒绝缺少正文", mutate: func(plan *Plan) { plan.Events[0].Event.Text = nil }},
		{name: "拒绝历史渠道", mutate: func(plan *Plan) {
			plan.Events[0].Event.Channel = string(notification.ChannelTelegram)
		}},
		{name: "拒绝历史来源", mutate: func(plan *Plan) {
			source := "Primary"
			plan.Events[0].Event.Source = &source
		}},
		{name: "拒绝历史 userid", mutate: func(plan *Plan) {
			userID := "alice"
			plan.Events[0].Event.UserID = &userID
		}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			plan := base.Clone()
			if tt.mutate != nil {
				tt.mutate(&plan)
			}
			if got := plan.Valid(); got != tt.want {
				t.Fatalf("Plan.Valid() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestPlanValidBoundsEvents(t *testing.T) {
	t.Parallel()

	batchID := uuid.New()
	valid := Plan{BatchID: batchID, Username: "alice"}
	if !valid.Valid() {
		t.Fatal("empty bounded event set should be valid")
	}
	valid.Events = make([]EventPlan, MaxEventsPerPlan)
	for index := range valid.Events {
		valid.Events[index].Event = notificationHistoryPlan(groupedEventKey(batchID, index))
	}
	if !valid.Valid() {
		t.Fatalf("Plan.Valid() rejected %d events", MaxEventsPerPlan)
	}
	valid.Events = append(valid.Events, EventPlan{})
	if valid.Valid() {
		t.Fatalf("Plan.Valid() accepted more than %d events", MaxEventsPerPlan)
	}
}

func TestPlanValidBoundsRequests(t *testing.T) {
	t.Parallel()

	atLimit := requestBoundPlan(uuid.New(), MaxRequestsPerPlan)
	if !atLimit.Valid() {
		t.Fatalf("Plan.Valid() rejected %d total requests", MaxRequestsPerPlan)
	}
	overTotal := requestBoundPlan(uuid.New(), MaxRequestsPerPlan+1)
	if overTotal.Valid() {
		t.Fatalf("Plan.Valid() accepted more than %d total requests", MaxRequestsPerPlan)
	}

	batchID := uuid.New()
	eventKey := groupedEventKey(batchID, 1)
	overEvent := Plan{
		BatchID: batchID, Username: "alice",
		Events: []EventPlan{{
			Event:    notificationHistoryPlan(eventKey),
			Requests: validRequests(batchID, eventKey, MaxRequestsPerEvent+1, "alice"),
		}},
	}
	if overEvent.Valid() {
		t.Fatalf("Plan.Valid() accepted more than %d requests for one event", MaxRequestsPerEvent)
	}
}

func TestPlanValidBoundsPersistedStringsAndNote(t *testing.T) {
	t.Parallel()

	batchID := uuid.New()
	eventKey := groupedEventKey(batchID, 1)
	base := Plan{
		BatchID: batchID, Username: "alice",
		Events: []EventPlan{{
			Event:    notificationHistoryPlan(eventKey),
			Requests: []RequestPlan{validRequestPlan(batchID, eventKey, "Primary", "alice")},
		}},
	}
	tests := []struct {
		name   string
		mutate func(*Plan)
	}{
		{name: "用户名按 UTF-8 字节超限", mutate: func(plan *Plan) {
			value := strings.Repeat("界", MaxUsernameBytes/3+1)
			plan.Username = value
			plan.Events[0].Requests[0].Message.Username = value
		}},
		{name: "用户名包含 NUL", mutate: func(plan *Plan) {
			plan.Username = "ali\x00ce"
			plan.Events[0].Requests[0].Message.Username = "ali\x00ce"
		}},
		{name: "来源超限", mutate: func(plan *Plan) {
			value := strings.Repeat("s", MaxSourceBytes+1)
			plan.Events[0].Requests[0].Source = value
			plan.Events[0].Requests[0].Message.Source = value
		}},
		{name: "来源包含 NUL", mutate: func(plan *Plan) {
			plan.Events[0].Requests[0].Source = "bad\x00source"
			plan.Events[0].Requests[0].Message.Source = "bad\x00source"
		}},
		{name: "标题超限", mutate: func(plan *Plan) {
			value := strings.Repeat("t", MaxTitleBytes+1)
			plan.Events[0].Event.Title = &value
			plan.Events[0].Requests[0].Message.Title = value
		}},
		{name: "标题包含 NUL", mutate: func(plan *Plan) {
			value := "bad\x00title"
			plan.Events[0].Event.Title = &value
			plan.Events[0].Requests[0].Message.Title = value
		}},
		{name: "正文超限", mutate: func(plan *Plan) {
			value := strings.Repeat("t", MaxTextBytes+1)
			plan.Events[0].Event.Text = &value
			plan.Events[0].Requests[0].Message.Text = value
		}},
		{name: "正文包含 NUL", mutate: func(plan *Plan) {
			value := "bad\x00text"
			plan.Events[0].Event.Text = &value
			plan.Events[0].Requests[0].Message.Text = value
		}},
		{name: "图片超限", mutate: func(plan *Plan) {
			value := strings.Repeat("i", MaxImageBytes+1)
			plan.Events[0].Event.Image = &value
			plan.Events[0].Requests[0].Message.Image = value
		}},
		{name: "图片包含 NUL", mutate: func(plan *Plan) {
			value := "https://example.com/\x00poster"
			plan.Events[0].Event.Image = &value
			plan.Events[0].Requests[0].Message.Image = value
		}},
		{name: "链接超限", mutate: func(plan *Plan) {
			value := strings.Repeat("l", MaxLinkBytes+1)
			plan.Events[0].Event.Link = &value
			plan.Events[0].Requests[0].Message.Link = value
		}},
		{name: "链接包含 NUL", mutate: func(plan *Plan) {
			value := "https://example.com/\x00history"
			plan.Events[0].Event.Link = &value
			plan.Events[0].Requests[0].Message.Link = value
		}},
		{name: "note 超限", mutate: func(plan *Plan) {
			plan.Events[0].Event.Note = []byte(strings.Repeat(" ", MaxNoteBytes+1))
		}},
		{name: "note 解码后包含 NUL", mutate: func(plan *Plan) {
			plan.Events[0].Event.Note = []byte(`{"value":"\u0000"}`)
		}},
	}
	if !base.Valid() {
		t.Fatal("base plan is invalid")
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			plan := base.Clone()
			tt.mutate(&plan)
			if plan.Valid() {
				t.Fatalf("Plan.Valid() accepted out-of-bound plan: %#v", plan)
			}
		})
	}
}

func notificationHistoryPlan(eventKey string) notificationhistory.Plan {
	mtype := string(notification.TypeOrganize)
	title := "title"
	text := "text"
	return notificationhistory.Plan{
		EventKey: eventKey,
		MType:    &mtype,
		Title:    &title,
		Text:     &text,
		Note:     []byte(`{}`),
	}
}

func validRequestPlan(batchID uuid.UUID, eventKey, source, username string) RequestPlan {
	target := notification.Target{Channel: notification.ChannelTelegram, Source: source}
	id, key, valid := organizationNotificationRequestIdentity(batchID, eventKey, target)
	if !valid {
		panic("invalid test notification identity")
	}
	return RequestPlan{
		ID:             id,
		Channel:        notification.ChannelTelegram,
		Source:         source,
		IdempotencyKey: key,
		Message: notification.Message{
			Channel:     notification.ChannelTelegram,
			Source:      source,
			Type:        notification.TypeOrganize,
			ContentType: notification.ContentOrganizeSuccess,
			Username:    username,
			Title:       "title",
			Text:        "text",
		},
	}
}

func groupedEventKey(batchID uuid.UUID, index int) string {
	return fmt.Sprintf("%s%s%s%064x", organizationEventKeyPrefix, batchID, groupedEventKeyMarker, index)
}

func validRequests(batchID uuid.UUID, eventKey string, count int, username string) []RequestPlan {
	requests := make([]RequestPlan, count)
	for index := range requests {
		requests[index] = validRequestPlan(
			batchID, eventKey, fmt.Sprintf("Primary-%05d", index), username,
		)
	}
	return requests
}

func requestBoundPlan(batchID uuid.UUID, requestCount int) Plan {
	plan := Plan{BatchID: batchID, Username: "alice"}
	remaining := requestCount
	for eventIndex := 0; remaining > 0; eventIndex++ {
		count := min(remaining, MaxRequestsPerEvent)
		eventKey := groupedEventKey(batchID, eventIndex+1)
		plan.Events = append(plan.Events, EventPlan{
			Event:    notificationHistoryPlan(eventKey),
			Requests: validRequests(batchID, eventKey, count, plan.Username),
		})
		remaining -= count
	}
	return plan
}
