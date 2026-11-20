package indexer

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/PuerkitoBio/goquery"
	"github.com/mmcdole/gofeed"
)

const nexusPHPMaxLimit = 100

var (
	nexusPHPIMDbPattern    = regexp.MustCompile(`(?i)\b(tt\d{6,9})\b`)
	nexusPHPRatioPattern   = regexp.MustCompile(`(?i)(?:分享率|ratio)\s*[:：]?\s*([0-9]+(?:\.[0-9]+)?|∞|inf|无限)`)
	nexusPHPUploadPattern  = regexp.MustCompile(`(?i)(?:上传量|上传|uploaded)\s*[:：]?\s*([0-9]+(?:\.[0-9]+)?\s*(?:[KMGTPE]i?B|B))`)
	nexusPHPDownPattern    = regexp.MustCompile(`(?i)(?:下载量|下载|downloaded)\s*[:：]?\s*([0-9]+(?:\.[0-9]+)?\s*(?:[KMGTPE]i?B|B))`)
	nexusPHPBonusPattern   = regexp.MustCompile(`(?i)(?:魔力值|魔力|积分|做种积分|bonus)\s*[:：]?\s*([0-9,]+(?:\.[0-9]+)?)`)
	nexusPHPDateTimeRegexp = regexp.MustCompile(`\d{4}-\d{2}-\d{2}\s+\d{2}:\d{2}(?::\d{2})?`)
	nexusPHPSizeCellRegexp = regexp.MustCompile(`(?i)^\s*[0-9]+(?:\.[0-9]+)?\s*(?:[KMGTPE]i?B|B)\s*$`)
	nexusPHPIntPattern     = regexp.MustCompile(`\d+`)
)

// NexusPHPAdapter 是面向通用 NexusPHP PT 站的适配器：
// 支持 torrents.php 检索、优惠/H&R/IMDb 解析、种子下载、连通测试、用户数据刷新及可选 RSS 增量拉取。
type NexusPHPAdapter struct {
	client      *HTTPClient
	id          int
	name        string
	base        *url.URL
	headers     http.Header
	searchPath  string
	searchParam string
	rssPath     string
	passkey     string
	extraParams map[string]string
}

// NewNexusPHPAdapter 构造通用 NexusPHP 适配器。
func NewNexusPHPAdapter(client *HTTPClient, id int, name, baseURL string, config map[string]any) (*NexusPHPAdapter, error) {
	if client == nil {
		return nil, errors.New("站点 HTTP 客户端不能为空")
	}
	name = strings.TrimSpace(name)
	if name == "" {
		name = "NexusPHP"
	}
	base, err := validateBaseURL(baseURL, client.allowPrivate)
	if err != nil {
		return nil, err
	}
	cookie := configString(config, "cookie")
	if cookie == "" {
		return nil, errors.New("NexusPHP 未配置 Cookie")
	}
	if len(cookie) > 64<<10 || strings.ContainsAny(cookie, "\r\n") {
		return nil, errors.New("NexusPHP Cookie 无效")
	}

	headers := make(http.Header)
	headers.Set("Cookie", cookie)
	headers.Set("Accept", "text/html,application/xhtml+xml,application/xml;q=0.9,*/*;q=0.8")
	if ua := configString(config, "ua", "user_agent"); ua != "" {
		if len(ua) > 1024 || strings.ContainsAny(ua, "\r\n") {
			return nil, errors.New("NexusPHP User-Agent 无效")
		}
		headers.Set("User-Agent", ua)
	}

	searchPath := configString(config, "search_path")
	if searchPath == "" {
		searchPath = "/torrents.php"
	}
	searchParam := configString(config, "search_param", "query_param")
	if searchParam == "" {
		searchParam = "search"
	}
	rssPath := configString(config, "rss", "rss_path")
	if rssPath != "" {
		if _, err := resolveSiteURL(base, rssPath); err != nil {
			return nil, fmt.Errorf("NexusPHP RSS 地址无效: %w", err)
		}
	}

	extraParams := map[string]string{}
	if rawParams, ok := config["params"].(map[string]any); ok {
		for k, v := range rawParams {
			key := strings.TrimSpace(k)
			if key != "" {
				extraParams[key] = strings.TrimSpace(valueString(v))
			}
		}
	}

	return &NexusPHPAdapter{
		client:      client,
		id:          id,
		name:        name,
		base:        base,
		headers:     headers,
		searchPath:  searchPath,
		searchParam: searchParam,
		rssPath:     rssPath,
		passkey:     configString(config, "passkey"),
		extraParams: extraParams,
	}, nil
}

func (a *NexusPHPAdapter) Name() string           { return a.name }
func (a *NexusPHPAdapter) SiteID() int            { return a.id }
func (a *NexusPHPAdapter) ResolverKind() string   { return "nexusphp" }
func (a *NexusPHPAdapter) ResolverVersion() int16 { return downloadResolverVersionV1 }

// Search 在 NexusPHP 站点的 torrents.php 执行关键词或 IMDb 搜索。
func (a *NexusPHPAdapter) Search(ctx context.Context, query Query) ([]Resource, error) {
	limit := min(max(query.Limit, 1), nexusPHPMaxLimit)
	endpoint, err := resolveSiteURL(a.base, a.searchPath)
	if err != nil {
		return nil, err
	}
	parsed, err := url.Parse(endpoint)
	if err != nil {
		return nil, err
	}
	values := parsed.Query()
	for k, v := range a.extraParams {
		values.Set(k, v)
	}
	keyword := strings.TrimSpace(query.Keyword)
	values.Set("incldead", "1")
	if keyword != "" {
		values.Set(a.searchParam, keyword)
		if nexusPHPIMDbPattern.MatchString(keyword) && strings.HasPrefix(strings.ToLower(keyword), "tt") {
			values.Set("search_area", "4")
		} else if values.Get("search_area") == "" {
			values.Set("search_area", "0")
		}
		if values.Get("search_mode") == "" {
			values.Set("search_mode", "0")
		}
	}
	if query.Page > 0 {
		values.Set("page", strconv.Itoa(query.Page))
	}
	parsed.RawQuery = values.Encode()

	body, err := a.client.get(ctx, parsed.String(), a.headers.Clone())
	if err != nil {
		return nil, err
	}
	if nexusPHPLooksUnauthenticated(body) {
		return nil, siteTestFailure("站点 Cookie 已失效或未通过认证")
	}

	return a.parseSearchHTML(body, limit)
}

func (a *NexusPHPAdapter) parseSearchHTML(body []byte, limit int) ([]Resource, error) {
	doc, err := goquery.NewDocumentFromReader(bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("解析 NexusPHP 搜索页面失败: %w", err)
	}

	rows := doc.Find("table.torrents > tbody > tr, table.torrents > tr")
	resources := make([]Resource, 0, min(limit, rows.Length()))

	rows.EachWithBreak(func(_ int, row *goquery.Selection) bool {
		if len(resources) >= limit {
			return false
		}
		if row.Find("th").Length() > 0 && row.Find("a[href*='details.php']").Length() == 0 {
			return true
		}
		detailLink := row.Find("table.torrentname a[href*='details.php']").First()
		if detailLink.Length() == 0 {
			detailLink = row.Find("a[href*='details.php']").First()
		}
		if detailLink.Length() == 0 {
			return true
		}

		title := strings.TrimSpace(detailLink.AttrOr("title", ""))
		if title == "" {
			title = strings.TrimSpace(detailLink.Text())
		}
		if title == "" {
			return true
		}

		detailHref := strings.TrimSpace(detailLink.AttrOr("href", ""))
		detailURL, err := resolveSiteURL(a.base, detailHref)
		if err != nil {
			return true
		}
		torrentID := nexusPHPExtractID(detailHref)

		downloadHref := strings.TrimSpace(row.Find("a[href*='download.php']").First().AttrOr("href", ""))
		if downloadHref == "" && torrentID != "" {
			downloadHref = "/download.php?id=" + url.QueryEscape(torrentID)
		}
		if downloadHref == "" {
			return true
		}
		if _, err := resolveSiteURL(a.base, downloadHref); err != nil {
			return true
		}
		if torrentID == "" {
			torrentID = nexusPHPExtractID(downloadHref)
		}

		downloadID := base64.RawURLEncoding.EncodeToString([]byte(downloadHref))
		downloadURL := fmt.Sprintf("/api/v1/resource/download/%d/%s", a.id, downloadID)

		description := nexusPHPExtractDescription(row, detailLink)
		downFactor, upFactor := nexusPHPExtractPromo(row)
		labels := nexusPHPExtractLabels(row)
		imdbID := nexusPHPExtractIMDb(row)
		hr := nexusPHPDetectHR(row)
		category := nexusPHPExtractCategory(row)
		sizeBytes, seeders, peers, grabs, publishedAt := nexusPHPExtractRowMetrics(row)

		resources = append(resources, Resource{
			SiteID:               a.id,
			Site:                 a.name,
			Title:                title,
			Description:          description,
			DetailURL:            detailURL,
			DownloadURL:          downloadURL,
			TorrentID:            torrentID,
			DownloadID:           downloadID,
			SizeBytes:            sizeBytes,
			Seeders:              seeders,
			Peers:                peers,
			Grabs:                grabs,
			PublishedAt:          publishedAt,
			IMDBID:               imdbID,
			Labels:               labels,
			Category:             category,
			DownloadVolumeFactor: &downFactor,
			UploadVolumeFactor:   &upFactor,
			HitAndRun:            hr,
		})
		return true
	})

	return resources, nil
}

// ResolveDownload 按二阶段协议下载 .torrent 文件。
func (a *NexusPHPAdapter) ResolveDownload(ctx context.Context, resourceID string) (ResolvedDownload, error) {
	if resourceID == "" || len(resourceID) > 2048 {
		return ResolvedDownload{}, errors.New("NexusPHP 下载标识无效")
	}
	decoded, err := base64.RawURLEncoding.DecodeString(resourceID)
	if err != nil || len(decoded) == 0 || len(decoded) > 1024 {
		return ResolvedDownload{}, errors.New("NexusPHP 下载标识无效")
	}
	rawTarget := strings.TrimSpace(string(decoded))
	endpoint, err := resolveSiteURL(a.base, rawTarget)
	if err != nil {
		return ResolvedDownload{}, errors.New("NexusPHP 下载地址不受信任")
	}
	if a.passkey != "" {
		if parsed, parseErr := url.Parse(endpoint); parseErr == nil {
			q := parsed.Query()
			if q.Get("passkey") == "" {
				q.Set("passkey", a.passkey)
				parsed.RawQuery = q.Encode()
				endpoint = parsed.String()
			}
		}
	}
	body, err := a.client.get(ctx, endpoint, a.headers.Clone())
	if err != nil {
		return ResolvedDownload{}, err
	}
	filename := fmt.Sprintf("nexusphp-%d.torrent", a.id)
	if tid := nexusPHPExtractID(rawTarget); tid != "" {
		filename = fmt.Sprintf("nexusphp-%d-%s.torrent", a.id, tid)
	}
	return ResolvedDownload{
		Body:        body,
		ContentType: "application/x-bittorrent",
		Filename:    filename,
	}, nil
}

// TestSite 探测 NexusPHP 站点连通性与 Cookie 有效性。
func (a *NexusPHPAdapter) TestSite(ctx context.Context) error {
	endpoint, err := resolveSiteURL(a.base, "/index.php")
	if err != nil {
		return err
	}
	resp, err := a.client.getResponseLimited(ctx, endpoint, a.headers.Clone(), maxProbeBodyBytes)
	if err != nil {
		return err
	}
	if resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden {
		return siteTestFailure("站点返回认证失败")
	}
	if nexusPHPLooksUnauthenticated(resp.Body) {
		return siteTestFailure("站点 Cookie 已失效或需要重新登录")
	}
	return nil
}

// FetchSiteUserData 从 NexusPHP 首页顶部信息栏解析用户数据。
func (a *NexusPHPAdapter) FetchSiteUserData(ctx context.Context) (SiteUserData, error) {
	endpoint, err := resolveSiteURL(a.base, "/index.php")
	if err != nil {
		return SiteUserData{}, err
	}
	body, err := a.client.getLimited(ctx, endpoint, a.headers.Clone(), maxProbeBodyBytes)
	if err != nil {
		return SiteUserData{}, err
	}
	if nexusPHPLooksUnauthenticated(body) {
		return SiteUserData{}, errors.New("站点 Cookie 已失效")
	}

	doc, err := goquery.NewDocumentFromReader(bytes.NewReader(body))
	if err != nil {
		return SiteUserData{}, fmt.Errorf("解析 NexusPHP 用户页面失败: %w", err)
	}

	data := newSiteUserData(a.base)
	nowText := time.Now().Format("15:04:05")
	data.UpdatedTime = &nowText

	infoBlock := doc.Find("#info_block")
	if infoBlock.Length() == 0 {
		infoBlock = doc.Selection
	}
	userLink := infoBlock.Find("a[href*='userdetails.php?id=']").First()
	if userLink.Length() > 0 {
		if username := strings.TrimSpace(userLink.Text()); username != "" {
			data.Username = &username
		}
		if uid := nexusPHPExtractID(userLink.AttrOr("href", "")); uid != "" {
			if n, convErr := strconv.Atoi(uid); convErr == nil {
				data.UserID = n
			} else {
				data.UserID = uid
			}
		}
		if cls := strings.TrimSpace(userLink.AttrOr("class", "")); cls != "" {
			level := strings.TrimSuffix(cls, "_Name")
			if level != "" {
				data.UserLevel = &level
			}
		}
	}

	text := strings.Join(strings.Fields(infoBlock.Text()), " ")
	if m := nexusPHPRatioPattern.FindStringSubmatch(text); len(m) > 1 {
		v := strings.ToLower(strings.TrimSpace(m[1]))
		if v == "∞" || v == "inf" || v == "无限" {
			data.Ratio = 0
		} else if parsed, parseErr := strconv.ParseFloat(v, 64); parseErr == nil {
			data.Ratio = parsed
		}
	}
	if m := nexusPHPUploadPattern.FindStringSubmatch(text); len(m) > 1 {
		data.Upload = parseSize(m[1])
	}
	if m := nexusPHPDownPattern.FindStringSubmatch(text); len(m) > 1 {
		data.Download = parseSize(m[1])
	}
	if m := nexusPHPBonusPattern.FindStringSubmatch(text); len(m) > 1 {
		clean := strings.ReplaceAll(m[1], ",", "")
		if parsed, parseErr := strconv.ParseFloat(clean, 64); parseErr == nil {
			data.Bonus = parsed
		}
	}

	if seedIcon := infoBlock.Find("img.arrowup").First(); seedIcon.Length() > 0 {
		if parentText := strings.TrimSpace(seedIcon.Parent().Text()); parentText != "" {
			if nums := nexusPHPIntPattern.FindAllString(parentText, 2); len(nums) >= 1 {
				if v, convErr := strconv.ParseInt(nums[0], 10, 64); convErr == nil {
					data.Seeding = v
				}
				if len(nums) >= 2 {
					if v, convErr := strconv.ParseInt(nums[1], 10, 64); convErr == nil {
						data.Leeching = v
					}
				}
			}
		}
	}
	if msgLink := infoBlock.Find("a[href*='messages.php']").First(); msgLink.Length() > 0 {
		if m := nexusPHPIntPattern.FindString(msgLink.Text()); m != "" {
			if v, convErr := strconv.Atoi(m); convErr == nil {
				data.MessageUnread = v
			}
		}
	}

	return data, nil
}

// RSSCursorKey 返回不暴露凭据的 RSS 游标键；未配置 RSS 时返回空串。
func (a *NexusPHPAdapter) RSSCursorKey() string {
	if a == nil || strings.TrimSpace(a.rssPath) == "" {
		return ""
	}
	digest := sha256.Sum256([]byte("moviepilot:nexusphp-rss:v1\x00" + a.base.String() + "\x00" + a.rssPath))
	return "sha256:" + hex.EncodeToString(digest[:])
}

// FetchRSS 拉取 NexusPHP 站点的 RSS 订阅流。
func (a *NexusPHPAdapter) FetchRSS(ctx context.Context, request RSSRequest) (RSSResponse, error) {
	if strings.TrimSpace(a.rssPath) == "" {
		return RSSResponse{}, errors.New("NexusPHP 未配置 RSS 地址")
	}
	endpoint, err := resolveSiteURL(a.base, a.rssPath)
	if err != nil {
		return RSSResponse{}, err
	}
	headers := a.headers.Clone()
	if strings.TrimSpace(request.ETag) != "" {
		headers.Set("If-None-Match", strings.TrimSpace(request.ETag))
	}
	if strings.TrimSpace(request.LastModified) != "" {
		headers.Set("If-Modified-Since", strings.TrimSpace(request.LastModified))
	}
	resp, err := a.client.getResponseLimited(ctx, endpoint, headers, a.client.maxBodyBytes)
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
	feed, err := gofeed.NewParser().Parse(bytes.NewReader(resp.Body))
	if err != nil {
		return RSSResponse{}, fmt.Errorf("解析 NexusPHP RSS 失败: %w", err)
	}
	limit := request.Limit
	if limit <= 0 || limit > nexusPHPMaxLimit {
		limit = nexusPHPMaxLimit
	}
	items := make([]RSSItem, 0, min(limit, len(feed.Items)))
	for _, entry := range feed.Items {
		if len(items) >= limit {
			break
		}
		if entry == nil {
			continue
		}
		title := strings.TrimSpace(entry.Title)
		if title == "" {
			continue
		}
		enclosureURL := ""
		var sizeBytes int64
		for _, enc := range entry.Enclosures {
			if enc != nil && strings.TrimSpace(enc.URL) != "" {
				enclosureURL = strings.TrimSpace(enc.URL)
				if parsedSize, convErr := strconv.ParseInt(strings.TrimSpace(enc.Length), 10, 64); convErr == nil && parsedSize > 0 {
					sizeBytes = parsedSize
				}
				break
			}
		}
		if enclosureURL == "" {
			enclosureURL = strings.TrimSpace(entry.Link)
		}
		if enclosureURL == "" {
			continue
		}
		if _, err := resolveSiteURL(a.base, enclosureURL); err != nil {
			continue
		}
		downloadID := base64.RawURLEncoding.EncodeToString([]byte(enclosureURL))
		detailURL := strings.TrimSpace(entry.Link)
		published := time.Time{}
		if entry.PublishedParsed != nil {
			published = entry.PublishedParsed.UTC()
		} else if entry.UpdatedParsed != nil {
			published = entry.UpdatedParsed.UTC()
		}
		itemKey := RSSItemKey(a.id, entry.GUID, detailURL, enclosureURL, title, published, sizeBytes)
		items = append(items, RSSItem{
			ItemKey: itemKey,
			GUID:    strings.TrimSpace(entry.GUID),
			Resource: Resource{
				SiteID:      a.id,
				Site:        a.name,
				ItemKey:     itemKey,
				Title:       title,
				Description: strings.TrimSpace(entry.Description),
				DetailURL:   detailURL,
				DownloadURL: fmt.Sprintf("/api/v1/resource/download/%d/%s", a.id, downloadID),
				TorrentID:   nexusPHPExtractID(detailURL),
				DownloadID:  downloadID,
				SizeBytes:   sizeBytes,
				PublishedAt: published,
			},
		})
	}
	return RSSResponse{
		ETag:         resp.ETag,
		LastModified: resp.LastModified,
		Items:        items,
	}, nil
}

func nexusPHPLooksUnauthenticated(body []byte) bool {
	lower := strings.ToLower(string(body))
	if strings.Contains(lower, "action=\"takelogin.php\"") || strings.Contains(lower, "action='takelogin.php'") {
		return true
	}
	if strings.Contains(lower, "name=\"username\"") && strings.Contains(lower, "name=\"password\"") && !strings.Contains(lower, "table class=\"torrents\"") && !strings.Contains(lower, "id=\"info_block\"") {
		return true
	}
	return false
}

func nexusPHPExtractID(rawURL string) string {
	parsed, err := url.Parse(strings.TrimSpace(rawURL))
	if err != nil {
		return ""
	}
	id := strings.TrimSpace(parsed.Query().Get("id"))
	if validNumericID(id) {
		return id
	}
	return ""
}

func nexusPHPExtractDescription(row, detailLink *goquery.Selection) string {
	// 优先取常见副标题元素，避免把整个单元格标签拼进描述。
	for _, selector := range []string{".torrent-subtitle", ".subtitle", "table.torrentname td.embedded"} {
		if sel := row.Find(selector).First(); sel.Length() > 0 {
			if selector == "table.torrentname td.embedded" {
				// 去掉链接与徽章，仅取最后一个 <br> 后的文本节点。
				clone := sel.Clone()
				clone.Find("a, img, span.tags, span.label, font.tag").Remove()
				text := strings.Join(strings.Fields(clone.Text()), " ")
				if text != "" {
					return text
				}
			} else if text := strings.Join(strings.Fields(sel.Text()), " "); text != "" {
				return text
			}
		}
	}
	if detailLink != nil {
		parent := detailLink.Parent()
		if parent.Length() > 0 {
			clone := parent.Clone()
			clone.Find("a, img, span, font").Remove()
			if text := strings.Join(strings.Fields(clone.Text()), " "); text != "" {
				return text
			}
		}
	}
	return ""
}

func nexusPHPExtractPromo(row *goquery.Selection) (float64, float64) {
	down, up := 1.0, 1.0
	if row.Find("img.pro_free2up, span.pro_free2up, .free2up").Length() > 0 {
		return 0, 2
	}
	if row.Find("img.pro_free, span.pro_free, .free").Length() > 0 {
		return 0, 1
	}
	if row.Find("img.pro_50pctdown2up, span.pro_50pctdown2up").Length() > 0 {
		return 0.5, 2
	}
	if row.Find("img.pro_50pctdown, span.pro_50pctdown").Length() > 0 {
		return 0.5, 1
	}
	if row.Find("img.pro_30pctdown, span.pro_30pctdown").Length() > 0 {
		return 0.3, 1
	}
	if row.Find("img.pro_2up, span.pro_2up").Length() > 0 {
		return 1, 2
	}
	row.Find("img").EachWithBreak(func(_ int, img *goquery.Selection) bool {
		alt := strings.ToLower(strings.TrimSpace(img.AttrOr("alt", "") + " " + img.AttrOr("title", "")))
		switch {
		case strings.Contains(alt, "2x free") || strings.Contains(alt, "2xfree"):
			down, up = 0, 2
			return false
		case strings.Contains(alt, "free"):
			down, up = 0, 1
			return false
		case strings.Contains(alt, "50%") && strings.Contains(alt, "2x"):
			down, up = 0.5, 2
			return false
		case strings.Contains(alt, "50%"):
			down, up = 0.5, 1
			return false
		case strings.Contains(alt, "30%"):
			down, up = 0.3, 1
			return false
		case strings.Contains(alt, "2x"):
			down, up = 1, 2
			return false
		}
		return true
	})
	return down, up
}

func nexusPHPExtractLabels(row *goquery.Selection) []string {
	seen := make(map[string]struct{})
	var labels []string
	row.Find("span.tags, span.label, span.tag, font.tag").Each(func(_ int, sel *goquery.Selection) {
		text := strings.TrimSpace(sel.Text())
		if text == "" || len(text) > 32 {
			return
		}
		if _, dup := seen[text]; !dup {
			seen[text] = struct{}{}
			labels = append(labels, text)
		}
	})
	return labels
}

func nexusPHPExtractIMDb(row *goquery.Selection) string {
	imdb := ""
	row.Find("a[href*='imdb.com']").EachWithBreak(func(_ int, a *goquery.Selection) bool {
		if m := nexusPHPIMDbPattern.FindStringSubmatch(a.AttrOr("href", "")); len(m) > 1 {
			imdb = strings.ToLower(m[1])
			return false
		}
		return true
	})
	if imdb == "" {
		if m := nexusPHPIMDbPattern.FindStringSubmatch(row.Text()); len(m) > 1 {
			imdb = strings.ToLower(m[1])
		}
	}
	return imdb
}

func nexusPHPDetectHR(row *goquery.Selection) bool {
	if row.Find("img.hitandrun, span.hitandrun, .hr").Length() > 0 {
		return true
	}
	found := false
	row.Find("img, span").EachWithBreak(func(_ int, s *goquery.Selection) bool {
		hint := strings.ToLower(s.AttrOr("title", "") + " " + s.AttrOr("alt", ""))
		if strings.Contains(hint, "hit and run") || strings.Contains(hint, "h&r") {
			found = true
			return false
		}
		return true
	})
	return found
}

func nexusPHPExtractCategory(row *goquery.Selection) string {
	img := row.ChildrenFiltered("td").First().Find("img").First()
	if img.Length() > 0 {
		if alt := strings.TrimSpace(img.AttrOr("alt", "")); alt != "" {
			return alt
		}
		if title := strings.TrimSpace(img.AttrOr("title", "")); title != "" {
			return title
		}
	}
	return ""
}

func nexusPHPExtractRowMetrics(row *goquery.Selection) (sizeBytes int64, seeders, peers, grabs int, publishedAt time.Time) {
	cells := row.ChildrenFiltered("td")
	if cells.Length() < 4 {
		return
	}
	sizeIdx := -1
	cells.Each(func(i int, cell *goquery.Selection) {
		if i < 2 {
			return
		}
		if publishedAt.IsZero() {
			if span := cell.Find("span[title]").First(); span.Length() > 0 {
				if t := parseTime(span.AttrOr("title", "")); !t.IsZero() {
					publishedAt = t
				}
			}
			if publishedAt.IsZero() {
				if m := nexusPHPDateTimeRegexp.FindString(cell.Text()); m != "" {
					if t := parseTime(m); !t.IsZero() {
						publishedAt = t
					}
				}
			}
		}
		if sizeIdx < 0 {
			raw := strings.Join(strings.Fields(cell.Text()), " ")
			if nexusPHPSizeCellRegexp.MatchString(raw) {
				if parsed := parseSize(raw); parsed > 0 {
					sizeBytes = parsed
					sizeIdx = i
				}
			}
		}
	})
	// 标准 NexusPHP 列表在体积列之后依次为：做种数、下载数、完成数。
	if sizeIdx >= 0 {
		if sizeIdx+1 < cells.Length() {
			seeders = parseCommaInt(cells.Eq(sizeIdx + 1).Text())
		}
		if sizeIdx+2 < cells.Length() {
			peers = parseCommaInt(cells.Eq(sizeIdx + 2).Text())
		}
		if sizeIdx+3 < cells.Length() {
			grabs = parseCommaInt(cells.Eq(sizeIdx + 3).Text())
		}
	}
	return
}

func parseCommaInt(raw string) int {
	clean := strings.ReplaceAll(strings.TrimSpace(raw), ",", "")
	if m := nexusPHPIntPattern.FindString(clean); m != "" {
		if v, err := strconv.Atoi(m); err == nil {
			return v
		}
	}
	return 0
}
