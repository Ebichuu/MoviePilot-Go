// Package notificationhistory owns persisted V2 business notification history.
package notificationhistory

import (
	"bytes"
	"encoding/json"
	"errors"
	"strings"
	"time"
)

const (
	// MaxEventKeyBytes is the persistence boundary for an event idempotency key.
	MaxEventKeyBytes = 255
	// V2ActionSent is the V2 history direction for business notifications.
	V2ActionSent = 1
)

var (
	// ErrInvalidEvent reports a corrupt or unpublished V2 history event.
	ErrInvalidEvent = errors.New("通知历史事件无效")
	// ErrInvalidPlan reports an event plan that is unsafe to persist.
	ErrInvalidPlan = errors.New("通知历史计划无效")
	// ErrInvalidPage reports out-of-range history pagination.
	ErrInvalidPage = errors.New("通知历史分页无效")
	// ErrInvalidScope reports an unsupported V2 clear scope.
	ErrInvalidScope = errors.New("通知历史清理范围无效")
	// ErrInvalidClearBefore reports an invalid clear timestamp.
	ErrInvalidClearBefore = errors.New("通知历史清理时间无效")
	// ErrPersistence is the stable boundary for notification history storage failures.
	ErrPersistence = errors.New("通知历史持久化失败")
)

// Scope selects one V2 notification history clear marker.
type Scope string

const (
	ScopeAll    Scope = "all"
	ScopeSystem Scope = "system"
	ScopeMedia  Scope = "media"
)

// Valid reports whether scope is part of the V2 API contract.
func (s Scope) Valid() bool {
	switch s {
	case ScopeAll, ScopeSystem, ScopeMedia:
		return true
	default:
		return false
	}
}

// ClearBefore is the process-global V2 history visibility boundary in Unix milliseconds.
type ClearBefore struct {
	All    int64 `json:"all"`
	System int64 `json:"system"`
	Media  int64 `json:"media"`
}

// Valid reports whether all clear markers can be safely compared.
func (c ClearBefore) Valid() bool {
	return c.All >= 0 && c.System >= 0 && c.Media >= 0
}

// Clone returns a detached clear marker value.
func (c ClearBefore) Clone() ClearBefore {
	return c
}

// Plan is one immutable business notification event before persistence.
type Plan struct {
	EventKey    string
	Channel     string
	Source      *string
	MType       *string
	Title       *string
	Text        *string
	Image       *string
	Link        *string
	UserID      *string
	Note        json.RawMessage
	PublishedAt *time.Time
}

// Valid reports whether the plan is safe to persist.
func (p Plan) Valid() bool {
	return validEventFields(
		p.EventKey, p.Channel, p.Source, p.MType, p.Title, p.Text,
		p.Image, p.Link, p.UserID, p.Note, p.PublishedAt,
	)
}

// Clone returns a detached plan copy.
func (p Plan) Clone() Plan {
	p.Source = cloneString(p.Source)
	p.MType = cloneString(p.MType)
	p.Title = cloneString(p.Title)
	p.Text = cloneString(p.Text)
	p.Image = cloneString(p.Image)
	p.Link = cloneString(p.Link)
	p.UserID = cloneString(p.UserID)
	p.Note = cloneNote(p.Note)
	p.PublishedAt = cloneTime(p.PublishedAt)
	return p
}

// Event is one persisted V2 business notification, independent of channel delivery state.
type Event struct {
	ID          int64
	EventKey    string
	Channel     string
	Source      *string
	MType       *string
	Title       *string
	Text        *string
	Image       *string
	Link        *string
	UserID      *string
	Note        json.RawMessage
	PublishedAt *time.Time
	CreatedAt   time.Time
}

// Valid reports whether the persisted event is internally consistent.
func (e Event) Valid() bool {
	return e.ID > 0 && !e.CreatedAt.IsZero() && validEventFields(
		e.EventKey, e.Channel, e.Source, e.MType, e.Title, e.Text,
		e.Image, e.Link, e.UserID, e.Note, e.PublishedAt,
	)
}

// Clone returns a detached event copy.
func (e Event) Clone() Event {
	e.Source = cloneString(e.Source)
	e.MType = cloneString(e.MType)
	e.Title = cloneString(e.Title)
	e.Text = cloneString(e.Text)
	e.Image = cloneString(e.Image)
	e.Link = cloneString(e.Link)
	e.UserID = cloneString(e.UserID)
	e.Note = cloneNote(e.Note)
	e.PublishedAt = cloneTime(e.PublishedAt)
	return e
}

// V2Item is the exact public projection used by the V2 notification history API.
type V2Item struct {
	ID      int64           `json:"id"`
	Channel string          `json:"channel"`
	Source  *string         `json:"source"`
	MType   *string         `json:"mtype"`
	Title   *string         `json:"title"`
	Text    *string         `json:"text"`
	Image   *string         `json:"image"`
	Link    *string         `json:"link"`
	UserID  *string         `json:"userid"`
	RegTime string          `json:"reg_time"`
	Action  int             `json:"action"`
	Note    json.RawMessage `json:"note"`
}

// V2 projects a published event into the V2 response shape using local wall time.
func (e Event) V2(location *time.Location) (V2Item, error) {
	if location == nil || !e.Valid() || e.PublishedAt == nil {
		return V2Item{}, ErrInvalidEvent
	}
	note := cloneNote(e.Note)
	if len(note) == 0 {
		note = json.RawMessage(`{}`)
	}
	return V2Item{
		ID: e.ID, Channel: e.Channel, Source: cloneString(e.Source), MType: cloneString(e.MType),
		Title: cloneString(e.Title), Text: cloneString(e.Text), Image: cloneString(e.Image),
		Link: cloneString(e.Link), UserID: cloneString(e.UserID),
		RegTime: e.PublishedAt.In(location).Format("2006-01-02 15:04:05"),
		Action:  V2ActionSent, Note: note,
	}, nil
}

// ValidatePage checks positive one-based pagination without adding limits that
// are absent from the V2 API contract.
func ValidatePage(page, count int) error {
	if page < 1 || count < 1 {
		return ErrInvalidPage
	}
	if int64(page-1) > int64(^uint64(0)>>1)/int64(count) {
		return ErrInvalidPage
	}
	return nil
}

func validEventFields(
	eventKey, channel string,
	source, mtype, title, text, image, link, userID *string,
	note json.RawMessage,
	publishedAt *time.Time,
) bool {
	if strings.TrimSpace(eventKey) != eventKey || eventKey == "" || len(eventKey) > MaxEventKeyBytes ||
		strings.ContainsRune(eventKey, '\x00') || strings.ContainsRune(channel, '\x00') ||
		!validOptionalString(source) || !validOptionalString(mtype) || !validOptionalString(title) ||
		!validOptionalString(text) || !validOptionalString(image) || !validOptionalString(link) ||
		!validOptionalString(userID) || !validNote(note) {
		return false
	}
	return publishedAt == nil || !publishedAt.IsZero()
}

func validOptionalString(value *string) bool {
	return value == nil || !strings.ContainsRune(*value, '\x00')
}

func validNote(note json.RawMessage) bool {
	trimmed := bytes.TrimSpace(note)
	if len(trimmed) == 0 {
		return true
	}
	return json.Valid(trimmed) && (trimmed[0] == '{' || trimmed[0] == '[')
}

func cloneString(value *string) *string {
	if value == nil {
		return nil
	}
	cloned := *value
	return &cloned
}

func cloneTime(value *time.Time) *time.Time {
	if value == nil {
		return nil
	}
	cloned := *value
	return &cloned
}

func cloneNote(note json.RawMessage) json.RawMessage {
	return append(json.RawMessage(nil), note...)
}
