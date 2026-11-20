package notificationhistory

import (
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"
)

func TestPlanValidation(t *testing.T) {
	t.Parallel()

	valid := Plan{
		EventKey: "organization-completed:batch-1",
		Channel:  "telegram",
		Source:   stringPointer("家庭通知"),
		MType:    stringPointer("整理入库"),
		Title:    stringPointer("整理完成"),
		Text:     stringPointer("文件已成功整理到媒体库"),
		Note:     json.RawMessage(`{"tmdb_id": 1}`),
	}
	tests := []struct {
		name string
		plan Plan
		want bool
	}{
		{name: "有效计划", plan: valid, want: true},
		{name: "允许未定向空渠道", plan: func() Plan { p := valid; p.Channel = ""; return p }(), want: true},
		{name: "拒绝空事件键", plan: func() Plan { p := valid; p.EventKey = ""; return p }()},
		{name: "拒绝事件键首尾空白", plan: func() Plan { p := valid; p.EventKey = " event "; return p }()},
		{name: "拒绝超长事件键", plan: func() Plan { p := valid; p.EventKey = strings.Repeat("a", MaxEventKeyBytes+1); return p }()},
		{name: "拒绝文本零字节", plan: func() Plan { p := valid; p.Text = stringPointer("bad\x00text"); return p }()},
		{name: "拒绝标量 note", plan: func() Plan { p := valid; p.Note = json.RawMessage(`1`); return p }()},
		{name: "拒绝损坏 note", plan: func() Plan { p := valid; p.Note = json.RawMessage(`{`); return p }()},
		{name: "允许空 note 使用数据库默认值", plan: func() Plan { p := valid; p.Note = nil; return p }(), want: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if got := tt.plan.Valid(); got != tt.want {
				t.Fatalf("Plan.Valid() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestPlanCloneIsDetached(t *testing.T) {
	t.Parallel()

	publishedAt := time.Date(2026, time.August, 14, 4, 5, 6, 0, time.UTC)
	plan := Plan{
		EventKey:    "event-1",
		Source:      stringPointer("source"),
		Title:       stringPointer("title"),
		Note:        json.RawMessage(`{"nested":{"value":1}}`),
		PublishedAt: &publishedAt,
	}
	clone := plan.Clone()
	*clone.Source = "changed"
	*clone.Title = "changed"
	clone.Note[0] = '['
	*clone.PublishedAt = clone.PublishedAt.Add(time.Hour)

	if *plan.Source != "source" || *plan.Title != "title" || string(plan.Note) != `{"nested":{"value":1}}` || !plan.PublishedAt.Equal(publishedAt) {
		t.Fatalf("Clone() 修改了原计划: %#v", plan)
	}
}

func TestEventV2Projection(t *testing.T) {
	t.Parallel()

	location := time.FixedZone("UTC+8", 8*60*60)
	publishedAt := time.Date(2026, time.August, 14, 4, 34, 56, 987000000, time.UTC)
	event := Event{
		ID:          42,
		EventKey:    "event-42",
		Channel:     "",
		Source:      nil,
		MType:       stringPointer("整理入库"),
		Title:       stringPointer("整理完成"),
		Text:        stringPointer("文件已成功整理到媒体库"),
		Note:        json.RawMessage(`{"tmdb_id":1}`),
		PublishedAt: &publishedAt,
		CreatedAt:   publishedAt.Add(-time.Minute),
	}
	got, err := event.V2(location)
	if err != nil {
		t.Fatal(err)
	}
	want := V2Item{
		ID: 42, Channel: "", Source: nil, MType: stringPointer("整理入库"),
		Title: stringPointer("整理完成"), Text: stringPointer("文件已成功整理到媒体库"),
		RegTime: "2026-08-14 12:34:56", Action: V2ActionSent,
		Note: json.RawMessage(`{"tmdb_id":1}`),
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("Event.V2() = %#v, want %#v", got, want)
	}
	encoded, err := json.Marshal(got)
	if err != nil {
		t.Fatal(err)
	}
	for _, field := range []string{`"source":null`, `"image":null`, `"link":null`, `"userid":null`, `"action":1`} {
		if !strings.Contains(string(encoded), field) {
			t.Fatalf("V2 JSON %s missing %s", encoded, field)
		}
	}
	got.Note[0] = '['
	if string(event.Note) != `{"tmdb_id":1}` {
		t.Fatal("Event.V2() 未复制 note")
	}

	unpublished := event
	unpublished.PublishedAt = nil
	if _, err := unpublished.V2(location); !errors.Is(err, ErrInvalidEvent) {
		t.Fatalf("未发布事件 error = %v, want ErrInvalidEvent", err)
	}
	if _, err := event.V2(nil); !errors.Is(err, ErrInvalidEvent) {
		t.Fatalf("nil location error = %v, want ErrInvalidEvent", err)
	}
}

func TestClearBeforeAndPageValidation(t *testing.T) {
	t.Parallel()

	clearBefore := ClearBefore{All: 1, System: 2, Media: 3}
	if !clearBefore.Valid() || clearBefore.Clone() != clearBefore {
		t.Fatalf("ClearBefore = %#v", clearBefore)
	}
	for _, scope := range []Scope{ScopeAll, ScopeSystem, ScopeMedia} {
		if !scope.Valid() {
			t.Fatalf("scope %q should be valid", scope)
		}
	}
	if Scope("other").Valid() {
		t.Fatal("unknown scope should be invalid")
	}
	if (ClearBefore{All: -1}).Valid() {
		t.Fatal("negative clear marker should be invalid")
	}

	tests := []struct {
		name  string
		page  int
		count int
		want  error
	}{
		{name: "默认分页", page: 1, count: 20},
		{name: "页码超过旧实现边界", page: 1_000_001, count: 20},
		{name: "条数超过旧实现边界", page: 1, count: 101},
		{name: "页码为零", page: 0, count: 20, want: ErrInvalidPage},
		{name: "条数为零", page: 1, count: 0, want: ErrInvalidPage},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if err := ValidatePage(tt.page, tt.count); !errors.Is(err, tt.want) {
				t.Fatalf("ValidatePage(%d, %d) = %v, want %v", tt.page, tt.count, err, tt.want)
			}
		})
	}
}

func stringPointer(value string) *string {
	return &value
}
