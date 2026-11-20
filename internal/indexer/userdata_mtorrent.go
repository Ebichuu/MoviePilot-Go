package indexer

import (
	"context"
	"net/http"
	"net/url"
	"strings"
)

const mTorrentUserDataPageSize = 200

func (a *MTorrentAdapter) FetchSiteUserData(ctx context.Context) (SiteUserData, error) {
	result := newSiteUserData(a.site)
	profileEndpoint, err := resolveSiteURL(a.api, "/api/member/profile")
	if err != nil {
		return result, err
	}
	profileBody, err := a.postUserDataForm(ctx, profileEndpoint, url.Values{"uid": {""}})
	if err != nil {
		return result, err
	}
	profile, err := decodeJSONObject(profileBody)
	if err != nil {
		return result, err
	}
	if valueString(profile["code"]) != "0" {
		return result, nil
	}
	user, ok := profile["data"].(map[string]any)
	if !ok {
		return result, nil
	}
	result.UserID = userDataID(user["id"])
	result.Username = userDataString(user["username"])
	result.UserLevel = mTorrentUserDataLevel(user["role"])
	if status, ok := user["memberStatus"].(map[string]any); ok {
		result.JoinAt = userDataDateTime(status["createdDate"])
	}
	if count, ok := user["memberCount"].(map[string]any); ok {
		result.Upload = userDataInt64(count["uploaded"])
		result.Download = userDataInt64(count["downloaded"])
		result.Ratio = userDataFloat(count["shareRate"])
		result.Bonus = userDataFloat(count["bonus"])
	}
	if siteUserDataMessagesEnabled(ctx) {
		if err := a.fetchMTorrentUserDataMessages(ctx, &result); err != nil {
			return result, err
		}
	}

	seedingEndpoint, err := resolveSiteURL(a.api, "/api/member/getUserTorrentList")
	if err != nil {
		return result, err
	}
	trackerEndpoint, err := resolveSiteURL(a.api, "/api/tracker/myPeerStatus")
	if err != nil {
		return result, err
	}
	for pageNumber := 1; ; pageNumber++ {
		if err := ctx.Err(); err != nil {
			return result, err
		}
		body, requestErr := a.client.postJSON(ctx, seedingEndpoint, a.headers.Clone(), map[string]any{
			"pageNumber": pageNumber,
			"pageSize":   mTorrentUserDataPageSize,
			"type":       "SEEDING",
			"userid":     result.UserID,
		})
		if requestErr != nil {
			return result, requestErr
		}
		seeding, decodeErr := decodeJSONObject(body)
		if decodeErr != nil {
			return result, decodeErr
		}
		if valueString(seeding["code"]) != "0" {
			return result, nil
		}
		data, ok := seeding["data"].(map[string]any)
		if !ok {
			return result, nil
		}
		items, ok := data["data"].([]any)
		if !ok {
			return result, nil
		}
		for _, raw := range items {
			if err := ctx.Err(); err != nil {
				return result, err
			}
			item, ok := raw.(map[string]any)
			if !ok {
				continue
			}
			torrent, ok := item["torrent"].(map[string]any)
			if !ok {
				continue
			}
			size := userDataInt64(torrent["size"])
			seeders := userDataInt64(torrent["source"])
			result.Seeding++
			result.SeedingSize += size
			result.SeedingInfo = append(result.SeedingInfo, []int64{seeders, size})
		}

		trackerBody, trackerErr := a.postUserDataForm(ctx, trackerEndpoint, url.Values{
			"uid": {valueString(result.UserID)},
		})
		if trackerErr != nil {
			if err := ctx.Err(); err != nil {
				return result, err
			}
			return result, nil
		}
		tracker, decodeErr := decodeJSONObject(trackerBody)
		if decodeErr != nil {
			return result, nil
		}
		trackerData, _ := tracker["data"].(map[string]any)
		total := userDataInt64(trackerData["seeder"])
		if total == 0 || result.Seeding >= total {
			return result, nil
		}
	}
}

func (a *MTorrentAdapter) fetchMTorrentUserDataMessages(ctx context.Context, result *SiteUserData) error {
	searchEndpoint, err := resolveSiteURL(a.api, "/api/msg/search")
	if err != nil {
		return err
	}
	body, err := a.postUserDataForm(ctx, searchEndpoint, url.Values{
		"keyword":  {""},
		"box":      {"-2"},
		"type":     {"pageNumber"},
		"pageSize": {"100"},
	})
	if err != nil {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return ctxErr
		}
		return nil
	}
	document, err := decodeJSONObject(body)
	if err != nil {
		return err
	}
	if valueString(document["code"]) != "0" {
		return nil
	}
	data, _ := document["data"].(map[string]any)
	messages, _ := data["data"].([]any)
	markReadEndpoint, err := resolveSiteURL(a.api, "/api/msg/markRead")
	if err != nil {
		return err
	}
	for _, raw := range messages {
		if err := ctx.Err(); err != nil {
			return err
		}
		message, ok := raw.(map[string]any)
		if !ok || !valueBool(message["unread"]) {
			continue
		}
		title := valueString(message["title"])
		date := valueString(message["createdDate"])
		content := valueString(message["context"])
		if title == "" || date == "" || content == "" {
			continue
		}
		result.MessageUnreadContents = append(result.MessageUnreadContents, []any{title, date, content})
		_, markErr := a.postUserDataForm(ctx, markReadEndpoint, url.Values{
			"msgId": {valueString(message["id"])},
		})
		if markErr != nil && ctx.Err() != nil {
			return ctx.Err()
		}
	}
	return nil
}

func (a *MTorrentAdapter) postUserDataForm(ctx context.Context, endpoint string, values url.Values) ([]byte, error) {
	headers := a.headers.Clone()
	headers.Set("Content-Type", "application/x-www-form-urlencoded; charset=UTF-8")
	return a.client.do(ctx, http.MethodPost, endpoint, headers, []byte(values.Encode()))
}

func mTorrentUserDataLevel(value any) *string {
	role := strings.TrimSpace(valueString(value))
	if role == "" {
		role = "1"
	}
	levels := map[string]string{
		"1": "User", "2": "Power User", "3": "Elite User", "4": "Crazy User",
		"5": "Insane User", "6": "Veteran User", "7": "Extreme User", "8": "Ultimate User",
		"9": "Nexus Master", "10": "VIP", "11": "Retiree", "12": "Uploader",
		"13": "Moderator", "14": "Administrator", "15": "Sysop", "16": "Staff",
		"17": "Offer memberStaff", "18": "Bet memberStaff",
	}
	level, ok := levels[role]
	if !ok {
		return nil
	}
	return &level
}
