package notificationflow

import (
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/moviepilot-go/moviepilot-go/internal/notification"
	"github.com/moviepilot-go/moviepilot-go/internal/organization"
)

func TestPlannerPlanResultsRebuildsV2Timeline(t *testing.T) {
	batchID := uuid.MustParse("11111111-2222-3333-4444-555555555555")
	intentID := uuid.MustParse("aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee")
	media := organization.MediaContext{
		Source: "tmdb", MediaID: "123", TitleYear: "示例剧 (2026)",
		MediaType: "电视剧", Category: "国产剧", VoteAverage: 8,
		BackdropURL: "https://img.example/original/backdrop.jpg",
	}
	first := plannerResultSnapshot(t, batchID, intentID, media, 1, []organization.FileFact{
		{Ordinal: 0, Role: "video", SizeBytes: 100, Season: 1, Episodes: []int{1}},
		{Ordinal: 1, Role: "video", SizeBytes: 200, Season: 1, Episodes: []int{2}},
	})
	second := plannerResultSnapshot(t, batchID, intentID, media, 2, []organization.FileFact{
		{Ordinal: 2, Role: "video", SizeBytes: 300, Season: 2, Episodes: []int{1}},
	})
	results, failures, err := organization.ProjectTerminalResults(
		[]organization.SuccessSnapshot{first, second}, []organization.FileOutcome{
			{Ordinal: 0, State: organization.FileOutcomeCompleted},
			{Ordinal: 1, State: organization.FileOutcomeTerminalFailed, Error: "磁盘空间不足"},
			{Ordinal: 2, State: organization.FileOutcomeTerminalFailed, Error: "权限不足"},
		},
	)
	if err != nil {
		t.Fatal(err)
	}
	planner, err := NewPlanner(targetProviderFunc(func(message notification.Message) []notification.Target {
		if message.Type == notification.TypeManual {
			return []notification.Target{{Channel: notification.ChannelTelegram, Source: "手动通知"}}
		}
		return []notification.Target{{Channel: notification.ChannelTelegram, Source: "整理通知"}}
	}))
	if err != nil {
		t.Fatal(err)
	}
	plan, err := planner.PlanResults(batchID, "alice", results, failures, "https://mp.example")
	if err != nil {
		t.Fatalf("PlanResults() error = %v", err)
	}
	if !plan.Valid() || len(plan.Events) != 3 {
		t.Fatalf("plan = %#v", plan)
	}
	wantKeys := []string{
		"organization-file-failed:" + batchID.String() + ":1",
		"organization-completed:" + batchID.String() + ":" + first.GroupKey,
		"organization-file-failed:" + batchID.String() + ":2",
	}
	for index, want := range wantKeys {
		if got := plan.Events[index].Event.EventKey; got != want {
			t.Fatalf("event[%d] key = %q, want %q", index, got, want)
		}
	}
	manual := plan.Events[0]
	if manual.Event.MType == nil || *manual.Event.MType != string(notification.TypeManual) ||
		manual.Event.Text == nil || *manual.Event.Text != "原因：磁盘空间不足" ||
		len(manual.Requests) != 1 || manual.Requests[0].Message.ContentType != "" ||
		!strings.HasPrefix(manual.Requests[0].IdempotencyKey, organizationFailureKeyPrefix) {
		t.Fatalf("manual event = %#v", manual)
	}
	success := plan.Events[1]
	if success.Event.Text == nil || !strings.Contains(*success.Event.Text, "以下文件处理失败：磁盘空间不足") ||
		len(success.Requests) != 1 || !strings.HasPrefix(success.Requests[0].IdempotencyKey, organizationEventKeyPrefix) {
		t.Fatalf("success event = %#v", success)
	}
}

func TestPlannerPlanResultsAllFailuresKeepsOnlyManualEvents(t *testing.T) {
	batchID := uuid.New()
	intentID := uuid.New()
	media := organization.MediaContext{TitleYear: "电影 (2026)", MediaType: "电影"}
	snapshot := plannerResultSnapshot(t, batchID, intentID, media, 0, []organization.FileFact{
		{Ordinal: 0, Role: "video", SizeBytes: 100, Season: 0},
		{Ordinal: 1, Role: "video", SizeBytes: 200, Season: 0},
	})
	results, failures, err := organization.ProjectTerminalResults(
		[]organization.SuccessSnapshot{snapshot}, []organization.FileOutcome{
			{Ordinal: 0, State: organization.FileOutcomeTerminalFailed, Error: "失败一"},
			{Ordinal: 1, State: organization.FileOutcomeTerminalFailed, Error: "失败二"},
		},
	)
	if err != nil {
		t.Fatal(err)
	}
	planner, _ := NewPlanner(staticTargetProvider{{Channel: notification.ChannelTelegram, Source: "手动"}})
	plan, err := planner.PlanResults(batchID, "alice", results, failures, "")
	if err != nil || len(plan.Events) != 2 {
		t.Fatalf("all failure plan = %#v, %v", plan, err)
	}
	for _, event := range plan.Events {
		if event.Event.MType == nil || *event.Event.MType != string(notification.TypeManual) {
			t.Fatalf("non-manual all-failure event = %#v", event)
		}
	}
}

func plannerResultSnapshot(
	t *testing.T,
	batchID, intentID uuid.UUID,
	media organization.MediaContext,
	season int,
	files []organization.FileFact,
) organization.SuccessSnapshot {
	t.Helper()
	groupKey, err := organization.BuildGroupKey(media, season)
	if err != nil {
		t.Fatal(err)
	}
	snapshot, err := organization.NormalizeSuccessSnapshot(organization.SuccessSnapshot{
		BatchID: batchID, IntentID: intentID, GroupKey: groupKey,
		Version: organization.CurrentSuccessVersion, Username: "alice",
		NeedNotify: true, Background: true, Media: media, Files: files,
	})
	if err != nil {
		t.Fatal(err)
	}
	return snapshot
}
