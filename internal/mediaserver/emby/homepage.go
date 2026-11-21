package emby

import (
	"context"
	"net/url"
	"strconv"
	"strings"
)

const (
	maxHomepageLimit     = 100
	maxHomepageRespBytes = 2 << 20
)

// PlayItemUserData 表示 Emby 用户观看进度数据。
type PlayItemUserData struct {
	PlayedPercentage *float64 `json:"PlayedPercentage,omitempty"`
	Played           bool     `json:"Played,omitempty"`
}

// PlayItem 表示 Emby `/Users/{id}/Items/Latest` 与 `/Users/{id}/Items/Resume` 返回的条目。
type PlayItem struct {
	ID                    string           `json:"Id"`
	Name                  string           `json:"Name"`
	Type                  string           `json:"Type"`
	Path                  string           `json:"Path"`
	ServerID              string           `json:"ServerId"`
	SeriesID              string           `json:"SeriesId"`
	SeriesName            string           `json:"SeriesName"`
	SeriesPrimaryImageTag string           `json:"SeriesPrimaryImageTag"`
	ProductionYear        int              `json:"ProductionYear"`
	ParentIndexNumber     *int             `json:"ParentIndexNumber"`
	IndexNumber           *int             `json:"IndexNumber"`
	BackdropImageTags     []string         `json:"BackdropImageTags"`
	UserData              PlayItemUserData `json:"UserData"`
}

// ListLatestItems 查询指定用户维度下的 Emby 最近入库视频条目（对齐 V2 `get_latest`）。
func (c *Client) ListLatestItems(ctx context.Context, userID string, limit int) ([]PlayItem, error) {
	userID, validUser := normalizeEmbyPathID(userID)
	if !validUser {
		return nil, newError(ErrorCodeInvalidArgument, 0, nil)
	}
	if limit <= 0 || limit > maxHomepageLimit {
		limit = maxHomepageLimit
	}
	ctx, cancel, err := c.operationContext(ctx)
	if err != nil {
		return nil, err
	}
	defer cancel()

	query := url.Values{
		"Limit":      {strconv.Itoa(limit)},
		"MediaTypes": {"Video"},
		"Fields":     {"ProductionYear,Path,BackdropImageTags"},
	}
	endpoint := c.endpoint("/Users/" + url.PathEscape(userID) + "/Items/Latest")
	var items []PlayItem
	if err := c.getJSON(ctx, endpoint+"?"+query.Encode(), maxHomepageRespBytes, &items); err != nil {
		return nil, err
	}
	if items == nil {
		return []PlayItem{}, nil
	}
	return items, nil
}

// ListResumeItems 查询指定用户维度下的 Emby 继续观看条目（对齐 V2 `get_resume`）。
func (c *Client) ListResumeItems(ctx context.Context, userID string, limit int) ([]PlayItem, error) {
	userID, validUser := normalizeEmbyPathID(userID)
	if !validUser {
		return nil, newError(ErrorCodeInvalidArgument, 0, nil)
	}
	if limit <= 0 || limit > maxHomepageLimit {
		limit = maxHomepageLimit
	}
	ctx, cancel, err := c.operationContext(ctx)
	if err != nil {
		return nil, err
	}
	defer cancel()

	query := url.Values{
		"Limit":      {strconv.Itoa(limit)},
		"MediaTypes": {"Video"},
		"Fields":     {"ProductionYear,Path"},
	}
	endpoint := c.endpoint("/Users/" + url.PathEscape(userID) + "/Items/Resume")
	var envelope struct {
		Items []PlayItem `json:"Items"`
	}
	if err := c.getJSON(ctx, endpoint+"?"+query.Encode(), maxHomepageRespBytes, &envelope); err != nil {
		return nil, err
	}
	if envelope.Items == nil {
		return []PlayItem{}, nil
	}
	return envelope.Items, nil
}

// PrimaryImageURL 按 V2 `__get_local_image_by_id` 约定拼装条目主图地址。
func (c *Client) PrimaryImageURL(itemID string) string {
	itemID = strings.TrimSpace(itemID)
	if c == nil || c.baseURL == nil || itemID == "" {
		return ""
	}
	return strings.TrimRight(c.baseURL.String(), "/") + "/Items/" + url.PathEscape(itemID) + "/Images/Primary"
}

// BackdropImageURL 按 V2 `get_backdrop_url` 约定拼装条目背景图地址。
func (c *Client) BackdropImageURL(itemID, imageTag string) string {
	itemID = strings.TrimSpace(itemID)
	imageTag = strings.TrimSpace(imageTag)
	if c == nil || c.baseURL == nil || itemID == "" || imageTag == "" {
		return ""
	}
	params := url.Values{}
	params.Set("tag", imageTag)
	if c.token != "" {
		params.Set("api_key", c.token)
	}
	return strings.TrimRight(c.baseURL.String(), "/") + "/Items/" + url.PathEscape(itemID) + "/Images/Backdrop?" + params.Encode()
}

// PlayURLWithServer 按 V2 `get_play_url` 拼装带 serverId 的条目播放链接。
func (c *Client) PlayURLWithServer(itemID, serverID string) (string, error) {
	itemID = strings.TrimSpace(itemID)
	serverID = strings.TrimSpace(serverID)
	if c == nil || c.playHost == nil || itemID == "" || len(itemID) > 4096 || strings.ContainsAny(itemID, "\r\n") {
		return "", newError(ErrorCodeInvalidArgument, 0, nil)
	}
	base := strings.TrimRight(c.playHost.String(), "/")
	if c.serverType == "jellyfin" {
		query := "id=" + url.QueryEscape(itemID)
		if serverID != "" {
			query += "&serverId=" + url.QueryEscape(serverID)
		}
		return base + "/web/index.html#!/details?" + query, nil
	}
	query := "id=" + url.QueryEscape(itemID) + "&context=home"
	if serverID != "" {
		query += "&serverId=" + url.QueryEscape(serverID)
	}
	return base + "/web/index.html#!/item?" + query, nil
}

// LibraryURL 按 V2 `get_librarys` 拼装媒体库跳转链接。
func (c *Client) LibraryURL(libraryID, serverID string) string {
	libraryID = strings.TrimSpace(libraryID)
	serverID = strings.TrimSpace(serverID)
	if c == nil || c.playHost == nil || libraryID == "" {
		return ""
	}
	base := strings.TrimRight(c.playHost.String(), "/")
	if c.serverType == "jellyfin" {
		return base + "/web/index.html#!/movies.html?topParentId=" + url.QueryEscape(libraryID)
	}
	query := "parentId=" + url.QueryEscape(libraryID)
	if serverID != "" {
		query = "serverId=" + url.QueryEscape(serverID) + "&" + query
	}
	return base + "/web/index.html#!/videos?" + query
}
