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

// siteStoreBranchFake 让每个存储方法都能注入失败，用来覆盖站点管理接口的
// 「存储不可用 / 记录不存在 / 冲突 / 一般失败」四类出口。
type siteStoreBranchFake struct {
	sites      map[int]database.Site
	listErr    error
	siteErr    error
	runtimeErr error
	upsertErr  error
	updateErr  error
	deleteErr  error
	deleted    bool
}

func (s *siteStoreBranchFake) ListSites(context.Context) ([]database.Site, error) {
	if s.listErr != nil {
		return nil, s.listErr
	}
	result := make([]database.Site, 0, len(s.sites))
	for _, site := range s.sites {
		result = append(result, site)
	}
	return result, nil
}

func (s *siteStoreBranchFake) SiteByID(_ context.Context, id int) (database.Site, error) {
	if s.siteErr != nil {
		return database.Site{}, s.siteErr
	}
	site, ok := s.sites[id]
	if !ok {
		return database.Site{}, pgx.ErrNoRows
	}
	return site, nil
}

func (s *siteStoreBranchFake) RuntimeSiteByID(ctx context.Context, id int) (database.Site, error) {
	if s.runtimeErr != nil {
		return database.Site{}, s.runtimeErr
	}
	return s.SiteByID(ctx, id)
}

func (s *siteStoreBranchFake) UpsertSite(_ context.Context, site database.Site) (int, error) {
	if s.upsertErr != nil {
		return 0, s.upsertErr
	}
	return 7, nil
}

func (s *siteStoreBranchFake) UpdateSite(context.Context, database.Site) error {
	return s.updateErr
}

func (s *siteStoreBranchFake) DeleteSite(context.Context, int) (bool, error) {
	return s.deleted, s.deleteErr
}

// sitRouteRequest 构造带 chi 路径参数的请求，便于直接调用 handler。
func siteRouteRequest(method, target, siteID, body string) *http.Request {
	var reader *strings.Reader
	if body == "" {
		reader = strings.NewReader("")
	} else {
		reader = strings.NewReader(body)
	}
	request := httptest.NewRequest(method, target, reader)
	rctx := chi.NewRouteContext()
	if siteID != "" {
		rctx.URLParams.Add("siteid", siteID)
	}
	return request.WithContext(context.WithValue(request.Context(), chi.RouteCtxKey, rctx))
}

func TestSiteListAndDetailBranches(t *testing.T) {
	// 存储未初始化。
	recorder := httptest.NewRecorder()
	Dependencies{}.listSites(recorder, siteRouteRequest(http.MethodGet, "/api/v1/site/", "", ""))
	if recorder.Code != http.StatusServiceUnavailable {
		t.Fatalf("无存储 listSites 状态码 = %d", recorder.Code)
	}
	// 查询失败。
	recorder = httptest.NewRecorder()
	Dependencies{Sites: &siteStoreBranchFake{listErr: errors.New("boom")}}.
		listSites(recorder, siteRouteRequest(http.MethodGet, "/api/v1/site/", "", ""))
	if recorder.Code != http.StatusInternalServerError {
		t.Fatalf("查询失败 listSites 状态码 = %d", recorder.Code)
	}
	// 正常返回。
	recorder = httptest.NewRecorder()
	Dependencies{Sites: &siteStoreBranchFake{sites: map[int]database.Site{
		1: {ID: 1, Name: "站点", URL: "https://a.example.com", Domain: "a.example.com", Adapter: "nexusphp"},
	}}}.listSites(recorder, siteRouteRequest(http.MethodGet, "/api/v1/site/", "", ""))
	if recorder.Code != http.StatusOK || !strings.Contains(recorder.Body.String(), "a.example.com") {
		t.Fatalf("正常 listSites 响应 = %d %s", recorder.Code, recorder.Body.String())
	}

	// 详情：无存储、ID 非法、记录不存在、查询失败、正常。
	cases := []struct {
		name  string
		deps  Dependencies
		site  string
		code  int
		extra string
	}{
		{name: "无存储", deps: Dependencies{}, site: "1", code: http.StatusServiceUnavailable},
		{name: "ID 非法", deps: Dependencies{Sites: &siteStoreBranchFake{}}, site: "abc", code: http.StatusBadRequest},
		{name: "不存在", deps: Dependencies{Sites: &siteStoreBranchFake{}}, site: "2", code: http.StatusNotFound},
		{name: "查询失败", deps: Dependencies{Sites: &siteStoreBranchFake{siteErr: errors.New("boom")}},
			site: "1", code: http.StatusInternalServerError},
		{name: "正常", deps: Dependencies{Sites: &siteStoreBranchFake{sites: map[int]database.Site{
			1: {ID: 1, Name: "站点", URL: "https://a.example.com", Domain: "a.example.com"},
		}}}, site: "1", code: http.StatusOK, extra: "a.example.com"},
	}
	for _, tc := range cases {
		t.Run("详情/"+tc.name, func(t *testing.T) {
			recorder := httptest.NewRecorder()
			tc.deps.getSite(recorder, siteRouteRequest(http.MethodGet, "/api/v1/site/"+tc.site, tc.site, ""))
			if recorder.Code != tc.code {
				t.Fatalf("状态码 = %d，期望 %d（%s）", recorder.Code, tc.code, recorder.Body.String())
			}
			if tc.extra != "" && !strings.Contains(recorder.Body.String(), tc.extra) {
				t.Fatalf("响应缺少 %q: %s", tc.extra, recorder.Body.String())
			}
		})
	}
}

func TestCreateSiteBranches(t *testing.T) {
	valid := `{"name":"新站点","url":"https://new.example.com","adapter":"nexusphp"}`
	cases := []struct {
		name     string
		deps     Dependencies
		body     string
		code     int
		contains string
	}{
		{name: "请求体非法", deps: Dependencies{}, body: "not-json", code: http.StatusBadRequest,
			contains: "站点请求体必须是有效 JSON"},
		{name: "多余内容", deps: Dependencies{}, body: `{"name":"a"} {"name":"b"}`, code: http.StatusBadRequest,
			contains: "站点请求体包含多余内容"},
		{name: "缺少站点地址", deps: Dependencies{}, body: `{"name":"a"}`, code: http.StatusBadRequest,
			contains: "有效的 HTTP"},
		{name: "适配器配置无效", deps: Dependencies{SiteValidator: func(database.Site) error {
			return errors.New("bad adapter")
		}}, body: valid, code: http.StatusBadRequest, contains: "适配器配置无效"},
		{name: "无存储", deps: Dependencies{}, body: valid, code: http.StatusServiceUnavailable},
		{name: "冲突且重载失败", deps: Dependencies{
			Sites:       &siteStoreBranchFake{upsertErr: database.ErrSiteConflict},
			ReloadSites: func(context.Context) error { return errors.New("reload") },
		}, body: valid, code: http.StatusServiceUnavailable},
		{name: "冲突", deps: Dependencies{
			Sites: &siteStoreBranchFake{upsertErr: database.ErrSiteConflict},
		}, body: valid, code: http.StatusOK, contains: "站点已存在"},
		{name: "保存失败", deps: Dependencies{
			Sites: &siteStoreBranchFake{upsertErr: errors.New("boom")},
		}, body: valid, code: http.StatusInternalServerError},
		{name: "创建后重载失败", deps: Dependencies{
			Sites:       &siteStoreBranchFake{},
			ReloadSites: func(context.Context) error { return errors.New("reload") },
		}, body: valid, code: http.StatusServiceUnavailable},
		{name: "正常", deps: Dependencies{
			Sites:       &siteStoreBranchFake{},
			ReloadSites: func(context.Context) error { return nil },
		}, body: valid, code: http.StatusOK, contains: `"id":7`},
		{name: "缺少适配器", deps: Dependencies{}, body: `{"url":"https://x.example.com"}`,
			code: http.StatusBadRequest, contains: "不能为空"},
		{name: "声明式格式推断", deps: Dependencies{Sites: &siteStoreBranchFake{}},
			body: `{"name":"甲","url":"https://jia.example.com","config":{"format":"yaml"}}`,
			code: http.StatusOK},
		{name: "写入扩展字段", deps: Dependencies{Sites: &siteStoreBranchFake{}},
			body: `{"name":"乙","url":"https://yi.example.com","adapter":"nexusphp",` +
				`"note":"备注","timeout":5,"limit_interval":1,"limit_count":2,"limit_seconds":3,` +
				`"proxy":true,"render":false,"public":true}`,
			code: http.StatusOK},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			recorder := httptest.NewRecorder()
			tc.deps.createSite(recorder, siteRouteRequest(http.MethodPost, "/api/v1/site/", "", tc.body))
			if recorder.Code != tc.code {
				t.Fatalf("状态码 = %d，期望 %d（%s）", recorder.Code, tc.code, recorder.Body.String())
			}
			if tc.contains != "" && !strings.Contains(recorder.Body.String(), tc.contains) {
				t.Fatalf("响应缺少 %q: %s", tc.contains, recorder.Body.String())
			}
		})
	}
}

func TestUpdateSiteBranches(t *testing.T) {
	valid := `{"id":1,"name":"改名","url":"https://a.example.com","adapter":"nexusphp"}`
	existing := map[int]database.Site{
		1: {ID: 1, Name: "站点", URL: "https://a.example.com", Domain: "a.example.com", Adapter: "nexusphp"},
	}
	cases := []struct {
		name     string
		deps     Dependencies
		site     string
		body     string
		code     int
		contains string
	}{
		{name: "请求体非法", deps: Dependencies{}, site: "1", body: "not-json", code: http.StatusBadRequest},
		{name: "路径 ID 非法", deps: Dependencies{}, site: "abc", body: valid, code: http.StatusBadRequest,
			contains: "站点 ID 无效"},
		{name: "路径与请求体不一致", deps: Dependencies{}, site: "2", body: valid, code: http.StatusBadRequest,
			contains: "请求体站点 ID 与路径不一致"},
		{name: "缺少 ID", deps: Dependencies{}, site: "", body: `{"name":"x"}`, code: http.StatusBadRequest,
			contains: "站点 ID 无效"},
		{name: "无存储", deps: Dependencies{}, site: "1", body: `{"name":"x"}`, code: http.StatusServiceUnavailable},
		{name: "记录不存在", deps: Dependencies{Sites: &siteStoreBranchFake{}}, site: "1",
			body: `{"name":"x"}`, code: http.StatusNotFound},
		{name: "查询失败", deps: Dependencies{Sites: &siteStoreBranchFake{runtimeErr: errors.New("boom")}},
			site: "1", body: `{"name":"x"}`, code: http.StatusInternalServerError},
		{name: "站点地址非法", deps: Dependencies{Sites: &siteStoreBranchFake{sites: existing}},
			site: "1", body: `{"url":"ftp://a"}`, code: http.StatusBadRequest},
		{name: "适配器配置无效", deps: Dependencies{
			Sites:         &siteStoreBranchFake{sites: existing},
			SiteValidator: func(database.Site) error { return errors.New("bad") },
		}, site: "1", body: valid, code: http.StatusBadRequest, contains: "适配器配置无效"},
		{name: "更新时记录消失", deps: Dependencies{
			Sites: &siteStoreBranchFake{sites: existing, updateErr: pgx.ErrNoRows},
		}, site: "1", body: valid, code: http.StatusNotFound},
		{name: "更新冲突", deps: Dependencies{
			Sites: &siteStoreBranchFake{sites: existing, updateErr: database.ErrSiteConflict},
		}, site: "1", body: valid, code: http.StatusOK, contains: "站点名称或域名已存在"},
		{name: "更新失败", deps: Dependencies{
			Sites: &siteStoreBranchFake{sites: existing, updateErr: errors.New("boom")},
		}, site: "1", body: valid, code: http.StatusInternalServerError},
		{name: "更新后重载失败", deps: Dependencies{
			Sites:       &siteStoreBranchFake{sites: existing},
			ReloadSites: func(context.Context) error { return errors.New("reload") },
		}, site: "1", body: valid, code: http.StatusServiceUnavailable},
		{name: "正常", deps: Dependencies{
			Sites:       &siteStoreBranchFake{sites: existing},
			ReloadSites: func(context.Context) error { return nil },
		}, site: "1", body: valid, code: http.StatusOK},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			recorder := httptest.NewRecorder()
			tc.deps.updateSite(recorder, siteRouteRequest(http.MethodPut, "/api/v1/site/"+tc.site, tc.site, tc.body))
			if recorder.Code != tc.code {
				t.Fatalf("状态码 = %d，期望 %d（%s）", recorder.Code, tc.code, recorder.Body.String())
			}
			if tc.contains != "" && !strings.Contains(recorder.Body.String(), tc.contains) {
				t.Fatalf("响应缺少 %q: %s", tc.contains, recorder.Body.String())
			}
		})
	}
}

func TestDeleteSiteBranches(t *testing.T) {
	cases := []struct {
		name string
		deps Dependencies
		site string
		code int
	}{
		{name: "ID 非法", deps: Dependencies{}, site: "abc", code: http.StatusBadRequest},
		{name: "无存储", deps: Dependencies{}, site: "1", code: http.StatusServiceUnavailable},
		{name: "删除失败", deps: Dependencies{Sites: &siteStoreBranchFake{deleteErr: errors.New("boom")}},
			site: "1", code: http.StatusInternalServerError},
		{name: "不存在且重载失败", deps: Dependencies{
			Sites:       &siteStoreBranchFake{deleted: false},
			ReloadSites: func(context.Context) error { return errors.New("reload") },
		}, site: "1", code: http.StatusServiceUnavailable},
		{name: "不存在", deps: Dependencies{Sites: &siteStoreBranchFake{deleted: false}},
			site: "1", code: http.StatusNotFound},
		{name: "删除后重载失败", deps: Dependencies{
			Sites:       &siteStoreBranchFake{deleted: true},
			ReloadSites: func(context.Context) error { return errors.New("reload") },
		}, site: "1", code: http.StatusServiceUnavailable},
		{name: "正常", deps: Dependencies{
			Sites:       &siteStoreBranchFake{deleted: true},
			ReloadSites: func(context.Context) error { return nil },
		}, site: "1", code: http.StatusOK},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			recorder := httptest.NewRecorder()
			tc.deps.deleteSite(recorder, siteRouteRequest(http.MethodDelete, "/api/v1/site/"+tc.site, tc.site, ""))
			if recorder.Code != tc.code {
				t.Fatalf("状态码 = %d，期望 %d（%s）", recorder.Code, tc.code, recorder.Body.String())
			}
		})
	}
}
