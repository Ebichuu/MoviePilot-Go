package indexer

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"
)

const rousiPageSize = 100

// RousiAdapter 实现 RousiPro API v1 搜索和详情下载地址解析。
type RousiAdapter struct {
	client         *HTTPClient
	id             int
	name           string
	base           *url.URL
	headers        http.Header
	messageHeaders http.Header
}

func NewRousiAdapter(client *HTTPClient, id int, name, baseURL string, config map[string]any) (*RousiAdapter, error) {
	if client == nil {
		return nil, errors.New("站点 HTTP 客户端不能为空")
	}
	name = strings.TrimSpace(name)
	if name == "" {
		name = "RousiPro"
	}
	base, err := validateBaseURL(baseURL, client.allowPrivate)
	if err != nil {
		return nil, err
	}
	apiKey := configString(config, "apikey", "api_key", "passkey")
	if apiKey == "" {
		return nil, errors.New("RousiPro 未配置 Passkey")
	}
	if len(apiKey) > 4096 || strings.ContainsAny(apiKey, "\r\n") {
		return nil, errors.New("RousiPro Passkey 无效")
	}
	headers := make(http.Header)
	headers.Set("Authorization", "Bearer "+apiKey)
	headers.Set("Accept", "application/json")
	var messageHeaders http.Header
	if token := configString(config, "token"); token != "" {
		if len(token) > 4096 || strings.ContainsAny(token, "\r\n") {
			return nil, errors.New("RousiPro Token 无效")
		}
		authorization := token
		if !strings.HasPrefix(authorization, "Bearer ") {
			authorization = "Bearer " + authorization
		}
		messageHeaders = make(http.Header)
		messageHeaders.Set("Authorization", authorization)
		messageHeaders.Set("Accept", "application/json, text/plain, */*")
		if userAgent := configString(config, "ua", "user_agent"); userAgent != "" {
			if len(userAgent) > 1024 || strings.ContainsAny(userAgent, "\r\n") {
				return nil, errors.New("RousiPro User-Agent 无效")
			}
			messageHeaders.Set("User-Agent", userAgent)
		}
	}
	return &RousiAdapter{
		client: client, id: id, name: name, base: base,
		headers: headers, messageHeaders: messageHeaders,
	}, nil
}

func (a *RousiAdapter) Name() string           { return a.name }
func (a *RousiAdapter) SiteID() int            { return a.id }
func (a *RousiAdapter) ResolverKind() string   { return "rousi" }
func (a *RousiAdapter) ResolverVersion() int16 { return downloadResolverVersionV1 }

func (a *RousiAdapter) Search(ctx context.Context, query Query) ([]Resource, error) {
	if query.Page < 0 {
		query.Page = 0
	}
	limit := min(max(query.Limit, 1), rousiPageSize)
	endpoint, err := resolveSiteURL(a.base, "/api/v1/torrents")
	if err != nil {
		return nil, err
	}
	parsed, _ := url.Parse(endpoint)
	values := parsed.Query()
	values.Set("page", strconv.Itoa(query.Page+1))
	values.Set("page_size", strconv.Itoa(limit))
	if keyword := strings.TrimSpace(query.Keyword); keyword != "" {
		values.Set("keyword", keyword)
	}
	parsed.RawQuery = values.Encode()
	body, err := a.client.get(ctx, parsed.String(), a.headers.Clone())
	if err != nil {
		return nil, err
	}
	document, err := decodeJSONObject(body)
	if err != nil {
		return nil, err
	}
	if valueString(document["code"]) != "0" {
		return nil, errors.New("RousiPro API 返回失败")
	}
	items, ok := lookupJSON(document, "data.torrents").([]any)
	if !ok {
		return nil, errors.New("RousiPro 搜索结果不是数组")
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
		title := strings.TrimSpace(valueString(item["title"]))
		if title == "" || !validNumericID(resourceID) {
			continue
		}
		downFactor, upFactor, freeDate := 1.0, 1.0, parseTime("")
		if promotion, ok := item["promotion"].(map[string]any); ok && valueBool(promotion["is_active"]) {
			downFactor = valueFloat(promotion["down_multiplier"], 1)
			upFactor = valueFloat(promotion["up_multiplier"], 1)
			freeDate = parseTime(valueString(promotion["until"]))
		}
		category := valueString(item["category"])
		if object, ok := item["category"].(map[string]any); ok {
			category = valueString(object["slug"])
			if category == "" {
				category = valueString(object["name"])
			}
		}
		uuid := strings.TrimSpace(valueString(item["uuid"]))
		detail := ""
		if uuid != "" && len(uuid) <= 128 {
			detail, _ = resolveSiteURL(a.base, "/torrent/"+url.PathEscape(uuid))
		}
		resources = append(resources, Resource{
			SiteID: a.id, Title: title, Description: strings.TrimSpace(valueString(item["subtitle"])),
			DetailURL: detail, TorrentID: resourceID, DownloadID: resourceID,
			DownloadURL: fmt.Sprintf("/api/v1/resource/download/%d/%s", a.id, resourceID),
			SizeBytes:   valueInt64(item["size"]), Seeders: valueInt(item["seeders"]), Peers: valueInt(item["leechers"]),
			Grabs: valueInt(item["downloads"]), PublishedAt: parseTime(valueString(item["created_at"])), FreeDate: freeDate,
			Category: rousiCategory(category), DownloadVolumeFactor: &downFactor, UploadVolumeFactor: &upFactor,
		})
		if len(resources) == limit {
			break
		}
	}
	return resources, nil
}

func (a *RousiAdapter) ResolveDownload(ctx context.Context, resourceID string) (ResolvedDownload, error) {
	if !validNumericID(resourceID) {
		return ResolvedDownload{}, errors.New("RousiPro 种子 ID 无效")
	}
	endpoint, err := resolveSiteURL(a.base, "/api/v1/torrents/"+url.PathEscape(resourceID))
	if err != nil {
		return ResolvedDownload{}, err
	}
	body, err := a.client.get(ctx, endpoint, a.headers.Clone())
	if err != nil {
		return ResolvedDownload{}, err
	}
	document, err := decodeJSONObject(body)
	if err != nil {
		return ResolvedDownload{}, err
	}
	downloadURL := strings.TrimSpace(valueString(lookupJSON(document, "data.download_url")))
	if downloadURL == "" || len(downloadURL) > 8192 {
		return ResolvedDownload{}, errors.New("RousiPro 下载地址无效")
	}
	resolved, err := resolveSiteURL(a.base, downloadURL)
	if err != nil {
		return ResolvedDownload{}, errors.New("RousiPro 下载地址不受信任")
	}
	torrent, err := a.client.get(ctx, resolved, a.headers.Clone())
	if err != nil {
		return ResolvedDownload{}, err
	}
	return ResolvedDownload{Body: torrent, ContentType: "application/x-bittorrent", Filename: resourceID + ".torrent"}, nil
}

func rousiCategory(value string) string {
	switch strings.ToLower(strings.TrimSpace(value)) {
	case "movie":
		return "电影"
	case "tv":
		return "电视剧"
	default:
		return "未知"
	}
}
