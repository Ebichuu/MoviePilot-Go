package indexer

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"math"
	"net/http"
	"net/url"
	"strconv"
)

func (a *RousiAdapter) FetchSiteUserData(ctx context.Context) (SiteUserData, error) {
	result := newSiteUserData(a.base)
	profileEndpoint, err := resolveSiteURL(a.base, "/api/v1/profile")
	if err != nil {
		return SiteUserData{}, err
	}
	profileURL, err := url.Parse(profileEndpoint)
	if err != nil {
		return SiteUserData{}, err
	}
	query := profileURL.Query()
	query.Set("include_fields[user]", "seeding_leeching_data")
	profileURL.RawQuery = query.Encode()
	profileHeaders := a.headers.Clone()
	profileHeaders.Set("Content-Type", "application/json")

	body, err := a.fetchRousiUserDataPage(ctx, profileURL.String(), profileHeaders)
	if err != nil {
		if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
			return SiteUserData{}, err
		}
	} else if document, ok := decodeRousiUserDataJSON(body); ok {
		if !rousiUserDataCodeZero(document["code"]) {
			message, exists := document["message"]
			if !exists {
				result.ErrMsg = userDataString("未知错误")
			} else {
				result.ErrMsg = userDataString(message)
			}
		} else if profile, ok := document["data"].(map[string]any); ok && len(profile) > 0 {
			result.UserID = userDataID(profile["id"])
			result.Username = userDataString(profile["username"])
			level := profile["level_text"]
			if valueString(level) == "" {
				level = profile["role_text"]
			}
			result.UserLevel = userDataString(level)
			result.JoinAt = userDataDateTime(profile["registered_at"])
			if result.JoinAt != nil && len(*result.JoinAt) > 19 {
				short := (*result.JoinAt)[:19]
				result.JoinAt = &short
			}
			result.Upload = userDataInt64(profile["uploaded"])
			result.Download = userDataInt64(profile["downloaded"])
			result.Ratio = math.Round(userDataFloat(profile["ratio"])*100) / 100
			result.Bonus = userDataFloat(profile["karma"])
			if activity, ok := profile["seeding_leeching_data"].(map[string]any); ok {
				result.Seeding = userDataInt64(activity["seeding_count"])
				result.SeedingSize = userDataInt64(activity["seeding_size"])
				result.Leeching = userDataInt64(activity["leeching_count"])
				result.LeechingSize = userDataInt64(activity["leeching_size"])
			}
		}
	}
	if a.messageHeaders == nil || !siteUserDataMessagesEnabled(ctx) {
		return result, nil
	}

	firstPage, err := a.fetchRousiUserDataMessages(ctx, 0)
	if err != nil {
		return SiteUserData{}, err
	}
	messages := rousiUserDataMessages(firstPage["messages"])
	totalPages := userDataInt(firstPage["total_pages"])
	for page := 1; page < totalPages; page++ {
		pageData, err := a.fetchRousiUserDataMessages(ctx, page)
		if err != nil {
			return SiteUserData{}, err
		}
		messages = append(messages, rousiUserDataMessages(pageData["messages"])...)
	}
	result.MessageUnread = len(messages)
	for _, message := range messages {
		var createdAt any
		if parsed := userDataDateTime(message["created_at"]); parsed != nil {
			createdAt = *parsed
		}
		result.MessageUnreadContents = append(result.MessageUnreadContents, []any{
			message["title"],
			createdAt,
			message["content"],
		})
	}

	readAllEndpoint, err := resolveSiteURL(a.base, "/api/messages/read-all")
	if err != nil {
		return SiteUserData{}, err
	}
	// V2 treats marking messages read as best-effort after the snapshot is parsed.
	_, _ = a.client.do(ctx, http.MethodPost, readAllEndpoint, a.messageHeaders.Clone(), nil)
	return result, nil
}

func (a *RousiAdapter) fetchRousiUserDataPage(ctx context.Context, endpoint string, headers http.Header) ([]byte, error) {
	response, err := a.client.getResponseLimited(ctx, endpoint, headers, a.client.maxBodyBytes)
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

func (a *RousiAdapter) fetchRousiUserDataMessages(ctx context.Context, page int) (map[string]any, error) {
	endpoint, err := resolveSiteURL(a.base, "/api/messages")
	if err != nil {
		return nil, err
	}
	parsed, err := url.Parse(endpoint)
	if err != nil {
		return nil, err
	}
	query := parsed.Query()
	query.Set("page", strconv.Itoa(page))
	query.Set("page_size", "100")
	query.Set("unread_only", "true")
	parsed.RawQuery = query.Encode()
	response, err := a.client.getResponseLimited(ctx, parsed.String(), a.messageHeaders.Clone(), a.client.maxBodyBytes)
	if err != nil {
		if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
			return nil, err
		}
		return map[string]any{}, nil
	}
	if response.StatusCode != http.StatusOK {
		return map[string]any{}, nil
	}
	document, ok := decodeRousiUserDataJSON(response.Body)
	if !ok || !rousiUserDataCodeZero(document["code"]) {
		return map[string]any{}, nil
	}
	data, ok := document["data"].(map[string]any)
	if !ok {
		return map[string]any{}, nil
	}
	return data, nil
}

func decodeRousiUserDataJSON(body []byte) (map[string]any, bool) {
	if len(bytes.TrimSpace(body)) == 0 {
		return nil, false
	}
	decoder := json.NewDecoder(bytes.NewReader(body))
	decoder.UseNumber()
	var payload any
	if err := decoder.Decode(&payload); err != nil {
		return nil, false
	}
	if err := decoder.Decode(new(any)); err != io.EOF {
		return nil, false
	}
	document, ok := payload.(map[string]any)
	return document, ok
}

func rousiUserDataCodeZero(value any) bool {
	switch typed := value.(type) {
	case json.Number:
		parsed, err := typed.Int64()
		return err == nil && parsed == 0
	case float64:
		return typed == 0
	case int:
		return typed == 0
	case int64:
		return typed == 0
	default:
		return false
	}
}

func rousiUserDataMessages(value any) []map[string]any {
	items, ok := value.([]any)
	if !ok {
		return nil
	}
	messages := make([]map[string]any, 0, len(items))
	for _, item := range items {
		if message, ok := item.(map[string]any); ok {
			messages = append(messages, message)
		}
	}
	return messages
}
