package indexer

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"
)

const hddolbyPageSize = 100

// HDDolbyAdapter 实现独立 API 域搜索和带 downhash 的 Cookie 下载。
type HDDolbyAdapter struct {
	client  *HTTPClient
	id      int
	name    string
	site    *url.URL
	api     *url.URL
	headers http.Header
}

func NewHDDolbyAdapter(client *HTTPClient, id int, name, baseURL string, config map[string]any) (*HDDolbyAdapter, error) {
	if client == nil {
		return nil, errors.New("站点 HTTP 客户端不能为空")
	}
	name = strings.TrimSpace(name)
	if name == "" {
		name = "HDDolby"
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
		return nil, fmt.Errorf("HDDolby API 地址无效: %w", err)
	}
	apiKey := configString(config, "apikey", "api_key")
	cookie := configString(config, "cookie")
	if apiKey == "" || cookie == "" {
		return nil, errors.New("HDDolby 必须配置 API Key 和 Cookie")
	}
	if len(apiKey) > 4096 || len(cookie) > 64<<10 || strings.ContainsAny(apiKey+cookie, "\r\n") {
		return nil, errors.New("HDDolby 鉴权配置无效")
	}
	headers := make(http.Header)
	headers.Set("x-api-key", apiKey)
	headers.Set("Cookie", cookie)
	headers.Set("Accept", "application/json")
	headers.Set("Referer", site.String())
	return &HDDolbyAdapter{client: client, id: id, name: name, site: site, api: api, headers: headers}, nil
}

func (a *HDDolbyAdapter) Name() string           { return a.name }
func (a *HDDolbyAdapter) SiteID() int            { return a.id }
func (a *HDDolbyAdapter) ResolverKind() string   { return "hddolby" }
func (a *HDDolbyAdapter) ResolverVersion() int16 { return downloadResolverVersionV1 }

func (a *HDDolbyAdapter) Search(ctx context.Context, query Query) ([]Resource, error) {
	if query.Page < 0 {
		query.Page = 0
	}
	limit := min(max(query.Limit, 1), hddolbyPageSize)
	endpoint, err := resolveSiteURL(a.api, "/api/v1/torrent/search")
	if err != nil {
		return nil, err
	}
	body, err := a.client.postJSON(ctx, endpoint, a.headers.Clone(), map[string]any{
		"keyword": strings.TrimSpace(query.Keyword), "page_number": query.Page, "page_size": limit,
		"categories": []any{401, 402, 403, 404, 405}, "visible": 1,
	})
	if err != nil {
		return nil, err
	}
	document, err := decodeJSONObject(body)
	if err != nil {
		return nil, err
	}
	if document["error"] != nil {
		return nil, errors.New("HDDolby API 返回失败")
	}
	items, ok := document["data"].([]any)
	if !ok {
		return nil, errors.New("HDDolby 搜索结果不是数组")
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
		downhash := strings.TrimSpace(valueString(item["downhash"]))
		title := strings.TrimSpace(valueString(item["name"]))
		if title == "" || !validNumericID(resourceID) || downhash == "" || len(downhash) > 512 || strings.ContainsAny(downhash, "\r\n") {
			continue
		}
		downloadID := base64.RawURLEncoding.EncodeToString([]byte(resourceID + "\n" + downhash))
		detail, _ := resolveSiteURL(a.site, "/details.php?id="+url.QueryEscape(resourceID)+"&hit=1")
		downFactor, upFactor := hddolbyFactors(valueInt(item["promotion_time_type"]))
		resources = append(resources, Resource{
			SiteID: a.id, Title: title, Description: strings.TrimSpace(valueString(item["small_descr"])),
			DetailURL: detail, TorrentID: resourceID, DownloadID: downloadID,
			DownloadURL: fmt.Sprintf("/api/v1/resource/download/%d/%s", a.id, downloadID),
			SizeBytes:   valueInt64(item["size"]), Seeders: valueInt(item["seeders"]), Peers: valueInt(item["leechers"]),
			Grabs: valueInt(item["times_completed"]), PublishedAt: parseTime(valueString(item["added"])),
			FreeDate: parseTime(valueString(item["promotion_until"])), Labels: hddolbyLabels(valueString(item["tags"])),
			Category: hddolbyCategory(valueInt(item["category"])), DownloadVolumeFactor: &downFactor, UploadVolumeFactor: &upFactor,
		})
		if len(resources) == limit {
			break
		}
	}
	return resources, nil
}

func (a *HDDolbyAdapter) ResolveDownload(ctx context.Context, resourceID string) (ResolvedDownload, error) {
	if resourceID == "" || len(resourceID) > 1024 {
		return ResolvedDownload{}, errors.New("HDDolby 下载标识无效")
	}
	decoded, err := base64.RawURLEncoding.DecodeString(resourceID)
	if err != nil {
		return ResolvedDownload{}, errors.New("HDDolby 下载标识无效")
	}
	parts := strings.SplitN(string(decoded), "\n", 2)
	if len(parts) != 2 || !validNumericID(parts[0]) || parts[1] == "" || len(parts[1]) > 512 || strings.ContainsAny(parts[1], "\r\n") {
		return ResolvedDownload{}, errors.New("HDDolby 下载标识无效")
	}
	endpoint, err := resolveSiteURL(a.site, "/download.php?id="+url.QueryEscape(parts[0])+"&downhash="+url.QueryEscape(parts[1]))
	if err != nil {
		return ResolvedDownload{}, err
	}
	body, err := a.client.get(ctx, endpoint, a.headers.Clone())
	if err != nil {
		return ResolvedDownload{}, err
	}
	return ResolvedDownload{Body: body, ContentType: "application/x-bittorrent", Filename: parts[0] + ".torrent"}, nil
}

func hddolbyFactors(value int) (float64, float64) {
	switch value {
	case 2:
		return 0, 1
	case 3:
		return 1, 2
	case 4:
		return 1, 2
	case 5:
		return 0.5, 1
	case 6:
		return 1, 2
	case 7:
		return 0.3, 1
	default:
		return 1, 1
	}
}

func hddolbyCategory(value int) string {
	if value == 401 || value == 404 {
		return "电影"
	}
	for _, category := range []int{402, 403} {
		if value == category {
			return "电视剧"
		}
	}
	if value == 405 {
		return "电影"
	}
	return "未知"
}

func hddolbyLabels(value string) []string {
	labels := map[string]string{
		"gf": "官方", "gy": "国语", "yy": "粤语", "ja": "日语", "ko": "韩语", "zz": "中文字幕",
		"jz": "禁转", "xz": "限转", "diy": "DIY", "sf": "首发", "yq": "应求", "m0": "零魔",
		"yc": "原创", "gz": "官字", "db": "Dolby Vision", "hdr10": "HDR10", "hdrm": "HDR10+",
		"tx": "特效", "lz": "连载", "wj": "完结", "hdrv": "HDR Vivid", "hlg": "HLG", "hq": "高码率", "hfr": "高帧率",
	}
	parts := strings.FieldsFunc(value, func(r rune) bool { return r == ';' || r == ',' })
	result := make([]string, 0, min(len(parts), 64))
	for _, part := range parts {
		if label := labels[strings.ToLower(strings.TrimSpace(part))]; label != "" && len(result) < 64 {
			result = append(result, label)
		}
	}
	return result
}
