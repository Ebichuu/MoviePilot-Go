// Package notificationflow owns durable per-channel notification delivery.
package notificationflow

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/moviepilot-go/moviepilot-go/internal/notification"
	"github.com/moviepilot-go/moviepilot-go/internal/notificationhistory"
)

const (
	// MaxAttempts is the hard retry bound for one channel delivery.
	MaxAttempts = 5
	// SweepPageSize bounds one recovery pass.
	SweepPageSize = 100
	// MaxEventsPerPlan bounds the number of business events produced by one batch.
	MaxEventsPerPlan = 10_000
	// MaxRequestsPerEvent bounds fan-out for one business event.
	MaxRequestsPerEvent = 64
	// MaxRequestsPerPlan bounds total durable delivery intents for one batch.
	MaxRequestsPerPlan = 10_000
	// MaxSourceBytes bounds one notification configuration name.
	MaxSourceBytes = 4 * 1024
	// MaxUsernameBytes bounds the persisted submitting username.
	MaxUsernameBytes = 512
	// MaxTitleBytes bounds one rendered notification title.
	MaxTitleBytes = 1024
	// MaxTextBytes bounds one rendered notification body.
	MaxTextBytes = 64 * 1024
	// MaxImageBytes bounds one rendered image URL.
	MaxImageBytes = 4 * 1024
	// MaxLinkBytes bounds one rendered action URL.
	MaxLinkBytes = 4 * 1024
	// MaxNoteBytes bounds one V2 history context document.
	MaxNoteBytes = 64 * 1024
	// MaxMTypeBytes bounds the V2 message type stored in history.
	MaxMTypeBytes = 256
	// MaxUserIDBytes bounds the optional V2 history recipient identifier.
	MaxUserIDBytes = 512
)

const (
	organizationEventKeyPrefix   = "organization-completed:"
	organizationFailureKeyPrefix = "organization-file-failed:"
	groupedEventKeyMarker        = ":sha256:"
)

type organizationEventKind uint8

const (
	organizationEventInvalid organizationEventKind = iota
	organizationEventLegacySuccess
	organizationEventGroupedSuccess
	organizationEventFileFailure
)

// State is the durable delivery state.
type State string

const (
	StateBlocked State = "blocked"
	StatePending State = "pending"
	StateWorking State = "working"
	StateFailed  State = "failed"
	StateSent    State = "sent"
	StateSkipped State = "skipped"
)

// RequestPlan is one immutable channel delivery intent.
type RequestPlan struct {
	ID             uuid.UUID
	Channel        notification.Channel
	Source         string
	IdempotencyKey string
	Message        notification.Message
}

// EventPlan owns one channel-independent V2 history record and all of its
// channel delivery intents.
type EventPlan struct {
	Event    notificationhistory.Plan
	Requests []RequestPlan
}

// Clone returns a detached event plan copy.
func (p EventPlan) Clone() EventPlan {
	p.Event = p.Event.Clone()
	p.Requests = append([]RequestPlan(nil), p.Requests...)
	for index := range p.Requests {
		preview := p.Requests[index].Message.DisableWebPagePreview
		if preview == nil {
			continue
		}
		value := *preview
		p.Requests[index].Message.DisableWebPagePreview = &value
	}
	return p
}

// Plan is the complete immutable notification snapshot for one batch.
type Plan struct {
	BatchID  uuid.UUID
	Username string
	Events   []EventPlan
}

// Clone returns a detached plan copy.
func (p Plan) Clone() Plan {
	p.Events = append([]EventPlan(nil), p.Events...)
	for index := range p.Events {
		p.Events[index] = p.Events[index].Clone()
	}
	return p
}

// Valid reports whether the plan is safe to persist.
func (p Plan) Valid() bool {
	if p.BatchID == uuid.Nil || !validRequiredTrimmedString(p.Username, MaxUsernameBytes) ||
		len(p.Events) > MaxEventsPerPlan {
		return false
	}
	type targetIdentity struct {
		channel notification.Channel
		source  string
	}
	seenEventKeys := make(map[string]struct{}, len(p.Events))
	seenRequestIDs := make(map[uuid.UUID]struct{})
	seenIdempotencyKeys := make(map[string]struct{})
	totalRequests := 0
	for _, event := range p.Events {
		kind := organizationEventKeyKind(event.Event.EventKey, p.BatchID)
		legacy := kind == organizationEventLegacySuccess
		if kind == organizationEventInvalid || !validEventBoundaries(event.Event, kind) || !event.Event.Valid() ||
			len(event.Requests) > MaxRequestsPerEvent {
			return false
		}
		totalRequests += len(event.Requests)
		if totalRequests > MaxRequestsPerPlan {
			return false
		}
		if _, exists := seenEventKeys[event.Event.EventKey]; exists {
			return false
		}
		seenEventKeys[event.Event.EventKey] = struct{}{}

		seenTargets := make(map[targetIdentity]struct{}, len(event.Requests))
		for _, request := range event.Requests {
			target := notification.Target{Channel: request.Channel, Source: request.Source}
			expectedID, expectedKey, identityValid := organizationNotificationRequestIdentity(
				p.BatchID, event.Event.EventKey, target,
			)
			if request.ID == uuid.Nil || !validChannel(request.Channel) || request.IdempotencyKey == "" ||
				len(request.IdempotencyKey) > 255 || request.Message.Channel != request.Channel ||
				request.Message.Source != request.Source || !validOptionalString(request.Source, MaxSourceBytes) ||
				request.Message.UserID != "" ||
				(!legacy && request.Message.Username != p.Username) ||
				(legacy && request.Message.Username != "" && request.Message.Username != p.Username) ||
				!validMessageBoundaries(request.Message, kind) || !eventProjectsMessage(event.Event, request.Message) ||
				!identityValid || request.ID != expectedID || request.IdempotencyKey != expectedKey {
				return false
			}
			if _, exists := seenRequestIDs[request.ID]; exists {
				return false
			}
			if _, exists := seenIdempotencyKeys[request.IdempotencyKey]; exists {
				return false
			}
			identity := targetIdentity{channel: request.Channel, source: request.Source}
			if _, exists := seenTargets[identity]; exists {
				return false
			}
			seenRequestIDs[request.ID] = struct{}{}
			seenIdempotencyKeys[request.IdempotencyKey] = struct{}{}
			seenTargets[identity] = struct{}{}
		}
	}
	return true
}

func classifyOrganizationEventKey(eventKey string, batchID uuid.UUID) (legacy bool, valid bool) {
	kind := organizationEventKeyKind(eventKey, batchID)
	return kind == organizationEventLegacySuccess, kind != organizationEventInvalid
}

func organizationEventKeyKind(eventKey string, batchID uuid.UUID) organizationEventKind {
	if batchID == uuid.Nil {
		return organizationEventInvalid
	}
	legacyKey := organizationEventKeyPrefix + batchID.String()
	if eventKey == legacyKey {
		return organizationEventLegacySuccess
	}
	groupPrefix := legacyKey + groupedEventKeyMarker
	if strings.HasPrefix(eventKey, groupPrefix) && len(eventKey) == len(groupPrefix)+sha256HexLength {
		for _, character := range eventKey[len(groupPrefix):] {
			if (character < '0' || character > '9') && (character < 'a' || character > 'f') {
				return organizationEventInvalid
			}
		}
		return organizationEventGroupedSuccess
	}
	failurePrefix := organizationFailureKeyPrefix + batchID.String() + ":"
	if !strings.HasPrefix(eventKey, failurePrefix) {
		return organizationEventInvalid
	}
	ordinalText := strings.TrimPrefix(eventKey, failurePrefix)
	ordinal, err := strconv.Atoi(ordinalText)
	if err != nil || ordinal < 0 || strconv.Itoa(ordinal) != ordinalText {
		return organizationEventInvalid
	}
	return organizationEventFileFailure
}

func validEventBoundaries(event notificationhistory.Plan, kind organizationEventKind) bool {
	wantType := notification.TypeOrganize
	if kind == organizationEventFileFailure {
		wantType = notification.TypeManual
	}
	if kind == organizationEventInvalid || event.Channel != "" || (event.Source != nil && *event.Source != "") ||
		event.MType == nil || *event.MType != string(wantType) ||
		event.Title == nil || !validRequiredString(*event.Title, MaxTitleBytes) ||
		event.Text == nil || !validRequiredString(*event.Text, MaxTextBytes) ||
		!validOptionalStringPointer(event.Source, MaxSourceBytes) ||
		!validOptionalStringPointer(event.Image, MaxImageBytes) ||
		!validOptionalStringPointer(event.Link, MaxLinkBytes) ||
		!validOptionalStringPointer(event.UserID, MaxUserIDBytes) ||
		(event.UserID != nil && *event.UserID != "") || !validBoundedNote(event.Note) {
		return false
	}
	return kind != organizationEventFileFailure || jsonObjectIsEmpty(event.Note)
}

func validMessageBoundaries(message notification.Message, kind organizationEventKind) bool {
	typeValid := message.Type == notification.TypeOrganize &&
		message.ContentType == notification.ContentOrganizeSuccess
	if kind == organizationEventFileFailure {
		typeValid = message.Type == notification.TypeManual && message.ContentType == ""
	}
	return typeValid &&
		validOptionalString(message.Source, MaxSourceBytes) &&
		validOptionalString(message.Username, MaxUsernameBytes) &&
		validRequiredString(message.Title, MaxTitleBytes) &&
		validRequiredString(message.Text, MaxTextBytes) &&
		validOptionalString(message.Image, MaxImageBytes) &&
		validOptionalString(message.Link, MaxLinkBytes) &&
		validOptionalString(message.UserID, MaxUserIDBytes) &&
		validOptionalString(string(message.Type), MaxMTypeBytes)
}

func jsonObjectIsEmpty(value json.RawMessage) bool {
	trimmed := bytes.TrimSpace(value)
	if len(trimmed) == 0 {
		return true
	}
	var decoded map[string]any
	return json.Unmarshal(trimmed, &decoded) == nil && len(decoded) == 0
}

func eventProjectsMessage(event notificationhistory.Plan, message notification.Message) bool {
	return optionalProjectionMatches(event.MType, string(message.Type)) &&
		optionalProjectionMatches(event.Title, message.Title) &&
		optionalProjectionMatches(event.Text, message.Text) &&
		optionalProjectionMatches(event.Image, message.Image) &&
		optionalProjectionMatches(event.Link, message.Link) &&
		optionalProjectionMatches(event.UserID, message.UserID)
}

func optionalProjectionMatches(projected *string, messageValue string) bool {
	if messageValue == "" {
		return projected == nil || *projected == ""
	}
	return projected != nil && *projected == messageValue
}

func validRequiredTrimmedString(value string, maxBytes int) bool {
	return validRequiredString(value, maxBytes) && strings.TrimSpace(value) == value
}

func validRequiredString(value string, maxBytes int) bool {
	return strings.TrimSpace(value) != "" && validOptionalString(value, maxBytes)
}

func validOptionalString(value string, maxBytes int) bool {
	return len(value) <= maxBytes && !strings.ContainsRune(value, '\x00')
}

func validOptionalStringPointer(value *string, maxBytes int) bool {
	return value == nil || validOptionalString(*value, maxBytes)
}

func validBoundedNote(note json.RawMessage) bool {
	if len(note) > MaxNoteBytes || bytes.IndexByte(note, 0) >= 0 {
		return false
	}
	trimmed := bytes.TrimSpace(note)
	if len(trimmed) == 0 {
		return true
	}
	var decoded any
	if err := json.Unmarshal(trimmed, &decoded); err != nil {
		return false
	}
	return !jsonValueContainsNUL(decoded)
}

func jsonValueContainsNUL(value any) bool {
	switch typed := value.(type) {
	case string:
		return strings.ContainsRune(typed, '\x00')
	case []any:
		for _, item := range typed {
			if jsonValueContainsNUL(item) {
				return true
			}
		}
	case map[string]any:
		for key, item := range typed {
			if strings.ContainsRune(key, '\x00') || jsonValueContainsNUL(item) {
				return true
			}
		}
	}
	return false
}

// Request is the durable projection consumed by one worker.
type Request struct {
	ID           uuid.UUID
	Channel      notification.Channel
	Source       string
	Message      notification.Message
	State        State
	AttemptCount int
}

// Lease is the token-CAS ownership returned by persistence.
type Lease struct {
	Request Request
	Token   uuid.UUID
}

// Valid reports whether a claimed delivery is executable.
func (l Lease) Valid(id uuid.UUID) bool {
	return l.Token != uuid.Nil && l.Request.ID == id && validChannel(l.Request.Channel) &&
		l.Request.Message.Channel == l.Request.Channel && l.Request.Message.Source == l.Request.Source && l.Request.AttemptCount >= 1 &&
		l.Request.AttemptCount <= MaxAttempts
}

// Store owns one delivery lease and its terminal or retryable transition.
type Store interface {
	ClaimNotification(context.Context, uuid.UUID, time.Time, time.Duration) (Lease, bool, error)
	CompleteNotification(context.Context, uuid.UUID, uuid.UUID, time.Time) error
	RescheduleNotification(context.Context, uuid.UUID, uuid.UUID, time.Time, string) error
}

// SweepStore owns bounded recovery discovery.
type SweepStore interface {
	RecoverExpiredNotifications(context.Context, time.Time, int) (int64, error)
	ListDueNotifications(context.Context, time.Time, int) ([]uuid.UUID, error)
}

// Enqueuer requests execution of one persisted delivery row.
type Enqueuer interface {
	EnqueueNotification(context.Context, uuid.UUID) error
}

var (
	ErrInvalidPlan = errors.New("通知计划无效")
	ErrPersistence = errors.New("通知状态持久化失败")
)

func validChannel(channel notification.Channel) bool {
	return notification.SupportedChannel(channel)
}
