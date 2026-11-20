package httpapi

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"
	"github.com/golang-jwt/jwt/v5"
	"github.com/jackc/pgx/v5"

	"github.com/moviepilot-go/moviepilot-go/internal/auth"
	"github.com/moviepilot-go/moviepilot-go/internal/database"
	"github.com/moviepilot-go/moviepilot-go/internal/indexer"
)

type fakeSiteStore struct {
	sites         map[int]database.Site
	nextID        int
	upsertCalls   int
	updateCalls   int
	deleteCalls   int
	priorityCalls int
	upsertErr     error
	updateErr     error
}

type fakeUserLookup struct {
	user auth.User
	err  error
}

func (s fakeUserLookup) UserByID(context.Context, int64) (auth.User, error) {
	return s.user, s.err
}

func (s *fakeSiteStore) ListSites(context.Context) ([]database.Site, error) {
	result := make([]database.Site, 0, len(s.sites))
	for _, site := range s.sites {
		result = append(result, site)
	}
	return result, nil
}

func (s *fakeSiteStore) SiteByID(_ context.Context, id int) (database.Site, error) {
	site, ok := s.sites[id]
	if !ok {
		return database.Site{}, pgx.ErrNoRows
	}
	return site, nil
}

func (s *fakeSiteStore) RuntimeSiteByID(ctx context.Context, id int) (database.Site, error) {
	return s.SiteByID(ctx, id)
}

func (s *fakeSiteStore) UpsertSite(_ context.Context, site database.Site) (int, error) {
	s.upsertCalls++
	if s.upsertErr != nil {
		return 0, s.upsertErr
	}
	if site.ID == 0 {
		s.nextID++
		site.ID = s.nextID
	}
	s.sites[site.ID] = site
	return site.ID, nil
}

func (s *fakeSiteStore) UpdateSite(_ context.Context, site database.Site) error {
	s.updateCalls++
	if s.updateErr != nil {
		return s.updateErr
	}
	if _, ok := s.sites[site.ID]; !ok {
		return pgx.ErrNoRows
	}
	s.sites[site.ID] = site
	return nil
}

func (s *fakeSiteStore) DeleteSite(_ context.Context, id int) (bool, error) {
	s.deleteCalls++
	if _, ok := s.sites[id]; !ok {
		return false, nil
	}
	delete(s.sites, id)
	return true, nil
}

func (s *fakeSiteStore) UpdateSitePriorities(_ context.Context, priorities []database.SitePriority) error {
	s.priorityCalls++
	for _, priority := range priorities {
		site, ok := s.sites[priority.ID]
		if !ok {
			return pgx.ErrNoRows
		}
		if site.Config == nil {
			site.Config = make(map[string]any)
		}
		site.Config["pri"] = priority.Priority
		s.sites[priority.ID] = site
	}
	return nil
}

func TestSiteManagementHidesSecretsAndReloadsAdapters(t *testing.T) {
	store := &fakeSiteStore{sites: make(map[int]database.Site), nextID: 10}
	reloads := 0
	validated := 0
	deps := Dependencies{
		Sites: store,
		SiteValidator: func(site database.Site) error {
			validated++
			if site.Config["cookie"] != "session=fixture" {
				t.Errorf("validator did not receive request secret: %#v", site.Config)
			}
			return nil
		},
		ReloadSites: func(context.Context) error { reloads++; return nil },
	}
	request := httptest.NewRequest(http.MethodPost, "/api/v1/site", strings.NewReader(`{"name":"Fixture","url":"https://fixture.example","adapter":"declarative","enabled":true,"config":{"format":"json"},"cookie":"session=fixture"}`))
	response := httptest.NewRecorder()
	deps.createSite(response, request)
	if response.Code != http.StatusOK || store.upsertCalls != 1 || reloads != 1 || validated != 1 {
		t.Fatalf("site create failed: status=%d upsert=%d reload=%d validated=%d", response.Code, store.upsertCalls, reloads, validated)
	}
	stored := store.sites[11]
	if stored.Config["cookie"] != "session=fixture" {
		t.Fatalf("fake store lost request secret: %#v", stored.Config)
	}
	listResponse := httptest.NewRecorder()
	deps.listSites(listResponse, httptest.NewRequest(http.MethodGet, "/api/v1/site", nil))
	var sites []map[string]any
	if err := json.Unmarshal(listResponse.Body.Bytes(), &sites); err != nil {
		t.Fatal(err)
	}
	if len(sites) != 1 || sites[0]["cookie"] != nil {
		t.Fatalf("site response leaked secret: %#v", sites)
	}
	if config, ok := sites[0]["config"].(map[string]any); !ok || config["cookie"] != nil {
		t.Fatalf("site config leaked secret: %#v", sites[0]["config"])
	}
}

func TestSiteManagementUpdatesAndDeletesByRouteID(t *testing.T) {
	store := &fakeSiteStore{sites: map[int]database.Site{7: {
		ID: 7, Name: "Fixture", URL: "https://fixture.example", Adapter: "declarative", Enabled: true,
		Config: map[string]any{"format": "json", "cookie": "preserved"},
	}}, nextID: 7}
	reloads := 0
	validatedSecret := false
	deps := Dependencies{
		Sites: store,
		SiteValidator: func(site database.Site) error {
			validatedSecret = site.Config["cookie"] == "preserved"
			return nil
		},
		ReloadSites: func(context.Context) error { reloads++; return nil },
	}
	request := httptest.NewRequest(http.MethodPut, "/api/v1/site/7", strings.NewReader(`{"enabled":false,"config":{"format":"rss"}}`))
	route := chi.NewRouteContext()
	route.URLParams.Add("siteid", "7")
	request = request.WithContext(context.WithValue(request.Context(), chi.RouteCtxKey, route))
	response := httptest.NewRecorder()
	deps.updateSite(response, request)
	if response.Code != http.StatusOK || store.updateCalls != 1 || reloads != 1 || store.sites[7].Enabled || !validatedSecret {
		t.Fatalf("site update failed: status=%d update=%d reload=%d site=%#v", response.Code, store.updateCalls, reloads, store.sites[7])
	}
	deleteRequest := httptest.NewRequest(http.MethodDelete, "/api/v1/site/7", nil)
	deleteRequest = deleteRequest.WithContext(context.WithValue(deleteRequest.Context(), chi.RouteCtxKey, route))
	deleteResponse := httptest.NewRecorder()
	deps.deleteSite(deleteResponse, deleteRequest)
	if deleteResponse.Code != http.StatusOK || store.deleteCalls != 1 || reloads != 2 {
		t.Fatalf("site delete failed: status=%d delete=%d reload=%d", deleteResponse.Code, store.deleteCalls, reloads)
	}
	if _, err := store.SiteByID(context.Background(), 7); !errors.Is(err, pgx.ErrNoRows) {
		t.Fatal("site was not deleted")
	}
}

func TestSiteRequestAcceptsV2FieldsAndInfersAdapter(t *testing.T) {
	request := httptest.NewRequest(http.MethodPost, "/api/v1/site/", strings.NewReader(`{
		"id":99,
		"url":"https://sunnypt.example/",
		"rss":"https://sunnypt.example/rss",
		"cookie":"session=fixture",
		"ua":"MoviePilot-Test",
		"apikey":"api-secret",
		"token":"token-secret",
		"pri":3,
		"proxy":1,
		"render":0,
		"public":0,
		"timeout":20,
		"limit_interval":60,
		"limit_count":2,
		"limit_seconds":5,
		"is_active":false,
		"downloader":"qb"
	}`))
	input, err := decodeSiteRequest(request)
	if err != nil {
		t.Fatal(err)
	}
	site, err := input.toSite(true, database.Site{})
	if err != nil {
		t.Fatal(err)
	}
	if site.ID != 0 || site.Name != "sunnypt.example" || site.Adapter != "SunnyPT" || site.Enabled {
		t.Fatalf("V2 payload mapping failed: %#v", site)
	}
	if site.Config["pri"] != 3 || site.Config["timeout"] != 20 || site.Config["token"] != "token-secret" {
		t.Fatalf("V2 config fields missing: %#v", site.Config)
	}
	view := siteView(site)
	if view["is_active"] != false || view["pri"] != 3 || view["proxy"] != 1 || view["timeout"] != 20 {
		t.Fatalf("V2 response fields missing: %#v", view)
	}
	if view["cookie"] != nil || view["apikey"] != nil || view["token"] != nil {
		t.Fatalf("V2 response leaked credentials: %#v", view)
	}
}

func TestSiteUpdateRejectsMismatchedBodyAndRouteIDs(t *testing.T) {
	store := &fakeSiteStore{sites: map[int]database.Site{
		7: {ID: 7, Name: "Fixture", URL: "https://fixture.example", Adapter: "declarative", Enabled: true, Config: map[string]any{"format": "rss"}},
		8: {ID: 8, Name: "Other", URL: "https://other.example", Adapter: "declarative", Enabled: true, Config: map[string]any{"format": "rss"}},
	}}
	deps := Dependencies{Sites: store}
	request := httptest.NewRequest(http.MethodPut, "/api/v1/site/7", strings.NewReader(`{"id":8,"is_active":false}`))
	route := chi.NewRouteContext()
	route.URLParams.Add("siteid", "7")
	request = request.WithContext(context.WithValue(request.Context(), chi.RouteCtxKey, route))
	response := httptest.NewRecorder()
	deps.updateSite(response, request)
	if response.Code != http.StatusBadRequest || store.updateCalls != 0 {
		t.Fatalf("mismatched IDs were accepted: status=%d updates=%d", response.Code, store.updateCalls)
	}
}

func TestSiteCompatibilityRoutesDoNotFallThroughToSiteID(t *testing.T) {
	store := &fakeSiteStore{sites: map[int]database.Site{1: {
		ID: 1, Name: "Fixture", URL: "https://fixture.example/", Adapter: "declarative", Enabled: true,
		Config: map[string]any{"format": "rss"},
	}}}
	deps := Dependencies{
		Sites: store,
		Users: fakeUserLookup{user: auth.User{ID: 1, SuperUser: true}},
	}
	router := chi.NewRouter()
	deps.registerSiteCompatibility(router)
	paths := []struct {
		method string
		path   string
		body   string
	}{
		{http.MethodGet, "/site/auth", ""},
		{http.MethodPost, "/site/auth", `{}`},
		{http.MethodGet, "/site/cookiecloud", ""},
		{http.MethodGet, "/site/reset", ""},
		{http.MethodPost, "/site/priorities", `[{"id":1,"pri":3}]`},
		{http.MethodGet, "/site/cookie/1", ""},
		{http.MethodPost, "/site/cookie/1", `{}`},
		{http.MethodGet, "/site/category/1", ""},
		{http.MethodGet, "/site/domain/fixture.example", ""},
		{http.MethodGet, "/site/icon/1", ""},
		{http.MethodGet, "/site/mapping", ""},
		{http.MethodGet, "/site/resource/1", ""},
		{http.MethodGet, "/site/rss", ""},
		{http.MethodGet, "/site/statistic", ""},
		{http.MethodGet, "/site/statistic/fixture.example", ""},
		{http.MethodGet, "/site/supporting", ""},
		{http.MethodGet, "/site/test/1", ""},
		{http.MethodGet, "/site/userdata/latest", ""},
		{http.MethodGet, "/site/userdata/1", ""},
		{http.MethodPost, "/site/userdata/1", `{}`},
	}
	for _, item := range paths {
		req := httptest.NewRequest(item.method, item.path, strings.NewReader(item.body))
		req = req.WithContext(context.WithValue(req.Context(), claimsContextKey, auth.Claims{
			SuperUser: true, RegisteredClaims: jwt.RegisteredClaims{Subject: "1"},
		}))
		response := httptest.NewRecorder()
		router.ServeHTTP(response, req)
		if response.Code == http.StatusNotFound {
			t.Errorf("compatibility route fell through: %s %s", item.method, item.path)
		}
	}
	if store.priorityCalls != 1 || store.sites[1].Config["pri"] != 3 {
		t.Fatalf("priority compatibility route did not update store: %#v", store.sites[1].Config)
	}
}

func TestSiteCreateReportsPersistedReloadFailure(t *testing.T) {
	store := &fakeSiteStore{sites: make(map[int]database.Site), nextID: 4}
	deps := Dependencies{
		Sites:         store,
		SiteValidator: func(database.Site) error { return nil },
		ReloadSites:   func(context.Context) error { return errors.New("database temporarily unavailable") },
	}
	request := httptest.NewRequest(http.MethodPost, "/api/v1/site/", strings.NewReader(`{
		"name":"Fixture","url":"https://fixture.example","adapter":"declarative","config":{"format":"rss"}
	}`))
	response := httptest.NewRecorder()
	deps.createSite(response, request)
	if response.Code != http.StatusServiceUnavailable || store.upsertCalls != 1 {
		t.Fatalf("reload failure was not explicit: status=%d writes=%d body=%s", response.Code, store.upsertCalls, response.Body.String())
	}
	var result Response
	if err := json.Unmarshal(response.Body.Bytes(), &result); err != nil {
		t.Fatal(err)
	}
	data, ok := result.Data.(map[string]any)
	if result.Success || !ok || data["id"] != float64(5) || data["reload"] != false {
		t.Fatalf("reload failure response missing persisted state: %#v", result)
	}
}

func TestSiteCreateConflictReloadsPersistedSnapshot(t *testing.T) {
	store := &fakeSiteStore{sites: make(map[int]database.Site), upsertErr: database.ErrSiteConflict}
	reloads := 0
	deps := Dependencies{
		Sites:         store,
		SiteValidator: func(database.Site) error { return nil },
		ReloadSites:   func(context.Context) error { reloads++; return nil },
	}
	request := httptest.NewRequest(http.MethodPost, "/api/v1/site/", strings.NewReader(`{
		"name":"Fixture","url":"https://fixture.example","adapter":"declarative","config":{"format":"rss"}
	}`))
	response := httptest.NewRecorder()
	deps.createSite(response, request)
	if response.Code != http.StatusOK || reloads != 1 {
		t.Fatalf("duplicate create response mismatch: status=%d reloads=%d body=%s", response.Code, reloads, response.Body.String())
	}
	var result Response
	if err := json.Unmarshal(response.Body.Bytes(), &result); err != nil {
		t.Fatal(err)
	}
	if result.Success {
		t.Fatalf("duplicate create was reported as success: %#v", result)
	}
}

func TestSiteUpdateCanExplicitlyClearStringConfiguration(t *testing.T) {
	store := &fakeSiteStore{sites: map[int]database.Site{7: {
		ID: 7, Name: "Fixture", Domain: "fixture.example", URL: "https://fixture.example", Adapter: "declarative", Enabled: true,
		Config: map[string]any{
			"format": "rss", "rss": "https://fixture.example/rss", "ua": "MoviePilot",
			"cookie": "session=fixture", "apikey": "api-key", "token": "token",
			"filter": "rule", "downloader": "qb",
		},
	}}}
	deps := Dependencies{Sites: store}
	request := httptest.NewRequest(http.MethodPut, "/api/v1/site/7", strings.NewReader(`{
		"rss":"","ua":"","cookie":"","apikey":"","token":"","filter":"","downloader":""
	}`))
	route := chi.NewRouteContext()
	route.URLParams.Add("siteid", "7")
	request = request.WithContext(context.WithValue(request.Context(), chi.RouteCtxKey, route))
	response := httptest.NewRecorder()
	deps.updateSite(response, request)
	if response.Code != http.StatusOK {
		t.Fatalf("clear update failed: status=%d body=%s", response.Code, response.Body.String())
	}
	for _, key := range []string{"rss", "ua", "cookie", "apikey", "token", "filter", "downloader"} {
		if _, exists := store.sites[7].Config[key]; exists {
			t.Errorf("configuration %q was not cleared: %#v", key, store.sites[7].Config)
		}
	}
	if store.sites[7].Config["format"] != "rss" {
		t.Fatalf("unrelated configuration was lost: %#v", store.sites[7].Config)
	}
}

func TestSiteUpdateReportsDomainConflictAsV2Failure(t *testing.T) {
	store := &fakeSiteStore{
		sites: map[int]database.Site{7: {
			ID: 7, Name: "Fixture", Domain: "fixture.example", URL: "https://fixture.example", Adapter: "declarative", Enabled: true,
			Config: map[string]any{"format": "rss"},
		}},
		updateErr: database.ErrSiteConflict,
	}
	deps := Dependencies{Sites: store}
	request := httptest.NewRequest(http.MethodPut, "/api/v1/site/7", strings.NewReader(`{"url":"https://duplicate.example"}`))
	route := chi.NewRouteContext()
	route.URLParams.Add("siteid", "7")
	request = request.WithContext(context.WithValue(request.Context(), chi.RouteCtxKey, route))
	response := httptest.NewRecorder()
	deps.updateSite(response, request)
	if response.Code != http.StatusOK {
		t.Fatalf("domain conflict status=%d body=%s", response.Code, response.Body.String())
	}
	var result Response
	if err := json.Unmarshal(response.Body.Bytes(), &result); err != nil {
		t.Fatal(err)
	}
	if result.Success {
		t.Fatalf("domain conflict was reported as success: %#v", result)
	}
}

func TestSiteDeleteRetryReloadsPersistedSnapshot(t *testing.T) {
	store := &fakeSiteStore{sites: make(map[int]database.Site)}
	reloads := 0
	deps := Dependencies{Sites: store, ReloadSites: func(context.Context) error {
		reloads++
		return nil
	}}
	request := httptest.NewRequest(http.MethodDelete, "/api/v1/site/7", nil)
	route := chi.NewRouteContext()
	route.URLParams.Add("siteid", "7")
	request = request.WithContext(context.WithValue(request.Context(), chi.RouteCtxKey, route))
	response := httptest.NewRecorder()
	deps.deleteSite(response, request)
	if response.Code != http.StatusNotFound || reloads != 1 {
		t.Fatalf("delete retry did not repair runtime snapshot: status=%d reloads=%d", response.Code, reloads)
	}
}

func TestSiteResourceBrowseUsesSelectedAdapter(t *testing.T) {
	coordinator, err := indexer.NewCoordinator(1)
	if err != nil {
		t.Fatal(err)
	}
	defer coordinator.Close()
	deps := Dependencies{
		Search: coordinator,
		Users:  fakeUserLookup{user: auth.User{ID: 1, SuperUser: true}},
		SearchAdapters: []indexer.Adapter{searchTestAdapter{
			id: 1, name: "Fixture", resources: []indexer.Resource{{
				Title: "Fixture Movie 2026", DetailURL: "https://fixture.example/torrent/1", DownloadURL: "https://fixture.example/download/1",
			}}},
		},
	}
	router := chi.NewRouter()
	deps.registerSiteCompatibility(router)
	request := httptest.NewRequest(http.MethodGet, "/site/resource/1?keyword=Fixture&page=0", nil)
	request = request.WithContext(context.WithValue(request.Context(), claimsContextKey, auth.Claims{
		SuperUser: true, RegisteredClaims: jwt.RegisteredClaims{Subject: "1"},
	}))
	response := httptest.NewRecorder()
	router.ServeHTTP(response, request)
	if response.Code != http.StatusOK {
		t.Fatalf("resource browse failed: status=%d body=%s", response.Code, response.Body.String())
	}
	var resources []map[string]any
	if err := json.Unmarshal(response.Body.Bytes(), &resources); err != nil {
		t.Fatal(err)
	}
	if len(resources) != 1 || resources[0]["title"] != "Fixture Movie 2026" {
		t.Fatalf("resource browse response mismatch: %#v", resources)
	}
}
