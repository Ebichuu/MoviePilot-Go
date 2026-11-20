package httpapi

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"
	"github.com/golang-jwt/jwt/v5"

	"github.com/moviepilot-go/moviepilot-go/internal/auth"
	"github.com/moviepilot-go/moviepilot-go/internal/database"
	"github.com/moviepilot-go/moviepilot-go/internal/indexer"
)

type fakeSiteUserDataPostStore struct {
	*fakeSiteUserDataAPIStore
	saveCalls   int
	savedDomain string
	savedName   string
	savedData   database.SiteUserData
	saveErr     error
	operations  *[]string
}

func (s *fakeSiteUserDataPostStore) SaveSiteUserData(
	_ context.Context,
	domain string,
	name string,
	payload database.SiteUserData,
) error {
	if s.operations != nil {
		*s.operations = append(*s.operations, "save")
	}
	s.saveCalls++
	s.savedDomain = domain
	s.savedName = name
	s.savedData = payload
	return s.saveErr
}

type fakeSiteUserDataEffects struct {
	calls      int
	siteID     int
	siteName   string
	siteURL    string
	data       indexer.SiteUserData
	err        error
	operations *[]string
}

func (f *fakeSiteUserDataEffects) AfterSiteUserDataRefresh(
	_ context.Context,
	siteID int,
	siteName string,
	siteURL string,
	data indexer.SiteUserData,
) error {
	if f.operations != nil {
		*f.operations = append(*f.operations, "effects")
	}
	f.calls++
	f.siteID, f.siteName, f.siteURL, f.data = siteID, siteName, siteURL, data
	return f.err
}

type fakeSiteUserDataParserAdapter struct {
	siteID     int
	data       indexer.SiteUserData
	err        error
	parseCalls int
}

func (a *fakeSiteUserDataParserAdapter) Name() string { return "site-userdata-parser" }

func (a *fakeSiteUserDataParserAdapter) SiteID() int { return a.siteID }

func (a *fakeSiteUserDataParserAdapter) Search(context.Context, indexer.Query) ([]indexer.Resource, error) {
	return nil, nil
}

func (a *fakeSiteUserDataParserAdapter) FetchSiteUserData(context.Context) (indexer.SiteUserData, error) {
	a.parseCalls++
	return a.data, a.err
}

type fakeSiteUserDataNoParserAdapter struct {
	siteID int
}

func (a fakeSiteUserDataNoParserAdapter) Name() string { return "site-userdata-no-parser" }

func (a fakeSiteUserDataNoParserAdapter) SiteID() int { return a.siteID }

func (a fakeSiteUserDataNoParserAdapter) Search(context.Context, indexer.Query) ([]indexer.Resource, error) {
	return nil, nil
}

func TestRefreshSiteUserDataMatchesV2156PostContract(t *testing.T) {
	site := database.Site{
		ID: 7, Domain: "tracker.example", Name: "Tracker", URL: "https://tracker.example", Enabled: true,
	}
	completeData := completeSiteUserDataPostFixture()
	completePersistedData := databaseSiteUserData(completeData)
	successParser := &fakeSiteUserDataParserAdapter{siteID: site.ID, data: completeData}
	errorParser := &fakeSiteUserDataParserAdapter{
		siteID: site.ID,
		err:    errors.New("parser credentials expired"),
	}
	saveFailureParser := &fakeSiteUserDataParserAdapter{siteID: site.ID, data: completeData}

	tests := []struct {
		name           string
		path           string
		sites          map[int]database.Site
		hasBuilder     bool
		adapter        indexer.Adapter
		buildErr       error
		saveErr        error
		wantStatus     int
		wantSuccess    bool
		wantMessage    string
		wantEmptyData  bool
		wantData       *indexer.SiteUserData
		wantBuildCalls int
		parser         *fakeSiteUserDataParserAdapter
		wantParseCalls int
		wantSaveCalls  int
	}{
		{
			name: "v1 non integer is 422", path: "/api/v1/site/userdata/not-an-int",
			sites: map[int]database.Site{}, wantStatus: http.StatusUnprocessableEntity,
			wantMessage: "站点 ID 无效",
		},
		{
			name: "v2 zero is missing", path: "/api/v2/site/userdata/0",
			sites: map[int]database.Site{}, wantStatus: http.StatusNotFound,
			wantMessage: "站点 0 不存在",
		},
		{
			name: "v1 negative is missing", path: "/api/v1/site/userdata/-1",
			sites: map[int]database.Site{}, wantStatus: http.StatusNotFound,
			wantMessage: "站点 -1 不存在",
		},
		{
			name: "v2 unknown site is missing", path: "/api/v2/site/userdata/99",
			sites: map[int]database.Site{}, wantStatus: http.StatusNotFound,
			wantMessage: "站点 99 不存在",
		},
		{
			name: "v1 missing runtime indexer is compatible failure", path: "/api/v1/site/userdata/7",
			sites: map[int]database.Site{site.ID: site}, wantStatus: http.StatusOK,
			wantMessage: "站点不支持索引或未通过用户认证！", wantEmptyData: true,
		},
		{
			name: "v2 adapter build failure is compatible failure", path: "/api/v2/site/userdata/7",
			sites: map[int]database.Site{site.ID: site}, hasBuilder: true,
			buildErr: errors.New("legacy parser is unsupported"), wantStatus: http.StatusOK,
			wantMessage: "站点不支持索引或未通过用户认证！", wantEmptyData: true,
			wantBuildCalls: 1,
		},
		{
			name: "v1 adapter without parser returns empty success", path: "/api/v1/site/userdata/7",
			sites: map[int]database.Site{site.ID: site}, hasBuilder: true,
			adapter: fakeSiteUserDataNoParserAdapter{siteID: site.ID}, wantStatus: http.StatusOK,
			wantSuccess: true, wantEmptyData: true, wantBuildCalls: 1,
		},
		{
			name: "v2 parser success persists once and returns all data", path: "/api/v2/site/userdata/7",
			sites: map[int]database.Site{site.ID: site}, hasBuilder: true,
			adapter: successParser, wantStatus: http.StatusOK, wantSuccess: true,
			wantData: &completeData, wantBuildCalls: 1, parser: successParser,
			wantParseCalls: 1, wantSaveCalls: 1,
		},
		{
			name: "v1 parser error returns empty success without persistence", path: "/api/v1/site/userdata/7",
			sites: map[int]database.Site{site.ID: site}, hasBuilder: true,
			adapter: errorParser, wantStatus: http.StatusOK, wantSuccess: true,
			wantEmptyData: true, wantBuildCalls: 1, parser: errorParser, wantParseCalls: 1,
		},
		{
			name: "v2 persistence failure remains 500", path: "/api/v2/site/userdata/7",
			sites: map[int]database.Site{site.ID: site}, hasBuilder: true,
			adapter: saveFailureParser, saveErr: errors.New("postgres unavailable"),
			wantStatus: http.StatusInternalServerError, wantBuildCalls: 1,
			parser: saveFailureParser, wantParseCalls: 1, wantSaveCalls: 1,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			store := &fakeSiteUserDataPostStore{
				fakeSiteUserDataAPIStore: &fakeSiteUserDataAPIStore{
					fakeSiteStore: &fakeSiteStore{sites: tt.sites},
				},
				saveErr: tt.saveErr,
			}
			buildCalls := 0
			var builder func(database.Site) (indexer.Adapter, error)
			if tt.hasBuilder {
				builder = func(got database.Site) (indexer.Adapter, error) {
					buildCalls++
					if !reflect.DeepEqual(got, site) {
						t.Errorf("BuildSiteAdapter site = %#v, want %#v", got, site)
					}
					return tt.adapter, tt.buildErr
				}
			}
			router := siteUserDataPostRouter(t, store, builder)
			response := performSiteUserDataPostRequest(router, tt.path)

			if response.Code != tt.wantStatus {
				t.Fatalf("status = %d, want %d; body=%s", response.Code, tt.wantStatus, response.Body.String())
			}
			if buildCalls != tt.wantBuildCalls {
				t.Errorf("BuildSiteAdapter calls = %d, want %d", buildCalls, tt.wantBuildCalls)
			}
			if tt.parser != nil && tt.parser.parseCalls != tt.wantParseCalls {
				t.Errorf("ParseUserData calls = %d, want %d", tt.parser.parseCalls, tt.wantParseCalls)
			}
			if store.saveCalls != tt.wantSaveCalls {
				t.Errorf("SaveSiteUserData calls = %d, want %d", store.saveCalls, tt.wantSaveCalls)
			}
			if tt.wantSaveCalls == 1 {
				if store.savedDomain != site.Domain || store.savedName != site.Name ||
					!reflect.DeepEqual(store.savedData, completePersistedData) {
					t.Errorf(
						"saved = domain %q name %q data %#v, want %q %q %#v",
						store.savedDomain, store.savedName, store.savedData, site.Domain, site.Name, completePersistedData,
					)
				}
			}
			if tt.wantStatus == http.StatusInternalServerError {
				return
			}

			var result struct {
				Success bool            `json:"success"`
				Message string          `json:"message"`
				Data    json.RawMessage `json:"data"`
			}
			if err := json.Unmarshal(response.Body.Bytes(), &result); err != nil {
				t.Fatalf("decode response: %v; body=%s", err, response.Body.String())
			}
			if result.Success != tt.wantSuccess || result.Message != tt.wantMessage {
				t.Errorf(
					"response = success %v message %q, want %v %q; body=%s",
					result.Success, result.Message, tt.wantSuccess, tt.wantMessage, response.Body.String(),
				)
			}
			if tt.wantEmptyData && strings.TrimSpace(string(result.Data)) != "{}" {
				t.Errorf("data = %s, want {}", result.Data)
			}
			if tt.wantData != nil {
				assertCompleteSiteUserDataPostResponse(t, result.Data, *tt.wantData)
			}
		})
	}
}

func TestRefreshSiteUserDataRunsV2EffectsOnlyAfterSuccessfulSave(t *testing.T) {
	site := database.Site{
		ID: 7, Domain: "tracker.example", Name: "Tracker", URL: "https://tracker.example", Enabled: true,
	}
	sentinel := errors.New("side effect failed")
	tests := []struct {
		name           string
		parserErr      error
		saveErr        error
		effectsErr     error
		wantStatus     int
		wantOperations []string
		wantEffects    int
	}{
		{name: "success", wantStatus: http.StatusOK, wantOperations: []string{"save", "effects"}, wantEffects: 1},
		{name: "parser failure", parserErr: sentinel, wantStatus: http.StatusOK, wantOperations: []string{}},
		{name: "save failure", saveErr: sentinel, wantStatus: http.StatusInternalServerError, wantOperations: []string{"save"}},
		{name: "effect failure", effectsErr: sentinel, wantStatus: http.StatusInternalServerError,
			wantOperations: []string{"save", "effects"}, wantEffects: 1},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			operations := make([]string, 0, 2)
			data := completeSiteUserDataPostFixture()
			parser := &fakeSiteUserDataParserAdapter{siteID: site.ID, data: data, err: tt.parserErr}
			store := &fakeSiteUserDataPostStore{
				fakeSiteUserDataAPIStore: &fakeSiteUserDataAPIStore{
					fakeSiteStore: &fakeSiteStore{sites: map[int]database.Site{site.ID: site}},
				},
				saveErr: tt.saveErr, operations: &operations,
			}
			effects := &fakeSiteUserDataEffects{err: tt.effectsErr, operations: &operations}
			router := siteUserDataPostRouter(t, store, func(database.Site) (indexer.Adapter, error) {
				return parser, nil
			}, effects)

			response := performSiteUserDataPostRequest(router, "/api/v1/site/userdata/7")
			if response.Code != tt.wantStatus || !reflect.DeepEqual(operations, tt.wantOperations) ||
				effects.calls != tt.wantEffects {
				t.Fatalf(
					"status=%d operations=%#v effects=%d, want %d/%#v/%d; body=%s",
					response.Code, operations, effects.calls,
					tt.wantStatus, tt.wantOperations, tt.wantEffects, response.Body.String(),
				)
			}
			if effects.calls == 1 && (effects.siteID != site.ID || effects.siteName != site.Name ||
				effects.siteURL != site.URL || !reflect.DeepEqual(effects.data, data)) {
				t.Fatalf("effects args = %#v", effects)
			}
		})
	}
}

func siteUserDataPostRouter(
	t *testing.T,
	store *fakeSiteUserDataPostStore,
	builder func(database.Site) (indexer.Adapter, error),
	effects ...SiteUserDataEffects,
) http.Handler {
	t.Helper()
	deps := Dependencies{
		Sites:            store,
		Users:            fakeUserLookup{user: auth.User{ID: 1, SuperUser: true}},
		BuildSiteAdapter: builder,
	}
	if len(effects) > 0 {
		deps.SiteUserDataEffects = effects[0]
	}
	coordinator, err := indexer.NewCoordinator(2)
	if err != nil {
		t.Fatalf("NewCoordinator() error = %v", err)
	}
	t.Cleanup(coordinator.Close)
	deps.Search = coordinator
	router := chi.NewRouter()
	router.Route("/api/v1", deps.registerSiteCompatibility)
	router.Route("/api/v2", deps.registerSiteCompatibility)
	return router
}

func performSiteUserDataPostRequest(handler http.Handler, path string) *httptest.ResponseRecorder {
	request := httptest.NewRequest(http.MethodPost, path, strings.NewReader(`{}`))
	request = request.WithContext(context.WithValue(request.Context(), claimsContextKey, auth.Claims{
		SuperUser:        true,
		RegisteredClaims: jwt.RegisteredClaims{Subject: "1"},
	}))
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	return response
}

func assertCompleteSiteUserDataPostResponse(t *testing.T, raw json.RawMessage, want indexer.SiteUserData) {
	t.Helper()
	var got indexer.SiteUserData
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatalf("decode user data: %v; data=%s", err, raw)
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("user data = %#v, want %#v", got, want)
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(raw, &fields); err != nil {
		t.Fatalf("decode user data fields: %v; data=%s", err, raw)
	}
	if _, exists := fields["id"]; exists {
		t.Errorf("POST user data leaked database id: %s", raw)
	}
	if _, exists := fields["name"]; exists {
		t.Errorf("POST user data leaked database name: %s", raw)
	}
}

func completeSiteUserDataPostFixture() indexer.SiteUserData {
	username := "alice"
	userID := "42"
	userLevel := "VIP"
	joinAt := "2020-01-02"
	errMessage := ""
	updatedTime := "12:34:56"
	return indexer.SiteUserData{
		Domain: "parser.example", Username: &username, UserID: userID, UserLevel: &userLevel, JoinAt: &joinAt,
		Bonus: 12.5, Upload: 4096, Download: 1024, Ratio: 4,
		Seeding: 12, Leeching: 3, SeedingSize: 8192, LeechingSize: 256,
		SeedingInfo: [][]int64{{7, 8192}}, MessageUnread: 2,
		MessageUnreadContents: [][]any{{"title", "2026-08-16", "body", "source-1"}},
		ErrMsg:                &errMessage, UpdatedDay: "2026-08-16", UpdatedTime: &updatedTime,
	}
}

var _ SiteUserDataStore = (*fakeSiteUserDataPostStore)(nil)
var _ SiteUserDataEffects = (*fakeSiteUserDataEffects)(nil)
var _ indexer.SiteUserDataFetcher = (*fakeSiteUserDataParserAdapter)(nil)
var _ indexer.Adapter = (*fakeSiteUserDataParserAdapter)(nil)
var _ indexer.SiteIDAdapter = (*fakeSiteUserDataParserAdapter)(nil)
var _ indexer.Adapter = fakeSiteUserDataNoParserAdapter{}
var _ indexer.SiteIDAdapter = fakeSiteUserDataNoParserAdapter{}
