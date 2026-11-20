package notificationflow

import (
	"net/url"
	"strings"

	"github.com/google/uuid"

	"github.com/moviepilot-go/moviepilot-go/internal/notification"
	"github.com/moviepilot-go/moviepilot-go/internal/notificationhistory"
)

const (
	downloadAddedEventKeyPrefix     = "download-added:"
	subscribeAddedEventKeyPrefix    = "subscribe-added:"
	subscribeCompleteEventKeyPrefix = "subscribe-complete:"
	businessRequestKeyPrefix        = "business-event:"
)

// BusinessInput 是 V2 `DownloadAdded` / `SubscribeComplete` 通知的冻结输入。
type BusinessInput struct {
	// EventKey 是业务侧稳定幂等来源键（例如下载意图 ID 或订阅完成事件键）。
	EventKey    string
	Username    string
	ContentType notification.ContentType
	Title       string
	Text        string
	Image       string
	Link        string
	LinkRoute   string
	UserID      string
	Context     map[string]any
}

// BusinessTemplateRenderer renders one frozen business input with the current
// NotificationTemplates snapshot.
type BusinessTemplateRenderer interface {
	Apply(notification.Message, map[string]any) (notification.Message, error)
}

// BusinessPlanner builds one business event for atomic outbox publication.
type BusinessPlanner interface {
	PlanBusiness(BusinessInput) (BusinessPlan, error)
}

// BusinessRequest 是一次待投递的渠道请求。
type BusinessRequest struct {
	ID             uuid.UUID
	Channel        notification.Channel
	Source         string
	IdempotencyKey string
	Message        notification.Message
}

// BusinessEvent 是一个业务通知事件及其渠道请求。
type BusinessEvent struct {
	History  notificationhistory.Plan
	Requests []BusinessRequest
}

// BusinessPlan 是一批业务通知事件，独立于整理批次的事件键空间。
type BusinessPlan struct {
	Namespace uuid.UUID
	Username  string
	Events    []BusinessEvent
}

// NewBusinessPlanner creates the planner used by the durable business outbox.
func NewBusinessPlanner(provider TargetProvider, renderer BusinessTemplateRenderer, appDomain string) (*Planner, error) {
	planner, err := NewPlanner(provider)
	if err != nil {
		return nil, err
	}
	planner.businessRenderer = renderer
	planner.businessAppDomain = strings.TrimSpace(appDomain)
	return planner, nil
}

// PlanBusiness dispatches one persisted outbox fact to its V2 content type.
func (p *Planner) PlanBusiness(input BusinessInput) (BusinessPlan, error) {
	switch input.ContentType {
	case notification.ContentDownloadAdded:
		return p.PlanDownloadAdded(input)
	case notification.ContentSubscribeAdded:
		return p.PlanSubscribeAdded(input)
	case notification.ContentSubscribeComplete:
		return p.PlanSubscribeComplete(input)
	default:
		return BusinessPlan{}, ErrInvalidPlan
	}
}

// PlanDownloadAdded 复刻 V2 `downloadAdded` 通知：接受阶段结算后按下载意图触发。
func (p *Planner) PlanDownloadAdded(input BusinessInput) (BusinessPlan, error) {
	input.ContentType = notification.ContentDownloadAdded
	return p.planBusinessEvent(downloadAddedEventKeyPrefix, input)
}

// PlanSubscribeAdded 复刻 V2 `subscribeAdded` 通知。
func (p *Planner) PlanSubscribeAdded(input BusinessInput) (BusinessPlan, error) {
	input.ContentType = notification.ContentSubscribeAdded
	return p.planBusinessEvent(subscribeAddedEventKeyPrefix, input)
}

// PlanSubscribeComplete 复刻 V2 `subscribeComplete` 通知：订阅全部完成时触发。
func (p *Planner) PlanSubscribeComplete(input BusinessInput) (BusinessPlan, error) {
	input.ContentType = notification.ContentSubscribeComplete
	return p.planBusinessEvent(subscribeCompleteEventKeyPrefix, input)
}

func (p *Planner) planBusinessEvent(prefix string, input BusinessInput) (BusinessPlan, error) {
	username := strings.TrimSpace(input.Username)
	eventKey := strings.TrimSpace(input.EventKey)
	mtype, contentType := businessContentType(input.ContentType)
	if p == nil || p.provider == nil || mtype == "" ||
		!validRequiredTrimmedString(username, MaxUsernameBytes) ||
		!validRequiredTrimmedString(eventKey, MaxSourceBytes) {
		return BusinessPlan{}, ErrInvalidPlan
	}
	title := strings.TrimSpace(input.Title)
	text := input.Text
	if p.businessRenderer == nil && (!validRequiredString(title, MaxTitleBytes) || !validRequiredString(text, MaxTextBytes)) {
		return BusinessPlan{}, ErrInvalidPlan
	}
	if !validOptionalString(title, MaxTitleBytes) || !validOptionalString(text, MaxTextBytes) {
		return BusinessPlan{}, ErrInvalidPlan
	}
	link := strings.TrimSpace(input.Link)
	if link == "" && input.LinkRoute != "" {
		link = businessNotificationLink(p.businessAppDomain, input.LinkRoute)
	}
	base := notification.Message{
		Type: mtype, ContentType: contentType, Title: title, Text: text,
		Image: strings.TrimSpace(input.Image), Link: link, UserID: strings.TrimSpace(input.UserID), Username: username,
	}
	if p.businessRenderer != nil {
		rendered, err := p.businessRenderer.Apply(base, input.Context)
		if err != nil {
			return BusinessPlan{}, ErrInvalidPlan
		}
		base = rendered
		base.Title = strings.TrimSpace(base.Title)
		if !validOptionalString(base.Title, MaxTitleBytes) || !validOptionalString(base.Text, MaxTextBytes) ||
			!validOptionalString(base.Image, MaxImageBytes) || !validOptionalString(base.Link, MaxLinkBytes) {
			return BusinessPlan{}, ErrInvalidPlan
		}
	}
	historyKey := prefix + eventKey
	history := notificationhistory.Plan{
		EventKey: historyKey, MType: stringPointer(string(mtype)),
		Title: stringPointer(base.Title), Text: stringPointer(base.Text),
		Image: optionalStringPointer(base.Image), Link: optionalStringPointer(base.Link),
		Note: []byte(`{}`),
	}
	targets := p.provider.Targets(base)
	if len(targets) > MaxRequestsPerEvent {
		return BusinessPlan{}, ErrInvalidPlan
	}
	if len(targets) == 0 {
		p.warnNoTargets(mtype, contentType, historyKey, username)
	}
	namespace := uuid.NewSHA1(uuid.NameSpaceOID, []byte(historyKey))
	event := BusinessEvent{History: history}
	for _, target := range targets {
		message := base
		message.Channel, message.Source = target.Channel, target.Source
		id, key, ok := businessRequestIdentity(namespace, historyKey, target)
		if !ok {
			return BusinessPlan{}, ErrInvalidPlan
		}
		event.Requests = append(event.Requests, BusinessRequest{
			ID: id, Channel: target.Channel, Source: target.Source,
			IdempotencyKey: key, Message: message,
		})
	}
	result := BusinessPlan{Namespace: namespace, Username: username, Events: []BusinessEvent{event}}
	if !result.Valid() {
		return BusinessPlan{}, ErrInvalidPlan
	}
	return result, nil
}

func businessNotificationLink(appDomain, route string) string {
	if appDomain == "" || len(appDomain) > MaxLinkBytes || len(route) > MaxLinkBytes || strings.ContainsRune(appDomain, '\x00') || strings.ContainsRune(route, '\x00') {
		return ""
	}
	host := appDomain
	if !strings.HasSuffix(host, "/") {
		host += "/"
	}
	if !strings.HasPrefix(host, "http://") && !strings.HasPrefix(host, "https://") {
		host = "http://" + host
	}
	base, err := url.Parse(host)
	if err != nil {
		return ""
	}
	fragment := strings.TrimPrefix(strings.TrimSpace(route), "#")
	if fragment == "" {
		return ""
	}
	if !strings.HasPrefix(fragment, "/") {
		fragment = "/" + fragment
	}
	link := base.ResolveReference(&url.URL{Fragment: fragment}).String()
	if len(link) > MaxLinkBytes {
		return ""
	}
	return link
}
