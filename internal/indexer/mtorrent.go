package indexer

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"regexp"
	"strings"
)

const mTorrentPageSize = 100

var imdbIDPattern = regexp.MustCompile(`tt\d+`)

// MTorrentAdapter 实现 mTorrent API 搜索、促销解析和临时下载地址兑换。
type MTorrentAdapter struct {
	client  *HTTPClient
	id      int
	name    string
	site    *url.URL
	api     *url.URL
	headers http.Header
}

func NewMTorrentAdapter(client *HTTPClient, id int, name, baseURL string, config map[string]any) (*MTorrentAdapter, error) {
	if client == nil {
		return nil, errors.New("站点 HTTP 客户端不能为空")
	}
	name = strings.TrimSpace(name)
	if name == "" {
		name = "mTorrent"
	}
	site, err := validateBaseURL(baseURL, client.allowPrivate)
	if err != nil {
		return nil, err
	}
	apiURL := configString(config, "api_url")
	if apiURL == "" {
		host := strings.TrimPrefix(site.Hostname(), "www.")
		apiURL = site.Scheme + "://api." + host
	}
	api, err := validateBaseURL(apiURL, client.allowPrivate)
	if err != nil {
		return nil, fmt.Errorf("mTorrent API 地址无效: %w", err)
	}
	apiKey := configString(config, "apikey", "api_key")
	if apiKey == "" {
		return nil, errors.New("mTorrent 未配置 API Key")
	}
	if len(apiKey) > 4096 || strings.ContainsAny(apiKey, "\r\n") {
		return nil, errors.New("mTorrent API Key 无效")
	}
	headers := make(http.Header)
	headers.Set("x-api-key", apiKey)
	headers.Set("Accept", "application/json")
	if userAgent := configString(config, "ua", "user_agent"); userAgent != "" {
		if len(userAgent) > 1024 || strings.ContainsAny(userAgent, "\r\n") {
			return nil, errors.New("mTorrent User-Agent 无效")
		}
		headers.Set("User-Agent", userAgent)
	}
	return &MTorrentAdapter{client: client, id: id, name: name, site: site, api: api, headers: headers}, nil
}

func (a *MTorrentAdapter) Name() string           { return a.name }
func (a *MTorrentAdapter) SiteID() int            { return a.id }
func (a *MTorrentAdapter) ResolverKind() string   { return "mtorrent" }
func (a *MTorrentAdapter) ResolverVersion() int16 { return downloadResolverVersionV1 }
func (a *MTorrentAdapter) RSSNoCache() bool       { return rssDomainUsesNoCacheKey(a.site) }

func (a *MTorrentAdapter) Search(ctx context.Context, query Query) ([]Resource, error) {
	if query.Page < 0 {
		query.Page = 0
	}
	limit := min(max(query.Limit, 1), mTorrentPageSize)
	keyword := strings.TrimSpace(query.Keyword)
	if strings.HasPrefix(strings.ToLower(keyword), "tt") {
		keyword = "https://www.imdb.com/title/" + keyword
	}
	endpoint, err := resolveSiteURL(a.api, "/api/torrent/search")
	if err != nil {
		return nil, err
	}
	body, err := a.client.postJSON(ctx, endpoint, a.headers.Clone(), map[string]any{
		"keyword": keyword, "categories": []any{}, "pageNumber": query.Page + 1, "pageSize": limit, "visible": 1,
	})
	if err != nil {
		return nil, err
	}
	document, err := decodeJSONObject(body)
	if err != nil {
		return nil, err
	}
	items, ok := lookupJSON(document, "data.data").([]any)
	if !ok {
		return nil, errors.New("mTorrent 搜索结果不是数组")
	}
	resources := make([]Resource, 0, min(limit, len(items)))
	for _, raw := range items {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		item, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		resourceID := strings.TrimSpace(valueString(item["id"]))
		title := strings.TrimSpace(valueString(item["name"]))
		if title == "" || !validNumericID(resourceID) {
			continue
		}
		status, _ := item["status"].(map[string]any)
		downFactor, upFactor := mTorrentFactors(valueString(status["discount"]))
		freeDate := parseTime(valueString(status["discountEndTime"]))
		if promotion, ok := status["promotionRule"].(map[string]any); ok {
			downFactor, _ = mTorrentFactors(valueString(promotion["discount"]))
			if parsed := parseTime(valueString(promotion["endTime"])); !parsed.IsZero() {
				freeDate = parsed
			}
		}
		if mall, ok := status["mallSingleFree"].(map[string]any); ok && strings.EqualFold(valueString(mall["status"]), "ONGOING") {
			downFactor = 0
			if parsed := parseTime(valueString(mall["endDate"])); !parsed.IsZero() {
				freeDate = parsed
			}
		}
		detail, _ := resolveSiteURL(a.site, "/detail/"+url.PathEscape(resourceID))
		resources = append(resources, Resource{
			SiteID: a.id, Title: title, Description: strings.TrimSpace(valueString(item["smallDescr"])),
			DetailURL: detail, TorrentID: resourceID, DownloadID: resourceID,
			DownloadURL: fmt.Sprintf("/api/v1/resource/download/%d/%s", a.id, resourceID),
			SizeBytes:   valueInt64(item["size"]), Seeders: valueInt(status["seeders"]), Peers: valueInt(status["leechers"]),
			Grabs: valueInt(status["timesCompleted"]), PublishedAt: parseTime(valueString(item["createdDate"])),
			FreeDate: freeDate, IMDBID: imdbIDPattern.FindString(valueString(item["imdb"])), Labels: stringList(item["labelsNew"], 64),
			Category: mTorrentCategory(valueString(item["category"])), DownloadVolumeFactor: &downFactor, UploadVolumeFactor: &upFactor,
		})
		if len(resources) == limit {
			break
		}
	}
	return resources, nil
}

func (a *MTorrentAdapter) ResolveDownload(ctx context.Context, resourceID string) (ResolvedDownload, error) {
	if !validNumericID(resourceID) {
		return ResolvedDownload{}, errors.New("mTorrent 种子 ID 无效")
	}
	endpoint, err := resolveSiteURL(a.api, "/api/torrent/genDlToken")
	if err != nil {
		return ResolvedDownload{}, err
	}
	body, err := a.client.postJSON(ctx, endpoint, a.headers.Clone(), map[string]any{"id": resourceID})
	if err != nil {
		return ResolvedDownload{}, err
	}
	document, err := decodeJSONObject(body)
	if err != nil {
		return ResolvedDownload{}, err
	}
	downloadURL := strings.TrimSpace(valueString(document["data"]))
	if downloadURL == "" || len(downloadURL) > 8192 {
		return ResolvedDownload{}, errors.New("mTorrent 下载地址无效")
	}
	resolved, err := resolveSiteURL(a.api, downloadURL)
	if err != nil {
		return ResolvedDownload{}, errors.New("mTorrent 下载地址不受信任")
	}
	torrent, err := a.client.get(ctx, resolved, a.headers.Clone())
	if err != nil {
		return ResolvedDownload{}, err
	}
	return ResolvedDownload{Body: torrent, ContentType: "application/x-bittorrent", Filename: resourceID + ".torrent"}, nil
}

func mTorrentFactors(discount string) (float64, float64) {
	down, up := 1.0, 1.0
	switch strings.ToUpper(discount) {
	case "FREE", "_2X_FREE":
		down = 0
	case "PERCENT_50", "_2X_PERCENT_50":
		down = 0.5
	case "PERCENT_70":
		down = 0.3
	}
	switch strings.ToUpper(discount) {
	case "_2X", "_2X_FREE", "_2X_PERCENT_50":
		up = 2
	}
	return down, up
}

func mTorrentCategory(value string) string {
	for _, category := range []string{"401", "419", "420", "421", "439", "405", "404"} {
		if value == category {
			return "电影"
		}
	}
	for _, category := range []string{"403", "402", "435", "438"} {
		if value == category {
			return "电视剧"
		}
	}
	return "未知"
}

func stringList(value any, limit int) []string {
	items, ok := value.([]any)
	if !ok {
		return nil
	}
	result := make([]string, 0, min(limit, len(items)))
	for _, item := range items {
		text := strings.TrimSpace(valueString(item))
		if text != "" && len(result) < limit {
			result = append(result, text)
		}
	}
	return result
}
