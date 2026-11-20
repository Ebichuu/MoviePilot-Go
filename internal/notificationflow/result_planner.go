package notificationflow

import (
	"encoding/json"
	"sort"
	"strconv"
	"strings"

	"github.com/google/uuid"

	"github.com/moviepilot-go/moviepilot-go/internal/notification"
	"github.com/moviepilot-go/moviepilot-go/internal/notificationhistory"
	"github.com/moviepilot-go/moviepilot-go/internal/organization"
)

type resultEvent struct {
	ordinal int
	manual  bool
	event   EventPlan
}

// PlanResults builds the V2 terminal-event timeline. A file failure is
// published at its own ordinal; a group's success aggregate is published when
// that group's last file reaches a terminal state. Manual is ordered before
// the aggregate when both happen at the same ordinal.
func (p *Planner) PlanResults(
	batchID uuid.UUID,
	username string,
	results []organization.ResultSnapshot,
	failures []organization.FileFailure,
	appDomain string,
) (Plan, error) {
	username = strings.TrimSpace(username)
	if p == nil || p.provider == nil || batchID == uuid.Nil ||
		!validRequiredTrimmedString(username, MaxUsernameBytes) ||
		len(results) > MaxEventsPerPlan || len(failures) > MaxEventsPerPlan {
		return Plan{}, ErrInvalidPlan
	}

	events := make([]resultEvent, 0, len(results)+len(failures))
	groups := make(map[string]struct{}, len(results))
	resultByGroup := make(map[string]organization.ResultSnapshot, len(results))
	failureOrdinals := make(map[int]struct{}, len(failures))
	for _, result := range results {
		snapshot := result.Snapshot
		if snapshot.BatchID != batchID || snapshot.Username != username {
			return Plan{}, ErrInvalidPlan
		}
		if _, exists := groups[snapshot.GroupKey]; exists {
			return Plan{}, ErrInvalidPlan
		}
		groups[snapshot.GroupKey] = struct{}{}
		resultByGroup[snapshot.GroupKey] = result
	}
	for _, failure := range failures {
		if failure.Snapshot.BatchID != batchID || failure.Snapshot.Username != username {
			return Plan{}, ErrInvalidPlan
		}
		if _, exists := failureOrdinals[failure.File.Ordinal]; exists {
			return Plan{}, ErrInvalidPlan
		}
		failureOrdinals[failure.File.Ordinal] = struct{}{}
		rendered, err := organization.RenderV2ManualFailure(failure, appDomain)
		if err != nil {
			return Plan{}, ErrInvalidPlan
		}
		event, err := p.manualEvent(batchID, failure.File.Ordinal, rendered)
		if err != nil {
			return Plan{}, ErrInvalidPlan
		}
		events = append(events, resultEvent{ordinal: failure.File.Ordinal, manual: true, event: event})
	}

	for _, result := range results {
		if result.Snapshot.Legacy {
			if result.ErrMsg != "" || len(results) != 1 || len(failures) != 0 {
				return Plan{}, ErrInvalidPlan
			}
			legacy, err := p.planWithCount(batchID, username, len(result.Snapshot.Files))
			if err != nil || len(legacy.Events) != 1 {
				return Plan{}, ErrInvalidPlan
			}
			events = append(events, resultEvent{ordinal: result.TerminalOrdinal, event: legacy.Events[0]})
			continue
		}
		if !result.Snapshot.Eligible() {
			continue
		}
		rendered, err := organization.RenderV2OrganizeResult(result, appDomain)
		if err != nil {
			return Plan{}, ErrInvalidPlan
		}
		event, err := p.successEvent(batchID, result.Snapshot, rendered)
		if err != nil {
			return Plan{}, ErrInvalidPlan
		}
		events = append(events, resultEvent{ordinal: result.TerminalOrdinal, event: event})
	}

	for _, failure := range failures {
		result, exists := resultByGroup[failure.Snapshot.GroupKey]
		if !exists {
			continue
		}
		if result.ErrMsg != "" && result.TerminalOrdinal == failure.File.Ordinal && result.ErrMsg != failure.Error {
			return Plan{}, ErrInvalidPlan
		}
		if result.ErrMsg == "" && failure.File.Ordinal > result.TerminalOrdinal {
			return Plan{}, ErrInvalidPlan
		}
	}

	sort.SliceStable(events, func(i, j int) bool {
		if events[i].ordinal != events[j].ordinal {
			return events[i].ordinal < events[j].ordinal
		}
		if events[i].manual != events[j].manual {
			return events[i].manual
		}
		return events[i].event.Event.EventKey < events[j].event.Event.EventKey
	})
	result := Plan{BatchID: batchID, Username: username}
	result.Events = make([]EventPlan, 0, len(events))
	for _, item := range events {
		result.Events = append(result.Events, item.event)
	}
	if !result.Valid() {
		return Plan{}, ErrInvalidPlan
	}
	return result, nil
}

func (p *Planner) manualEvent(
	batchID uuid.UUID,
	ordinal int,
	rendered organization.V2ManualFailure,
) (EventPlan, error) {
	if ordinal < 0 || ordinal > 1<<31-1 {
		return EventPlan{}, ErrInvalidPlan
	}
	eventKey := organizationFailureKeyPrefix + batchID.String() + ":" + strconvItoa(ordinal)
	base := notification.Message{
		Type: notification.TypeManual, Title: rendered.Title, Text: rendered.Text,
		Image: rendered.Image, Username: rendered.Username,
	}
	if rendered.Link != nil {
		base.Link = *rendered.Link
	}
	event := EventPlan{Event: notificationhistory.Plan{
		EventKey: eventKey,
		MType:    stringPointer(string(notification.TypeManual)),
		Title:    stringPointer(base.Title), Text: stringPointer(base.Text),
		Image: optionalStringPointer(base.Image), Link: optionalStringPointer(base.Link),
		Note: []byte(`{}`),
	}}
	return p.withTargets(batchID, event, base)
}

func (p *Planner) successEvent(
	batchID uuid.UUID,
	snapshot organization.SuccessSnapshot,
	rendered organization.V2OrganizeSuccess,
) (EventPlan, error) {
	base := notification.Message{
		Type: notification.TypeOrganize, ContentType: notification.ContentOrganizeSuccess,
		Title: rendered.Title, Text: rendered.Text, Image: rendered.Image, Username: rendered.Username,
	}
	if rendered.Link != nil {
		base.Link = *rendered.Link
	}
	eventKey := organizationEventKeyPrefix + batchID.String() + ":" + snapshot.GroupKey
	event := EventPlan{Event: notificationhistory.Plan{
		EventKey: eventKey, MType: stringPointer(string(notification.TypeOrganize)),
		Title: stringPointer(base.Title), Text: stringPointer(base.Text),
		Image: optionalStringPointer(base.Image), Link: optionalStringPointer(base.Link),
	}}
	note, err := json.Marshal(rendered.Context)
	if err != nil || len(note) > MaxNoteBytes {
		return EventPlan{}, ErrInvalidPlan
	}
	event.Event.Note = note
	return p.withTargets(batchID, event, base)
}

func (p *Planner) withTargets(batchID uuid.UUID, event EventPlan, base notification.Message) (EventPlan, error) {
	targets := p.provider.Targets(base)
	if len(targets) > MaxRequestsPerEvent {
		return EventPlan{}, ErrInvalidPlan
	}
	event.Requests = make([]RequestPlan, 0, len(targets))
	for _, target := range targets {
		message := base
		message.Channel, message.Source = target.Channel, target.Source
		id, key, ok := organizationNotificationRequestIdentity(batchID, event.Event.EventKey, target)
		if !ok {
			return EventPlan{}, ErrInvalidPlan
		}
		event.Requests = append(event.Requests, RequestPlan{
			ID: id, Channel: target.Channel, Source: target.Source,
			IdempotencyKey: key, Message: message,
		})
	}
	return event, nil
}

func strconvItoa(value int) string {
	return strconv.Itoa(value)
}
