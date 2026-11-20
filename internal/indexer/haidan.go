package indexer

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
)

const haidanPageSize = 100

// HaidanAdapter 处理以 torrent ID 为键的对象型搜索响应。
type HaidanAdapter struct {
	client  *HTTPClient
	id      int
	name    string
	base    *url.URL
	headers http.Header
}

func NewHaidanAdapter(client *HTTPClient, id int, name, baseURL string, config map[string]any) (*HaidanAdapter, error) {
	if client == nil {
		return nil, errors.New("站点 HTTP 客户端不能为空")
	}
	name = strings.TrimSpace(name)
	if name == "" {
		name = "Haidan"
	}
	base, err := validateBaseURL(baseURL, client.allowPrivate)
	if err != nil {
		return nil, err
	}
	cookie := configString(config, "cookie")
	if cookie == "" {
		return nil, errors.New("Haidan 未配置 Cookie")
	}
	if len(cookie) > 64<<10 || strings.ContainsAny(cookie, "\r\n") {
		return nil, errors.New("Haidan Cookie 无效")
	}
	headers := make(http.Header)
	headers.Set("Cookie", cookie)
	headers.Set("Accept", "application/json")
	if userAgent := configString(config, "ua", "user_agent"); userAgent != "" {
		if len(userAgent) > 1024 || strings.ContainsAny(userAgent, "\r\n") {
			return nil, errors.New("Haidan User-Agent 无效")
		}
		headers.Set("User-Agent", userAgent)
	}
	return &HaidanAdapter{client: client, id: id, name: name, base: base, headers: headers}, nil
}

func (a *HaidanAdapter) Name() string           { return a.name }
func (a *HaidanAdapter) SiteID() int            { return a.id }
func (a *HaidanAdapter) ResolverKind() string   { return "haidan" }
func (a *HaidanAdapter) ResolverVersion() int16 { return downloadResolverVersionV1 }

func (a *HaidanAdapter) Search(ctx context.Context, query Query) ([]Resource, error) {
	limit := min(max(query.Limit, 1), haidanPageSize)
	endpoint, err := resolveSiteURL(a.base, "/torrents.php")
	if err != nil {
		return nil, err
	}
	parsed, _ := url.Parse(endpoint)
	values := parsed.Query()
	values.Set("isapi", "1")
	values.Set("search_area", "0")
	values.Set("search", strings.TrimSpace(query.Keyword))
	values.Set("search_mode", "0")
	values.Set("cat", "")
	if strings.HasPrefix(strings.ToLower(strings.TrimSpace(query.Keyword)), "tt") {
		values.Set("search_area", "4")
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
		return nil, errors.New("Haidan API 返回失败")
	}
	items, ok := document["data"].(map[string]any)
	if !ok {
		return nil, errors.New("Haidan 搜索结果不是对象")
	}
	ids := make([]string, 0, min(limit, len(items)))
	for torrentID := range items {
		if validNumericID(torrentID) {
			ids = append(ids, torrentID)
		}
	}
	sort.Strings(ids)
	if len(ids) > limit {
		ids = ids[:limit]
	}
	resources := make([]Resource, 0, len(ids))
	for _, torrentID := range ids {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		item, ok := items[torrentID].(map[string]any)
		if !ok {
			continue
		}
		title := strings.TrimSpace(valueString(item["name"]))
		rawDownload := strings.TrimSpace(valueString(item["url"]))
		if title == "" || rawDownload == "" || len(rawDownload) > 512 {
			continue
		}
		if _, err := resolveSiteURL(a.base, rawDownload); err != nil {
			continue
		}
		downloadID := base64.RawURLEncoding.EncodeToString([]byte(rawDownload))
		groupID := strings.TrimSpace(valueString(item["group_id"]))
		detail := ""
		if validNumericID(groupID) {
			detail, _ = resolveSiteURL(a.base, "/details.php?group_id="+url.QueryEscape(groupID)+"&torrent_id="+url.QueryEscape(torrentID))
		}
		downFactor, upFactor := haidanFactors(valueString(item["sp_state"]))
		resources = append(resources, Resource{
			SiteID: a.id, Title: title, Description: strings.TrimSpace(valueString(item["small_descr"])),
			DetailURL: detail, TorrentID: torrentID, DownloadID: downloadID,
			DownloadURL: fmt.Sprintf("/api/v1/resource/download/%d/%s", a.id, downloadID),
			SizeBytes:   valueInt64(item["size"]), Seeders: valueInt(item["seeders"]), Peers: valueInt(item["leechers"]),
			Grabs: valueInt(item["times_completed"]), PublishedAt: parseTime(valueString(item["added"])),
			Category: haidanCategory(valueString(item["category"])), DownloadVolumeFactor: &downFactor, UploadVolumeFactor: &upFactor,
		})
	}
	return resources, nil
}

func (a *HaidanAdapter) ResolveDownload(ctx context.Context, resourceID string) (ResolvedDownload, error) {
	if resourceID == "" || len(resourceID) > 1024 {
		return ResolvedDownload{}, errors.New("Haidan 下载标识无效")
	}
	decoded, err := base64.RawURLEncoding.DecodeString(resourceID)
	if err != nil || len(decoded) == 0 || len(decoded) > 512 {
		return ResolvedDownload{}, errors.New("Haidan 下载标识无效")
	}
	endpoint, err := resolveSiteURL(a.base, string(decoded))
	if err != nil {
		return ResolvedDownload{}, errors.New("Haidan 下载地址不受信任")
	}
	body, err := a.client.get(ctx, endpoint, a.headers.Clone())
	if err != nil {
		return ResolvedDownload{}, err
	}
	filename := "haidan-" + strconv.Itoa(a.id) + ".torrent"
	return ResolvedDownload{Body: body, ContentType: "application/x-bittorrent", Filename: filename}, nil
}

func haidanFactors(value string) (float64, float64) {
	switch strings.TrimSpace(value) {
	case "2":
		return 0, 1
	case "3":
		return 1, 2
	case "4":
		return 0, 2
	case "5":
		return 0.5, 1
	case "6":
		return 0.5, 2
	case "7":
		return 0.3, 1
	default:
		return 1, 1
	}
}

func haidanCategory(value string) string {
	for _, category := range []string{"401", "404", "405"} {
		if value == category {
			return "电影"
		}
	}
	for _, category := range []string{"402", "403"} {
		if value == category {
			return "电视剧"
		}
	}
	return "未知"
}
