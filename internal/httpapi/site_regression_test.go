package httpapi

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"

	"github.com/moviepilot-go/moviepilot-go/internal/database"
)

func TestSiteRequestAcceptsBooleanProxyAndRender(t *testing.T) {
	request := httptest.NewRequest(http.MethodPost, "/api/v1/site", strings.NewReader(`{
		"name":"Fixture",
		"url":"https://fixture.example",
		"adapter":"declarative",
		"proxy":true,
		"render":false
	}`))

	input, err := decodeSiteRequest(request)
	if err != nil {
		t.Fatalf("boolean proxy/render should be accepted: %v", err)
	}
	site, err := input.toSite(true, database.Site{})
	if err != nil {
		t.Fatal(err)
	}
	if site.Config["proxy"] != 1 || site.Config["render"] != 0 {
		t.Fatalf("boolean proxy/render were not normalized to V2 integer flags: %#v", site.Config)
	}
}

func TestNormalizedSiteDomainCanonicalizesHost(t *testing.T) {
	if got := normalizedSiteDomain("https://Example.COM:443/path"); got != "example.com" {
		t.Fatalf("normalizedSiteDomain() = %q, want %q", got, "example.com")
	}
}

func TestSiteViewPrefersPersistedDomain(t *testing.T) {
	view := siteView(database.Site{
		ID:      7,
		Name:    "Fixture",
		Domain:  "persisted.example",
		URL:     "https://url-derived.example/path",
		Adapter: "declarative",
		Enabled: true,
	})

	if got := view["domain"]; got != "persisted.example" {
		t.Fatalf("siteView domain = %#v, want persisted domain", got)
	}
}

func TestSiteViewNormalizesBooleanFlagsFromStoredConfig(t *testing.T) {
	view := siteView(database.Site{Config: map[string]any{"proxy": true, "render": false}})
	if view["proxy"] != 1 || view["render"] != 0 {
		t.Fatalf("stored boolean flags were not normalized: %#v", view)
	}
}

func TestSiteCreatePayloadGeneratesDomain(t *testing.T) {
	request := httptest.NewRequest(http.MethodPost, "/api/v1/site", strings.NewReader(`{
		"name":"Fixture",
		"url":"https://Example.COM:443/path",
		"adapter":"declarative"
	}`))

	input, err := decodeSiteRequest(request)
	if err != nil {
		t.Fatal(err)
	}
	site, err := input.toSite(true, database.Site{})
	if err != nil {
		t.Fatal(err)
	}
	if site.Domain != "example.com" {
		t.Fatalf("created site domain = %q, want %q", site.Domain, "example.com")
	}
}

func TestSiteCategoryRouteReturnsStableV2Payload(t *testing.T) {
	router := chi.NewRouter()
	router.Route("/api/v1", func(api chi.Router) {
		(Dependencies{}).registerSiteCompatibility(api)
	})

	response := httptest.NewRecorder()
	router.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/api/v1/site/category/7", nil))

	if response.Code != http.StatusOK {
		t.Fatalf("site category route status = %d, want %d; body=%s", response.Code, http.StatusOK, response.Body.String())
	}
	var categories []map[string]any
	if err := json.Unmarshal(response.Body.Bytes(), &categories); err != nil {
		t.Fatalf("site category payload is not a V2 JSON array: %v; body=%s", err, response.Body.String())
	}
	if len(categories) != 0 {
		t.Fatalf("unsupported site category payload = %#v, want empty array", categories)
	}
}
