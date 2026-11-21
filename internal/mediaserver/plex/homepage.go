package plex

import (
	"cmp"
	"context"
	"net/url"
	"slices"
	"strconv"
	"strings"
)

// PlayItem 表示 Plex 首页「最近入库」与「继续观看」单条媒体数据。
type PlayItem struct {
	Key              string
	RatingKey        string
	Type             string // "movie" | "show" | "season" | "episode"
	Title            string
	ParentTitle      string
	GrandparentTitle string
	Year             int
	Index            *int
	ParentIndex      *int
	ChildCount       int
	Thumb            string
	ParentThumb      string
	GrandparentThumb string
	Art              string
	ViewOffset       int64
	Duration         int64
	AddedAt          int64
}

type plexHubMetadataEntry struct {
	Key              string `json:"key"`
	RatingKey        string `json:"ratingKey"`
	Type             string `json:"type"`
	Title            string `json:"title"`
	ParentTitle      string `json:"parentTitle"`
	GrandparentTitle string `json:"grandparentTitle"`
	Year             int    `json:"year"`
	Index            *int   `json:"index"`
	ParentIndex      *int   `json:"parentIndex"`
	ChildCount       int    `json:"childCount"`
	Thumb            string `json:"thumb"`
	ParentThumb      string `json:"parentThumb"`
	GrandparentThumb string `json:"grandparentThumb"`
	Art              string `json:"art"`
	ViewOffset       int64  `json:"viewOffset"`
	Duration         int64  `json:"duration"`
	AddedAt          int64  `json:"addedAt"`
}

func (e plexHubMetadataEntry) toPlayItem() PlayItem {
	key := strings.TrimSpace(e.Key)
	ratingKey := strings.TrimSpace(e.RatingKey)
	if key == "" && ratingKey != "" {
		key = "/library/metadata/" + ratingKey
	}
	return PlayItem{
		Key:              key,
		RatingKey:        ratingKey,
		Type:             strings.ToLower(strings.TrimSpace(e.Type)),
		Title:            strings.TrimSpace(e.Title),
		ParentTitle:      strings.TrimSpace(e.ParentTitle),
		GrandparentTitle: strings.TrimSpace(e.GrandparentTitle),
		Year:             e.Year,
		Index:            e.Index,
		ParentIndex:      e.ParentIndex,
		ChildCount:       e.ChildCount,
		Thumb:            strings.TrimSpace(e.Thumb),
		ParentThumb:      strings.TrimSpace(e.ParentThumb),
		GrandparentThumb: strings.TrimSpace(e.GrandparentThumb),
		Art:              strings.TrimSpace(e.Art),
		ViewOffset:       e.ViewOffset,
		Duration:         e.Duration,
		AddedAt:          e.AddedAt,
	}
}

// ListContinueWatching 对应 V2 Plex `get_resume`：查询 `/hubs/continueWatching/items`。
func (c *Client) ListContinueWatching(ctx context.Context, sectionIDs []string, limit int) ([]PlayItem, error) {
	if limit <= 0 {
		limit = 12
	}
	if limit > 100 {
		limit = 100
	}
	params := url.Values{}
	params.Set("X-Plex-Container-Start", "0")
	params.Set("X-Plex-Container-Size", strconv.Itoa(limit))
	if cleaned := joinCleanSectionIDs(sectionIDs); cleaned != "" {
		params.Set("contentDirectoryID", cleaned)
	}

	var payload struct {
		MediaContainer struct {
			Metadata []plexHubMetadataEntry `json:"Metadata"`
		} `json:"MediaContainer"`
	}
	endpoint := c.endpoint("/hubs/continueWatching/items?" + params.Encode())
	if err := c.getJSON(ctx, endpoint, &payload); err != nil {
		return nil, err
	}
	items := make([]PlayItem, 0, len(payload.MediaContainer.Metadata))
	for _, entry := range payload.MediaContainer.Metadata {
		items = append(items, entry.toPlayItem())
		if len(items) >= limit {
			break
		}
	}
	return items, nil
}

// ListRecentlyAdded 对应 V2 Plex `get_latest`：查询 `/hubs/promoted` 并按 `addedAt` 降序合并。
func (c *Client) ListRecentlyAdded(ctx context.Context, sectionIDs []string, limit int) ([]PlayItem, error) {
	if limit <= 0 {
		limit = 20
	}
	if limit > 100 {
		limit = 100
	}
	params := url.Values{}
	params.Set("count", strconv.Itoa(limit))
	params.Set("excludeContinueWatching", "1")
	params.Set("X-Plex-Container-Start", "0")
	params.Set("X-Plex-Container-Size", strconv.Itoa(limit))
	if cleaned := joinCleanSectionIDs(sectionIDs); cleaned != "" {
		params.Set("contentDirectoryID", cleaned)
	}

	var payload struct {
		MediaContainer struct {
			Hub []struct {
				Metadata []plexHubMetadataEntry `json:"Metadata"`
			} `json:"Hub"`
		} `json:"MediaContainer"`
	}
	endpoint := c.endpoint("/hubs/promoted?" + params.Encode())
	if err := c.getJSON(ctx, endpoint, &payload); err != nil {
		return nil, err
	}
	merged := make([]PlayItem, 0, limit)
	for _, hub := range payload.MediaContainer.Hub {
		for _, entry := range hub.Metadata {
			merged = append(merged, entry.toPlayItem())
		}
	}
	slices.SortStableFunc(merged, func(a, b PlayItem) int {
		return cmp.Compare(b.AddedAt, a.AddedAt)
	})
	if len(merged) > limit {
		merged = merged[:limit]
	}
	return merged, nil
}

// ImageURL 将 Plex 相对图片路径（thumb/art/composite）拼装为带 X-Plex-Token 的完整地址。
func (c *Client) ImageURL(imgPath string) string {
	imgPath = strings.TrimSpace(imgPath)
	if c == nil || c.baseURL == nil || imgPath == "" {
		return ""
	}
	if strings.HasPrefix(imgPath, "http://") || strings.HasPrefix(imgPath, "https://") {
		return imgPath
	}
	if !strings.HasPrefix(imgPath, "/") {
		imgPath = "/" + imgPath
	}
	sep := "?"
	if strings.Contains(imgPath, "?") {
		sep = "&"
	}
	return strings.TrimRight(c.baseURL.String(), "/") + imgPath + sep + "X-Plex-Token=" + url.QueryEscape(c.token)
}

// LibraryURL 生成 Plex Web 客户端的媒体库跳转链接。
func (c *Client) LibraryURL(sectionKey string) string {
	sectionKey = strings.TrimSpace(sectionKey)
	if c == nil || c.playHost == nil || sectionKey == "" {
		return ""
	}
	c.machineMu.RLock()
	machineID := c.machineID
	c.machineMu.RUnlock()

	base := strings.TrimRight(c.playHost.String(), "/")
	if machineID != "" {
		return base + "/web/index.html#!/media/" + url.PathEscape(machineID) +
			"/com.plexapp.plugins.library?source=" + url.QueryEscape(sectionKey)
	}
	return base + "/web/index.html#!/media/com.plexapp.plugins.library?source=" + url.QueryEscape(sectionKey)
}

func joinCleanSectionIDs(ids []string) string {
	cleaned := make([]string, 0, len(ids))
	for _, id := range ids {
		if trimmed := strings.TrimSpace(id); trimmed != "" {
			cleaned = append(cleaned, trimmed)
		}
	}
	return strings.Join(cleaned, ",")
}
