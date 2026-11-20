package httpapi

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5"

	"github.com/moviepilot-go/moviepilot-go/internal/database"
)

type siteStoreStub struct {
	sites      []database.Site
	listErr    error
	runtime    database.Site
	runtimeErr error
}

func (s *siteStoreStub) ListSites(context.Context) ([]database.Site, error) {
	return s.sites, s.listErr
}

func (s *siteStoreStub) SiteByID(context.Context, int) (database.Site, error) {
	return s.runtime, s.runtimeErr
}

func (s *siteStoreStub) RuntimeSiteByID(context.Context, int) (database.Site, error) {
	return s.runtime, s.runtimeErr
}

func (s *siteStoreStub) UpsertSite(context.Context, database.Site) (int, error) { return 0, nil }
func (s *siteStoreStub) UpdateSite(context.Context, database.Site) error        { return nil }
func (s *siteStoreStub) DeleteSite(context.Context, int) (bool, error)          { return true, nil }

type siteStoreResetStub struct {
	siteStoreStub
	deleted   int64
	deleteErr error
}

func (s *siteStoreResetStub) DeleteAllSites(context.Context) (int64, error) {
	return s.deleted, s.deleteErr
}

type siteStoreSettingsStub struct {
	siteStoreStub
	selected   []int
	settingErr error
}

func (s *siteStoreSettingsStub) GetSetting(_ context.Context, _ string, target any) (bool, error) {
	if s.settingErr != nil {
		return false, s.settingErr
	}
	if ids, ok := target.(*[]int); ok {
		*ids = append([]int(nil), s.selected...)
	}
	return true, nil
}

type siteIconStub struct {
	base64 string
	url    string
	found  bool
	err    error
}

func (s siteIconStub) SiteIconByDomain(context.Context, string) (string, string, bool, error) {
	return s.base64, s.url, s.found, s.err
}

type cookieCloudSyncStub struct {
	message string
	err     error
}

func (s cookieCloudSyncStub) SyncCookieCloud(context.Context) (string, error) {
	return s.message, s.err
}

func decodeResponse(t *testing.T, recorder *httptest.ResponseRecorder) Response {
	t.Helper()
	var response Response
	if err := json.Unmarshal(recorder.Body.Bytes(), &response); err != nil {
		t.Fatalf("解析响应: %v（body=%q）", err, recorder.Body.String())
	}
	return response
}

// TestCookieCloudSyncHandler 覆盖 CookieCloud 同步端点的全部返回分支。
func TestCookieCloudSyncHandler(t *testing.T) {
	request := httptest.NewRequest(http.MethodPost, "/api/v1/site/cookiecloud", nil)

	recorder := httptest.NewRecorder()
	(Dependencies{}).cookieCloudSync(recorder, request)
	if response := decodeResponse(t, recorder); response.Success || response.Message != "CookieCloud 同步能力尚未接入" {
		t.Fatalf("未接入响应 = %+v", response)
	}

	recorder = httptest.NewRecorder()
	Dependencies{CookieCloud: cookieCloudSyncStub{err: errors.New("boom")}}.cookieCloudSync(recorder, request)
	if recorder.Code != http.StatusInternalServerError {
		t.Fatalf("同步失败状态码 = %d", recorder.Code)
	}

	canceled, cancel := context.WithCancel(context.Background())
	cancel()
	canceledRequest := httptest.NewRequest(http.MethodPost, "/api/v1/site/cookiecloud", nil).WithContext(canceled)
	recorder = httptest.NewRecorder()
	Dependencies{CookieCloud: cookieCloudSyncStub{err: context.Canceled}}.cookieCloudSync(recorder, canceledRequest)
	if recorder.Code != http.StatusServiceUnavailable || decodeResponse(t, recorder).Message != "CookieCloud 同步已取消" {
		t.Fatalf("取消响应 = (%d, %q)", recorder.Code, recorder.Body.String())
	}

	recorder = httptest.NewRecorder()
	Dependencies{CookieCloud: cookieCloudSyncStub{}}.cookieCloudSync(recorder, request)
	if response := decodeResponse(t, recorder); !response.Success || response.Message != "CookieCloud同步任务已启动！" {
		t.Fatalf("默认消息响应 = %+v", response)
	}

	recorder = httptest.NewRecorder()
	Dependencies{CookieCloud: cookieCloudSyncStub{message: "更新了1个站点"}}.cookieCloudSync(recorder, request)
	if response := decodeResponse(t, recorder); !response.Success || response.Message != "更新了1个站点" {
		t.Fatalf("自定义消息响应 = %+v", response)
	}
}

// TestResetSitesHandler 覆盖重置站点：能力未接入、删除失败、同步失败（仍成功）、取消失败。
func TestResetSitesHandler(t *testing.T) {
	request := httptest.NewRequest(http.MethodPost, "/api/v1/site/reset", nil)

	recorder := httptest.NewRecorder()
	Dependencies{Sites: &siteStoreStub{}}.resetSites(recorder, request)
	if response := decodeResponse(t, recorder); response.Success || response.Message != "站点重置能力尚未接入" {
		t.Fatalf("未接入响应 = %+v", response)
	}

	recorder = httptest.NewRecorder()
	Dependencies{Sites: &siteStoreResetStub{deleteErr: errors.New("删除失败")}}.resetSites(recorder, request)
	if recorder.Code != http.StatusInternalServerError {
		t.Fatalf("删除失败状态码 = %d", recorder.Code)
	}

	recorder = httptest.NewRecorder()
	Dependencies{Sites: &siteStoreResetStub{deleted: 2}}.resetSites(recorder, request)
	if response := decodeResponse(t, recorder); !response.Success || response.Message != "站点已重置！" {
		t.Fatalf("重置成功响应 = %+v", response)
	}

	// CookieCloud 同步失败不影响重置成功（V2 口径）。
	recorder = httptest.NewRecorder()
	Dependencies{
		Sites:       &siteStoreResetStub{deleted: 1},
		CookieCloud: cookieCloudSyncStub{err: errors.New("同步失败")},
	}.resetSites(recorder, request)
	if response := decodeResponse(t, recorder); !response.Success || response.Message != "站点已重置！" {
		t.Fatalf("同步失败仍重置响应 = %+v", response)
	}

	canceled, cancel := context.WithCancel(context.Background())
	cancel()
	canceledRequest := httptest.NewRequest(http.MethodPost, "/api/v1/site/reset", nil).WithContext(canceled)
	recorder = httptest.NewRecorder()
	Dependencies{
		Sites:       &siteStoreResetStub{deleted: 1},
		CookieCloud: cookieCloudSyncStub{err: context.Canceled},
	}.resetSites(recorder, canceledRequest)
	if recorder.Code != http.StatusServiceUnavailable || decodeResponse(t, recorder).Message != "站点重置已取消" {
		t.Fatalf("取消响应 = (%d, %q)", recorder.Code, recorder.Body.String())
	}
}

// TestSiteIconHandler 覆盖站点图标端点：存储缺失、ID 非法、站点不存在、查询失败、
// 图标源未接入、图标源报错、无图标、base64 优先与 URL 回退。
func TestSiteIconHandler(t *testing.T) {
	serve := func(dependencies Dependencies, siteID string) *httptest.ResponseRecorder {
		router := chi.NewRouter()
		router.Get("/api/v1/site/icon/{site_id}", dependencies.siteIcon)
		recorder := httptest.NewRecorder()
		router.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/api/v1/site/icon/"+siteID, nil))
		return recorder
	}

	if got := serve(Dependencies{}, "1").Code; got != http.StatusServiceUnavailable {
		t.Fatalf("存储缺失状态码 = %d", got)
	}
	if got := serve(Dependencies{Sites: &siteStoreStub{}}, "abc").Code; got != http.StatusUnprocessableEntity {
		t.Fatalf("非法 ID 状态码 = %d", got)
	}
	if got := serve(Dependencies{Sites: &siteStoreStub{runtimeErr: pgx.ErrNoRows}}, "7").Code; got != http.StatusNotFound {
		t.Fatalf("站点不存在状态码 = %d", got)
	}
	if got := serve(Dependencies{Sites: &siteStoreStub{runtimeErr: errors.New("查询失败")}}, "7").Code; got != http.StatusInternalServerError {
		t.Fatalf("查询失败状态码 = %d", got)
	}

	found := Dependencies{
		Sites:     &siteStoreStub{runtime: database.Site{ID: 7, Domain: "example.com"}},
		SiteIcons: siteIconStub{base64: "data:image/png;base64,AA==", url: "https://cdn/icon.png", found: true},
	}
	if response := decodeResponse(t, serve(found, "7")); !response.Success {
		t.Fatalf("图标命中响应 = %+v", response)
	}
	if data, ok := decodeResponse(t, serve(found, "7")).Data.(map[string]any); !ok || data["icon"] != "data:image/png;base64,AA==" {
		t.Fatalf("图标数据 = %#v", decodeResponse(t, serve(found, "7")).Data)
	}

	urlOnly := Dependencies{
		Sites:     &siteStoreStub{runtime: database.Site{ID: 7, URL: "https://example.com"}},
		SiteIcons: siteIconStub{url: "https://cdn/icon.png", found: true},
	}
	if data, ok := decodeResponse(t, serve(urlOnly, "7")).Data.(map[string]any); !ok || data["icon"] != "https://cdn/icon.png" {
		t.Fatalf("URL 图标数据 = %#v", data)
	}

	missing := Dependencies{Sites: &siteStoreStub{runtime: database.Site{ID: 7}}}
	if response := decodeResponse(t, serve(missing, "7")); response.Success || response.Message != "站点图标不存在！" {
		t.Fatalf("图标源未接入响应 = %+v", response)
	}

	notFound := Dependencies{
		Sites:     &siteStoreStub{runtime: database.Site{ID: 7}},
		SiteIcons: siteIconStub{},
	}
	if response := decodeResponse(t, serve(notFound, "7")); response.Success || response.Message != "站点图标不存在！" {
		t.Fatalf("无图标响应 = %+v", response)
	}

	broken := Dependencies{
		Sites:     &siteStoreStub{runtime: database.Site{ID: 7}},
		SiteIcons: siteIconStub{err: errors.New("查询图标失败")},
	}
	if got := serve(broken, "7").Code; got != http.StatusInternalServerError {
		t.Fatalf("图标源报错状态码 = %d", got)
	}
}

// TestRSSSitesHandler 覆盖订阅站点列表：存储缺失、查询失败、配置查询失败、
// 按配置过滤与不过滤两种输出。
func TestRSSSitesHandler(t *testing.T) {
	request := httptest.NewRequest(http.MethodGet, "/api/v1/site/rss", nil)
	sites := []database.Site{{ID: 1, Name: "站点一", Enabled: true}, {ID: 2, Name: "站点二"}}

	assertSites := func(recorder *httptest.ResponseRecorder, want int) {
		t.Helper()
		var items []map[string]any
		if err := json.Unmarshal(recorder.Body.Bytes(), &items); err != nil {
			t.Fatalf("解析站点列表: %v（body=%q）", err, recorder.Body.String())
		}
		if len(items) != want {
			t.Fatalf("站点数 = %d, want %d（body=%q）", len(items), want, recorder.Body.String())
		}
	}

	recorder := httptest.NewRecorder()
	(Dependencies{}).rssSites(recorder, request)
	if recorder.Code != http.StatusServiceUnavailable {
		t.Fatalf("存储缺失状态码 = %d", recorder.Code)
	}

	recorder = httptest.NewRecorder()
	Dependencies{Sites: &siteStoreStub{listErr: errors.New("查询失败")}}.rssSites(recorder, request)
	if recorder.Code != http.StatusInternalServerError {
		t.Fatalf("查询失败状态码 = %d", recorder.Code)
	}

	recorder = httptest.NewRecorder()
	Dependencies{Sites: &siteStoreSettingsStub{
		siteStoreStub: siteStoreStub{sites: sites},
		settingErr:    errors.New("配置查询失败"),
	}}.rssSites(recorder, request)
	if recorder.Code != http.StatusInternalServerError {
		t.Fatalf("配置查询失败状态码 = %d", recorder.Code)
	}

	recorder = httptest.NewRecorder()
	Dependencies{Sites: &siteStoreSettingsStub{
		siteStoreStub: siteStoreStub{sites: sites},
		selected:      []int{2},
	}}.rssSites(recorder, request)
	assertSites(recorder, 1)

	recorder = httptest.NewRecorder()
	Dependencies{Sites: &siteStoreSettingsStub{
		siteStoreStub: siteStoreStub{sites: sites},
	}}.rssSites(recorder, request)
	assertSites(recorder, 2)
}
