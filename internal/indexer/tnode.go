package indexer

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"
)

// TNodeAdapter 封装 CSRF 搜索与服务端 Cookie 下载，避免把站点凭据返回给前端。
type TNodeAdapter struct {
	search  *DeclarativeAdapter
	client  *HTTPClient
	id      int
	name    string
	base    *url.URL
	headers http.Header
}

func NewTNodeAdapter(client *HTTPClient, id int, name, baseURL string, config map[string]any) (*TNodeAdapter, error) {
	if client == nil {
		return nil, errors.New("站点 HTTP 客户端不能为空")
	}
	name = strings.TrimSpace(name)
	if name == "" {
		name = "TNode"
	}
	cookie := configString(config, "cookie")
	if cookie == "" {
		return nil, errors.New("TNode 未配置 Cookie")
	}
	userAgent := configString(config, "ua", "user_agent")
	csrfRegex := configString(config, "csrf_regex")
	if csrfRegex == "" {
		csrfRegex = `(?i)name=["']x-csrf-token["'][^>]+content=["']([^"']+)`
	}
	definition := Definition{
		ID: id, Name: name, BaseURL: baseURL, Format: "json", SearchPath: "/api/torrent/advancedSearch", Method: http.MethodPost, PageOffset: 1,
		ResultPath: "data.torrents", Cookie: cookie, UserAgent: userAgent, CSRFPath: "/", CSRFRegex: csrfRegex,
		Body: map[string]any{
			"page": "${page}", "size": "${limit}", "type": "${search_type}", "keyword": "${keyword}",
			"sorter": "id", "order": "desc", "tags": []any{}, "category": []any{501, 502, 503, 504},
			"medium": []any{}, "videoCoding": []any{}, "audioCoding": []any{}, "resolution": []any{}, "group": []any{},
		},
		Fields: Fields{
			Title: Field{Selector: "title"}, Description: Field{Selector: "subtitle"}, ResourceID: Field{Selector: "id"},
			DetailURL: Field{Selector: "id", Template: "/torrent/info/${value}"},
			Size:      Field{Selector: "size"}, Seeders: Field{Selector: "seeding"}, Peers: Field{Selector: "leeching"},
			Grabs: Field{Selector: "complete"}, PublishedAt: Field{Selector: "upload_time"}, IMDBID: Field{Selector: "imdb"},
			DownloadVolumeFactor: Field{Selector: "downloadRate"}, UploadVolumeFactor: Field{Selector: "uploadRate"},
		},
	}
	search, err := NewDeclarativeAdapter(client, definition)
	if err != nil {
		return nil, err
	}
	headers := make(http.Header)
	headers.Set("Cookie", cookie)
	if userAgent != "" {
		headers.Set("User-Agent", userAgent)
	}
	return &TNodeAdapter{search: search, client: client, id: id, name: name, base: search.base, headers: headers}, nil
}

func (a *TNodeAdapter) Name() string           { return a.name }
func (a *TNodeAdapter) SiteID() int            { return a.id }
func (a *TNodeAdapter) ResolverKind() string   { return "tnode" }
func (a *TNodeAdapter) ResolverVersion() int16 { return downloadResolverVersionV1 }

func (a *TNodeAdapter) Search(ctx context.Context, query Query) ([]Resource, error) {
	resources, err := a.search.Search(ctx, query)
	if err != nil {
		return nil, err
	}
	for index := range resources {
		if !validNumericID(resources[index].DownloadID) {
			resources[index].DownloadURL = ""
			continue
		}
		resources[index].DownloadURL = fmt.Sprintf("/api/v1/resource/download/%d/%s", a.id, url.PathEscape(resources[index].DownloadID))
	}
	return resources, nil
}

func (a *TNodeAdapter) ResolveDownload(ctx context.Context, resourceID string) (ResolvedDownload, error) {
	if !validNumericID(resourceID) {
		return ResolvedDownload{}, errors.New("TNode 种子 ID 无效")
	}
	endpoint, err := resolveSiteURL(a.base, "/api/torrent/download/"+url.PathEscape(resourceID))
	if err != nil {
		return ResolvedDownload{}, err
	}
	body, err := a.client.get(ctx, endpoint, a.headers.Clone())
	if err != nil {
		return ResolvedDownload{}, err
	}
	return ResolvedDownload{Body: body, ContentType: "application/x-bittorrent", Filename: resourceID + ".torrent"}, nil
}
