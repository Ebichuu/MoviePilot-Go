package indexer

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/url"
	"strconv"
)

func (a *SunnyPTAdapter) FetchSiteUserData(ctx context.Context) (SiteUserData, error) {
	result := newSiteUserData(a.site)
	body, err := a.fetchSunnyPTUserDataPage(ctx, sunnyPTEndpoint(a.api, "profile"))
	if err != nil {
		if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
			return SiteUserData{}, err
		}
		result.ErrMsg = userDataString("获取用户信息失败，未收到 API 响应")
	} else if profile, ok := sunnyPTUserDataPayload(body, "获取用户信息", &result); ok {
		result.UserID = userDataID(profile["id"])
		result.Username = userDataString(profile["username"])
		level := profile["level"]
		if valueString(level) == "" {
			level = profile["class"]
			if level == nil {
				level = ""
			}
		}
		result.UserLevel = userDataString(level)
		result.JoinAt = userDataDateTime(profile["registered_at"])
		result.Upload = userDataInt64(profile["uploaded"])
		result.Download = userDataInt64(profile["downloaded"])
		result.Ratio = userDataFloat(profile["ratio"])
		result.Bonus = userDataFloat(profile["bonus"])
		result.Seeding = userDataInt64(profile["seeding_count"])
		result.SeedingSize = userDataInt64(profile["seeding_size"])
		result.Leeching = userDataInt64(profile["leeching_count"])
		result.LeechingSize = userDataInt64(profile["leeching_size"])
		result.MessageUnread = userDataInt(profile["unread_messages"])
	}
	if !siteUserDataMessagesEnabled(ctx) {
		return result, nil
	}

	for page := 1; ; page++ {
		if err := ctx.Err(); err != nil {
			return SiteUserData{}, err
		}
		endpoint, err := url.Parse(sunnyPTEndpoint(a.api, "messages"))
		if err != nil {
			return SiteUserData{}, err
		}
		query := endpoint.Query()
		query.Set("unread_only", "true")
		query.Set("page", strconv.Itoa(page))
		query.Set("page_size", "100")
		endpoint.RawQuery = query.Encode()

		body, err := a.fetchSunnyPTUserDataPage(ctx, endpoint.String())
		if err != nil {
			if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
				return SiteUserData{}, err
			}
			result.ErrMsg = userDataString("获取未读消息失败，未收到 API 响应")
			break
		}
		messages, ok := sunnyPTUserDataPayload(body, "获取未读消息", &result)
		if !ok {
			break
		}
		result.MessageUnread = userDataInt(messages["unread_count"])
		if items, ok := messages["items"].([]any); ok {
			for _, raw := range items {
				message, ok := raw.(map[string]any)
				if !ok || !valueBool(message["unread"]) {
					continue
				}
				title := valueString(message["title"])
				content := valueString(message["content"])
				createdAt := userDataDateTime(message["created_at"])
				if title == "" || content == "" || createdAt == nil || *createdAt == "" {
					continue
				}
				var source any
				if messageID := message["id"]; messageID != nil {
					source = "sunnypt-message:" + valueString(messageID)
				}
				result.MessageUnreadContents = append(
					result.MessageUnreadContents,
					[]any{title, *createdAt, content, source},
				)
			}
		}
		if !valueBool(messages["has_more"]) {
			break
		}
	}
	return result, nil
}

func (a *SunnyPTAdapter) fetchSunnyPTUserDataPage(ctx context.Context, endpoint string) ([]byte, error) {
	response, err := a.client.getResponseLimited(ctx, endpoint, a.headers.Clone(), a.client.maxBodyBytes)
	if err != nil {
		if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
			return nil, err
		}
		return nil, nil
	}
	switch response.StatusCode {
	case http.StatusOK, http.StatusForbidden, http.StatusInternalServerError:
		return response.Body, nil
	default:
		return nil, nil
	}
}

func sunnyPTUserDataPayload(body []byte, operation string, result *SiteUserData) (map[string]any, bool) {
	if len(bytes.TrimSpace(body)) == 0 {
		result.ErrMsg = userDataString(operation + "失败，未收到 API 响应")
		return nil, false
	}
	decoder := json.NewDecoder(bytes.NewReader(body))
	decoder.UseNumber()
	var payload any
	if err := decoder.Decode(&payload); err != nil {
		result.ErrMsg = userDataString(operation + "失败，API 响应不是有效 JSON")
		return nil, false
	}
	if err := decoder.Decode(new(any)); err != io.EOF {
		result.ErrMsg = userDataString(operation + "失败，API 响应不是有效 JSON")
		return nil, false
	}
	document, ok := payload.(map[string]any)
	if !ok {
		result.ErrMsg = userDataString(operation + "失败，API 响应结构无效")
		return nil, false
	}
	if valueString(document["code"]) != "0" {
		message := valueString(document["msg"])
		if message == "" {
			message = operation + "失败"
		}
		result.ErrMsg = &message
		return nil, false
	}
	data, ok := document["data"].(map[string]any)
	if !ok || len(data) == 0 {
		return nil, false
	}
	return data, true
}
