package notificationhistory

import (
	"encoding/json"
	"errors"
	"testing"
	"time"
)

func TestEventCloneDetaches(t *testing.T) {
	source := "source"
	mtype := "Download"
	title := "标题"
	text := "正文"
	image := "https://example.test/i.png"
	link := "https://example.test/p"
	userID := "user-1"
	published := time.Date(2026, 11, 18, 12, 0, 0, 0, time.UTC)
	event := Event{
		ID: 7, EventKey: "key-1", Channel: "telegram",
		Source: &source, MType: &mtype, Title: &title, Text: &text,
		Image: &image, Link: &link, UserID: &userID,
		Note:        json.RawMessage(`{"a":1}`),
		PublishedAt: &published,
		CreatedAt:   published.Add(time.Minute),
	}

	cloned := event.Clone()
	if cloned.ID != event.ID || cloned.EventKey != event.EventKey || cloned.Channel != event.Channel {
		t.Fatalf("标量字段未复制: %#v", cloned)
	}
	// 指针必须各自独立。
	if cloned.Source == event.Source || cloned.MType == event.MType || cloned.Title == event.Title ||
		cloned.Text == event.Text || cloned.Image == event.Image || cloned.Link == event.Link ||
		cloned.UserID == event.UserID || cloned.PublishedAt == event.PublishedAt {
		t.Fatal("指针字段与源对象共享地址")
	}
	*event.Source = "changed"
	*event.PublishedAt = published.Add(time.Hour)
	event.Note[0] = '['
	if *cloned.Source != "source" {
		t.Fatalf("克隆的 source 被改写: %q", *cloned.Source)
	}
	if !cloned.PublishedAt.Equal(time.Date(2026, 11, 18, 12, 0, 0, 0, time.UTC)) {
		t.Fatalf("克隆的时间被改写: %v", cloned.PublishedAt)
	}
	if string(cloned.Note) != `{"a":1}` {
		t.Fatalf("克隆的 note 被改写: %s", cloned.Note)
	}

	// 零值事件克隆后仍为空字段。
	empty := Event{}.Clone()
	if empty.Source != nil || empty.Note != nil || empty.PublishedAt != nil {
		t.Fatalf("零值克隆 = %#v", empty)
	}
}

func TestCloneTimeNil(t *testing.T) {
	if cloneTime(nil) != nil {
		t.Fatal("nil 时间应返回 nil")
	}
	value := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	cloned := cloneTime(&value)
	if cloned == &value || !cloned.Equal(value) {
		t.Fatalf("时间克隆 = %v", cloned)
	}
}

func TestValidatePageOverflow(t *testing.T) {
	if err := ValidatePage(1, 1); err != nil {
		t.Fatalf("最小合法分页 = %v", err)
	}
	if err := ValidatePage(0, 10); !errors.Is(err, ErrInvalidPage) {
		t.Fatalf("page=0 = %v", err)
	}
	if err := ValidatePage(1, 0); !errors.Is(err, ErrInvalidPage) {
		t.Fatalf("count=0 = %v", err)
	}
	maxInt := int(^uint(0) >> 1)
	if err := ValidatePage(maxInt, 2); !errors.Is(err, ErrInvalidPage) {
		t.Fatalf("乘法溢出 = %v", err)
	}
}

func TestEventV2ProjectionNotesAndZone(t *testing.T) {
	published := time.Date(2026, 11, 18, 4, 5, 6, 0, time.UTC)
	title := "标题"
	valid := Event{
		ID: 3, EventKey: "key-1", Channel: "telegram",
		Title: &title, Note: json.RawMessage(`{"x":true}`),
		PublishedAt: &published, CreatedAt: published,
	}

	if _, err := valid.V2(nil); !errors.Is(err, ErrInvalidEvent) {
		t.Fatalf("nil 时区 = %v", err)
	}
	missingPublished := valid
	missingPublished.PublishedAt = nil
	if _, err := missingPublished.V2(time.UTC); !errors.Is(err, ErrInvalidEvent) {
		t.Fatalf("缺少发布时间 = %v", err)
	}
	invalid := valid
	invalid.ID = 0
	if _, err := invalid.V2(time.UTC); !errors.Is(err, ErrInvalidEvent) {
		t.Fatalf("非法事件 = %v", err)
	}

	zone := time.FixedZone("UTC+8", 8*3600)
	item, err := valid.V2(zone)
	if err != nil {
		t.Fatalf("投影失败: %v", err)
	}
	if item.RegTime != "2026-11-18 12:05:06" {
		t.Fatalf("RegTime = %q", item.RegTime)
	}
	if item.Action != V2ActionSent || item.ID != 3 || item.Channel != "telegram" || item.Title == nil || *item.Title != "标题" {
		t.Fatalf("投影字段 = %#v", item)
	}
	// note 必须是副本。
	valid.Note[0] = '['
	if string(item.Note) != `{"x":true}` {
		t.Fatalf("note 被共享: %s", item.Note)
	}
	// 空 note 投影为 {}。
	emptyNote := valid
	emptyNote.Note = nil
	item, err = emptyNote.V2(time.UTC)
	if err != nil || string(item.Note) != "{}" {
		t.Fatalf("空 note = (%#v, %v)", item, err)
	}
}
