package indexer

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/xml"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

const torznabMaxLimit = 100

// TorznabAdapter 兼容标准 Torznab XML 协议（Jackett、Prowlarr 及自建 Torznab 聚合器）。
// 支持关键词/IMDb 检索、caps 连通测试、RSS 增量拉取及二阶段种子下载。
type TorznabAdapter struct {
	client     *HTTPClient
	id         int
	name       string
	base       *url.URL
	endpoint   *url.URL
	apiKey     string
	categories string
	headers    http.Header
}

// NewTorznabAdapter 构造 Torznab / Jackett / Prowlarr 适配器。
func NewTorznabAdapter(client *HTTPClient, id int, name, baseURL string, config map[string]any) (*TorznabAdapter, error) {
	if client == nil {
		return nil, errors.New("站点 HTTP 客户端不能为空")
	}
	name = strings.TrimSpace(name)
	if name == "" {
		name = "Torznab"
	}
	base, err := validateBaseURL(baseURL, client.allowPrivate)
	if err != nil {
		return nil, err
	}

	endpoint := *base
	if customPath := configString(config, "search_path", "api_path"); customPath != "" {
		resolved, resolveErr := resolveSiteURL(base, customPath)
		if resolveErr != nil {
			return nil, fmt.Errorf("Torznab API 路径无效: %w", resolveErr)
		}
		parsed, parseErr := url.Parse(resolved)
		if parseErr != nil {
			return nil, parseErr
		}
		endpoint = *parsed
	} else if endpoint.Path == "" || endpoint.Path == "/" {
		endpoint.Path = "/api"
	}

	apiKey := configString(config, "apikey", "api_key", "token")
	if apiKey == "" {
		apiKey = strings.TrimSpace(endpoint.Query().Get("apikey"))
	}
	if apiKey == "" {
		return nil, errors.New("Torznab 未配置 apikey")
	}
	if len(apiKey) > 1024 || strings.ContainsAny(apiKey, "\r\n") {
		return nil, errors.New("Torznab apikey 无效")
	}
	// 避免 URL 上重复携带 apikey。
	cleanQuery := endpoint.Query()
	cleanQuery.Del("apikey")
	endpoint.RawQuery = cleanQuery.Encode()

	headers := make(http.Header)
	headers.Set("Accept", "application/rss+xml, application/xml, text/xml;q=0.9")
	if ua := configString(config, "ua", "user_agent"); ua != "" {
		if len(ua) > 1024 || strings.ContainsAny(ua, "\r\n") {
			return nil, errors.New("Torznab User-Agent 无效")
		}
		headers.Set("User-Agent", ua)
	}

	return &TorznabAdapter{
		client:     client,
		id:         id,
		name:       name,
		base:       base,
		endpoint:   &endpoint,
		apiKey:     apiKey,
		categories: configString(config, "categories", "cat"),
		headers:    headers,
	}, nil
}

func (a *TorznabAdapter) Name() string           { return a.name }
func (a *TorznabAdapter) SiteID() int            { return a.id }
func (a *TorznabAdapter) ResolverKind() string   { return "torznab" }
func (a *TorznabAdapter) ResolverVersion() int16 { return downloadResolverVersionV1 }

func (a *TorznabAdapter) buildURL(params url.Values) string {
	u := *a.endpoint
	q := u.Query()
	for k, vals := range params {
		for _, v := range vals {
			q.Set(k, v)
		}
	}
	q.Set("apikey", a.apiKey)
	u.RawQuery = q.Encode()
	return u.String()
}

// Search 向 Torznab 端点发送 t=search（或 IMDb 检索）并解析 RSS + torznab:attr。
func (a *TorznabAdapter) Search(ctx context.Context, query Query) ([]Resource, error) {
	limit := min(max(query.Limit, 1), torznabMaxLimit)
	params := url.Values{}
	params.Set("t", "search")
	params.Set("limit", strconv.Itoa(limit))
	if query.Page > 0 {
		params.Set("offset", strconv.Itoa(query.Page*limit))
	}
	if a.categories != "" {
		params.Set("cat", a.categories)
	}
	keyword := strings.TrimSpace(query.Keyword)
	if keyword != "" {
		if nexusPHPIMDbPattern.MatchString(keyword) && strings.HasPrefix(strings.ToLower(keyword), "tt") {
			params.Set("imdbid", strings.TrimPrefix(strings.ToLower(keyword), "tt"))
			params.Set("q", keyword)
		} else {
			params.Set("q", keyword)
		}
	}

	body, err := a.client.get(ctx, a.buildURL(params), a.headers.Clone())
	if err != nil {
		return nil, err
	}
	return a.parseTorznabXML(body, limit)
}

// TestSite 通过 ?t=caps 探测 Torznab 站点连通性与 API Key 有效性。
func (a *TorznabAdapter) TestSite(ctx context.Context) error {
	params := url.Values{"t": {"caps"}}
	resp, err := a.client.getResponseLimited(ctx, a.buildURL(params), a.headers.Clone(), maxProbeBodyBytes)
	if err != nil {
		return err
	}
	if resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden {
		return siteTestFailure("Torznab API Key 无效")
	}
	if apiErr := parseTorznabError(resp.Body); apiErr != nil {
		return siteTestFailure(apiErr.Error())
	}
	if !bytes.Contains(bytes.ToLower(resp.Body), []byte("<caps")) {
		return siteTestFailure("Torznab 返回格式无效（缺少 caps 根节点）")
	}
	return nil
}

// ResolveDownload 从 Torznab 服务端拉取真实 .torrent 字节。
func (a *TorznabAdapter) ResolveDownload(ctx context.Context, resourceID string) (ResolvedDownload, error) {
	if resourceID == "" || len(resourceID) > 4096 {
		return ResolvedDownload{}, errors.New("Torznab 下载标识无效")
	}
	decoded, err := base64.RawURLEncoding.DecodeString(resourceID)
	if err != nil || len(decoded) == 0 || len(decoded) > 2048 {
		return ResolvedDownload{}, errors.New("Torznab 下载标识无效")
	}
	rawTarget := strings.TrimSpace(string(decoded))
	endpoint, err := resolveSiteURL(a.base, rawTarget)
	if err != nil {
		return ResolvedDownload{}, errors.New("Torznab 下载地址不受信任")
	}
	body, err := a.client.get(ctx, endpoint, a.headers.Clone())
	if err != nil {
		return ResolvedDownload{}, err
	}
	return ResolvedDownload{
		Body:        body,
		ContentType: "application/x-bittorrent",
		Filename:    fmt.Sprintf("torznab-%d.torrent", a.id),
	}, nil
}

// RSSCursorKey 返回不暴露 API Key 的 RSS 游标键。
func (a *TorznabAdapter) RSSCursorKey() string {
	if a == nil || a.endpoint == nil {
		return ""
	}
	digest := sha256.Sum256([]byte("moviepilot:torznab-rss:v1\x00" + a.endpoint.String() + "\x00" + a.categories))
	return "sha256:" + hex.EncodeToString(digest[:])
}

// FetchRSS 拉取 Torznab 最新资源流，直接复用订阅 RSS 增量轮询。
func (a *TorznabAdapter) FetchRSS(ctx context.Context, request RSSRequest) (RSSResponse, error) {
	limit := request.Limit
	if limit <= 0 || limit > torznabMaxLimit {
		limit = torznabMaxLimit
	}
	params := url.Values{
		"t":     {"search"},
		"limit": {strconv.Itoa(limit)},
	}
	if a.categories != "" {
		params.Set("cat", a.categories)
	}
	headers := a.headers.Clone()
	if strings.TrimSpace(request.ETag) != "" {
		headers.Set("If-None-Match", strings.TrimSpace(request.ETag))
	}
	if strings.TrimSpace(request.LastModified) != "" {
		headers.Set("If-Modified-Since", strings.TrimSpace(request.LastModified))
	}
	resp, err := a.client.getResponseLimited(ctx, a.buildURL(params), headers, a.client.maxBodyBytes)
	if err != nil {
		return RSSResponse{}, err
	}
	if resp.StatusCode == http.StatusNotModified {
		return RSSResponse{
			NotModified:  true,
			ETag:         resp.ETag,
			LastModified: resp.LastModified,
		}, nil
	}
	resources, err := a.parseTorznabXML(resp.Body, limit)
	if err != nil {
		return RSSResponse{}, err
	}
	items := make([]RSSItem, 0, len(resources))
	for _, res := range resources {
		key := RSSItemKey(a.id, res.TorrentID, res.DetailURL, res.DownloadID, res.Title, res.PublishedAt, res.SizeBytes)
		res.ItemKey = key
		items = append(items, RSSItem{
			ItemKey:  key,
			GUID:     res.TorrentID,
			Resource: res,
		})
	}
	return RSSResponse{
		ETag:         resp.ETag,
		LastModified: resp.LastModified,
		Items:        items,
	}, nil
}

type torznabErrorXML struct {
	XMLName     xml.Name `xml:"error"`
	Code        string   `xml:"code,attr"`
	Description string   `xml:"description,attr"`
}

func parseTorznabError(body []byte) error {
	var errDoc torznabErrorXML
	if err := xml.Unmarshal(body, &errDoc); err == nil && errDoc.XMLName.Local == "error" {
		desc := strings.TrimSpace(errDoc.Description)
		if desc == "" {
			desc = "未知错误"
		}
		return fmt.Errorf("Torznab API 错误 (%s): %s", strings.TrimSpace(errDoc.Code), desc)
	}
	return nil
}

type torznabFeedXML struct {
	Channel struct {
		Items []torznabItemXML `xml:"item"`
	} `xml:"channel"`
}

type torznabItemXML struct {
	Title       string           `xml:"title"`
	GUID        string           `xml:"guid"`
	Link        string           `xml:"link"`
	Comments    string           `xml:"comments"`
	Description string           `xml:"description"`
	PubDate     string           `xml:"pubDate"`
	Size        int64            `xml:"size"`
	Category    []string         `xml:"category"`
	Enclosure   torznabEnclosure `xml:"enclosure"`
	Attrs       []torznabAttrXML `xml:"attr"`
}

type torznabEnclosure struct {
	URL    string `xml:"url,attr"`
	Length int64  `xml:"length,attr"`
	Type   string `xml:"type,attr"`
}

type torznabAttrXML struct {
	Name  string `xml:"name,attr"`
	Value string `xml:"value,attr"`
}

func (a *TorznabAdapter) parseTorznabXML(body []byte, limit int) ([]Resource, error) {
	if apiErr := parseTorznabError(body); apiErr != nil {
		return nil, apiErr
	}
	var feed torznabFeedXML
	if err := xml.Unmarshal(body, &feed); err != nil {
		return nil, fmt.Errorf("解析 Torznab XML 失败: %w", err)
	}

	resources := make([]Resource, 0, min(limit, len(feed.Channel.Items)))
	for _, item := range feed.Channel.Items {
		if len(resources) >= limit {
			break
		}
		title := strings.TrimSpace(item.Title)
		if title == "" {
			continue
		}
		rawDownload := strings.TrimSpace(item.Enclosure.URL)
		if rawDownload == "" {
			rawDownload = strings.TrimSpace(item.Link)
		}
		if rawDownload == "" {
			continue
		}
		if _, err := resolveSiteURL(a.base, rawDownload); err != nil {
			continue
		}

		downloadID := base64.RawURLEncoding.EncodeToString([]byte(rawDownload))
		downloadURL := fmt.Sprintf("/api/v1/resource/download/%d/%s", a.id, downloadID)
		detailURL := strings.TrimSpace(item.Comments)
		if detailURL == "" {
			detailURL = strings.TrimSpace(item.GUID)
		}

		sizeBytes := item.Size
		if sizeBytes <= 0 && item.Enclosure.Length > 0 {
			sizeBytes = item.Enclosure.Length
		}

		var (
			seeders     int
			totalPeers  int
			explicitLee int
			hasLeechers bool
			grabs       int
			imdbID      string
			downFactor  = 1.0
			upFactor    = 1.0
			hasDown     bool
			hasUp       bool
			labels      []string
			category    string
		)
		if len(item.Category) > 0 {
			category = strings.TrimSpace(item.Category[0])
		}
		for _, attr := range item.Attrs {
			val := strings.TrimSpace(attr.Value)
			switch strings.ToLower(strings.TrimSpace(attr.Name)) {
			case "seeders":
				if n, err := strconv.Atoi(val); err == nil {
					seeders = n
				}
			case "peers":
				if n, err := strconv.Atoi(val); err == nil {
					totalPeers = n
				}
			case "leechers":
				if n, err := strconv.Atoi(val); err == nil {
					explicitLee = n
					hasLeechers = true
				}
			case "grabs":
				if n, err := strconv.Atoi(val); err == nil {
					grabs = n
				}
			case "size":
				if n, err := strconv.ParseInt(val, 10, 64); err == nil && n > 0 {
					sizeBytes = n
				}
			case "imdb", "imdbid":
				if val != "" {
					if strings.HasPrefix(strings.ToLower(val), "tt") {
						imdbID = strings.ToLower(val)
					} else if validNumericID(val) {
						imdbID = "tt" + val
					}
				}
			case "downloadvolumefactor":
				if f, err := strconv.ParseFloat(val, 64); err == nil {
					downFactor = f
					hasDown = true
				}
			case "uploadvolumefactor":
				if f, err := strconv.ParseFloat(val, 64); err == nil {
					upFactor = f
					hasUp = true
				}
			case "tag", "label":
				if val != "" {
					labels = append(labels, val)
				}
			case "category":
				if category == "" && val != "" {
					category = val
				}
			}
		}

		peers := explicitLee
		if !hasLeechers {
			peers = max(totalPeers-seeders, 0)
		}

		var downPtr, upPtr *float64
		if hasDown {
			v := downFactor
			downPtr = &v
		}
		if hasUp {
			v := upFactor
			upPtr = &v
		}

		publishedAt := parseTorznabPubDate(item.PubDate)
		resources = append(resources, Resource{
			SiteID:               a.id,
			Site:                 a.name,
			Title:                title,
			Description:          strings.TrimSpace(item.Description),
			DetailURL:            detailURL,
			DownloadURL:          downloadURL,
			TorrentID:            strings.TrimSpace(item.GUID),
			DownloadID:           downloadID,
			SizeBytes:            sizeBytes,
			Seeders:              seeders,
			Peers:                peers,
			Grabs:                grabs,
			PublishedAt:          publishedAt,
			IMDBID:               imdbID,
			Labels:               labels,
			Category:             category,
			DownloadVolumeFactor: downPtr,
			UploadVolumeFactor:   upPtr,
		})
	}
	return resources, nil
}

func parseTorznabPubDate(raw string) time.Time {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return time.Time{}
	}
	for _, layout := range []string{
		time.RFC1123Z,
		time.RFC1123,
		time.RFC3339,
		"Mon, 2 Jan 2006 15:04:05 -0700",
		"Mon, 2 Jan 2006 15:04:05 MST",
	} {
		if t, err := time.Parse(layout, raw); err == nil {
			return t.UTC()
		}
	}
	return parseTime(raw)
}
