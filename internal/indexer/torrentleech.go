package indexer

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"unicode"
)

const torrentLeechPageSize = 100

// TorrentLeechAdapter 处理关键词/浏览 URL 分支，并代理需要 Cookie 的种子下载。
type TorrentLeechAdapter struct {
	client  *HTTPClient
	id      int
	name    string
	base    *url.URL
	headers http.Header
}

func NewTorrentLeechAdapter(client *HTTPClient, id int, name, baseURL string, config map[string]any) (*TorrentLeechAdapter, error) {
	if client == nil {
		return nil, errors.New("站点 HTTP 客户端不能为空")
	}
	name = strings.TrimSpace(name)
	if name == "" {
		name = "TorrentLeech"
	}
	base, err := validateBaseURL(baseURL, client.allowPrivate)
	if err != nil {
		return nil, err
	}
	cookie := configString(config, "cookie")
	if cookie == "" {
		return nil, errors.New("TorrentLeech 未配置 Cookie")
	}
	if len(cookie) > 64<<10 || strings.ContainsAny(cookie, "\r\n") {
		return nil, errors.New("TorrentLeech Cookie 无效")
	}
	headers := make(http.Header)
	headers.Set("Cookie", cookie)
	headers.Set("Accept", "application/json")
	if userAgent := configString(config, "ua", "user_agent"); userAgent != "" {
		if len(userAgent) > 1024 || strings.ContainsAny(userAgent, "\r\n") {
			return nil, errors.New("TorrentLeech User-Agent 无效")
		}
		headers.Set("User-Agent", userAgent)
	}
	return &TorrentLeechAdapter{client: client, id: id, name: name, base: base, headers: headers}, nil
}

func (a *TorrentLeechAdapter) Name() string           { return a.name }
func (a *TorrentLeechAdapter) SiteID() int            { return a.id }
func (a *TorrentLeechAdapter) ResolverKind() string   { return "torrentleech" }
func (a *TorrentLeechAdapter) ResolverVersion() int16 { return downloadResolverVersionV1 }

func (a *TorrentLeechAdapter) Search(ctx context.Context, query Query) ([]Resource, error) {
	keyword := strings.TrimSpace(query.Keyword)
	if containsHan(keyword) {
		return []Resource{}, nil
	}
	if query.Page < 0 {
		query.Page = 0
	}
	limit := min(max(query.Limit, 1), torrentLeechPageSize)
	path := "/torrents/browse/list/page/" + fmt.Sprint(query.Page+1)
	if keyword != "" {
		path = "/torrents/browse/list/query/" + url.PathEscape(keyword)
	}
	endpoint, err := resolveSiteURL(a.base, path)
	if err != nil {
		return nil, err
	}
	body, err := a.client.get(ctx, endpoint, a.headers.Clone())
	if err != nil {
		return nil, err
	}
	document, err := decodeJSONObject(body)
	if err != nil {
		return nil, err
	}
	items, ok := document["torrentList"].([]any)
	if !ok {
		return nil, errors.New("TorrentLeech 搜索结果不是数组")
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
		fid, filename := strings.TrimSpace(valueString(item["fid"])), strings.TrimSpace(valueString(item["filename"]))
		title := strings.TrimSpace(valueString(item["name"]))
		if title == "" || !validNumericID(fid) || filename == "" || len(filename) > 512 {
			continue
		}
		downloadID := encodeTorrentLeechDownloadID(fid, filename)
		detail, _ := resolveSiteURL(a.base, "/torrent/"+url.PathEscape(fid))
		downloadFactor := valueFloat(item["download_multiplier"], 1)
		resources = append(resources, Resource{
			SiteID: a.id, Title: title, DetailURL: detail, TorrentID: fid, DownloadID: downloadID,
			DownloadURL: fmt.Sprintf("/api/v1/resource/download/%d/%s", a.id, downloadID),
			SizeBytes:   valueInt64(item["size"]), Seeders: valueInt(item["seeders"]), Peers: valueInt(item["leechers"]),
			Grabs: valueInt(item["completed"]), PublishedAt: parseTime(valueString(item["addedTimestamp"])),
			IMDBID: strings.TrimSpace(valueString(item["imdbID"])), DownloadVolumeFactor: &downloadFactor,
		})
		if len(resources) == limit {
			break
		}
	}
	return resources, nil
}

func (a *TorrentLeechAdapter) ResolveDownload(ctx context.Context, resourceID string) (ResolvedDownload, error) {
	fid, filename, err := decodeTorrentLeechDownloadID(resourceID)
	if err != nil {
		return ResolvedDownload{}, err
	}
	endpoint, err := resolveSiteURL(a.base, "/download/"+url.PathEscape(fid)+"/"+url.PathEscape(filename))
	if err != nil {
		return ResolvedDownload{}, err
	}
	body, err := a.client.get(ctx, endpoint, a.headers.Clone())
	if err != nil {
		return ResolvedDownload{}, err
	}
	return ResolvedDownload{Body: body, ContentType: "application/x-bittorrent", Filename: filename}, nil
}

func containsHan(value string) bool {
	for _, character := range value {
		if unicode.Is(unicode.Han, character) {
			return true
		}
	}
	return false
}

func encodeTorrentLeechDownloadID(fid, filename string) string {
	return base64.RawURLEncoding.EncodeToString([]byte(fid + "\n" + filename))
}

func decodeTorrentLeechDownloadID(value string) (string, string, error) {
	if value == "" || len(value) > 1024 {
		return "", "", errors.New("TorrentLeech 下载标识无效")
	}
	decoded, err := base64.RawURLEncoding.DecodeString(value)
	if err != nil {
		return "", "", errors.New("TorrentLeech 下载标识无效")
	}
	parts := strings.SplitN(string(decoded), "\n", 2)
	if len(parts) != 2 || !validNumericID(parts[0]) || parts[1] == "" || len(parts[1]) > 512 || strings.ContainsAny(parts[1], "\r\n") {
		return "", "", errors.New("TorrentLeech 下载标识无效")
	}
	return parts[0], parts[1], nil
}
