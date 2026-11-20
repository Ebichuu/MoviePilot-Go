package indexer

import (
	"context"
	"math"
	"net/http"
)

func (a *TNodeAdapter) FetchSiteUserData(ctx context.Context) (SiteUserData, error) {
	result := newSiteUserData(a.base)
	headers := a.headers.Clone()
	token, err := a.search.csrf(ctx, headers)
	if err != nil {
		return result, err
	}
	if token != "" {
		headers.Set(a.search.def.CSRFHeader, token)
	}

	profileEndpoint, err := resolveSiteURL(a.base, "/api/user/getMainInfo")
	if err != nil {
		return result, err
	}
	profileBody, err := a.client.get(ctx, profileEndpoint, headers.Clone())
	if err != nil {
		return result, err
	}
	if profile, decodeErr := decodeJSONObject(profileBody); decodeErr == nil && userDataInt(profile["status"]) == http.StatusOK {
		if user, ok := profile["data"].(map[string]any); ok {
			result.UserID = userDataID(user["id"])
			result.Username = userDataString(user["username"])
			if class, ok := user["class"].(map[string]any); ok {
				result.UserLevel = userDataString(class["name"])
			}
			result.JoinAt = userDataDateTime(user["regTime"])
			result.Upload = userDataInt64(user["upload"])
			result.Download = userDataInt64(user["download"])
			if result.Download > 0 {
				result.Ratio = math.Round(float64(result.Upload)/float64(result.Download)*1000) / 1000
			}
			result.Bonus = userDataFloat(user["bonus"])
			result.MessageUnread = userDataInt(user["unreadAdmin"]) +
				userDataInt(user["unreadInbox"]) + userDataInt(user["unreadSystem"])
		}
	}

	seedingEndpoint, err := resolveSiteURL(a.base, "/api/user/listTorrentActivity?id=&type=seeding&page=1&size=20000")
	if err != nil {
		return result, err
	}
	seedingBody, err := a.client.get(ctx, seedingEndpoint, headers.Clone())
	if err != nil {
		return result, err
	}
	seeding, decodeErr := decodeJSONObject(seedingBody)
	if decodeErr != nil || userDataInt(seeding["status"]) != http.StatusOK {
		return result, nil
	}
	data, ok := seeding["data"].(map[string]any)
	if !ok {
		return result, nil
	}
	torrents, ok := data["torrents"].([]any)
	if !ok {
		return result, nil
	}
	for _, raw := range torrents {
		if err := ctx.Err(); err != nil {
			return result, err
		}
		torrent, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		size := userDataInt64(torrent["size"])
		seeders := userDataInt64(torrent["seeding"])
		result.Seeding++
		result.SeedingSize += size
		result.SeedingInfo = append(result.SeedingInfo, []int64{seeders, size})
	}
	return result, nil
}
