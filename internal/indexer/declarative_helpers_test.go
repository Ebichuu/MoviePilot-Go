package indexer

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"
)

func declarativeClient(t *testing.T) *HTTPClient {
	t.Helper()
	client, err := NewHTTPClient(2, 5*time.Second, 1<<20)
	if err != nil {
		t.Fatalf("创建站点客户端: %v", err)
	}
	t.Cleanup(client.CloseIdleConnections)
	return client
}

func declarativeDefinition(format string) Definition {
	definition := Definition{
		ID: 7, Name: "示例站", BaseURL: "https://indexer.example.com", Format: format,
	}
	switch format {
	case "html":
		definition.ResultSelector = ".row"
	case "json":
		definition.ResultPath = "data.items"
	case "rss":
		definition.RSSPath = "/rss"
	}
	return definition
}

// TestNewDeclarativeAdapterValidation 覆盖声明式站点定义的校验阶梯。
func TestNewDeclarativeAdapterValidation(t *testing.T) {
	client := declarativeClient(t)
	cases := []struct {
		name       string
		definition Definition
		message    string
	}{
		{name: "空名称", definition: Definition{BaseURL: "https://x.example.com"},
			message: "站点名称不能为空"},
		{name: "非法地址", definition: Definition{Name: "站", BaseURL: "ftp://x.example.com"},
			message: "站点地址必须是 HTTP 或 HTTPS URL"},
		{name: "非法格式", definition: Definition{Name: "站", BaseURL: "https://x.example.com", Format: "xml"},
			message: "站点格式必须是 html、rss 或 json"},
		{name: "非法方法", definition: Definition{Name: "站", BaseURL: "https://x.example.com",
			Format: "html", Method: "DELETE"}, message: "声明式站点只支持 GET 或 POST"},
		{name: "HTML 缺少选择器", definition: Definition{Name: "站", BaseURL: "https://x.example.com"},
			message: "HTML 站点必须设置 result_selector"},
		{name: "JSON 缺少结果路径", definition: DeclarationJSONMissingPath(),
			message: "JSON 站点必须设置 result_path"},
		{name: "CSRF 缺少正则", definition: Definition{Name: "站", BaseURL: "https://x.example.com",
			Format: "html", ResultSelector: ".row", CSRFPath: "/csrf"}, message: "设置 csrf_path 时必须设置 csrf_regex"},
		{name: "CSRF 请求头非法", definition: Definition{Name: "站", BaseURL: "https://x.example.com",
			Format: "html", ResultSelector: ".row", CSRFPath: "/csrf", CSRFRegex: "token=\"(\\w+)\"",
			CSRFHeader: "Host"}, message: "CSRF 请求头无效"},
		{name: "CSRF 正则非法", definition: Definition{Name: "站", BaseURL: "https://x.example.com",
			Format: "html", ResultSelector: ".row", CSRFPath: "/csrf", CSRFRegex: "("},
			message: "CSRF 正则无效"},
		{name: "CSRF 正则缺少捕获组", definition: Definition{Name: "站", BaseURL: "https://x.example.com",
			Format: "html", ResultSelector: ".row", CSRFPath: "/csrf", CSRFRegex: "token"},
			message: "CSRF 正则必须包含捕获组"},
		{name: "只有 CSRF 正则", definition: Definition{Name: "站", BaseURL: "https://x.example.com",
			Format: "html", ResultSelector: ".row", CSRFRegex: "token=\"(\\w+)\""},
			message: "设置 csrf_regex 时必须同时设置 csrf_path"},
		{name: "POST 缺少请求体", definition: Definition{Name: "站", BaseURL: "https://x.example.com",
			Format: "html", ResultSelector: ".row", Method: "POST"}, message: "POST 站点必须设置 body"},
		{name: "Cookie 过长", definition: Definition{Name: "站", BaseURL: "https://x.example.com",
			Format: "html", ResultSelector: ".row", Cookie: strings.Repeat("c", 64<<10+1)},
			message: "站点 Cookie 或 User-Agent 超过长度限制"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := NewDeclarativeAdapter(client, tc.definition)
			if err == nil {
				t.Fatal("应返回错误")
			}
			if tc.message != "" && !strings.Contains(err.Error(), tc.message) {
				t.Fatalf("错误 = %v，期望包含 %q", err, tc.message)
			}
		})
	}

	if _, err := NewDeclarativeAdapter(nil, declarativeDefinition("html")); err == nil ||
		!strings.Contains(err.Error(), "站点 HTTP 客户端不能为空") {
		t.Fatalf("空客户端 = %v", err)
	}

	// 合法定义：CSRF 与默认参数都会被补齐。
	definition := declarativeDefinition("html")
	definition.CSRFPath = "/csrf"
	definition.CSRFRegex = "token=\"(\\w+)\""
	adapter, err := NewDeclarativeAdapter(client, definition)
	if err != nil {
		t.Fatalf("合法定义: %v", err)
	}
	if adapter.def.CSRFHeader != "X-CSRF-TOKEN" || adapter.def.QueryParam != "q" ||
		adapter.def.PageParam != "page" || adapter.def.Method != http.MethodGet {
		t.Fatalf("默认值未补齐: %+v", adapter.def)
	}

	post := declarativeDefinition("json")
	post.Method = "POST"
	post.Body = map[string]any{"kw": "${keyword}"}
	postAdapter, err := NewDeclarativeAdapter(client, post)
	if err != nil || postAdapter.def.Method != http.MethodPost {
		t.Fatalf("POST 站点: %v", err)
	}
	rss := declarativeDefinition("rss")
	rssAdapter, err := NewDeclarativeAdapter(client, rss)
	if err != nil || rssAdapter.def.RSSPath != "/rss" {
		t.Fatalf("RSS 站点: %v", err)
	}
}

// DeclarationJSONMissingPath 构造一个缺少 result_path 的 JSON 定义。
func DeclarationJSONMissingPath() Definition {
	return Definition{Name: "站", BaseURL: "https://x.example.com", Format: "json"}
}

// TestRenderBodyTemplates 覆盖请求体模板的替换、嵌套与非法输入。
func TestRenderBodyTemplates(t *testing.T) {
	query := Query{Keyword: "某剧", Page: 2, Limit: 20}

	rendered, err := renderBody(map[string]any{
		"kw":    "x${keyword}y",
		"page":  "${page}",
		"limit": "${limit}",
		"list":  []any{"${page}", 5, true, nil},
		"nest":  map[string]any{"deep": "${limit}"},
	}, query, 0)
	if err != nil {
		t.Fatalf("渲染失败: %v", err)
	}
	object := rendered.(map[string]any)
	if object["kw"] != "x某剧y" || object["page"] != 2 || object["limit"] != 20 {
		t.Fatalf("渲染结果 = %+v", object)
	}
	list := object["list"].([]any)
	if list[0] != 2 || list[1] != 5 || list[2] != true || list[3] != nil {
		t.Fatalf("数组渲染 = %+v", list)
	}
	if object["nest"].(map[string]any)["deep"] != 20 {
		t.Fatalf("嵌套渲染 = %+v", object["nest"])
	}

	if _, err := renderBody("${search_type}", Query{Keyword: " tt123 "}, 0); err != nil {
		t.Fatalf("search_type: %v", err)
	}
	// search_type 关键字以 tt 开头时按 IMDB 搜索。
	value, err := renderBody("${search_type}", Query{Keyword: "tt123"}, 0)
	if err != nil || value != "imdbid" {
		t.Fatalf("IMDB 搜索类型 = %v（%v）", value, err)
	}
	value, err = renderBody("${search_type}", Query{Keyword: "某剧"}, 0)
	if err != nil || value != "title" {
		t.Fatalf("标题搜索类型 = %v（%v）", value, err)
	}

	if _, err := renderBody("x", query, 17); err == nil ||
		!strings.Contains(err.Error(), "嵌套过深") {
		t.Fatalf("嵌套过深 = %v", err)
	}
	if _, err := renderBody(map[string]any{"": "x"}, query, 0); err == nil ||
		!strings.Contains(err.Error(), "字段名无效") {
		t.Fatalf("空字段名 = %v", err)
	}
	if _, err := renderBody(map[string]any{"a\r\nb": "x"}, query, 0); err == nil ||
		!strings.Contains(err.Error(), "字段名无效") {
		t.Fatalf("换行字段名 = %v", err)
	}
	if _, err := renderBody(map[string]any{"ok": map[string]any{"": "x"}}, query, 0); err == nil {
		t.Fatal("嵌套非法字段名应向上传递错误")
	}
}

// TestDeclarativeEndpointParams 覆盖搜索地址的参数拼装。
func TestDeclarativeEndpointParams(t *testing.T) {
	client := declarativeClient(t)
	definition := declarativeDefinition("html")
	definition.SearchPath = "/search"
	definition.Params = map[string]string{"cat": "2"}
	definition.QueryParam = "kw"
	definition.PageParam = "p"
	definition.PageOffset = 1
	definition.LimitParam = "n"
	adapter, err := NewDeclarativeAdapter(client, definition)
	if err != nil {
		t.Fatalf("构造适配器: %v", err)
	}
	endpoint, err := adapter.endpoint(Query{Keyword: "某剧", Page: 1, Limit: 20})
	if err != nil {
		t.Fatalf("拼装地址: %v", err)
	}
	for _, fragment := range []string{"cat=2", "kw=", "p=2", "n=20"} {
		if !strings.Contains(endpoint, fragment) {
			t.Fatalf("地址 = %s，缺少 %q", endpoint, fragment)
		}
	}

	// 空关键字不设置查询参数。
	endpoint, err = adapter.endpoint(Query{Page: 0, Limit: 10})
	if err != nil || strings.Contains(endpoint, "kw=") {
		t.Fatalf("空关键字地址 = %s（%v）", endpoint, err)
	}
}

// TestParseHTMLResources 覆盖 HTML 结果解析与字段回落。
func TestParseHTMLResources(t *testing.T) {
	client := declarativeClient(t)
	definition := declarativeDefinition("html")
	definition.Fields = Fields{
		Title:       Field{Selector: "a.title"},
		DetailURL:   Field{Selector: "a.title", Attribute: "href"},
		DownloadURL: Field{Selector: "a.dl", Attribute: "href"},
		Size:        Field{Selector: "span.size"},
		Seeders:     Field{Selector: "span.seed"},
	}
	adapter, err := NewDeclarativeAdapter(client, definition)
	if err != nil {
		t.Fatalf("构造适配器: %v", err)
	}
	body := []byte(`<html><body>
		<div class="row">
			<a class="title" href="/detail/1">某剧.S01E01</a>
			<a class="dl" href="/download/1.torrent">下载</a>
			<span class="size">1.5 GB</span>
			<span class="seed">12</span>
		</div>
		<div class="row"><span class="size">空标题行</span></div>
	</body></html>`)
	resources, err := adapter.parseHTML(body, 10)
	if err != nil {
		t.Fatalf("解析 HTML: %v", err)
	}
	if len(resources) != 1 {
		t.Fatalf("资源条数 = %d", len(resources))
	}
	first := resources[0]
	if first.Title != "某剧.S01E01" || first.SiteID != 7 {
		t.Fatalf("首条 = %+v", first)
	}
	if !strings.Contains(first.DownloadURL, "/download/1.torrent") {
		t.Fatalf("下载地址 = %q", first.DownloadURL)
	}
	if first.DownloadID == "" || first.SizeBytes != 1610612736 || first.Seeders != 12 {
		t.Fatalf("字段解析 = %+v", first)
	}

	// limit 生效。
	limited, err := adapter.parseHTML(body, 1)
	if err != nil || len(limited) != 1 {
		t.Fatalf("limit 生效 = %d（%v）", len(limited), err)
	}
}

// TestParseJSONResources 覆盖 JSON 结果解析、定位符回落与错误分支。
func TestParseJSONResources(t *testing.T) {
	client := declarativeClient(t)
	definition := declarativeDefinition("json")
	definition.Fields = Fields{
		Title:       Field{Selector: "name"},
		ResourceID:  Field{Selector: "id"},
		DetailURL:   Field{Selector: "detail"},
		DownloadURL: Field{Selector: "url"},
		Size:        Field{Selector: "size"},
		Seeders:     Field{Selector: "seeders"},
		Labels:      Field{Selector: "labels"},
	}
	adapter, err := NewDeclarativeAdapter(client, definition)
	if err != nil {
		t.Fatalf("构造适配器: %v", err)
	}

	body := []byte(`{"data":{"items":[
		{"name":"某剧.S01E01","id":"77","size":"2 GB","seeders":"5","labels":["国配","中字"]},
		{"name":"无定位符","detail":"/d/88"},
		{"name":""},
		{"name":"只有下载地址","url":"/download/99.torrent"}
	]}}`)
	resources, err := adapter.parseJSON(body, 10)
	if err != nil {
		t.Fatalf("解析 JSON: %v", err)
	}
	if len(resources) != 3 {
		t.Fatalf("资源条数 = %d", len(resources))
	}
	if resources[0].TorrentID != "77" || resources[0].DownloadID != "77" {
		t.Fatalf("资源 ID = %+v", resources[0])
	}
	if resources[0].SizeBytes != 2<<30 || resources[0].Seeders != 5 || len(resources[0].Labels) != 2 {
		t.Fatalf("字段解析 = %+v", resources[0])
	}
	if !strings.Contains(resources[1].DownloadID, "/d/88") {
		t.Fatalf("详情地址回落 = %q", resources[1].DownloadID)
	}
	if !strings.Contains(resources[2].DownloadID, "/download/99.torrent") {
		t.Fatalf("下载地址回落 = %q", resources[2].DownloadID)
	}

	if _, err := adapter.parseJSON([]byte(`not json`), 10); err == nil ||
		!strings.Contains(err.Error(), "解析站点 JSON 失败") {
		t.Fatalf("非法 JSON = %v", err)
	}
	if _, err := adapter.parseJSON([]byte(`{"a":1} {"b":2}`), 10); err == nil ||
		!strings.Contains(err.Error(), "多余内容") {
		t.Fatalf("多余内容 = %v", err)
	}
	if _, err := adapter.parseJSON([]byte(`{"data":{"items":{}}}`), 10); err == nil ||
		!strings.Contains(err.Error(), "结果路径不是数组") {
		t.Fatalf("结果路径类型 = %v", err)
	}
	limited, err := adapter.parseJSON(body, 1)
	if err != nil || len(limited) != 1 {
		t.Fatalf("limit 生效 = %d（%v）", len(limited), err)
	}
}

// TestLookupJSONAndFieldHelpers 覆盖 JSON 取值与字段串化。
func TestLookupJSONPathHelpers(t *testing.T) {
	document := map[string]any{
		"data": map[string]any{
			"items": []any{
				map[string]any{"name": " 甲 ", "id": float64(0), "labels": []any{"x", " ", 3}},
			},
		},
	}
	if value := lookupJSON(document, "data.items.0.name"); value != " 甲 " {
		t.Fatalf("路径取值 = %v", value)
	}
	if lookupJSON(document, "data.items.9.name") != nil {
		t.Fatal("越界下标应为 nil")
	}
	if lookupJSON(document, "data.missing") != nil {
		t.Fatal("缺失字段应为 nil")
	}
	if lookupJSON(document, "data.items.0.name.deep") != nil {
		t.Fatal("对字符串继续下钻应为 nil")
	}
	if lookupJSON(document, "") == nil {
		t.Fatal("空路径应返回原值")
	}

	item := document["data"].(map[string]any)["items"].([]any)[0]
	if got := jsonFieldString(item, Field{Selector: "name"}); got != "甲" {
		t.Fatalf("字段串 = %q", got)
	}
	if got := jsonFieldString(item, Field{}); got != "" {
		t.Fatalf("空选择器 = %q", got)
	}
	if got := jsonFieldString(item, Field{Selector: "id"}); got != "0" {
		t.Fatalf("数字字段 = %q", got)
	}
	if got := jsonFieldString(item, Field{Selector: "labels"}); !strings.Contains(got, "x") {
		t.Fatalf("复合字段 = %q", got)
	}
	if got := jsonFieldStrings(item, Field{Selector: "labels"}); len(got) != 2 || got[0] != "x" {
		t.Fatalf("数组字段 = %+v", got)
	}
	if got := jsonFieldStrings(item, Field{Selector: "name"}); len(got) != 1 || got[0] != "甲" {
		t.Fatalf("字符串回落 = %+v", got)
	}
	if got := jsonFieldStrings(item, Field{}); got != nil {
		t.Fatalf("空选择器数组 = %+v", got)
	}

	adapter, err := NewDeclarativeAdapter(declarativeClient(t), declarativeDefinition("json"))
	if err != nil {
		t.Fatalf("构造适配器: %v", err)
	}
	if resolved := adapter.resolveJSONFieldURL(item, Field{Selector: "name"}); !strings.Contains(resolved, "%E7%94%B2") {
		t.Fatalf("字段地址 = %q", resolved)
	}
	if resolved := adapter.resolveJSONFieldURL(item, Field{Selector: "name",
		Template: "https://indexer.example.com/f/${value}"}); !strings.HasPrefix(resolved, "https://indexer.example.com/f/") {
		t.Fatalf("模板地址 = %q", resolved)
	}
}

// TestCSRFRefreshRequired 覆盖 CSRF 重试判定。
func TestCSRFRefreshRequired(t *testing.T) {
	if !csrfRefreshRequired(&HTTPStatusError{StatusCode: http.StatusUnauthorized}) ||
		!csrfRefreshRequired(&HTTPStatusError{StatusCode: http.StatusForbidden}) {
		t.Fatal("401/403 应触发 CSRF 刷新")
	}
	if csrfRefreshRequired(&HTTPStatusError{StatusCode: http.StatusInternalServerError}) {
		t.Fatal("500 不应触发 CSRF 刷新")
	}
	if csrfRefreshRequired(errors.New("boom")) {
		t.Fatal("普通错误不应触发 CSRF 刷新")
	}
	wrapped := fmt.Errorf("包装: %w", &HTTPStatusError{StatusCode: http.StatusUnauthorized})
	if !csrfRefreshRequired(wrapped) {
		t.Fatal("包装后的 401 应触发 CSRF 刷新")
	}
}

// TestDeclarativeSearchKeywordLimit 覆盖搜索关键词长度上限。
func TestDeclarativeSearchKeywordLimit(t *testing.T) {
	client := declarativeClient(t)
	adapter, err := NewDeclarativeAdapter(client, declarativeDefinition("html"))
	if err != nil {
		t.Fatalf("构造适配器: %v", err)
	}
	if _, err := adapter.Search(context.Background(), Query{Keyword: strings.Repeat("长", 513)}); err == nil ||
		!strings.Contains(err.Error(), "关键词不能超过 512") {
		t.Fatalf("超长关键词 = %v", err)
	}
	if _, err := adapter.Search(context.Background(), Query{Keyword: "剧", Page: -1}); err == nil {
		t.Fatal("负页码应报错")
	}
}
