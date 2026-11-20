package httpapi

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"
	"github.com/golang-jwt/jwt/v5"
	"github.com/jackc/pgx/v5"

	"github.com/moviepilot-go/moviepilot-go/internal/auth"
	"github.com/moviepilot-go/moviepilot-go/internal/config"
	"github.com/moviepilot-go/moviepilot-go/internal/database"
	"github.com/moviepilot-go/moviepilot-go/internal/indexer"
)

type siteUserDataFake struct {
	siteStoreBranchFake
	latest      []database.SiteUserData
	latestErr   error
	byDomain    []database.SiteUserData
	byDomainErr error
	saveErr     error
	saved       *database.SiteUserData
	savedDomain string
}

func (s *siteUserDataFake) LatestSiteUserData(context.Context) ([]database.SiteUserData, error) {
	return s.latest, s.latestErr
}

func (s *siteUserDataFake) SiteUserDataByDomain(
	_ context.Context, _, _ string,
) ([]database.SiteUserData, error) {
	return s.byDomain, s.byDomainErr
}

func (s *siteUserDataFake) SaveSiteUserData(
	_ context.Context, domain, _ string, data database.SiteUserData,
) error {
	s.saved, s.savedDomain = &data, domain
	return s.saveErr
}

// siteUserDataAdapterFake 同时是搜索适配器与站点用户数据抓取器。
type siteUserDataAdapterFake struct {
	siteID int
	data   indexer.SiteUserData
	err    error
	fetch  bool
}

func (a siteUserDataAdapterFake) Name() string { return "站U" }

func (a siteUserDataAdapterFake) SiteID() int { return a.siteID }

func (a siteUserDataAdapterFake) Search(context.Context, indexer.Query) ([]indexer.Resource, error) {
	return nil, nil
}

func (a siteUserDataAdapterFake) FetchSiteUserData(context.Context) (indexer.SiteUserData, error) {
	return a.data, a.err
}

// siteUserDataPlainAdapter 不实现 SiteUserDataFetcher。
type siteUserDataPlainAdapter struct{ siteID int }

func (a siteUserDataPlainAdapter) Name() string { return "站P" }

func (a siteUserDataPlainAdapter) SiteID() int { return a.siteID }

func (a siteUserDataPlainAdapter) Search(context.Context, indexer.Query) ([]indexer.Resource, error) {
	return nil, nil
}

type siteUserDataEffectsFake struct {
	err   error
	calls int
}

func (f *siteUserDataEffectsFake) AfterSiteUserDataRefresh(
	context.Context, int, string, string, indexer.SiteUserData,
) error {
	f.calls++
	return f.err
}

func siteUserDataRequest(target, siteID string, claims *auth.Claims) *http.Request {
	request := httptest.NewRequest(http.MethodGet, target, nil)
	rctx := chi.NewRouteContext()
	if siteID != "" {
		rctx.URLParams.Add("site_id", siteID)
	}
	ctx := context.WithValue(request.Context(), chi.RouteCtxKey, rctx)
	if claims != nil {
		ctx = context.WithValue(ctx, claimsContextKey, *claims)
	}
	return request.WithContext(ctx)
}

func siteUserDataClaims(username string) auth.Claims {
	return auth.Claims{Username: username, RegisteredClaims: jwt.RegisteredClaims{Subject: "1"}}
}

// TestLatestSiteUserDataBranches 覆盖最新快照列表的无存储、查询失败与正常映射。
func TestLatestSiteUserDataBranches(t *testing.T) {
	// 没有存储：V2 前缀返回 success=true + 空数组，v1 返回裸数组。
	recorder := httptest.NewRecorder()
	Dependencies{}.latestSiteUserData(recorder, siteUserDataRequest("/api/v2/site/userdata/latest", "", nil))
	if !strings.Contains(recorder.Body.String(), `"data":[]`) {
		t.Fatalf("无存储 v2 = %s", recorder.Body.String())
	}
	recorder = httptest.NewRecorder()
	Dependencies{}.latestSiteUserData(recorder, siteUserDataRequest("/api/v1/site/userdata/latest", "", nil))
	if strings.TrimSpace(recorder.Body.String()) != "[]" {
		t.Fatalf("无存储 v1 = %s", recorder.Body.String())
	}

	// 查询失败。
	recorder = httptest.NewRecorder()
	deps := Dependencies{Sites: &siteUserDataFake{latestErr: errors.New("boom")}}
	deps.latestSiteUserData(recorder, siteUserDataRequest("/api/v1/site/userdata/latest", "", nil))
	if recorder.Code != http.StatusInternalServerError {
		t.Fatalf("查询失败 = %d", recorder.Code)
	}

	// 正常映射。
	username := "alice"
	recorder = httptest.NewRecorder()
	deps = Dependencies{Sites: &siteUserDataFake{latest: []database.SiteUserData{
		{Domain: "a.example.com", Username: siteUserDataStringPtr(username), UpdatedDay: "2024-05-06"},
	}}}
	deps.latestSiteUserData(recorder, siteUserDataRequest("/api/v1/site/userdata/latest", "", nil))
	if !strings.Contains(recorder.Body.String(), "a.example.com") {
		t.Fatalf("正常映射 = %s", recorder.Body.String())
	}
}

// TestSiteUserDataHistoryBranches 覆盖站点历史快照的站点解析与空值归一。
func TestSiteUserDataHistoryBranches(t *testing.T) {
	sites := &siteUserDataFake{siteStoreBranchFake: siteStoreBranchFake{sites: map[int]database.Site{
		1: {ID: 1, Name: "站A", Domain: "a.example.com"},
	}}}
	cases := []struct {
		name    string
		deps    Dependencies
		siteID  string
		code    int
		message string
	}{
		{name: "站点 ID 非法", deps: Dependencies{Sites: sites}, siteID: "abc", code: http.StatusUnprocessableEntity},
		{name: "站点存储缺失", deps: Dependencies{}, siteID: "1", code: http.StatusServiceUnavailable},
		{name: "站点不存在", deps: Dependencies{Sites: &siteUserDataFake{siteStoreBranchFake: siteStoreBranchFake{
			siteErr: pgx.ErrNoRows,
		}}}, siteID: "2", code: http.StatusNotFound},
		{name: "站点查询失败", deps: Dependencies{Sites: &siteUserDataFake{siteStoreBranchFake: siteStoreBranchFake{
			siteErr: errors.New("boom"),
		}}}, siteID: "2", code: http.StatusInternalServerError},
		{name: "历史查询失败", deps: Dependencies{Sites: &siteUserDataFake{
			siteStoreBranchFake: siteStoreBranchFake{sites: sites.sites},
			byDomainErr:         errors.New("boom"),
		}}, siteID: "1", code: http.StatusInternalServerError},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			recorder := httptest.NewRecorder()
			tc.deps.siteUserDataHistory(recorder, siteUserDataRequest("/api/v2/site/userdata/history", tc.siteID, nil))
			if recorder.Code != tc.code {
				t.Fatalf("状态码 = %d，期望 %d（%s）", recorder.Code, tc.code, recorder.Body.String())
			}
		})
	}

	// 站点存在但没有历史存储：返回空数组且 success=false。
	recorder := httptest.NewRecorder()
	Dependencies{Sites: sites}.siteUserDataHistory(
		recorder, siteUserDataRequest("/api/v2/site/userdata/history", "1", nil))
	if !strings.Contains(recorder.Body.String(), `"data":[]`) {
		t.Fatalf("无历史存储 = %s", recorder.Body.String())
	}

	// 存储返回 nil 行也归一成空数组；有行时 success=true。
	recorder = httptest.NewRecorder()
	noRows := &siteUserDataFake{siteStoreBranchFake: siteStoreBranchFake{sites: sites.sites}}
	Dependencies{Sites: noRows}.siteUserDataHistory(
		recorder, siteUserDataRequest("/api/v2/site/userdata/history", "1", nil))
	if !strings.Contains(recorder.Body.String(), `"data":[]`) {
		t.Fatalf("nil 行 = %s", recorder.Body.String())
	}
	recorder = httptest.NewRecorder()
	withRows := &siteUserDataFake{
		siteStoreBranchFake: siteStoreBranchFake{sites: sites.sites},
		byDomain:            []database.SiteUserData{{Domain: "a.example.com"}},
	}
	Dependencies{Sites: withRows}.siteUserDataHistory(
		recorder, siteUserDataRequest("/api/v2/site/userdata/history", "1", nil))
	if !strings.Contains(recorder.Body.String(), `"success":true`) {
		t.Fatalf("有行 = %s", recorder.Body.String())
	}
}

// TestRefreshSiteUserDataBranches 覆盖刷新链路的每一处中断点。
func TestRefreshSiteUserDataBranches(t *testing.T) {
	sites := func() *siteUserDataFake {
		return &siteUserDataFake{siteStoreBranchFake: siteStoreBranchFake{sites: map[int]database.Site{
			1: {ID: 1, Name: "站A", Domain: "a.example.com", URL: "https://a.example.com"},
		}}}
	}
	coordinator, err := indexer.NewCoordinator(1)
	if err != nil {
		t.Fatalf("构造协调器: %v", err)
	}

	cases := []struct {
		name    string
		deps    Dependencies
		siteID  string
		code    int
		message string
	}{
		{name: "站点 ID 非法", deps: Dependencies{Sites: sites()}, siteID: "abc", code: http.StatusUnprocessableEntity},
		{name: "站点存储缺失", deps: Dependencies{}, siteID: "1", code: http.StatusServiceUnavailable},
		{name: "站点不存在", deps: Dependencies{Sites: &siteUserDataFake{
			siteStoreBranchFake: siteStoreBranchFake{siteErr: pgx.ErrNoRows},
		}}, siteID: "2", code: http.StatusNotFound},
		{name: "站点查询失败", deps: Dependencies{Sites: &siteUserDataFake{
			siteStoreBranchFake: siteStoreBranchFake{siteErr: errors.New("boom")},
		}}, siteID: "2", code: http.StatusInternalServerError},
		{name: "运行时站点构建失败", deps: Dependencies{Sites: &siteUserDataFake{
			siteStoreBranchFake: siteStoreBranchFake{
				sites:      sites().sites,
				runtimeErr: errors.New("boom"),
			},
		}, BuildSiteAdapter: func(database.Site) (indexer.Adapter, error) {
			return siteUserDataAdapterFake{siteID: 1}, nil
		}}, siteID: "1", code: http.StatusOK, message: "站点不支持索引或未通过用户认证！"},
		{name: "站点没有适配器", deps: Dependencies{Sites: sites()}, siteID: "1",
			code: http.StatusOK, message: "站点不支持索引或未通过用户认证！"},
		{name: "适配器不抓取用户数据", deps: Dependencies{
			Sites: sites(), BuildSiteAdapter: func(database.Site) (indexer.Adapter, error) {
				return siteUserDataPlainAdapter{siteID: 1}, nil
			},
		}, siteID: "1", code: http.StatusOK},
		{name: "刷新服务缺失", deps: Dependencies{
			Sites: sites(), BuildSiteAdapter: func(database.Site) (indexer.Adapter, error) {
				return siteUserDataAdapterFake{siteID: 1}, nil
			},
		}, siteID: "1", code: http.StatusServiceUnavailable},
		{name: "抓取失败降级成功", deps: Dependencies{
			Sites:  sites(),
			Search: coordinator,
			BuildSiteAdapter: func(database.Site) (indexer.Adapter, error) {
				return siteUserDataAdapterFake{siteID: 1, err: errors.New("boom")}, nil
			},
			Logger: discardHTTPAPILogger(),
		}, siteID: "1", code: http.StatusOK},
		{name: "写入存储缺失", deps: Dependencies{
			Sites:  sitesWithoutWriter(),
			Search: coordinator,
			BuildSiteAdapter: func(database.Site) (indexer.Adapter, error) {
				return siteUserDataAdapterFake{siteID: 1}, nil
			},
		}, siteID: "1", code: http.StatusServiceUnavailable},
		{name: "保存失败", deps: Dependencies{
			Sites:  sitesWithSaveError(),
			Search: coordinator,
			BuildSiteAdapter: func(database.Site) (indexer.Adapter, error) {
				return siteUserDataAdapterFake{siteID: 1}, nil
			},
		}, siteID: "1", code: http.StatusInternalServerError},
		{name: "副作用失败", deps: Dependencies{
			Sites:  sites(),
			Search: coordinator,
			BuildSiteAdapter: func(database.Site) (indexer.Adapter, error) {
				return siteUserDataAdapterFake{siteID: 1}, nil
			},
			SiteUserDataEffects: &siteUserDataEffectsFake{err: errors.New("boom")},
		}, siteID: "1", code: http.StatusInternalServerError},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			recorder := httptest.NewRecorder()
			tc.deps.refreshSiteUserData(recorder, siteUserDataRequest("/api/v2/site/userdata/refresh", tc.siteID, nil))
			if recorder.Code != tc.code {
				t.Fatalf("状态码 = %d，期望 %d（%s）", recorder.Code, tc.code, recorder.Body.String())
			}
			if tc.message != "" && !strings.Contains(recorder.Body.String(), tc.message) {
				t.Fatalf("响应 = %s，期望包含 %q", recorder.Body.String(), tc.message)
			}
		})
	}

	// 没有 BuildSiteAdapter 时按站点 ID 从搜索注册表里挑适配器。
	registryStore := sites()
	recorder := httptest.NewRecorder()
	Dependencies{
		Sites:          registryStore,
		Search:         coordinator,
		SearchRegistry: indexer.NewRegistry([]indexer.Adapter{siteUserDataAdapterFake{siteID: 1}}),
	}.refreshSiteUserData(recorder, siteUserDataRequest("/api/v2/site/userdata/refresh", "1", nil))
	if recorder.Code != http.StatusOK || !strings.Contains(recorder.Body.String(), `"success":true`) {
		t.Fatalf("注册表刷新 = %d %s", recorder.Code, recorder.Body.String())
	}

	// 正常刷新：保存到站点域、副作用只调一次、响应带抓取结果。
	store := sites()
	effects := &siteUserDataEffectsFake{}
	recorder = httptest.NewRecorder()
	Dependencies{
		Sites:  store,
		Search: coordinator,
		BuildSiteAdapter: func(database.Site) (indexer.Adapter, error) {
			return siteUserDataAdapterFake{siteID: 1, data: indexer.SiteUserData{
				Domain: "a.example.com", Username: siteUserDataStringPtr("alice"),
			}}, nil
		},
		SiteUserDataEffects: effects,
	}.refreshSiteUserData(recorder, siteUserDataRequest("/api/v2/site/userdata/refresh", "1", nil))
	if recorder.Code != http.StatusOK || !strings.Contains(recorder.Body.String(), `"success":true`) {
		t.Fatalf("正常刷新 = %d %s", recorder.Code, recorder.Body.String())
	}
	if store.saved == nil || store.savedDomain != "a.example.com" {
		t.Fatalf("保存结果 = %+v 域 = %q", store.saved, store.savedDomain)
	}
	if effects.calls != 1 {
		t.Fatalf("副作用调用次数 = %d", effects.calls)
	}
}

// sitesWithoutWriter 只把 Sites 当站点存储，不提供写入能力。
func sitesWithoutWriter() SiteStore {
	return &siteStoreOnlyFake{siteStoreBranchFake: siteStoreBranchFake{sites: map[int]database.Site{
		1: {ID: 1, Name: "站A", Domain: "a.example.com"},
	}}}
}

func sitesWithSaveError() SiteStore {
	fake := &siteUserDataFake{siteStoreBranchFake: siteStoreBranchFake{sites: map[int]database.Site{
		1: {ID: 1, Name: "站A", Domain: "a.example.com"},
	}}, saveErr: errors.New("boom")}
	return fake
}

type siteStoreOnlyFake struct {
	siteStoreBranchFake
}

// TestRequireSiteUserDataManage 覆盖权限中间件的三条出口。
func TestRequireSiteUserDataManage(t *testing.T) {
	reached := 0
	next := http.HandlerFunc(func(http.ResponseWriter, *http.Request) { reached++ })

	recorder := httptest.NewRecorder()
	Dependencies{}.requireSiteUserDataManage(next).
		ServeHTTP(recorder, siteUserDataRequest("/api/v2/site/userdata/refresh", "", nil))
	if recorder.Code != http.StatusBadRequest || reached != 0 {
		t.Fatalf("无登录态 = %d（reached=%d）", recorder.Code, reached)
	}

	plain := siteUserDataClaims("alice")
	recorder = httptest.NewRecorder()
	Dependencies{Users: downloadUserLookupFake{user: auth.User{ID: 1, Username: "alice"}}}.
		requireSiteUserDataManage(next).ServeHTTP(recorder, siteUserDataRequest("/x", "", &plain))
	if recorder.Code != http.StatusBadRequest || reached != 0 {
		t.Fatalf("无管理权限 = %d", recorder.Code)
	}

	manager := siteUserDataClaims("alice")
	recorder = httptest.NewRecorder()
	Dependencies{Users: downloadUserLookupFake{user: auth.User{
		ID: 1, Username: "alice", Permissions: map[string]any{"manage": true},
	}}}.requireSiteUserDataManage(next).ServeHTTP(recorder, siteUserDataRequest("/x", "", &manager))
	if reached != 1 || recorder.Code != http.StatusOK {
		t.Fatalf("有管理权限 = %d（reached=%d）", recorder.Code, reached)
	}

	superuser := siteUserDataClaims("root")
	recorder = httptest.NewRecorder()
	Dependencies{Users: downloadUserLookupFake{user: auth.User{
		ID: 1, Username: "root", SuperUser: true,
	}}}.requireSiteUserDataManage(next).ServeHTTP(recorder, siteUserDataRequest("/x", "", &superuser))
	if reached != 2 {
		t.Fatalf("超管未放行（reached=%d）", reached)
	}
}

// TestDatabaseSiteUserDataProjection 覆盖快照落库投影的空值与非空值。
func TestDatabaseSiteUserDataProjection(t *testing.T) {
	row := databaseSiteUserData(indexer.SiteUserData{Domain: "a.example.com", Username: siteUserDataStringPtr("alice")})
	if row.UserID != nil || row.UpdatedTime != "" {
		t.Fatalf("空值投影 = %+v", row)
	}
	userID, updated := "42", "2024-05-06 01:02:03"
	row = databaseSiteUserData(indexer.SiteUserData{
		Domain: "a.example.com", Username: siteUserDataStringPtr("alice"), UserID: &userID, UpdatedTime: &updated,
	})
	if row.UserID == nil || *row.UserID == "" || row.UpdatedTime != updated {
		t.Fatalf("非空投影 = %+v", row)
	}
	if row.Bonus == nil || row.SeedingInfo == nil || row.MessageUnreadContents == nil {
		t.Fatalf("指针字段未填充 = %+v", row)
	}
}

// TestSiteMessagesEnabledBranches 覆盖环境设置优先于配置开关。
func TestSiteMessagesEnabledBranches(t *testing.T) {
	if !(Dependencies{Config: config.Config{SiteMessage: true}}).siteMessagesEnabled() {
		t.Fatal("配置开关未生效")
	}
	if (Dependencies{}).siteMessagesEnabled() {
		t.Fatal("默认应关闭")
	}
	if !(Dependencies{EnvironmentSettings: siteMessagesEnabledFake{enabled: true}}).siteMessagesEnabled() {
		t.Fatal("环境设置未优先")
	}
}

type siteMessagesEnabledFake struct{ enabled bool }

func (f siteMessagesEnabledFake) SiteMessagesEnabled() bool { return f.enabled }

func (f siteMessagesEnabledFake) Snapshot() map[string]any { return map[string]any{} }

func (f siteMessagesEnabledFake) Update(
	context.Context, map[string]any,
) (map[string]config.EnvironmentUpdateResult, error) {
	return nil, nil
}

func siteUserDataStringPtr(value string) *string { return &value }
