package notificationflow

import (
	"crypto/sha256"
	"encoding/hex"

	"github.com/google/uuid"

	"github.com/moviepilot-go/moviepilot-go/internal/notification"
)

func businessContentType(ctype notification.ContentType) (notification.Type, notification.ContentType) {
	switch ctype {
	case notification.ContentDownloadAdded:
		return notification.TypeDownload, ctype
	case notification.ContentSubscribeAdded:
		return notification.TypeSubscribe, ctype
	case notification.ContentSubscribeComplete:
		return notification.TypeSubscribe, ctype
	default:
		return "", ""
	}
}

func businessRequestIdentity(
	namespace uuid.UUID,
	historyKey string,
	target notification.Target,
) (uuid.UUID, string, bool) {
	if !validChannel(target.Channel) || !validOptionalString(target.Source, MaxSourceBytes) {
		return uuid.Nil, "", false
	}
	identity := appendIdentityField(nil, historyKey)
	identity = appendIdentityField(identity, string(target.Channel))
	identity = appendIdentityField(identity, target.Source)
	digest := sha256.Sum256(identity)
	return uuid.NewSHA1(namespace, identity), businessRequestKeyPrefix + hex.EncodeToString(digest[:]), true
}

// Valid 校验业务计划边界：事件/渠道身份一致、请求 ID 与幂等键唯一。
func (p BusinessPlan) Valid() bool {
	if p.Namespace == uuid.Nil || !validRequiredTrimmedString(p.Username, MaxUsernameBytes) ||
		len(p.Events) > MaxEventsPerPlan {
		return false
	}
	seenEvents := make(map[string]struct{}, len(p.Events))
	seenIDs := make(map[uuid.UUID]struct{})
	seenKeys := make(map[string]struct{})
	totalRequests := 0
	for _, event := range p.Events {
		if event.History.EventKey == "" || event.History.MType == nil || event.History.Title == nil ||
			event.History.Text == nil || !event.History.Valid() || len(event.Requests) > MaxRequestsPerEvent {
			return false
		}
		if _, exists := seenEvents[event.History.EventKey]; exists {
			return false
		}
		seenEvents[event.History.EventKey] = struct{}{}
		totalRequests += len(event.Requests)
		if totalRequests > MaxRequestsPerPlan {
			return false
		}
		seenTargets := make(map[string]struct{}, len(event.Requests))
		for _, request := range event.Requests {
			if request.ID == uuid.Nil || !validChannel(request.Channel) || request.IdempotencyKey == "" ||
				len(request.IdempotencyKey) > 255 || request.Message.Channel != request.Channel ||
				request.Message.Source != request.Source || request.Message.Username != p.Username {
				return false
			}
			if _, exists := seenIDs[request.ID]; exists {
				return false
			}
			if _, exists := seenKeys[request.IdempotencyKey]; exists {
				return false
			}
			targetKey := string(request.Channel) + "\x00" + request.Source
			if _, exists := seenTargets[targetKey]; exists {
				return false
			}
			seenIDs[request.ID] = struct{}{}
			seenKeys[request.IdempotencyKey] = struct{}{}
			seenTargets[targetKey] = struct{}{}
		}
	}
	return true
}
