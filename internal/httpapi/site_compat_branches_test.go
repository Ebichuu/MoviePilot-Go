package httpapi

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5"

	"github.com/moviepilot-go/moviepilot-go/internal/database"
)

// siteCompatStoreFake 提供站点兼容接口所需的存储能力，全部故障可注入。
type siteCompatStoreFake struct {
	sites         map[int]database.Site
	listErr       error
	runtimeErr    error
	priorityErr   error
	priorityCalls int
	deleteAllErr  error
}

func (s *siteCompatStoreFake) ListSites(context.Context) ([]database.Site, error) {
	if s.listErr != nil {
		return nil, s.listErr
	}
	result := make([]database.Site, 0, len(s.sites))
	for _, site := range s.sites {
		result = append(result, site)
	}
	return result, nil
}

func (s *siteCompatStoreFake) SiteByID(_ context.Context, id int) (database.Site, error) {
	site, ok := s.sites[id]
	if !ok {
		return database.Site{}, pgx.ErrNoRows
	}
	return site, nil
}

func (s *siteCompatStoreFake) RuntimeSiteByID(_ context.Context, id int) (database.Site, error) {
	if s.runtimeErr != nil {
		return database.Site{}, s.runtimeErr
	}
	return s.SiteByID(context.Background(), id)
}

func (s *siteCompatStoreFake) UpsertSite(_ context.Context, site database.Site) (int, error) {
	return site.ID, nil
}

func (s *siteCompatStoreFake) UpdateSite(context.Context, database.Site) error { return nil }

func (s *siteCompatStoreFake) DeleteSite(context.Context, int) (bool, error) { return true, nil }

// sitePriorityStoreFake 在基础假存储上补出两个可选能力：
// 更新站点优先级与清空站点，用来覆盖 V2 兼容端点的成功与失败出口。
type sitePriorityStoreFake struct {
	siteCompatStoreFake
	deleteAllCount int64
}

func (s *sitePriorityStoreFake) UpdateSitePriorities(context.Context, []database.SitePriority) error {
	s.priorityCalls++
	return s.priorityErr
}

func (s *sitePriorityStoreFake) DeleteAllSites(context.Context) (int64, error) {
	if s.deleteAllErr != nil {
		return 0, s.deleteAllErr
	}
	return s.deleteAllCount, nil
}

type siteCatalogFake struct {
	categories []map[string]any
	supported  bool
	err        error
}

func (c siteCatalogFake) SiteCategories(context.Context, string) ([]map[string]any, bool, error) {
	return c.categories, c.supported, c.err
}

type siteIconFake struct {
	base64 string
	url    string
	found  bool
	err    error
}

func (i siteIconFake) SiteIconByDomain(context.Context, string) (string, string, bool, error) {
	return i.base64, i.url, i.found, i.err
}

type siteCookieUpdaterFake struct {
	success bool
	message string
	err     error
}

func (u siteCookieUpdaterFake) UpdateSiteCookie(context.Context, int, string, string, string) (bool, string, error) {
	return u.success, u.message, u.err
}

// siteParamRequest 构造带指定 chi 路径参数的请求。
func siteParamRequest(method, target, param, value, body string) *http.Request {
	request := httptest.NewRequest(method, target, strings.NewReader(body))
	rctx := chi.NewRouteContext()
	if value != "" {
		rctx.URLParams.Add(param, value)
	}
	return request.WithContext(context.WithValue(request.Context(), chi.RouteCtxKey, rctx))
}

func TestUpdateSitePrioritiesBranches(t *testing.T) {
	good := `[{"id":1,"pri":2},{"id":2,"pri":3}]`
	cases := []struct {
		name     string
		deps     Dependencies
		body     string
		code     int
		contains string
	}{
		{name: "能力未接入", deps: Dependencies{Sites: &siteCompatStoreFake{}}, body: good,
			code: http.StatusOK, contains: "站点优先级能力尚未接入"},
		{name: "请求体非法", deps: Dependencies{Sites: &sitePriorityStoreFake{}}, body: "nope",
			code: http.StatusBadRequest, contains: "站点优先级请求无效"},
		{name: "多余内容", deps: Dependencies{Sites: &sitePriorityStoreFake{}},
			body: good + " []", code: http.StatusBadRequest, contains: "包含多余内容"},
		{name: "请求过大", deps: Dependencies{Sites: &sitePriorityStoreFake{}},
			body:     "[" + strings.Join(repeatJSON(`{"id":1,"pri":1}`, 1001), ",") + "]",
			code:     http.StatusBadRequest,
			contains: "站点优先级请求过大"},
		{name: "ID 非法", deps: Dependencies{Sites: &sitePriorityStoreFake{}},
			body: `[{"id":0,"pri":1}]`, code: http.StatusBadRequest, contains: "站点优先级参数无效"},
		{name: "缺少优先级", deps: Dependencies{Sites: &sitePriorityStoreFake{}},
			body: `[{"id":1}]`, code: http.StatusBadRequest, contains: "站点优先级参数无效"},
		{name: "优先级为负", deps: Dependencies{Sites: &sitePriorityStoreFake{}},
			body: `[{"id":1,"pri":-1}]`, code: http.StatusBadRequest, contains: "站点优先级参数无效"},
		{name: "站点不存在", deps: Dependencies{Sites: &sitePriorityStoreFake{
			siteCompatStoreFake: siteCompatStoreFake{priorityErr: pgx.ErrNoRows},
		}}, body: good, code: http.StatusNotFound},
		{name: "更新失败", deps: Dependencies{Sites: &sitePriorityStoreFake{
			siteCompatStoreFake: siteCompatStoreFake{priorityErr: errors.New("boom")},
		}}, body: good, code: http.StatusInternalServerError},
		{name: "重载失败", deps: Dependencies{
			Sites:       &sitePriorityStoreFake{},
			ReloadSites: func(context.Context) error { return errors.New("reload") },
		}, body: good, code: http.StatusServiceUnavailable},
		{name: "正常", deps: Dependencies{Sites: &sitePriorityStoreFake{}}, body: good,
			code: http.StatusOK, contains: `"updated":2`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			recorder := httptest.NewRecorder()
			tc.deps.updateSitePriorities(recorder,
				siteParamRequest(http.MethodPost, "/api/v1/site/priority", "", "", tc.body))
			if recorder.Code != tc.code {
				t.Fatalf("状态码 = %d，期望 %d（%s）", recorder.Code, tc.code, recorder.Body.String())
			}
			if tc.contains != "" && !strings.Contains(recorder.Body.String(), tc.contains) {
				t.Fatalf("响应缺少 %q: %s", tc.contains, recorder.Body.String())
			}
		})
	}
}

func repeatJSON(item string, count int) []string {
	result := make([]string, 0, count)
	for index := 0; index < count; index++ {
		result = append(result, item)
	}
	return result
}

func TestSiteCategoriesBranches(t *testing.T) {
	site := map[int]database.Site{
		3: {ID: 3, Name: "站点", URL: "https://c.example.com", Domain: "c.example.com"},
	}
	cases := []struct {
		name     string
		deps     Dependencies
		site     string
		code     int
		contains string
	}{
		{name: "无存储", deps: Dependencies{}, site: "3", code: http.StatusOK},
		{name: "ID 非法", deps: Dependencies{Sites: &siteCompatStoreFake{sites: site}},
			site: "abc", code: http.StatusUnprocessableEntity},
		{name: "站点不存在", deps: Dependencies{Sites: &siteCompatStoreFake{sites: site}},
			site: "9", code: http.StatusNotFound, contains: "站点 9 不存在"},
		{name: "查询失败", deps: Dependencies{Sites: &siteCompatStoreFake{
			sites: site, runtimeErr: errors.New("boom"),
		}}, site: "3", code: http.StatusInternalServerError},
		{name: "无目录能力", deps: Dependencies{Sites: &siteCompatStoreFake{sites: site}},
			site: "3", code: http.StatusOK},
		{name: "目录查询失败", deps: Dependencies{
			Sites:       &siteCompatStoreFake{sites: site},
			SiteCatalog: siteCatalogFake{err: errors.New("boom")},
		}, site: "3", code: http.StatusInternalServerError},
		{name: "索引器不支持", deps: Dependencies{
			Sites:       &siteCompatStoreFake{sites: site},
			SiteCatalog: siteCatalogFake{supported: false},
		}, site: "3", code: http.StatusNotFound, contains: "c.example.com 不支持"},
		{name: "空分类", deps: Dependencies{
			Sites:       &siteCompatStoreFake{sites: site},
			SiteCatalog: siteCatalogFake{supported: true},
		}, site: "3", code: http.StatusOK},
		{name: "正常", deps: Dependencies{
			Sites: &siteCompatStoreFake{sites: site},
			SiteCatalog: siteCatalogFake{supported: true, categories: []map[string]any{
				{"id": "movie", "name": "电影"},
			}},
		}, site: "3", code: http.StatusOK, contains: "movie"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			recorder := httptest.NewRecorder()
			tc.deps.siteCategories(recorder,
				siteParamRequest(http.MethodGet, "/api/v1/site/category/"+tc.site, "site_id", tc.site, ""))
			if recorder.Code != tc.code {
				t.Fatalf("状态码 = %d，期望 %d（%s）", recorder.Code, tc.code, recorder.Body.String())
			}
			if tc.contains != "" && !strings.Contains(recorder.Body.String(), tc.contains) {
				t.Fatalf("响应缺少 %q: %s", tc.contains, recorder.Body.String())
			}
		})
	}
}

func TestUpdateSiteCookieBranches(t *testing.T) {
	sites := map[int]database.Site{
		5: {ID: 5, Name: "站点", URL: "https://k.example.com", Domain: "k.example.com"},
	}
	cases := []struct {
		name     string
		deps     Dependencies
		target   string
		site     string
		method   string
		body     string
		code     int
		contains string
	}{
		{name: "ID 非法", deps: Dependencies{Sites: &siteCompatStoreFake{sites: sites}},
			site: "abc", code: http.StatusUnprocessableEntity},
		{name: "无存储", deps: Dependencies{}, site: "5", code: http.StatusServiceUnavailable},
		{name: "站点不存在", deps: Dependencies{Sites: &siteCompatStoreFake{sites: sites}},
			site: "9", code: http.StatusNotFound, contains: "站点 9 不存在"},
		{name: "查询失败", deps: Dependencies{Sites: &siteCompatStoreFake{
			sites: sites, runtimeErr: errors.New("boom"),
		}}, site: "5", code: http.StatusInternalServerError},
		{name: "缺少凭据", deps: Dependencies{Sites: &siteCompatStoreFake{sites: sites}},
			site: "5", code: http.StatusUnprocessableEntity, contains: "不能为空"},
		{name: "请求体非法", deps: Dependencies{Sites: &siteCompatStoreFake{sites: sites}},
			site: "5", method: http.MethodPost, body: "nope",
			code: http.StatusBadRequest, contains: "站点 Cookie 更新请求无效"},
		{name: "能力未接入", deps: Dependencies{Sites: &siteCompatStoreFake{sites: sites}},
			site: "5", target: "/api/v1/site/cookie/5?username=u&password=p",
			code: http.StatusOK, contains: "站点 Cookie 更新能力尚未接入"},
		{name: "更新失败", deps: Dependencies{
			Sites:             &siteCompatStoreFake{sites: sites},
			SiteCookieUpdater: siteCookieUpdaterFake{err: errors.New("boom")},
		}, site: "5", target: "/api/v1/site/cookie/5?username=u&password=p",
			code: http.StatusInternalServerError},
		{name: "正常", deps: Dependencies{
			Sites:             &siteCompatStoreFake{sites: sites},
			SiteCookieUpdater: siteCookieUpdaterFake{success: true, message: "Cookie 已更新"},
		}, site: "5", target: "/api/v1/site/cookie/5?username=u&password=p",
			code: http.StatusOK, contains: "Cookie 已更新"},
		{name: "POST 正常", deps: Dependencies{
			Sites:             &siteCompatStoreFake{sites: sites},
			SiteCookieUpdater: siteCookieUpdaterFake{success: false, message: "登录失败"},
		}, site: "5", method: http.MethodPost,
			body: `{"username":"u","password":"p","code":"c"}`, code: http.StatusOK,
			contains: "登录失败"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			method := tc.method
			if method == "" {
				method = http.MethodGet
			}
			target := tc.target
			if target == "" {
				target = "/api/v1/site/cookie/" + tc.site
			}
			recorder := httptest.NewRecorder()
			tc.deps.updateSiteCookie(recorder,
				siteParamRequest(method, target, "site_id", tc.site, tc.body))
			if recorder.Code != tc.code {
				t.Fatalf("状态码 = %d，期望 %d（%s）", recorder.Code, tc.code, recorder.Body.String())
			}
			if tc.contains != "" && !strings.Contains(recorder.Body.String(), tc.contains) {
				t.Fatalf("响应缺少 %q: %s", tc.contains, recorder.Body.String())
			}
		})
	}
}

func TestSiteByDomainAndMappingBranches(t *testing.T) {
	sites := map[int]database.Site{
		1: {ID: 1, Name: "甲", URL: "https://a.example.com", Domain: "a.example.com"},
		2: {ID: 2, Name: "乙", URL: "https://b.example.com"},
	}

	recorder := httptest.NewRecorder()
	Dependencies{}.siteByDomain(recorder, siteParamRequest(http.MethodGet, "/api/v1/site/domain/x", "site_url", "x", ""))
	if recorder.Code != http.StatusServiceUnavailable {
		t.Fatalf("无存储 siteByDomain 状态码 = %d", recorder.Code)
	}
	recorder = httptest.NewRecorder()
	Dependencies{Sites: &siteCompatStoreFake{listErr: errors.New("boom")}}.
		siteByDomain(recorder, siteParamRequest(http.MethodGet, "/api/v1/site/domain/x", "site_url", "x", ""))
	if recorder.Code != http.StatusInternalServerError {
		t.Fatalf("查询失败 siteByDomain 状态码 = %d", recorder.Code)
	}
	recorder = httptest.NewRecorder()
	Dependencies{Sites: &siteCompatStoreFake{sites: sites}}.
		siteByDomain(recorder, siteParamRequest(http.MethodGet, "/api/v1/site/domain/a.example.com",
			"site_url", "https%3A%2F%2FA.Example.com", ""))
	if recorder.Code != http.StatusOK || !strings.Contains(recorder.Body.String(), "甲") {
		t.Fatalf("按域名命中响应 = %d %s", recorder.Code, recorder.Body.String())
	}
	recorder = httptest.NewRecorder()
	Dependencies{Sites: &siteCompatStoreFake{sites: sites}}.
		siteByDomain(recorder, siteParamRequest(http.MethodGet, "/api/v1/site/domain/none", "site_url", "none", ""))
	if recorder.Code != http.StatusNotFound {
		t.Fatalf("未命中 siteByDomain 状态码 = %d", recorder.Code)
	}

	recorder = httptest.NewRecorder()
	Dependencies{}.siteMapping(recorder, siteParamRequest(http.MethodGet, "/api/v1/site/mapping", "", "", ""))
	if recorder.Code != http.StatusServiceUnavailable {
		t.Fatalf("无存储 siteMapping 状态码 = %d", recorder.Code)
	}
	recorder = httptest.NewRecorder()
	Dependencies{Sites: &siteCompatStoreFake{listErr: errors.New("boom")}}.
		siteMapping(recorder, siteParamRequest(http.MethodGet, "/api/v1/site/mapping", "", "", ""))
	if recorder.Code != http.StatusInternalServerError {
		t.Fatalf("查询失败 siteMapping 状态码 = %d", recorder.Code)
	}
	recorder = httptest.NewRecorder()
	Dependencies{Sites: &siteCompatStoreFake{sites: sites}}.
		siteMapping(recorder, siteParamRequest(http.MethodGet, "/api/v1/site/mapping", "", "", ""))
	if recorder.Code != http.StatusOK {
		t.Fatalf("正常 siteMapping 状态码 = %d", recorder.Code)
	}
	body := recorder.Body.String()
	if !strings.Contains(body, "a.example.com") || !strings.Contains(body, "b.example.com") {
		t.Fatalf("映射缺少域名: %s", body)
	}
}

func TestSiteResourcesAndStatisticBranches(t *testing.T) {
	// 站点 ID 非法。
	recorder := httptest.NewRecorder()
	Dependencies{}.siteResources(recorder,
		siteParamRequest(http.MethodGet, "/api/v1/site/resource/abc", "site_id", "abc", ""))
	if recorder.Code != http.StatusBadRequest {
		t.Fatalf("ID 非法 siteResources 状态码 = %d", recorder.Code)
	}
	// 没有搜索协调器时返回空数组。
	recorder = httptest.NewRecorder()
	Dependencies{}.siteResources(recorder,
		siteParamRequest(http.MethodGet, "/api/v1/site/resource/1", "site_id", "1", ""))
	if recorder.Code != http.StatusOK || strings.TrimSpace(recorder.Body.String()) != "[]" {
		t.Fatalf("空资源响应 = %d %s", recorder.Code, recorder.Body.String())
	}
	// 无统计存储时返回空数组。
	recorder = httptest.NewRecorder()
	Dependencies{}.siteStatistics(recorder,
		siteParamRequest(http.MethodGet, "/api/v1/site/statistic", "", "", ""))
	if recorder.Code != http.StatusOK || strings.TrimSpace(recorder.Body.String()) != "[]" {
		t.Fatalf("空统计响应 = %d %s", recorder.Code, recorder.Body.String())
	}
	// 无统计存储时单站点统计返回未命中形态。
	recorder = httptest.NewRecorder()
	Dependencies{}.siteStatistic(recorder,
		siteParamRequest(http.MethodGet, "/api/v1/site/statistic/a.example.com", "site_url", "a.example.com", ""))
	if recorder.Code != http.StatusOK || !strings.Contains(recorder.Body.String(), "a.example.com") {
		t.Fatalf("单站点统计响应 = %d %s", recorder.Code, recorder.Body.String())
	}
	// 重置站点：能力未接入、删除失败、正常。
	recorder = httptest.NewRecorder()
	Dependencies{Sites: &siteCompatStoreFake{}}.
		resetSites(recorder, siteParamRequest(http.MethodGet, "/api/v1/site/reset", "", "", ""))
	if recorder.Code != http.StatusOK || !strings.Contains(recorder.Body.String(), "站点重置能力尚未接入") {
		t.Fatalf("重置能力未接入响应 = %d %s", recorder.Code, recorder.Body.String())
	}
	recorder = httptest.NewRecorder()
	Dependencies{Sites: &sitePriorityStoreFake{siteCompatStoreFake: siteCompatStoreFake{deleteAllErr: errors.New("boom")}}}.
		resetSites(recorder, siteParamRequest(http.MethodGet, "/api/v1/site/reset", "", "", ""))
	if recorder.Code != http.StatusInternalServerError {
		t.Fatalf("重置失败响应 = %d", recorder.Code)
	}
	recorder = httptest.NewRecorder()
	Dependencies{Sites: &sitePriorityStoreFake{deleteAllCount: 4}}.
		resetSites(recorder, siteParamRequest(http.MethodGet, "/api/v1/site/reset", "", "", ""))
	if recorder.Code != http.StatusOK || !strings.Contains(recorder.Body.String(), "站点已重置") {
		t.Fatalf("重置成功响应 = %d %s", recorder.Code, recorder.Body.String())
	}

}
