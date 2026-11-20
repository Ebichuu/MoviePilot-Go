package indexer

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/PuerkitoBio/goquery"
	"github.com/mmcdole/gofeed"
)

// Field 描述 HTML CSS 选择器或 JSON 路径；Attribute 非空时读取 HTML 属性。
type Field struct {
	Selector  string `json:"selector"`
	Attribute string `json:"attribute,omitempty"`
	Template  string `json:"template,omitempty"`
}

// Fields 是统一资源模型的声明式字段映射。
type Fields struct {
	Title                Field `json:"title"`
	Description          Field `json:"description"`
	DetailURL            Field `json:"detail_url"`
	DownloadURL          Field `json:"download_url"`
	Size                 Field `json:"size"`
	Seeders              Field `json:"seeders"`
	Peers                Field `json:"peers"`
	Grabs                Field `json:"grabs"`
	PublishedAt          Field `json:"published_at"`
	IMDBID               Field `json:"imdbid"`
	Labels               Field `json:"labels"`
	Category             Field `json:"category"`
	FreeDate             Field `json:"freedate"`
	DownloadVolumeFactor Field `json:"downloadvolumefactor"`
	UploadVolumeFactor   Field `json:"uploadvolumefactor"`
	HitAndRun            Field `json:"hit_and_run"`
	ResourceID           Field `json:"resource_id"`
}

// Definition 是可保存到 sites.config 的站点适配定义。
type Definition struct {
	ID             int               `json:"id"`
	Name           string            `json:"name"`
	BaseURL        string            `json:"base_url"`
	Format         string            `json:"format"`
	SearchPath     string            `json:"search_path"`
	RSSPath        string            `json:"rss_path,omitempty"`
	SigninPath     string            `json:"signin_path,omitempty"`
	SigninUserKey  string            `json:"signin_username_field,omitempty"`
	SigninPassKey  string            `json:"signin_password_field,omitempty"`
	SigninParams   map[string]string `json:"signin_params,omitempty"`
	Method         string            `json:"method"`
	QueryParam     string            `json:"query_param"`
	PageParam      string            `json:"page_param"`
	PageOffset     int               `json:"page_offset"`
	LimitParam     string            `json:"limit_param"`
	ResultSelector string            `json:"result_selector"`
	ResultPath     string            `json:"result_path"`
	Params         map[string]string `json:"params"`
	Body           map[string]any    `json:"body"`
	Headers        map[string]string `json:"headers"`
	Cookie         string            `json:"cookie"`
	UserAgent      string            `json:"user_agent"`
	CSRFPath       string            `json:"csrf_path"`
	CSRFRegex      string            `json:"csrf_regex"`
	CSRFHeader     string            `json:"csrf_header"`
	Fields         Fields            `json:"fields"`
}

// DeclarativeAdapter 使用同一套资源模型解析 HTML、RSS 或 JSON 站点。
type DeclarativeAdapter struct {
	client      *HTTPClient
	base        *url.URL
	def         Definition
	csrfMu      sync.Mutex
	csrfToken   string
	csrfExpiry  time.Time
	csrfPattern *regexp.Regexp
}

// RSSNoCache reports the V2 m-team cache policy for declarative site configs.
// The method is optional at the RSS source boundary and does not alter search
// or download behavior for non-RSS declarative adapters.
func (a *DeclarativeAdapter) RSSNoCache() bool {
	return a != nil && rssDomainUsesNoCacheKey(a.base)
}

// NewAdapterFromConfig 将 sites.config 转成声明式适配器；旧 Python parser 会明确拒绝。
func NewAdapterFromConfig(client *HTTPClient, id int, name, baseURL, adapter string, rawConfig map[string]any) (Adapter, error) {
	encoded, err := json.Marshal(rawConfig)
	if err != nil {
		return nil, errors.New("站点配置无法编码")
	}
	if len(encoded) > 256<<10 {
		return nil, errors.New("站点配置超过 256 KiB 限制")
	}
	var definition Definition
	if len(encoded) > 0 {
		if err := json.Unmarshal(encoded, &definition); err != nil {
			return nil, errors.New("站点配置格式无效")
		}
	}
	if definition.RSSPath == "" {
		if value, ok := rawConfig["rss"].(string); ok {
			definition.RSSPath = strings.TrimSpace(value)
		}
	}
	definition.ID, definition.Name, definition.BaseURL = id, strings.TrimSpace(name), strings.TrimSpace(baseURL)
	kind := strings.ToLower(strings.TrimSpace(adapter))
	switch kind {
	case "yema", "yemaspider":
		return NewYemaAdapter(client, id, name, baseURL, rawConfig)
	case "tnode", "tnodespider":
		return NewTNodeAdapter(client, id, name, baseURL, rawConfig)
	case "torrentleech", "torrentleechspider":
		return NewTorrentLeechAdapter(client, id, name, baseURL, rawConfig)
	case "mtorrent", "mtorrentspider", "mteam":
		return NewMTorrentAdapter(client, id, name, baseURL, rawConfig)
	case "sunnypt", "sunnyptspider":
		return NewSunnyPTAdapter(client, id, name, baseURL, rawConfig)
	case "rousi", "rousispider", "rousipro":
		return NewRousiAdapter(client, id, name, baseURL, rawConfig)
	case "haidan", "haidanspider":
		return NewHaidanAdapter(client, id, name, baseURL, rawConfig)
	case "hddolby", "hddolbyspider":
		return NewHDDolbyAdapter(client, id, name, baseURL, rawConfig)
	case "html", "goquery", "declarative-html":
		definition.Format = "html"
	case "rss", "gofeed", "declarative-rss":
		definition.Format = "rss"
	case "json", "declarative-json":
		definition.Format = "json"
	case "declarative":
		if definition.Format == "" {
			return nil, errors.New("declarative 站点必须设置 format")
		}
	default:
		return nil, fmt.Errorf("暂不支持站点 parser: %s", adapter)
	}
	declarative, err := NewDeclarativeAdapter(client, definition)
	if err != nil {
		return nil, err
	}
	// 站点同时配置 RSS 地址时（V2 里一个站点既支持搜索又支持 RSS），
	// 统一挂上 RSS 游标与下载解析能力；未配置 RSS 的 HTML/JSON 站点不claim该能力。
	if strings.EqualFold(declarative.def.Format, "rss") || strings.TrimSpace(declarative.def.RSSPath) != "" {
		return &declarativeRSSAdapter{DeclarativeAdapter: declarative}, nil
	}
	return declarative, nil
}

// NewDeclarativeAdapter 校验定义并创建适配器。
func NewDeclarativeAdapter(client *HTTPClient, definition Definition) (*DeclarativeAdapter, error) {
	if client == nil {
		return nil, errors.New("站点 HTTP 客户端不能为空")
	}
	definition.Name = strings.TrimSpace(definition.Name)
	if definition.Name == "" {
		return nil, errors.New("站点名称不能为空")
	}
	base, err := validateBaseURL(definition.BaseURL, client.allowPrivate)
	if err != nil {
		return nil, err
	}
	definition.Format = strings.ToLower(strings.TrimSpace(definition.Format))
	if definition.Format == "" {
		definition.Format = "html"
	}
	if definition.Format != "html" && definition.Format != "rss" && definition.Format != "json" {
		return nil, errors.New("站点格式必须是 html、rss 或 json")
	}
	definition.Method = strings.ToUpper(strings.TrimSpace(definition.Method))
	if definition.Method == "" {
		definition.Method = http.MethodGet
	}
	if definition.Method != http.MethodGet && definition.Method != http.MethodPost {
		return nil, errors.New("声明式站点只支持 GET 或 POST")
	}
	if definition.SearchPath == "" {
		definition.SearchPath = "/"
	}
	if definition.Format == "rss" && definition.RSSPath == "" {
		definition.RSSPath = definition.SearchPath
	}
	if definition.Method == http.MethodGet {
		if definition.QueryParam == "" {
			definition.QueryParam = "q"
		}
		if definition.PageParam == "" {
			definition.PageParam = "page"
		}
	}
	if definition.Format == "html" && definition.ResultSelector == "" {
		return nil, errors.New("HTML 站点必须设置 result_selector")
	}
	if definition.Format == "json" && definition.ResultPath == "" {
		return nil, errors.New("JSON 站点必须设置 result_path")
	}
	if err := validateHeaders(definition.Headers); err != nil {
		return nil, err
	}
	if len(definition.Cookie) > 64<<10 || len(definition.UserAgent) > 1024 {
		return nil, errors.New("站点 Cookie 或 User-Agent 超过长度限制")
	}
	if definition.Method == http.MethodPost && definition.Body == nil {
		return nil, errors.New("POST 站点必须设置 body")
	}
	if definition.CSRFPath != "" {
		if definition.CSRFRegex == "" {
			return nil, errors.New("设置 csrf_path 时必须设置 csrf_regex")
		}
		if definition.CSRFHeader == "" {
			definition.CSRFHeader = "X-CSRF-TOKEN"
		}
		if strings.ContainsAny(definition.CSRFHeader, "\r\n") || strings.EqualFold(definition.CSRFHeader, "Host") {
			return nil, errors.New("CSRF 请求头无效")
		}
		pattern, patternErr := regexp.Compile(definition.CSRFRegex)
		if patternErr != nil {
			return nil, errors.New("CSRF 正则无效")
		}
		if pattern.NumSubexp() < 1 {
			return nil, errors.New("CSRF 正则必须包含捕获组")
		}
		return &DeclarativeAdapter{client: client, base: base, def: definition, csrfPattern: pattern}, nil
	}
	if definition.CSRFRegex != "" {
		return nil, errors.New("设置 csrf_regex 时必须同时设置 csrf_path")
	}
	return &DeclarativeAdapter{client: client, base: base, def: definition}, nil
}

func (a *DeclarativeAdapter) Name() string { return a.def.Name }
func (a *DeclarativeAdapter) SiteID() int  { return a.def.ID }

// Search 执行单页搜索并把结果限制在调用方给出的上限内。
func (a *DeclarativeAdapter) Search(ctx context.Context, query Query) ([]Resource, error) {
	keyword := strings.TrimSpace(query.Keyword)
	if len([]rune(keyword)) > 512 {
		return nil, errors.New("站点搜索关键词不能超过 512 个字符")
	}
	if query.Page < 0 {
		query.Page = 0
	}
	if query.Limit < 1 || query.Limit > 200 {
		query.Limit = 100
	}
	endpoint, err := a.endpoint(query)
	if err != nil {
		return nil, err
	}
	headers := make(http.Header, len(a.def.Headers)+2)
	for key, value := range a.def.Headers {
		headers.Set(key, value)
	}
	if a.def.Cookie != "" {
		headers.Set("Cookie", a.def.Cookie)
	}
	if a.def.UserAgent != "" {
		headers.Set("User-Agent", a.def.UserAgent)
	}
	var body []byte
	if a.def.Method == http.MethodPost {
		templateQuery := query
		templateQuery.Page += a.def.PageOffset
		payload, renderErr := renderBody(a.def.Body, templateQuery, 0)
		if renderErr != nil {
			return nil, renderErr
		}
		token, tokenErr := a.csrf(ctx, headers)
		if tokenErr != nil {
			return nil, tokenErr
		}
		if token != "" {
			headers.Set(a.def.CSRFHeader, token)
		}
		body, err = a.client.postJSON(ctx, endpoint, headers, payload)
		if token != "" && csrfRefreshRequired(err) {
			a.invalidateCSRF(token)
			token, tokenErr = a.csrf(ctx, headers)
			if tokenErr != nil {
				return nil, tokenErr
			}
			headers.Set(a.def.CSRFHeader, token)
			body, err = a.client.postJSON(ctx, endpoint, headers, payload)
		}
	} else {
		body, err = a.client.get(ctx, endpoint, headers)
	}
	if err != nil {
		return nil, err
	}
	switch a.def.Format {
	case "html":
		return a.parseHTML(body, query.Limit)
	case "rss":
		return a.parseRSS(ctx, body, query.Limit)
	case "json":
		return a.parseJSON(body, query.Limit)
	default:
		return nil, errors.New("未知站点响应格式")
	}
}

func (a *DeclarativeAdapter) csrf(ctx context.Context, headers http.Header) (string, error) {
	if a.def.CSRFPath == "" {
		return "", nil
	}
	a.csrfMu.Lock()
	defer a.csrfMu.Unlock()
	if a.csrfToken != "" && time.Now().Before(a.csrfExpiry) {
		return a.csrfToken, nil
	}
	endpoint, err := resolveSiteURL(a.base, a.def.CSRFPath)
	if err != nil {
		return "", err
	}
	tokenHeaders := headers.Clone()
	tokenHeaders.Del(a.def.CSRFHeader)
	body, err := a.client.getLimited(ctx, endpoint, tokenHeaders, 1<<20)
	if err != nil {
		return "", err
	}
	match := a.csrfPattern.FindSubmatch(body)
	if len(match) < 2 || len(match[1]) == 0 || len(match[1]) > 4096 {
		return "", errors.New("站点 CSRF token 缺失")
	}
	token := string(match[1])
	a.csrfToken, a.csrfExpiry = token, time.Now().Add(time.Hour)
	return token, nil
}

func (a *DeclarativeAdapter) invalidateCSRF(token string) {
	a.csrfMu.Lock()
	defer a.csrfMu.Unlock()
	if a.csrfToken == token {
		a.csrfToken = ""
		a.csrfExpiry = time.Time{}
	}
}

func csrfRefreshRequired(err error) bool {
	var statusError *HTTPStatusError
	return errors.As(err, &statusError) && (statusError.StatusCode == http.StatusUnauthorized || statusError.StatusCode == http.StatusForbidden)
}

func renderBody(value any, query Query, depth int) (any, error) {
	if depth > 16 {
		return nil, errors.New("站点请求模板嵌套过深")
	}
	switch typed := value.(type) {
	case map[string]any:
		result := make(map[string]any, len(typed))
		for key, item := range typed {
			if strings.TrimSpace(key) == "" || strings.ContainsAny(key, "\r\n") {
				return nil, errors.New("站点请求模板字段名无效")
			}
			rendered, err := renderBody(item, query, depth+1)
			if err != nil {
				return nil, err
			}
			result[key] = rendered
		}
		return result, nil
	case []any:
		result := make([]any, len(typed))
		for index, item := range typed {
			rendered, err := renderBody(item, query, depth+1)
			if err != nil {
				return nil, err
			}
			result[index] = rendered
		}
		return result, nil
	case string:
		switch typed {
		case "${keyword}":
			return query.Keyword, nil
		case "${page}":
			return query.Page, nil
		case "${limit}":
			return query.Limit, nil
		case "${search_type}":
			if strings.HasPrefix(strings.ToLower(strings.TrimSpace(query.Keyword)), "tt") {
				return "imdbid", nil
			}
			return "title", nil
		}
		return strings.NewReplacer(
			"${keyword}", query.Keyword,
			"${page}", strconv.Itoa(query.Page),
			"${limit}", strconv.Itoa(query.Limit),
		).Replace(typed), nil
	default:
		return value, nil
	}
}

func (a *DeclarativeAdapter) endpoint(query Query) (string, error) {
	endpoint, err := resolveSiteURL(a.base, a.def.SearchPath)
	if err != nil {
		return "", err
	}
	parsed, err := url.Parse(endpoint)
	if err != nil {
		return "", errors.New("站点搜索地址无效")
	}
	values := parsed.Query()
	for key, value := range a.def.Params {
		values.Set(key, value)
	}
	if a.def.QueryParam != "" && query.Keyword != "" {
		values.Set(a.def.QueryParam, query.Keyword)
	}
	if a.def.PageParam != "" {
		values.Set(a.def.PageParam, strconv.Itoa(query.Page+a.def.PageOffset))
	}
	if a.def.LimitParam != "" {
		values.Set(a.def.LimitParam, strconv.Itoa(query.Limit))
	}
	parsed.RawQuery = values.Encode()
	return resolveSiteURL(a.base, parsed.String())
}

func (a *DeclarativeAdapter) parseHTML(body []byte, limit int) ([]Resource, error) {
	document, err := goquery.NewDocumentFromReader(bytes.NewReader(body))
	if err != nil {
		return nil, errors.New("解析站点 HTML 失败")
	}
	resources := make([]Resource, 0, min(limit, 32))
	document.Find(a.def.ResultSelector).EachWithBreak(func(_ int, row *goquery.Selection) bool {
		resource, parseErr := a.htmlResource(row)
		if parseErr == nil && resource.Title != "" {
			resources = append(resources, resource)
		}
		return len(resources) < limit
	})
	return resources, nil
}

func (a *DeclarativeAdapter) htmlResource(row *goquery.Selection) (Resource, error) {
	resource := Resource{
		SiteID: a.def.ID, Title: htmlField(row, a.def.Fields.Title),
		Description:          htmlField(row, a.def.Fields.Description),
		DetailURL:            a.resolveFieldURL(row, a.def.Fields.DetailURL),
		DownloadURL:          a.resolveFieldURL(row, a.def.Fields.DownloadURL),
		SizeBytes:            parseSize(htmlField(row, a.def.Fields.Size)),
		Seeders:              parseInt(htmlField(row, a.def.Fields.Seeders)),
		Peers:                parseInt(htmlField(row, a.def.Fields.Peers)),
		Grabs:                parseInt(htmlField(row, a.def.Fields.Grabs)),
		PublishedAt:          parseTime(htmlField(row, a.def.Fields.PublishedAt)),
		IMDBID:               htmlField(row, a.def.Fields.IMDBID),
		Labels:               splitLabels(htmlField(row, a.def.Fields.Labels)),
		Category:             htmlField(row, a.def.Fields.Category),
		FreeDate:             parseTime(htmlField(row, a.def.Fields.FreeDate)),
		DownloadVolumeFactor: parseOptionalFloat(htmlField(row, a.def.Fields.DownloadVolumeFactor)),
		UploadVolumeFactor:   parseOptionalFloat(htmlField(row, a.def.Fields.UploadVolumeFactor)),
		HitAndRun:            parseBool(htmlField(row, a.def.Fields.HitAndRun)),
	}
	// V2 资源必须带可下载定位符，否则前端拿不到 enclosure、下载入口判为「下载来源无效」：
	// 先取资源 ID 字段，再回落到下载地址，最后回落到详情地址。
	resource.DownloadID = strings.TrimSpace(htmlField(row, a.def.Fields.ResourceID))
	if resource.DownloadID == "" {
		resource.DownloadID = resource.DownloadURL
	}
	if resource.DownloadID == "" {
		resource.DownloadID = resource.DetailURL
	}
	return resource, nil
}

func (a *DeclarativeAdapter) resolveFieldURL(row *goquery.Selection, field Field) string {
	raw := htmlField(row, field)
	resolved, _ := resolveSiteURL(a.base, raw)
	return resolved
}

func htmlField(row *goquery.Selection, field Field) string {
	if field.Selector == "" {
		return ""
	}
	target := row
	if field.Selector != ":scope" {
		target = row.Find(field.Selector).First()
	}
	if field.Attribute != "" {
		value, _ := target.Attr(field.Attribute)
		return strings.TrimSpace(value)
	}
	return strings.TrimSpace(target.Text())
}

func (a *DeclarativeAdapter) parseRSS(ctx context.Context, body []byte, limit int) ([]Resource, error) {
	items, err := a.parseRSSItems(ctx, body, limit)
	if err != nil {
		return nil, err
	}
	resources := make([]Resource, 0, len(items))
	for _, item := range items {
		resources = append(resources, item.Resource)
	}
	return resources, nil
}

func (a *DeclarativeAdapter) parseRSSItems(ctx context.Context, body []byte, limit int) ([]RSSItem, error) {
	parser := gofeed.NewParser()
	feed, err := parser.ParseString(string(body))
	if err != nil {
		return nil, errors.New("解析站点 RSS 失败")
	}
	items := make([]RSSItem, 0, min(limit, len(feed.Items)))
	for _, item := range feed.Items {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		resource := Resource{SiteID: a.def.ID, Title: strings.TrimSpace(item.Title), Description: strings.TrimSpace(item.Description)}
		if resource.Title == "" {
			continue
		}
		resource.DetailURL, _ = resolveSiteURL(a.base, item.Link)
		if len(item.Enclosures) > 0 && item.Enclosures[0] != nil {
			resource.DownloadURL, _ = resolveSiteURL(a.base, item.Enclosures[0].URL)
			resource.SizeBytes = parseSize(item.Enclosures[0].Length)
		}
		if item.PublishedParsed != nil {
			resource.PublishedAt = item.PublishedParsed.UTC()
		}
		if resource.DownloadURL != "" {
			resource.DownloadID = resource.DownloadURL
		} else {
			resource.DownloadID = resource.DetailURL
		}
		resource.ItemKey = RSSItemKey(a.def.ID, item.GUID, resource.DetailURL, resource.DownloadURL, resource.Title, resource.PublishedAt, resource.SizeBytes)
		if resource.ItemKey == "" {
			continue
		}
		if len(items) == limit {
			break
		}
		items = append(items, RSSItem{ItemKey: resource.ItemKey, GUID: strings.TrimSpace(item.GUID), Resource: resource})
	}
	return items, nil
}

// FetchRSS performs a bounded conditional request for a declarative RSS feed.
func (a *DeclarativeAdapter) FetchRSS(ctx context.Context, request RSSRequest) (RSSResponse, error) {
	if a == nil {
		return RSSResponse{}, errors.New("站点不是 RSS 适配器")
	}
	if strings.TrimSpace(a.def.RSSPath) == "" {
		return RSSResponse{}, errors.New("站点未配置 RSS 地址")
	}
	limit := request.Limit
	if limit < 1 || limit > 200 {
		limit = 200
	}
	endpoint, err := resolveSiteURL(a.base, a.def.RSSPath)
	if err != nil {
		return RSSResponse{}, err
	}
	headers := make(http.Header, len(a.def.Headers)+4)
	for key, value := range a.def.Headers {
		headers.Set(key, value)
	}
	if a.def.Cookie != "" {
		headers.Set("Cookie", a.def.Cookie)
	}
	if a.def.UserAgent != "" {
		headers.Set("User-Agent", a.def.UserAgent)
	}
	if request.ETag != "" {
		headers.Set("If-None-Match", request.ETag)
	}
	if request.LastModified != "" {
		headers.Set("If-Modified-Since", request.LastModified)
	}
	response, err := a.client.getResponseLimited(ctx, endpoint, headers, a.client.maxBodyBytes)
	if err != nil {
		return RSSResponse{}, err
	}
	result := RSSResponse{ETag: response.ETag, LastModified: response.LastModified}
	if response.StatusCode == http.StatusNotModified {
		result.NotModified = true
		return result, nil
	}
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return RSSResponse{}, &HTTPStatusError{StatusCode: response.StatusCode}
	}
	result.Items, err = a.parseRSSItems(ctx, response.Body, limit)
	if err != nil {
		return RSSResponse{}, err
	}
	return result, nil
}

// TestSite 对声明式站点做一次只读连通性探测：优先请求 RSS 地址（带 passkey 时
// 同时验证凭据可用），未配置 RSS 时退化为站点根地址。探测不创建下载凭据、不改远端状态。
func (a *DeclarativeAdapter) TestSite(ctx context.Context) error {
	if a == nil || a.client == nil {
		return siteTestFailure("站点适配器未初始化")
	}
	target := strings.TrimSpace(a.def.RSSPath)
	if target == "" {
		target = "/"
	}
	endpoint, err := resolveSiteURL(a.base, target)
	if err != nil {
		return siteTestFailure("站点地址无效")
	}
	headers := make(http.Header, len(a.def.Headers)+2)
	for key, value := range a.def.Headers {
		headers.Set(key, value)
	}
	if a.def.Cookie != "" {
		headers.Set("Cookie", a.def.Cookie)
	}
	if a.def.UserAgent != "" {
		headers.Set("User-Agent", a.def.UserAgent)
	}
	response, err := a.client.getResponseLimited(ctx, endpoint, headers, maxProbeBodyBytes)
	if err != nil {
		return err
	}
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return &HTTPStatusError{StatusCode: response.StatusCode}
	}
	if len(bytes.TrimSpace(response.Body)) == 0 {
		return siteTestFailure("站点返回空响应")
	}
	return nil
}

// SignIn 用用户名/密码登录声明式站点，返回后续请求可用的 Cookie 串。
// 登录地址与字段名来自站点配置（signin_path / signin_username_field /
// signin_password_field / signin_params），未配置时如实拒绝而不是猜测。
func (a *DeclarativeAdapter) SignIn(ctx context.Context, username, password string) (string, error) {
	if a == nil || a.client == nil {
		return "", siteTestFailure("站点适配器未初始化")
	}
	path := strings.TrimSpace(a.def.SigninPath)
	if path == "" {
		return "", siteTestFailure("站点未配置登录地址（signin_path）")
	}
	endpoint, err := resolveSiteURL(a.base, path)
	if err != nil {
		return "", siteTestFailure("站点登录地址无效")
	}
	usernameField := strings.TrimSpace(a.def.SigninUserKey)
	if usernameField == "" {
		usernameField = "username"
	}
	passwordField := strings.TrimSpace(a.def.SigninPassKey)
	if passwordField == "" {
		passwordField = "password"
	}
	form := url.Values{}
	form.Set(usernameField, username)
	form.Set(passwordField, password)
	for key, value := range a.def.SigninParams {
		form.Set(key, value)
	}
	headers := make(http.Header, len(a.def.Headers)+1)
	for key, value := range a.def.Headers {
		headers.Set(key, value)
	}
	if a.def.UserAgent != "" {
		headers.Set("User-Agent", a.def.UserAgent)
	}
	cookies, status, err := a.client.signInForm(ctx, endpoint, headers, form)
	if err != nil {
		return "", err
	}
	if status >= 400 {
		return "", &HTTPStatusError{StatusCode: status}
	}
	pairs := make([]string, 0, len(cookies))
	for _, cookie := range cookies {
		if cookie == nil || strings.TrimSpace(cookie.Name) == "" {
			continue
		}
		pairs = append(pairs, cookie.Name+"="+cookie.Value)
	}
	if len(pairs) == 0 {
		return "", siteTestFailure("站点未返回登录 Cookie，请确认用户名密码或站点登录方式")
	}
	return strings.Join(pairs, "; "), nil
}

// declarativeRSSAdapter adds a same-host download resolver to adapters that
// declare an RSS endpoint. HTML/JSON adapters without rss_path keep the plain
// adapter and must not claim this capability.
type declarativeRSSAdapter struct {
	*DeclarativeAdapter
}

// rssCapable 报告该声明式站点是否配置了可轮询的 RSS 地址。
func (a *declarativeRSSAdapter) rssCapable() bool {
	return a != nil && a.DeclarativeAdapter != nil && strings.TrimSpace(a.def.RSSPath) != ""
}

func (a *declarativeRSSAdapter) RSSCursorKey() string {
	if !a.rssCapable() {
		return ""
	}
	endpoint, err := resolveSiteURL(a.base, a.def.RSSPath)
	if err != nil {
		return ""
	}
	digest := sha256.Sum256([]byte("moviepilot:rss-cursor:v1\x00" + strconv.Itoa(a.def.ID) + "\x00" + endpoint))
	return "sha256:" + hex.EncodeToString(digest[:])
}

func (a *declarativeRSSAdapter) ResolverKind() string { return "declarative-rss" }

func (a *declarativeRSSAdapter) ResolverVersion() int16 { return downloadResolverVersionV1 }

func (a *declarativeRSSAdapter) ResolveDownload(ctx context.Context, resourceID string) (ResolvedDownload, error) {
	if !a.rssCapable() {
		return ResolvedDownload{}, errors.New("站点下载解析器不可用")
	}
	endpoint, err := resolveSiteURL(a.base, resourceID)
	if err != nil {
		return ResolvedDownload{}, err
	}
	headers := make(http.Header, len(a.def.Headers)+2)
	for key, value := range a.def.Headers {
		headers.Set(key, value)
	}
	if a.def.Cookie != "" {
		headers.Set("Cookie", a.def.Cookie)
	}
	if a.def.UserAgent != "" {
		headers.Set("User-Agent", a.def.UserAgent)
	}
	body, err := a.client.getLimited(ctx, endpoint, headers, a.client.maxBodyBytes)
	if err != nil {
		return ResolvedDownload{}, err
	}
	return ResolvedDownload{Body: body, ContentType: "application/x-bittorrent"}, nil
}

func (a *DeclarativeAdapter) parseJSON(body []byte, limit int) ([]Resource, error) {
	decoder := json.NewDecoder(bytes.NewReader(body))
	decoder.UseNumber()
	var document any
	if err := decoder.Decode(&document); err != nil {
		return nil, errors.New("解析站点 JSON 失败")
	}
	if decoder.Decode(new(any)) != io.EOF {
		return nil, errors.New("站点 JSON 包含多余内容")
	}
	items, ok := lookupJSON(document, a.def.ResultPath).([]any)
	if !ok {
		return nil, errors.New("站点 JSON 结果路径不是数组")
	}
	resources := make([]Resource, 0, min(limit, len(items)))
	for _, item := range items {
		resource := Resource{SiteID: a.def.ID, Title: jsonFieldString(item, a.def.Fields.Title)}
		resource.Description = jsonFieldString(item, a.def.Fields.Description)
		resource.DetailURL = a.resolveJSONFieldURL(item, a.def.Fields.DetailURL)
		resource.DownloadURL = a.resolveJSONFieldURL(item, a.def.Fields.DownloadURL)
		resource.TorrentID = jsonFieldString(item, a.def.Fields.ResourceID)
		// JSON 站点同样要给出可下载定位符，否则前端拿不到 enclosure，
		// `/download` 会判成「下载来源无效」。顺序与 HTML 适配器保持一致。
		resource.DownloadID = resource.TorrentID
		if resource.DownloadID == "" {
			resource.DownloadID = resource.DownloadURL
		}
		if resource.DownloadID == "" {
			resource.DownloadID = resource.DetailURL
		}
		resource.SizeBytes = parseSize(jsonFieldString(item, a.def.Fields.Size))
		resource.Seeders = parseInt(jsonFieldString(item, a.def.Fields.Seeders))
		resource.Peers = parseInt(jsonFieldString(item, a.def.Fields.Peers))
		resource.Grabs = parseInt(jsonFieldString(item, a.def.Fields.Grabs))
		resource.PublishedAt = parseTime(jsonFieldString(item, a.def.Fields.PublishedAt))
		resource.IMDBID = jsonFieldString(item, a.def.Fields.IMDBID)
		resource.Labels = jsonFieldStrings(item, a.def.Fields.Labels)
		resource.Category = jsonFieldString(item, a.def.Fields.Category)
		resource.FreeDate = parseTime(jsonFieldString(item, a.def.Fields.FreeDate))
		resource.DownloadVolumeFactor = parseOptionalFloat(jsonFieldString(item, a.def.Fields.DownloadVolumeFactor))
		resource.UploadVolumeFactor = parseOptionalFloat(jsonFieldString(item, a.def.Fields.UploadVolumeFactor))
		resource.HitAndRun = parseBool(jsonFieldString(item, a.def.Fields.HitAndRun))
		if resource.Title == "" {
			continue
		}
		resources = append(resources, resource)
		if len(resources) == limit {
			break
		}
	}
	return resources, nil
}

func (a *DeclarativeAdapter) resolveJSONFieldURL(item any, field Field) string {
	raw := jsonFieldString(item, field)
	if field.Template != "" {
		raw = strings.ReplaceAll(field.Template, "${value}", url.PathEscape(raw))
	}
	resolved, _ := resolveSiteURL(a.base, raw)
	return resolved
}

func lookupJSON(value any, path string) any {
	for _, part := range strings.Split(strings.Trim(path, "."), ".") {
		if part == "" {
			continue
		}
		if index, err := strconv.Atoi(part); err == nil {
			items, ok := value.([]any)
			if !ok || index < 0 || index >= len(items) {
				return nil
			}
			value = items[index]
			continue
		}
		object, ok := value.(map[string]any)
		if !ok {
			return nil
		}
		value = object[part]
	}
	return value
}

func jsonFieldString(item any, field Field) string {
	if field.Selector == "" {
		return ""
	}
	value := lookupJSON(item, field.Selector)
	switch typed := value.(type) {
	case nil:
		return ""
	case string:
		return strings.TrimSpace(typed)
	case json.Number:
		return typed.String()
	default:
		return strings.TrimSpace(fmt.Sprint(typed))
	}
}

func jsonFieldStrings(item any, field Field) []string {
	if field.Selector == "" {
		return nil
	}
	value := lookupJSON(item, field.Selector)
	items, ok := value.([]any)
	if !ok {
		return splitLabels(jsonFieldString(item, field))
	}
	result := make([]string, 0, len(items))
	for _, item := range items {
		label := strings.TrimSpace(fmt.Sprint(item))
		if label != "" && len(result) < 64 {
			result = append(result, label)
		}
	}
	return result
}

// sizeUnitSeparator 把 "2.69GB" 这类紧贴写法拆成 "2.69 GB" 供体积解析使用。
var sizeUnitSeparator = regexp.MustCompile(`(?i)([0-9])([a-z])`)

func parseSize(value string) int64 {
	// NexusPHP 一类站点会把「2.69<br />GB」渲染成 "2.69GB"，数字与单位之间没有空格，
	// 先拆出单位再解析，避免整行体积被当成 0。
	normalized := sizeUnitSeparator.ReplaceAllString(strings.ReplaceAll(strings.TrimSpace(value), ",", ""), "$1 $2")
	parts := strings.Fields(normalized)
	if len(parts) == 0 {
		return 0
	}
	number, err := strconv.ParseFloat(parts[0], 64)
	if err != nil || number < 0 {
		return 0
	}
	multiplier := float64(1)
	if len(parts) > 1 {
		switch strings.ToLower(strings.TrimSpace(parts[1])) {
		case "kb", "kib":
			multiplier = 1 << 10
		case "mb", "mib":
			multiplier = 1 << 20
		case "gb", "gib":
			multiplier = 1 << 30
		case "tb", "tib":
			multiplier = 1 << 40
		}
	}
	bytes := number * multiplier
	if math.IsNaN(bytes) || math.IsInf(bytes, 0) || bytes > float64(math.MaxInt64) {
		return 0
	}
	return int64(bytes)
}

func parseInt(value string) int {
	value = strings.ReplaceAll(strings.TrimSpace(value), ",", "")
	parsed, err := strconv.Atoi(value)
	if err == nil && parsed > 0 {
		return parsed
	}
	number, _ := strconv.ParseFloat(value, 64)
	if math.IsNaN(number) || math.IsInf(number, 0) || number <= 0 || number > float64(math.MaxInt) {
		return 0
	}
	return int(number)
}

func parseOptionalFloat(value string) *float64 {
	value = strings.TrimSpace(value)
	if value == "" {
		return nil
	}
	parsed, err := strconv.ParseFloat(value, 64)
	if err != nil || math.IsNaN(parsed) || math.IsInf(parsed, 0) || parsed < 0 || parsed > 100 {
		return nil
	}
	return &parsed
}

func parseBool(value string) bool {
	parsed, _ := strconv.ParseBool(strings.TrimSpace(value))
	return parsed
}

func splitLabels(value string) []string {
	parts := strings.FieldsFunc(value, func(r rune) bool { return r == ',' || r == '|' || r == ';' })
	result := make([]string, 0, min(len(parts), 64))
	for _, part := range parts {
		part = strings.TrimSpace(part)
		if part != "" && len(result) < 64 {
			result = append(result, part)
		}
	}
	return result
}

func parseTime(value string) time.Time {
	value = strings.TrimSpace(value)
	if timestamp, err := strconv.ParseInt(value, 10, 64); err == nil {
		if timestamp > 1_000_000_000_000 {
			timestamp /= 1000
		}
		if timestamp > 0 {
			return time.Unix(timestamp, 0).UTC()
		}
	}
	for _, layout := range []string{time.RFC3339, http.TimeFormat, "2006-01-02 15:04:05", "2006-01-02"} {
		if parsed, err := time.Parse(layout, value); err == nil {
			return parsed.UTC()
		}
	}
	return time.Time{}
}
