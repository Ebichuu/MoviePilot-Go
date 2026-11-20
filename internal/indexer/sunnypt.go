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

const sunnyPTPageSize = 100

// SunnyPTAdapter 实现 SunnyPT MoviePilot API 搜索和短时下载地址兑换。
type SunnyPTAdapter struct {
	client  *HTTPClient
	id      int
	name    string
	site    *url.URL
	api     *url.URL
	headers http.Header
}

func NewSunnyPTAdapter(client *HTTPClient, id int, name, baseURL string, config map[string]any) (*SunnyPTAdapter, error) {
	if client == nil {
		return nil, errors.New("站点 HTTP 客户端不能为空")
	}
	name = strings.TrimSpace(name)
	if name == "" {
		name = "SunnyPT"
	}
	site, err := validateBaseURL(baseURL, client.allowPrivate)
	if err != nil {
		return nil, err
	}
	apiURL := configString(config, "api_url")
	if apiURL == "" {
		apiURL = "https://api.sunnypt.top/api/v1/mp"
	}
	api, err := validateBaseURL(apiURL, client.allowPrivate)
	if err != nil {
		return nil, fmt.Errorf("SunnyPT API 地址无效: %w", err)
	}
	apiKey := configString(config, "apikey", "api_key")
	if apiKey == "" {
		return nil, errors.New("SunnyPT 未配置 API Key")
	}
	if len(apiKey) > 4096 || strings.ContainsAny(apiKey, "\r\n") {
		return nil, errors.New("SunnyPT API Key 无效")
	}
	headers := make(http.Header)
	headers.Set("X-API-Key", apiKey)
	headers.Set("Accept", "application/json")
	if userAgent := configString(config, "ua", "user_agent"); userAgent != "" {
		if len(userAgent) > 1024 || strings.ContainsAny(userAgent, "\r\n") {
			return nil, errors.New("SunnyPT User-Agent 无效")
		}
		headers.Set("User-Agent", userAgent)
	}
	return &SunnyPTAdapter{client: client, id: id, name: name, site: site, api: api, headers: headers}, nil
}

func (a *SunnyPTAdapter) Name() string           { return a.name }
func (a *SunnyPTAdapter) SiteID() int            { return a.id }
func (a *SunnyPTAdapter) ResolverKind() string   { return "sunnypt" }
func (a *SunnyPTAdapter) ResolverVersion() int16 { return downloadResolverVersionV1 }

func (a *SunnyPTAdapter) Search(ctx context.Context, query Query) ([]Resource, error) {
	if query.Page < 0 {
		query.Page = 0
	}
	limit := min(max(query.Limit, 1), sunnyPTPageSize)
	endpoint := sunnyPTEndpoint(a.api, "torrents")
	parsed, _ := url.Parse(endpoint)
	values := parsed.Query()
	values.Set("page", strconv.Itoa(query.Page+1))
	values.Set("page_size", strconv.Itoa(limit))
	values.Set("sort", "created_at")
	values.Set("order", "desc")
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
		return nil, errors.New("SunnyPT API 返回失败")
	}
	items, ok := lookupJSON(document, "data.items").([]any)
	if !ok {
		return nil, errors.New("SunnyPT 搜索结果不是数组")
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
		detail := ""
		if rawDetail := strings.TrimSpace(valueString(item["details_url"])); rawDetail != "" {
			detail, _ = resolveSiteURL(a.site, rawDetail)
		}
		resources = append(resources, Resource{
			SiteID: a.id, Title: title, Description: strings.TrimSpace(valueString(item["subtitle"])),
			DetailURL: detail, TorrentID: resourceID, DownloadID: resourceID,
			DownloadURL: fmt.Sprintf("/api/v1/resource/download/%d/%s", a.id, resourceID),
			SizeBytes:   valueInt64(item["size"]), Seeders: valueInt(item["seeders"]), Peers: valueInt(item["leechers"]),
			Grabs: valueInt(item["completed"]), PublishedAt: parseTime(valueString(item["created_at"])), FreeDate: freeDate,
			IMDBID: strings.TrimSpace(valueString(item["imdb_id"])), Labels: stringList(item["tags"], 64),
			Category: sunnyPTCategory(valueString(item["media_type"])), HitAndRun: valueBool(item["hit_and_run"]),
			DownloadVolumeFactor: &downFactor, UploadVolumeFactor: &upFactor,
		})
		if len(resources) == limit {
			break
		}
	}
	return resources, nil
}

func (a *SunnyPTAdapter) ResolveDownload(ctx context.Context, resourceID string) (ResolvedDownload, error) {
	if !validNumericID(resourceID) {
		return ResolvedDownload{}, errors.New("SunnyPT 种子 ID 无效")
	}
	endpoint := sunnyPTEndpoint(a.api, "torrents/"+url.PathEscape(resourceID)+"/download-token")
	body, err := a.client.postJSON(ctx, endpoint, a.headers.Clone(), map[string]any{})
	if err != nil {
		return ResolvedDownload{}, err
	}
	document, err := decodeJSONObject(body)
	if err != nil {
		return ResolvedDownload{}, err
	}
	if valueString(document["code"]) != "0" {
		return ResolvedDownload{}, errors.New("SunnyPT 下载 token 生成失败")
	}
	downloadURL := strings.TrimSpace(valueString(lookupJSON(document, "data.download_url")))
	if downloadURL == "" || len(downloadURL) > 8192 {
		return ResolvedDownload{}, errors.New("SunnyPT 下载地址无效")
	}
	resolved, err := a.resolveDownloadURL(downloadURL)
	if err != nil {
		return ResolvedDownload{}, errors.New("SunnyPT 下载地址不受信任")
	}
	downloadHeaders := a.headers.Clone()
	downloadHeaders.Del("X-API-Key")
	torrent, err := a.client.get(ctx, resolved, downloadHeaders)
	if err != nil {
		return ResolvedDownload{}, err
	}
	return ResolvedDownload{Body: torrent, ContentType: "application/x-bittorrent", Filename: resourceID + ".torrent"}, nil
}

func (a *SunnyPTAdapter) resolveDownloadURL(raw string) (string, error) {
	parsed, err := url.Parse(strings.TrimSpace(raw))
	if err != nil || parsed.User != nil || (parsed.IsAbs() && parsed.Scheme != "http" && parsed.Scheme != "https") {
		return "", errors.New("invalid SunnyPT download URL")
	}
	resolved := a.api.ResolveReference(parsed)
	apiOrigin := strings.EqualFold(resolved.Scheme, a.api.Scheme) && strings.EqualFold(resolved.Host, a.api.Host)
	siteOrigin := strings.EqualFold(resolved.Scheme, a.site.Scheme) && strings.EqualFold(resolved.Host, a.site.Host)
	if !apiOrigin && !siteOrigin {
		return "", errors.New("untrusted SunnyPT download origin")
	}
	return resolved.String(), nil
}

func sunnyPTCategory(value string) string {
	switch strings.ToLower(value) {
	case "movie":
		return "电影"
	case "tv":
		return "电视剧"
	default:
		return "未知"
	}
}

func sunnyPTEndpoint(base *url.URL, suffix string) string {
	endpoint := *base
	endpoint.Path = strings.TrimRight(endpoint.Path, "/") + "/" + strings.TrimLeft(suffix, "/")
	endpoint.RawPath = ""
	endpoint.RawQuery = ""
	endpoint.Fragment = ""
	return endpoint.String()
}
