package notificationflow

import (
	"encoding/json"
	"fmt"
	"reflect"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/moviepilot-go/moviepilot-go/internal/notification"
	"github.com/moviepilot-go/moviepilot-go/internal/organization"
	"github.com/moviepilot-go/moviepilot-go/internal/organizer"
)

func TestPlannerCreatesOneRequestPerTarget(t *testing.T) {
	t.Parallel()

	provider := &targetProviderFake{targets: []notification.Target{
		{Channel: notification.ChannelTelegram, Source: "家庭通知"},
		{Channel: notification.ChannelTelegram, Source: "管理通知"},
	}}
	planner, err := NewPlanner(provider)
	if err != nil {
		t.Fatal(err)
	}
	batchID := uuid.New()
	plan, err := planner.planWithCount(batchID, " alice ", 2)
	if err != nil {
		t.Fatal(err)
	}
	if !plan.Valid() || plan.BatchID != batchID || plan.Username != "alice" || len(plan.Events) != 1 ||
		len(plan.Events[0].Requests) != 2 {
		t.Fatalf("plan = %#v", plan)
	}
	event := plan.Events[0]
	if event.Event.EventKey != "organization-completed:"+batchID.String() ||
		event.Event.MType == nil || *event.Event.MType != string(notification.TypeOrganize) ||
		event.Event.Title == nil || *event.Event.Title != "整理完成" ||
		event.Event.Text == nil || *event.Event.Text != "已整理 2 个文件到媒体库" ||
		event.Event.PublishedAt != nil {
		t.Fatalf("history event = %#v", event.Event)
	}
	if provider.calls != 1 || provider.message.Channel != "" || provider.message.Source != "" ||
		provider.message.UserID != "" || provider.message.Username != "" ||
		provider.message.Type != notification.TypeOrganize ||
		provider.message.ContentType != notification.ContentOrganizeSuccess {
		t.Fatalf("provider base message = %#v, calls=%d", provider.message, provider.calls)
	}
	seenIDs := make(map[uuid.UUID]struct{}, 2)
	seenKeys := make(map[string]struct{}, 2)
	for index, request := range event.Requests {
		want := provider.targets[index]
		if request.Channel != want.Channel || request.Source != want.Source ||
			request.Message.Channel != want.Channel || request.Message.Source != want.Source ||
			request.Message.UserID != "" || request.Message.Username != "" {
			t.Fatalf("request[%d] = %#v", index, request)
		}
		seenIDs[request.ID] = struct{}{}
		seenKeys[request.IdempotencyKey] = struct{}{}
	}
	if len(seenIDs) != 2 || len(seenKeys) != 2 {
		t.Fatalf("deterministic identities are not source-specific: ids=%v keys=%v", seenIDs, seenKeys)
	}
}

func TestPlannerAcceptsAllConfiguredTargets(t *testing.T) {
	t.Parallel()

	const targetCount = 17
	targets := make([]notification.Target, targetCount)
	for index := range targets {
		targets[index] = notification.Target{
			Channel: notification.ChannelTelegram,
			Source:  fmt.Sprintf("Telegram-%02d", index+1),
		}
	}
	planner, err := NewPlanner(targetProviderFunc(func(notification.Message) []notification.Target {
		return targets
	}))
	if err != nil {
		t.Fatal(err)
	}

	plan, err := planner.planWithCount(uuid.New(), "alice", 1)
	if err != nil {
		t.Fatal(err)
	}
	if !plan.Valid() || len(plan.Events) != 1 || len(plan.Events[0].Requests) != targetCount {
		t.Fatalf("plan = %#v, want one event with %d requests", plan, targetCount)
	}
}

func TestPlannerEmptyTargetsProduceValidPlan(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		provider TargetProvider
	}{
		{name: "空配置", provider: targetProviderFunc(func(notification.Message) []notification.Target { return nil })},
		{name: "开关过滤后为空", provider: targetProviderFunc(func(message notification.Message) []notification.Target {
			if message.Type != notification.TypeOrganize {
				t.Fatalf("message type = %q", message.Type)
			}
			return []notification.Target{}
		})},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			planner, err := NewPlanner(tt.provider)
			if err != nil {
				t.Fatal(err)
			}
			plan, err := planner.planWithCount(uuid.New(), "alice", 0)
			if err != nil || !plan.Valid() || len(plan.Events) != 1 ||
				len(plan.Events[0].Requests) != 0 || plan.Events[0].Event.EventKey == "" {
				t.Fatalf("plan=%#v error=%v", plan, err)
			}
		})
	}
}

func TestPlannerIdentityIsDeterministicAndUsesCompleteSource(t *testing.T) {
	t.Parallel()

	longSource := strings.Repeat("配置:", 200)
	provider := targetProviderFunc(func(notification.Message) []notification.Target {
		return []notification.Target{
			{Channel: notification.ChannelTelegram, Source: "a:b"},
			{Channel: notification.ChannelTelegram, Source: "a"},
			{Channel: notification.ChannelTelegram, Source: longSource},
		}
	})
	planner, err := NewPlanner(provider)
	if err != nil {
		t.Fatal(err)
	}
	batchID := uuid.New()
	first, err := planner.planWithCount(batchID, "alice", 1)
	if err != nil {
		t.Fatal(err)
	}
	second, err := planner.planWithCount(batchID, "alice", 1)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(first, second) {
		t.Fatalf("same input produced different plans:\nfirst=%#v\nsecond=%#v", first, second)
	}
	seenIDs := make(map[uuid.UUID]struct{}, len(first.Events[0].Requests))
	seenKeys := make(map[string]struct{}, len(first.Events[0].Requests))
	for _, request := range first.Events[0].Requests {
		if len(request.IdempotencyKey) > 255 || !strings.HasPrefix(request.IdempotencyKey, "organization-completed:") {
			t.Fatalf("idempotency key = %q", request.IdempotencyKey)
		}
		seenIDs[request.ID] = struct{}{}
		seenKeys[request.IdempotencyKey] = struct{}{}
	}
	if len(seenIDs) != 3 || len(seenKeys) != 3 {
		t.Fatalf("source identity collision: ids=%v keys=%v", seenIDs, seenKeys)
	}
}

func TestPlannerLegacyIdentityGolden(t *testing.T) {
	t.Parallel()

	batchID := uuid.MustParse("11111111-2222-3333-8444-555555555555")
	planner, err := NewPlanner(targetProviderFunc(func(notification.Message) []notification.Target {
		return []notification.Target{{Channel: notification.ChannelTelegram, Source: "家庭通知"}}
	}))
	if err != nil {
		t.Fatal(err)
	}
	plan, err := planner.planWithCount(batchID, "alice", 1)
	if err != nil {
		t.Fatal(err)
	}
	request := plan.Events[0].Requests[0]
	if got, want := request.ID.String(), "8fd8e0ab-c3db-55b7-8af6-05a25f3147bb"; got != want {
		t.Fatalf("legacy request UUID = %q, want %q", got, want)
	}
	if got, want := request.IdempotencyKey,
		"organization-completed:7c569173443fb0e6d0245a88ba77bf01e361b87fd3a2b5597e51ace33de080e7"; got != want {
		t.Fatalf("legacy idempotency key = %q, want %q", got, want)
	}
}

func TestPlanValidUsesChannelAndSourceIdentity(t *testing.T) {
	t.Parallel()

	batchID := uuid.New()
	eventKey := organizationEventKeyPrefix + batchID.String()
	request := validRequestPlan(batchID, eventKey, "家庭通知", "")
	base := Plan{
		BatchID: batchID, Username: "alice",
		Events: []EventPlan{{
			Event:    notificationHistoryPlan(eventKey),
			Requests: []RequestPlan{request},
		}},
	}
	tests := []struct {
		name string
		plan Plan
		want bool
	}{
		{name: "单目标", plan: base, want: true},
		{name: "同渠道不同来源", plan: func() Plan {
			p := base.Clone()
			second := validRequestPlan(batchID, eventKey, "管理通知", "")
			p.Events[0].Requests = append(p.Events[0].Requests, second)
			return p
		}(), want: true},
		{name: "重复渠道来源", plan: func() Plan {
			p := base.Clone()
			second := request
			second.ID = uuid.New()
			second.IdempotencyKey = "key-b"
			p.Events[0].Requests = append(p.Events[0].Requests, second)
			return p
		}()},
		{name: "来源与消息不一致", plan: func() Plan {
			p := base.Clone()
			p.Events[0].Requests[0].Message.Source = "管理通知"
			return p
		}()},
		{name: "消息不应带持久 owner", plan: func() Plan {
			p := base.Clone()
			p.Events[0].Requests[0].Message.UserID = "alice"
			return p
		}()},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if got := tt.plan.Valid(); got != tt.want {
				t.Fatalf("Plan.Valid() = %v, want %v; plan=%#v", got, tt.want, tt.plan)
			}
		})
	}
}

func TestPlannerRejectsInvalidInputsAndTargets(t *testing.T) {
	t.Parallel()

	if _, err := NewPlanner(nil); err == nil {
		t.Fatal("NewPlanner(nil) error = nil")
	}
	validProvider := targetProviderFunc(func(notification.Message) []notification.Target {
		return []notification.Target{{Channel: notification.ChannelTelegram, Source: "家庭通知"}}
	})
	planner, err := NewPlanner(validProvider)
	if err != nil {
		t.Fatal(err)
	}
	for _, tt := range []struct {
		name      string
		batchID   uuid.UUID
		username  string
		fileCount int
	}{
		{name: "空 batch", username: "alice"},
		{name: "空 owner", batchID: uuid.New()},
		{name: "负文件数", batchID: uuid.New(), username: "alice", fileCount: -1},
	} {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if _, err := planner.planWithCount(tt.batchID, tt.username, tt.fileCount); err == nil {
				t.Fatal("planWithCount() error = nil")
			}
		})
	}

	invalidTargets := []struct {
		name    string
		targets []notification.Target
	}{
		{name: "无效渠道", targets: []notification.Target{{Channel: notification.Channel("invalid"), Source: "source"}}},
		{name: "重复目标", targets: []notification.Target{
			{Channel: notification.ChannelTelegram, Source: "source"},
			{Channel: notification.ChannelTelegram, Source: "source"},
		}},
	}
	for _, tt := range invalidTargets {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			invalidPlanner, newErr := NewPlanner(targetProviderFunc(func(notification.Message) []notification.Target {
				return tt.targets
			}))
			if newErr != nil {
				t.Fatal(newErr)
			}
			if _, planErr := invalidPlanner.planWithCount(uuid.New(), "alice", 1); planErr == nil {
				t.Fatal("planWithCount() error = nil")
			}
		})
	}
}

func TestBuildPlanCompatibilityUsesStaticTargets(t *testing.T) {
	t.Parallel()

	batchID := uuid.New()
	plan, err := BuildPlan(batchID, "alice", 2, []notification.Channel{
		notification.ChannelTelegram,
		notification.ChannelTelegram,
		notification.ChannelWebhook,
	})
	if err != nil {
		t.Fatal(err)
	}
	if !plan.Valid() || len(plan.Events) != 1 || len(plan.Events[0].Requests) != 2 {
		t.Fatalf("plan = %#v", plan)
	}
	for _, request := range plan.Events[0].Requests {
		if request.Source != "" || request.Message.Source != "" ||
			request.Message.Channel != request.Channel || request.Message.UserID != "" ||
			request.Message.Username != "" {
			t.Fatalf("request = %#v", request)
		}
	}

	if _, err := BuildPlan(batchID, "alice", -1, nil); err == nil {
		t.Fatal("negative file count error = nil")
	}
	if _, err := BuildPlan(batchID, "alice", 1, []notification.Channel{"invalid"}); err == nil {
		t.Fatal("invalid compatibility channel error = nil")
	}
}

func TestPlannerPlanUsesOrganizerOperationCount(t *testing.T) {
	t.Parallel()

	planner, err := NewPlanner(targetProviderFunc(func(notification.Message) []notification.Target {
		return []notification.Target{{Channel: notification.ChannelTelegram, Source: "家庭通知"}}
	}))
	if err != nil {
		t.Fatal(err)
	}
	plan, err := planner.Plan(uuid.New(), "alice", organizer.Plan{Operations: make([]organizer.Operation, 3)})
	if err != nil {
		t.Fatal(err)
	}
	if len(plan.Events) != 1 || len(plan.Events[0].Requests) != 1 ||
		plan.Events[0].Requests[0].Message.Text != "已整理 3 个文件到媒体库" {
		t.Fatalf("plan = %#v", plan)
	}
}

func TestPlannerPlanSuccessProjectsSingleV2Group(t *testing.T) {
	t.Parallel()

	batchID := uuid.MustParse("11111111-1111-4111-8111-111111111111")
	intentID := uuid.MustParse("22222222-2222-4222-8222-222222222222")
	media := organization.MediaContext{
		Source: "themoviedb", MediaID: "7", TitleYear: "示例剧 (2026)",
		MediaType: "电视剧", Category: "国产剧", VoteAverage: 8.6,
		PosterURL:   "https://image.example/original/poster.jpg",
		BackdropURL: "https://image.example/original/backdrop.jpg",
	}
	snapshot := plannerSuccessSnapshot(t, batchID, intentID, " alice ", 1, media)
	snapshot.Files[0].SizeBytes = 1024
	snapshot.Files[0].Episodes = []int{2, 1}
	snapshot.Files[0].ResourceTerm = "WEB-DL"
	provider := &targetProviderFake{targets: []notification.Target{
		{Channel: notification.ChannelTelegram, Source: "家庭通知"},
		{Channel: notification.ChannelTelegram, Source: "管理通知"},
	}}
	planner, err := NewPlanner(provider)
	if err != nil {
		t.Fatal(err)
	}

	plan, err := planner.PlanSuccess(batchID, " alice ", []organization.SuccessSnapshot{snapshot}, "https://mp.example")
	if err != nil {
		t.Fatal(err)
	}
	if !plan.Valid() || plan.Username != "alice" || len(plan.Events) != 1 || len(plan.Events[0].Requests) != 2 {
		t.Fatalf("plan = %#v", plan)
	}
	event := plan.Events[0]
	wantEventKey := "organization-completed:" + batchID.String() + ":" + snapshot.GroupKey
	wantTitle := "示例剧 (2026) S01 E01-E02 已入库"
	wantText := "评分：8.6，类型：电视剧，类别：国产剧，质量：WEB-DL，共1个文件，大小：1.0K"
	wantImage := "https://image.example/w500/backdrop.jpg"
	wantLink := "https://mp.example/#/history"
	if event.Event.EventKey != wantEventKey || event.Event.Title == nil || *event.Event.Title != wantTitle ||
		event.Event.Text == nil || *event.Event.Text != wantText ||
		event.Event.Image == nil || *event.Event.Image != wantImage ||
		event.Event.Link == nil || *event.Event.Link != wantLink {
		t.Fatalf("history event = %#v", event.Event)
	}
	var note organization.V2OrganizeSuccessContext
	if err := json.Unmarshal(event.Event.Note, &note); err != nil {
		t.Fatalf("decode V2 context note: %v", err)
	}
	if note.TitleYear != media.TitleYear || note.SeasonEpisode != "S01 E01-E02" ||
		note.ResourceTerm != "WEB-DL" || note.FileCount != 1 || note.TotalSize != "1.0K" ||
		note.Username != "alice" || strings.Contains(string(event.Event.Note), "downloads") {
		t.Fatalf("V2 context note = %#v / %s", note, event.Event.Note)
	}
	if provider.calls != 1 || provider.message.Channel != "" || provider.message.Source != "" ||
		provider.message.Username != "alice" || provider.message.Title != wantTitle ||
		provider.message.Text != wantText || provider.message.Image != wantImage || provider.message.Link != wantLink {
		t.Fatalf("provider message = %#v, calls=%d", provider.message, provider.calls)
	}
	for index, request := range event.Requests {
		wantTarget := provider.targets[index]
		if request.Channel != wantTarget.Channel || request.Source != wantTarget.Source ||
			request.Message.Username != "alice" || request.Message.Title != wantTitle ||
			request.Message.Text != wantText || request.Message.Image != wantImage || request.Message.Link != wantLink {
			t.Fatalf("request[%d] = %#v", index, request)
		}
	}
}

func TestPlannerPlanSuccessCreatesOneEventPerSeasonGroup(t *testing.T) {
	t.Parallel()

	batchID := uuid.New()
	intentID := uuid.New()
	media := organization.MediaContext{Source: "themoviedb", MediaID: "8", TitleYear: "跨季剧", MediaType: "电视剧"}
	first := plannerSuccessSnapshot(t, batchID, intentID, "alice", 1, media)
	first.Files[0].Episodes = []int{1}
	second := plannerSuccessSnapshot(t, batchID, intentID, "alice", 2, media)
	second.Files[0].Episodes = []int{3}
	var messages []notification.Message
	planner, err := NewPlanner(targetProviderFunc(func(message notification.Message) []notification.Target {
		messages = append(messages, message)
		return []notification.Target{{Channel: notification.ChannelTelegram, Source: "家庭通知"}}
	}))
	if err != nil {
		t.Fatal(err)
	}

	plan, err := planner.PlanSuccess(batchID, "alice", []organization.SuccessSnapshot{first, second}, "")
	if err != nil {
		t.Fatal(err)
	}
	if len(plan.Events) != 2 || len(messages) != 2 || first.GroupKey == second.GroupKey {
		t.Fatalf("plan/events = %#v, messages=%#v", plan.Events, messages)
	}
	for index, snapshot := range []organization.SuccessSnapshot{first, second} {
		wantKey := "organization-completed:" + batchID.String() + ":" + snapshot.GroupKey
		if plan.Events[index].Event.EventKey != wantKey || len(plan.Events[index].Requests) != 1 ||
			plan.Events[index].Requests[0].Message.Title != messages[index].Title {
			t.Fatalf("event[%d] = %#v", index, plan.Events[index])
		}
	}
	if plan.Events[0].Requests[0].ID == plan.Events[1].Requests[0].ID ||
		plan.Events[0].Requests[0].IdempotencyKey == plan.Events[1].Requests[0].IdempotencyKey {
		t.Fatal("跨组请求身份发生碰撞")
	}
}

func TestPlannerPlanSuccessEligibilityMatchesV2(t *testing.T) {
	t.Parallel()

	batchID := uuid.New()
	intentID := uuid.New()
	media := organization.MediaContext{TitleYear: "资格", MediaType: "电影"}
	snapshots := make([]organization.SuccessSnapshot, 0, 8)
	wantEvents := 0
	for mask := 0; mask < 8; mask++ {
		snapshot := plannerSuccessSnapshot(t, batchID, intentID, "alice", mask, media)
		snapshot.NeedNotify = mask&1 != 0
		snapshot.Manual = mask&2 != 0
		snapshot.Background = mask&4 != 0
		if snapshot.Eligible() {
			wantEvents++
		}
		snapshots = append(snapshots, snapshot)
	}
	provider := &targetProviderFake{}
	planner, err := NewPlanner(provider)
	if err != nil {
		t.Fatal(err)
	}
	plan, err := planner.PlanSuccess(batchID, "alice", snapshots, "")
	if err != nil {
		t.Fatal(err)
	}
	if len(plan.Events) != wantEvents || provider.calls != wantEvents {
		t.Fatalf("events/provider calls = %d/%d, want %d", len(plan.Events), provider.calls, wantEvents)
	}
}

func TestPlannerPlanSuccessKeepsEmptyV2FieldsAndHistoryWithoutTargets(t *testing.T) {
	t.Parallel()

	batchID := uuid.New()
	snapshot := plannerSuccessSnapshot(t, batchID, uuid.New(), "alice", 0, organization.MediaContext{})
	planner, err := NewPlanner(targetProviderFunc(func(notification.Message) []notification.Target { return nil }))
	if err != nil {
		t.Fatal(err)
	}
	plan, err := planner.PlanSuccess(batchID, "alice", []organization.SuccessSnapshot{snapshot}, "")
	if err != nil {
		t.Fatal(err)
	}
	if !plan.Valid() || len(plan.Events) != 1 || len(plan.Events[0].Requests) != 0 {
		t.Fatalf("plan = %#v", plan)
	}
	event := plan.Events[0].Event
	if event.Title == nil || *event.Title != " 已入库" ||
		event.Text == nil || *event.Text != "类型：，共1个文件，大小：0.0B" ||
		event.Image != nil || event.Link != nil {
		t.Fatalf("empty V2 event = %#v", event)
	}
}

func TestPlannerPlanSuccessPreservesExplicitLegacyRendering(t *testing.T) {
	t.Parallel()

	batchID := uuid.New()
	intentID := uuid.New()
	legacy := plannerSuccessSnapshot(t, batchID, intentID, "alice", 0, organization.MediaContext{})
	legacy.Version = 0
	legacy.Legacy = true
	planner, err := NewPlanner(targetProviderFunc(func(notification.Message) []notification.Target {
		return []notification.Target{{Channel: notification.ChannelTelegram, Source: "家庭通知"}}
	}))
	if err != nil {
		t.Fatal(err)
	}
	plan, err := planner.PlanSuccess(batchID, "alice", []organization.SuccessSnapshot{legacy}, "https://ignored.example")
	if err != nil {
		t.Fatal(err)
	}
	event := plan.Events[0]
	if event.Event.EventKey != "organization-completed:"+batchID.String() ||
		event.Event.Title == nil || *event.Event.Title != "整理完成" ||
		event.Event.Text == nil || *event.Event.Text != "已整理 1 个文件到媒体库" ||
		string(event.Event.Note) != `{}` || event.Requests[0].Message.Username != "" {
		t.Fatalf("legacy event = %#v", event)
	}
	second := plannerSuccessSnapshot(t, batchID, intentID, "alice", 1, organization.MediaContext{})
	second.Version = 0
	second.Legacy = true
	if _, err := planner.PlanSuccess(batchID, "alice", []organization.SuccessSnapshot{legacy, second}, ""); err == nil {
		t.Fatal("duplicate legacy event key error = nil")
	}
}

func TestPlannerPlanSuccessReplayIsStable(t *testing.T) {
	t.Parallel()

	batchID := uuid.MustParse("aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa")
	snapshot := plannerSuccessSnapshot(
		t, batchID, uuid.MustParse("bbbbbbbb-bbbb-4bbb-8bbb-bbbbbbbbbbbb"), "alice", 0,
		organization.MediaContext{TitleYear: "稳定重放", MediaType: "电影"},
	)
	planner, err := NewPlanner(targetProviderFunc(func(notification.Message) []notification.Target {
		return []notification.Target{{Channel: notification.ChannelTelegram, Source: "家庭通知"}}
	}))
	if err != nil {
		t.Fatal(err)
	}
	first, err := planner.PlanSuccess(batchID, "alice", []organization.SuccessSnapshot{snapshot}, "mp.example")
	if err != nil {
		t.Fatal(err)
	}
	second, err := planner.PlanSuccess(batchID, "alice", []organization.SuccessSnapshot{snapshot}, "mp.example")
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(first, second) {
		t.Fatalf("重放结果不稳定:\nfirst=%#v\nsecond=%#v", first, second)
	}
}

func TestPlannerPlanSuccessRejectsInconsistentSnapshots(t *testing.T) {
	t.Parallel()

	batchID := uuid.New()
	intentID := uuid.New()
	base := plannerSuccessSnapshot(t, batchID, intentID, "alice", 0, organization.MediaContext{TitleYear: "边界", MediaType: "电影"})
	otherSeason := plannerSuccessSnapshot(t, batchID, intentID, "alice", 1, base.Media)
	tests := []struct {
		name      string
		username  string
		snapshots []organization.SuccessSnapshot
	}{
		{name: "批次不一致", username: "alice", snapshots: []organization.SuccessSnapshot{func() organization.SuccessSnapshot {
			snapshot := base.Clone()
			snapshot.BatchID = uuid.New()
			return snapshot
		}()}},
		{name: "用户名不一致", username: "alice", snapshots: []organization.SuccessSnapshot{func() organization.SuccessSnapshot {
			snapshot := base.Clone()
			snapshot.Username = "bob"
			return snapshot
		}()}},
		{name: "intent 不一致", username: "alice", snapshots: []organization.SuccessSnapshot{base, func() organization.SuccessSnapshot {
			snapshot := otherSeason.Clone()
			snapshot.IntentID = uuid.New()
			return snapshot
		}()}},
		{name: "重复 group", username: "alice", snapshots: []organization.SuccessSnapshot{base, base.Clone()}},
		{name: "非法快照", username: "alice", snapshots: []organization.SuccessSnapshot{func() organization.SuccessSnapshot {
			snapshot := base.Clone()
			snapshot.GroupKey = ""
			return snapshot
		}()}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			planner, err := NewPlanner(targetProviderFunc(func(notification.Message) []notification.Target { return nil }))
			if err != nil {
				t.Fatal(err)
			}
			if _, err := planner.PlanSuccess(batchID, tt.username, tt.snapshots, ""); err == nil {
				t.Fatal("PlanSuccess() error = nil")
			}
		})
	}
}

func TestPlannerPlanSuccessEnforcesCollectionBounds(t *testing.T) {
	batchID := uuid.New()
	intentID := uuid.New()
	media := organization.MediaContext{TitleYear: "边界", MediaType: "电影"}
	atLimit := make([]organization.SuccessSnapshot, MaxEventsPerPlan)
	for index := range atLimit {
		atLimit[index] = plannerSuccessSnapshot(t, batchID, intentID, "alice", index, media)
		atLimit[index].NeedNotify = false
	}
	emptyPlanner, err := NewPlanner(targetProviderFunc(func(notification.Message) []notification.Target { return nil }))
	if err != nil {
		t.Fatal(err)
	}
	if plan, err := emptyPlanner.PlanSuccess(batchID, "alice", atLimit, ""); err != nil || len(plan.Events) != 0 {
		t.Fatalf("at-limit snapshots plan=%#v error=%v", plan, err)
	}
	if _, err := emptyPlanner.PlanSuccess(
		batchID, "alice", append(atLimit, organization.SuccessSnapshot{}), "",
	); err == nil {
		t.Fatal("over-limit snapshots error = nil")
	}

	overEventTargets := make([]notification.Target, MaxRequestsPerEvent+1)
	for index := range overEventTargets {
		overEventTargets[index] = notification.Target{Channel: notification.ChannelTelegram, Source: fmt.Sprintf("target-%d", index)}
	}
	overEventPlanner, err := NewPlanner(targetProviderFunc(func(notification.Message) []notification.Target {
		return overEventTargets
	}))
	if err != nil {
		t.Fatal(err)
	}
	oneEligible := atLimit[0].Clone()
	oneEligible.NeedNotify = true
	if _, err := overEventPlanner.PlanSuccess(batchID, "alice", []organization.SuccessSnapshot{oneEligible}, ""); err == nil {
		t.Fatal("over-limit per-event targets error = nil")
	}

	targets := overEventTargets[:MaxRequestsPerEvent]
	overTotalPlanner, err := NewPlanner(targetProviderFunc(func(notification.Message) []notification.Target { return targets }))
	if err != nil {
		t.Fatal(err)
	}
	eligible := atLimit[:MaxRequestsPerPlan/MaxRequestsPerEvent+1]
	for index := range eligible {
		eligible[index].NeedNotify = true
	}
	if _, err := overTotalPlanner.PlanSuccess(batchID, "alice", eligible, ""); err == nil {
		t.Fatal("over-limit total requests error = nil")
	}
}

func plannerSuccessSnapshot(
	t *testing.T,
	batchID uuid.UUID,
	intentID uuid.UUID,
	username string,
	season int,
	media organization.MediaContext,
) organization.SuccessSnapshot {
	t.Helper()
	groupKey, err := organization.BuildGroupKey(media, season)
	if err != nil {
		t.Fatalf("BuildGroupKey(): %v", err)
	}
	return organization.SuccessSnapshot{
		BatchID: batchID, IntentID: intentID, GroupKey: groupKey,
		Version: organization.CurrentSuccessVersion, Username: username, NeedNotify: true,
		Media: media,
		Files: []organization.FileFact{{Ordinal: 0, Role: "media", Season: season}},
	}
}

type targetProviderFake struct {
	targets []notification.Target
	message notification.Message
	calls   int
}

func (p *targetProviderFake) Targets(message notification.Message) []notification.Target {
	p.calls++
	p.message = message
	return append([]notification.Target(nil), p.targets...)
}

type targetProviderFunc func(notification.Message) []notification.Target

func (f targetProviderFunc) Targets(message notification.Message) []notification.Target {
	return f(message)
}
