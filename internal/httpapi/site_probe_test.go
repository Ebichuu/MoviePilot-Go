package httpapi

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/moviepilot-go/moviepilot-go/internal/config"
	"github.com/moviepilot-go/moviepilot-go/internal/database"
	"github.com/moviepilot-go/moviepilot-go/internal/indexer"
)

type siteProbeTestAdapter struct {
	id    int
	name  string
	probe func(context.Context) error
}

func (a siteProbeTestAdapter) Name() string { return a.name }
func (a siteProbeTestAdapter) SiteID() int  { return a.id }
func (a siteProbeTestAdapter) Search(context.Context, indexer.Query) ([]indexer.Resource, error) {
	return []indexer.Resource{}, nil
}
func (a siteProbeTestAdapter) TestSite(ctx context.Context) error {
	if a.probe == nil {
		return nil
	}
	return a.probe(ctx)
}

type unsupportedSiteProbeAdapter struct {
	id   int
	name string
}

func (a unsupportedSiteProbeAdapter) Name() string { return a.name }
func (a unsupportedSiteProbeAdapter) SiteID() int  { return a.id }
func (a unsupportedSiteProbeAdapter) Search(context.Context, indexer.Query) ([]indexer.Resource, error) {
	return []indexer.Resource{}, nil
}

func TestSiteProbeReturnsV2ResultWithoutManagePermission(t *testing.T) {
	coordinator, err := indexer.NewCoordinator(2)
	if err != nil {
		t.Fatal(err)
	}
	defer coordinator.Close()
	store := &fakeSiteStore{sites: map[int]database.Site{7: {
		ID: 7, Name: "Fixture", URL: "https://fixture.example", Adapter: "fixture", Enabled: true,
		Config: map[string]any{"timeout": 5},
	}}}
	deps := Dependencies{
		Sites: store, Search: coordinator,
		SearchAdapters: []indexer.Adapter{siteProbeTestAdapter{id: 7, name: "Fixture"}},
	}
	router := chi.NewRouter()
	deps.registerSiteCompatibility(router)
	response := httptest.NewRecorder()
	router.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/site/test/7", nil))

	if response.Code != http.StatusOK {
		t.Fatalf("site probe status=%d body=%s", response.Code, response.Body.String())
	}
	var result Response
	if err := json.Unmarshal(response.Body.Bytes(), &result); err != nil {
		t.Fatal(err)
	}
	data, ok := result.Data.(map[string]any)
	if !result.Success || result.Message != "连接成功" || !ok || len(data) != 0 {
		t.Fatalf("site probe response mismatch: %#v", result)
	}
}

func TestSiteProbeUsesRequestDeadlineAndReturnsSafeTimeout(t *testing.T) {
	coordinator, err := indexer.NewCoordinator(1)
	if err != nil {
		t.Fatal(err)
	}
	defer coordinator.Close()
	store := &fakeSiteStore{sites: map[int]database.Site{9: {
		ID: 9, Name: "Slow", URL: "https://slow.example", Adapter: "fixture", Enabled: true,
		Config: map[string]any{"timeout": 120},
	}}}
	adapter := siteProbeTestAdapter{id: 9, name: "Slow", probe: func(ctx context.Context) error {
		<-ctx.Done()
		return ctx.Err()
	}}
	deps := Dependencies{
		Config: config.Config{RequestTimeout: 25 * time.Millisecond},
		Sites:  store, Search: coordinator, SearchAdapters: []indexer.Adapter{adapter},
	}
	router := chi.NewRouter()
	deps.registerSiteCompatibility(router)
	request := httptest.NewRequest(http.MethodGet, "/site/test/9", nil)
	response := httptest.NewRecorder()
	router.ServeHTTP(response, request)

	var result Response
	if err := json.Unmarshal(response.Body.Bytes(), &result); err != nil {
		t.Fatal(err)
	}
	if response.Code != http.StatusOK || result.Success || result.Message != "连接超时" {
		t.Fatalf("timeout response mismatch: status=%d result=%#v", response.Code, result)
	}
}

func TestSiteProbeReportsMissingAndUnavailableAdapters(t *testing.T) {
	coordinator, err := indexer.NewCoordinator(1)
	if err != nil {
		t.Fatal(err)
	}
	defer coordinator.Close()
	store := &fakeSiteStore{sites: map[int]database.Site{
		1: {ID: 1, Name: "Unsupported", URL: "https://unsupported.example", Adapter: "declarative", Enabled: true},
		2: {ID: 2, Name: "Disabled", URL: "https://disabled.example", Adapter: "fixture", Enabled: false},
		3: {ID: 3, Name: "NotLoaded", URL: "https://notloaded.example", Adapter: "fixture", Enabled: true},
		4: {ID: 4, Name: "Proxy", URL: "https://proxy.example", Adapter: "fixture", Enabled: true, Config: map[string]any{"proxy": 1}},
		5: {ID: 5, Name: "Render", URL: "https://render.example", Adapter: "fixture", Enabled: true, Config: map[string]any{"render": true}},
	}}
	deps := Dependencies{
		Sites: store, Search: coordinator,
		SearchAdapters: []indexer.Adapter{unsupportedSiteProbeAdapter{id: 1, name: "Unsupported"}},
		BuildSiteAdapter: func(site database.Site) (indexer.Adapter, error) {
			if site.ID == 2 {
				return siteProbeTestAdapter{id: site.ID, name: site.Name}, nil
			}
			return nil, nil
		},
	}
	router := chi.NewRouter()
	deps.registerSiteCompatibility(router)

	tests := []struct {
		path    string
		status  int
		success bool
		message string
	}{
		{path: "/site/test/not-an-id", status: http.StatusUnprocessableEntity, message: "站点 ID 无效"},
		{path: "/site/test/99", status: http.StatusNotFound, message: "站点 99 不存在"},
		{path: "/site/test/1", status: http.StatusOK, message: "当前站点适配器不支持连接测试"},
		{path: "/site/test/2", status: http.StatusOK, success: true, message: "连接成功"},
		{path: "/site/test/3", status: http.StatusOK, message: "站点适配器未加载"},
		{path: "/site/test/4", status: http.StatusOK, message: "代理站点连接测试尚未支持"},
		{path: "/site/test/5", status: http.StatusOK, message: "仿真渲染站点连接测试尚未支持"},
	}
	for _, test := range tests {
		response := httptest.NewRecorder()
		router.ServeHTTP(response, httptest.NewRequest(http.MethodGet, test.path, nil))
		var result Response
		if err := json.Unmarshal(response.Body.Bytes(), &result); err != nil {
			t.Fatalf("%s decode response: %v", test.path, err)
		}
		if response.Code != test.status || result.Success != test.success || result.Message != test.message {
			t.Errorf("%s response mismatch: status=%d result=%#v", test.path, response.Code, result)
		}
	}
}
