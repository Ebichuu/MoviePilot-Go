package indexer

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"net/url"
	"strconv"
	"strings"
)

const yemaPageSize = 40

// YemaAdapter 实现 YemaPT 开放 API 搜索和二阶段种子下载。
type YemaAdapter struct {
	client  *HTTPClient
	id      int
	name    string
	base    *url.URL
	headers http.Header
}

func NewYemaAdapter(client *HTTPClient, id int, name, baseURL string, config map[string]any) (*YemaAdapter, error) {
	if client == nil {
		return nil, errors.New("站点 HTTP 客户端不能为空")
	}
	name = strings.TrimSpace(name)
	if name == "" {
		name = "YemaPT"
	}
	base, err := validateBaseURL(baseURL, client.allowPrivate)
	if err != nil {
		return nil, err
	}
	apiKey := configString(config, "apikey", "api_key")
	if apiKey == "" {
		return nil, errors.New("YemaPT 未配置 API AuthKey")
	}
	if len(apiKey) > 4096 {
		return nil, errors.New("YemaPT API AuthKey 超过长度限制")
	}
	headers := make(http.Header)
	headers.Set("Authorization", apiKey)
	headers.Set("Accept", "application/json")
	if userAgent := configString(config, "ua", "user_agent"); userAgent != "" {
		if len(userAgent) > 1024 || strings.ContainsAny(userAgent, "\r\n") {
			return nil, errors.New("YemaPT User-Agent 无效")
		}
		headers.Set("User-Agent", userAgent)
	}
	return &YemaAdapter{client: client, id: id, name: name, base: base, headers: headers}, nil
}

func (a *YemaAdapter) Name() string           { return a.name }
func (a *YemaAdapter) SiteID() int            { return a.id }
func (a *YemaAdapter) ResolverKind() string   { return "yema" }
func (a *YemaAdapter) ResolverVersion() int16 { return downloadResolverVersionV1 }

func (a *YemaAdapter) Search(ctx context.Context, query Query) ([]Resource, error) {
	if query.Page < 0 {
		query.Page = 0
	}
	limit := min(max(query.Limit, 1), yemaPageSize)
	payload := map[string]any{
		"pageParam": map[string]any{"current": query.Page + 1, "pageSize": limit},
		"sorter":    map[string]any{},
	}
	if keyword := strings.TrimSpace(query.Keyword); keyword != "" {
		payload["keyword"] = keyword
	}
	endpoint, err := resolveSiteURL(a.base, "/openApi/torrent/fetchOpenTorrentList.json")
	if err != nil {
		return nil, err
	}
	body, err := a.client.postJSON(ctx, endpoint, a.headers.Clone(), payload)
	if err != nil {
		return nil, err
	}
	document, err := decodeJSONObject(body)
	if err != nil {
		return nil, err
	}
	if success, ok := document["success"].(bool); !ok || !success {
		return nil, errors.New("YemaPT API 返回失败")
	}
	items, ok := document["data"].([]any)
	if !ok {
		return nil, errors.New("YemaPT 搜索结果不是数组")
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
		resourceID := valueString(item["id"])
		title := strings.TrimSpace(valueString(item["showName"]))
		if title == "" || resourceID == "" {
			continue
		}
		detail, _ := resolveSiteURL(a.base, "/#/torrent/detail/"+url.PathEscape(resourceID)+"/")
		download := fmt.Sprintf("/api/v1/resource/download/%d/%s", a.id, url.PathEscape(resourceID))
		downFactor := yemaDownloadFactor(valueString(item["downloadPromotion"]))
		upFactor := yemaUploadFactor(valueString(item["uploadPromotion"]))
		resource := Resource{
			SiteID: a.id, Title: title, Description: strings.TrimSpace(valueString(item["shortDesc"])),
			DetailURL: detail, DownloadURL: download, TorrentID: resourceID, DownloadID: resourceID,
			SizeBytes: valueInt64(item["fileSize"]), Seeders: valueInt(item["seedNum"]),
			Peers: valueInt(item["leechNum"]), Grabs: valueInt(item["completedNum"]),
			PublishedAt: parseTime(valueString(item["listingTime"])), FreeDate: parseTime(valueString(item["downloadPromotionEndTime"])),
			Labels: yemaLabels(item["tagList"]), Category: yemaCategory(valueInt(item["categoryId"])),
			HitAndRun: valueBool(item["hrPunishEnable"]), DownloadVolumeFactor: &downFactor, UploadVolumeFactor: &upFactor,
		}
		resources = append(resources, resource)
		if len(resources) == limit {
			break
		}
	}
	return resources, nil
}

func (a *YemaAdapter) ResolveDownload(ctx context.Context, resourceID string) (ResolvedDownload, error) {
	if !validNumericID(resourceID) {
		return ResolvedDownload{}, errors.New("YemaPT 种子 ID 无效")
	}
	credentialEndpoint, err := resolveSiteURL(a.base, "/openApi/torrent/generateDownloadKey.json")
	if err != nil {
		return ResolvedDownload{}, err
	}
	id, err := strconv.ParseInt(resourceID, 10, 64)
	if err != nil {
		return ResolvedDownload{}, errors.New("YemaPT torrent ID out of range")
	}
	body, err := a.client.postJSON(ctx, credentialEndpoint, a.headers.Clone(), map[string]any{"id": id})
	if err != nil {
		return ResolvedDownload{}, err
	}
	document, err := decodeJSONObject(body)
	if err != nil {
		return ResolvedDownload{}, err
	}
	if success, ok := document["success"].(bool); !ok || !success {
		return ResolvedDownload{}, errors.New("YemaPT 下载凭证生成失败")
	}
	token := strings.TrimSpace(valueString(document["data"]))
	if token == "" || len(token) > 4096 {
		return ResolvedDownload{}, errors.New("YemaPT 下载凭证无效")
	}
	downloadEndpoint, err := resolveSiteURL(a.base, "/api/torrent/download1?token="+url.QueryEscape(token))
	if err != nil {
		return ResolvedDownload{}, err
	}
	torrent, err := a.client.get(ctx, downloadEndpoint, a.headers.Clone())
	if err != nil {
		return ResolvedDownload{}, err
	}
	return ResolvedDownload{Body: torrent, ContentType: "application/x-bittorrent", Filename: resourceID + ".torrent"}, nil
}

func decodeJSONObject(body []byte) (map[string]any, error) {
	decoder := json.NewDecoder(bytes.NewReader(body))
	decoder.UseNumber()
	var document map[string]any
	if err := decoder.Decode(&document); err != nil {
		return nil, errors.New("解析站点 JSON 失败")
	}
	if decoder.Decode(new(any)) != io.EOF {
		return nil, errors.New("站点 JSON 包含多余内容")
	}
	return document, nil
}

func configString(config map[string]any, keys ...string) string {
	for _, key := range keys {
		if value := strings.TrimSpace(valueString(config[key])); value != "" {
			return value
		}
	}
	return ""
}

func valueString(value any) string {
	switch typed := value.(type) {
	case nil:
		return ""
	case string:
		return typed
	case json.Number:
		return typed.String()
	default:
		return fmt.Sprint(typed)
	}
}

func valueInt64(value any) int64 {
	parsed, err := strconv.ParseInt(strings.TrimSpace(valueString(value)), 10, 64)
	if err != nil || parsed < 0 {
		return 0
	}
	return parsed
}

func valueInt(value any) int {
	parsed := valueInt64(value)
	if uint64(parsed) > uint64(^uint(0)>>1) {
		return 0
	}
	return int(parsed)
}

func valueBool(value any) bool {
	if typed, ok := value.(bool); ok {
		return typed
	}
	parsed, _ := strconv.ParseBool(strings.TrimSpace(valueString(value)))
	return parsed
}

func valueFloat(value any, fallback float64) float64 {
	parsed, err := strconv.ParseFloat(strings.TrimSpace(valueString(value)), 64)
	if err != nil || math.IsNaN(parsed) || math.IsInf(parsed, 0) || parsed < 0 || parsed > 100 {
		return fallback
	}
	return parsed
}

func validNumericID(value string) bool {
	if value == "" || len(value) > 20 {
		return false
	}
	for _, character := range value {
		if character < '0' || character > '9' {
			return false
		}
	}
	return true
}

func yemaDownloadFactor(value string) float64 {
	switch strings.ToLower(value) {
	case "free":
		return 0
	case "half":
		return 0.5
	default:
		return 1
	}
}

func yemaUploadFactor(value string) float64 {
	switch strings.ToLower(value) {
	case "one_half":
		return 1.5
	case "double_upload":
		return 2
	default:
		return 1
	}
}

func yemaLabels(value any) []string {
	labels := map[string]string{
		"1": "禁转", "2": "首发", "3": "官方", "4": "自制", "5": "国语", "6": "中字",
		"7": "粤语", "8": "英字", "9": "HDR10", "10": "杜比视界", "11": "分集", "12": "完结",
	}
	items, ok := value.([]any)
	if !ok {
		return nil
	}
	result := make([]string, 0, min(len(items), 32))
	for _, item := range items {
		if label := labels[valueString(item)]; label != "" && len(result) < 32 {
			result = append(result, label)
		}
	}
	return result
}

func yemaCategory(value int) string {
	if value == 4 {
		return "电影"
	}
	for _, category := range []int{5, 6, 13, 14, 15, 16, 17} {
		if value == category {
			return "电视剧"
		}
	}
	return "未知"
}
