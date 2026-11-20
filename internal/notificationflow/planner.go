package notificationflow

import (
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log/slog"
	"strings"

	"github.com/google/uuid"

	"github.com/moviepilot-go/moviepilot-go/internal/notification"
	"github.com/moviepilot-go/moviepilot-go/internal/notificationhistory"
	"github.com/moviepilot-go/moviepilot-go/internal/organization"
	"github.com/moviepilot-go/moviepilot-go/internal/organizer"
)

const sha256HexLength = sha256.Size * 2

// TargetProvider resolves the immutable delivery targets for one base message.
type TargetProvider interface {
	Targets(notification.Message) []notification.Target
}

// Planner builds a deterministic per-target notification snapshot. An empty
// target list intentionally produces a valid plan with no external effects.
type Planner struct {
	provider          TargetProvider
	businessRenderer  BusinessTemplateRenderer
	businessAppDomain string
	logger            *slog.Logger
}

// SetLogger 注入结构化日志器，用于记录「没有可投递渠道」这类静默失败。
func (p *Planner) SetLogger(logger *slog.Logger) {
	if p == nil || logger == nil {
		return
	}
	p.logger = logger
}

// warnNoTargets 记录「本轮没有任何渠道被选中」的原因：这类空计划不会产生
// notifications 行，如果只看数据库会误判成投递成功。
func (p *Planner) warnNoTargets(mtype notification.Type, contentType notification.ContentType, eventKey, username string) {
	logger := p.logger
	if logger == nil {
		logger = slog.Default()
	}
	logger.Warn("通知没有可投递的渠道，未产生投递行",
		slog.String("event_key", eventKey),
		slog.String("mtype", string(mtype)),
		slog.String("content_type", string(contentType)),
		slog.String("username", username),
		slog.String("reason", "没有启用的渠道在 switchs 里勾选该通知类型"))
}

// BuildPlan is the compatibility constructor used by organizationflow tests
// and adapters that only have a file count rather than the full organizer plan.
// Duplicate channels are ignored deterministically while preserving order.
func BuildPlan(batchID uuid.UUID, username string, fileCount int, channels []notification.Channel) (Plan, error) {
	if fileCount < 0 {
		return Plan{}, ErrInvalidPlan
	}
	targets := make([]notification.Target, 0, min(len(channels), MaxRequestsPerEvent))
	seen := make(map[notification.Channel]struct{}, len(channels))
	for _, channel := range channels {
		if !validChannel(channel) {
			return Plan{}, ErrInvalidPlan
		}
		if _, exists := seen[channel]; exists {
			continue
		}
		if len(targets) == MaxRequestsPerEvent {
			return Plan{}, ErrInvalidPlan
		}
		seen[channel] = struct{}{}
		targets = append(targets, notification.Target{Channel: channel})
	}
	planner, err := NewPlanner(staticTargetProvider(targets))
	if err != nil {
		return Plan{}, err
	}
	return planner.planWithCount(batchID, username, fileCount)
}

// NewPlanner creates a planner backed by the current notification target snapshot.
func NewPlanner(provider TargetProvider) (*Planner, error) {
	if provider == nil {
		return nil, ErrInvalidPlan
	}
	return &Planner{provider: provider}, nil
}

// Plan builds one immutable organization-completion delivery per channel.
func (p *Planner) Plan(batchID uuid.UUID, username string, plan organizer.Plan) (Plan, error) {
	if p == nil {
		return Plan{}, ErrInvalidPlan
	}
	return p.planWithCount(batchID, username, len(plan.Operations))
}

// PlanSuccess builds the V2 organizeSuccess history and delivery events for
// the immutable media-and-season snapshots produced by one organization batch.
func (p *Planner) PlanSuccess(
	batchID uuid.UUID,
	username string,
	snapshots []organization.SuccessSnapshot,
	appDomain string,
) (Plan, error) {
	username = strings.TrimSpace(username)
	if p == nil || p.provider == nil || batchID == uuid.Nil ||
		!validRequiredTrimmedString(username, MaxUsernameBytes) || len(snapshots) > MaxEventsPerPlan {
		return Plan{}, ErrInvalidPlan
	}
	result := Plan{BatchID: batchID, Username: username}
	seenGroups := make(map[string]struct{}, len(snapshots))
	var intentID uuid.UUID
	legacyEventCreated := false
	totalRequests := 0
	for _, snapshot := range snapshots {
		normalized, err := organization.NormalizeSuccessSnapshot(snapshot)
		if err != nil || normalized.BatchID != batchID || normalized.Username != username ||
			(intentID != uuid.Nil && normalized.IntentID != intentID) {
			return Plan{}, ErrInvalidPlan
		}
		if intentID == uuid.Nil {
			intentID = normalized.IntentID
		}
		if _, exists := seenGroups[normalized.GroupKey]; exists {
			return Plan{}, ErrInvalidPlan
		}
		seenGroups[normalized.GroupKey] = struct{}{}
		if !normalized.Eligible() {
			continue
		}

		var event EventPlan
		if normalized.Legacy {
			if legacyEventCreated {
				return Plan{}, ErrInvalidPlan
			}
			legacyPlan, planErr := p.planWithCount(batchID, username, len(normalized.Files))
			if planErr != nil || len(legacyPlan.Events) != 1 {
				return Plan{}, ErrInvalidPlan
			}
			event = legacyPlan.Events[0]
			legacyEventCreated = true
		} else {
			event, err = p.planV2SuccessEvent(batchID, normalized, appDomain)
			if err != nil {
				return Plan{}, ErrInvalidPlan
			}
		}
		if len(result.Events) == MaxEventsPerPlan ||
			totalRequests > MaxRequestsPerPlan-len(event.Requests) {
			return Plan{}, ErrInvalidPlan
		}
		totalRequests += len(event.Requests)
		result.Events = append(result.Events, event)
	}
	if !result.Valid() {
		return Plan{}, ErrInvalidPlan
	}
	return result, nil
}

func (p *Planner) planV2SuccessEvent(
	batchID uuid.UUID,
	snapshot organization.SuccessSnapshot,
	appDomain string,
) (EventPlan, error) {
	rendered, err := organization.RenderV2OrganizeSuccess(snapshot, appDomain)
	if err != nil {
		return EventPlan{}, err
	}
	note, err := json.Marshal(rendered.Context)
	if err != nil || len(note) > MaxNoteBytes {
		return EventPlan{}, ErrInvalidPlan
	}
	eventKey := organizationEventKeyPrefix + batchID.String() + ":" + snapshot.GroupKey
	baseMessage := notification.Message{
		Type:        notification.TypeOrganize,
		ContentType: notification.ContentOrganizeSuccess,
		Title:       rendered.Title,
		Text:        rendered.Text,
		Image:       rendered.Image,
		Username:    rendered.Username,
	}
	if rendered.Link != nil {
		baseMessage.Link = *rendered.Link
	}
	event := EventPlan{Event: notificationhistory.Plan{
		EventKey: eventKey,
		MType:    stringPointer(string(baseMessage.Type)),
		Title:    stringPointer(baseMessage.Title),
		Text:     stringPointer(baseMessage.Text),
		Image:    optionalStringPointer(baseMessage.Image),
		Link:     optionalStringPointer(baseMessage.Link),
		Note:     note,
	}}
	targets := p.provider.Targets(baseMessage)
	if len(targets) > MaxRequestsPerEvent {
		return EventPlan{}, ErrInvalidPlan
	}
	if len(targets) == 0 {
		p.warnNoTargets(baseMessage.Type, baseMessage.ContentType, eventKey, rendered.Username)
	}
	event.Requests = make([]RequestPlan, 0, len(targets))
	for _, target := range targets {
		message := baseMessage
		message.Channel = target.Channel
		message.Source = target.Source
		requestID, idempotencyKey, identityValid := organizationNotificationRequestIdentity(
			batchID, eventKey, target,
		)
		if !identityValid {
			return EventPlan{}, ErrInvalidPlan
		}
		event.Requests = append(event.Requests, RequestPlan{
			ID: requestID, Channel: target.Channel, Source: target.Source,
			IdempotencyKey: idempotencyKey, Message: message,
		})
	}
	return event, nil
}

func (p *Planner) planWithCount(batchID uuid.UUID, username string, fileCount int) (Plan, error) {
	username = strings.TrimSpace(username)
	if p == nil || p.provider == nil || batchID == uuid.Nil || username == "" || fileCount < 0 {
		return Plan{}, ErrInvalidPlan
	}
	text := "文件已成功整理到媒体库"
	if fileCount > 0 {
		text = fmt.Sprintf("已整理 %d 个文件到媒体库", fileCount)
	}
	baseMessage := notification.Message{
		Type:        notification.TypeOrganize,
		ContentType: notification.ContentOrganizeSuccess,
		Title:       "整理完成",
		Text:        text,
	}
	result := Plan{
		BatchID:  batchID,
		Username: username,
		Events: []EventPlan{{
			Event: notificationhistory.Plan{
				EventKey: "organization-completed:" + batchID.String(),
				MType:    stringPointer(string(baseMessage.Type)),
				Title:    stringPointer(baseMessage.Title),
				Text:     stringPointer(baseMessage.Text),
				Note:     []byte(`{}`),
			},
		}},
	}
	event := &result.Events[0]
	targets := p.provider.Targets(baseMessage)
	if len(targets) > MaxRequestsPerEvent || len(targets) > MaxRequestsPerPlan {
		return Plan{}, ErrInvalidPlan
	}
	if len(targets) == 0 {
		p.warnNoTargets(baseMessage.Type, baseMessage.ContentType, event.Event.EventKey, username)
	}
	for _, target := range targets {
		message := baseMessage
		message.Channel = target.Channel
		message.Source = target.Source
		requestID, idempotencyKey, identityValid := organizationNotificationRequestIdentity(
			batchID, event.Event.EventKey, target,
		)
		if !identityValid {
			return Plan{}, ErrInvalidPlan
		}
		event.Requests = append(event.Requests, RequestPlan{
			ID:             requestID,
			Channel:        target.Channel,
			Source:         target.Source,
			IdempotencyKey: idempotencyKey,
			Message:        message,
		})
	}
	if !result.Valid() {
		return Plan{}, ErrInvalidPlan
	}
	return result, nil
}

func stringPointer(value string) *string {
	return &value
}

func optionalStringPointer(value string) *string {
	if value == "" {
		return nil
	}
	return stringPointer(value)
}

type staticTargetProvider []notification.Target

func (p staticTargetProvider) Targets(notification.Message) []notification.Target {
	return append([]notification.Target(nil), p...)
}

func organizationNotificationIdentity(batchID uuid.UUID, target notification.Target) []byte {
	identity := make([]byte, 0, 64+len(target.Source))
	identity = appendIdentityField(identity, "organization-completed")
	identity = appendIdentityField(identity, batchID.String())
	identity = appendIdentityField(identity, string(target.Channel))
	return appendIdentityField(identity, target.Source)
}

func groupedOrganizationNotificationIdentity(eventKey string, target notification.Target) []byte {
	identity := make([]byte, 0, len(eventKey)+len(target.Source)+64)
	identity = appendIdentityField(identity, eventKey)
	identity = appendIdentityField(identity, string(target.Channel))
	return appendIdentityField(identity, target.Source)
}

func organizationNotificationRequestIdentity(
	batchID uuid.UUID,
	eventKey string,
	target notification.Target,
) (uuid.UUID, string, bool) {
	kind := organizationEventKeyKind(eventKey, batchID)
	if kind == organizationEventInvalid || !validChannel(target.Channel) || !validOptionalString(target.Source, MaxSourceBytes) {
		return uuid.Nil, "", false
	}
	identity := organizationNotificationIdentity(batchID, target)
	if kind != organizationEventLegacySuccess {
		identity = groupedOrganizationNotificationIdentity(eventKey, target)
	}
	digest := sha256.Sum256(identity)
	idempotencyPrefix := organizationEventKeyPrefix
	if kind == organizationEventFileFailure {
		idempotencyPrefix = organizationFailureKeyPrefix
	}
	return uuid.NewSHA1(batchID, identity),
		idempotencyPrefix + hex.EncodeToString(digest[:]), true
}

func appendIdentityField(identity []byte, value string) []byte {
	var size [8]byte
	binary.BigEndian.PutUint64(size[:], uint64(len(value)))
	identity = append(identity, size[:]...)
	return append(identity, value...)
}
