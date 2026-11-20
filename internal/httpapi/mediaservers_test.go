package httpapi

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/moviepilot-go/moviepilot-go/internal/auth"
	"github.com/moviepilot-go/moviepilot-go/internal/config"
	"github.com/moviepilot-go/moviepilot-go/internal/mediaconfig"
)

const mediaServerTestID = "2cc1444f-854d-4adb-8cd8-2a6bb63585df"

func TestMediaServersSettingRedactsAndPreservesSecret(t *testing.T) {
	t.Parallel()
	store := &fakeMediaServerSettingStore{
		found: true,
		snapshot: mediaconfig.Snapshot{
			RequiredID: mediaServerTestID,
			Servers: []mediaconfig.Server{{
				ID: mediaServerTestID, Name: "home", Type: "emby", Enabled: true,
				Config: mediaconfig.Config{Host: "http://emby:8096/emby", APIKey: "secret-token"},
			}},
		},
	}
	handler, token := newMediaServerTestRouter(t, store, nil, nil)
	response := performDownloaderRequest(handler, http.MethodGet, "/api/v1/system/setting/MediaServers", "", token)
	if response.Code != http.StatusOK || strings.Contains(response.Body.String(), "secret-token") {
		t.Fatalf("GET status/body = %d %s", response.Code, response.Body.String())
	}
	var getPayload struct {
		Success bool `json:"success"`
		Data    struct {
			Required string               `json:"required"`
			Value    []mediaconfig.Server `json:"value"`
		} `json:"data"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &getPayload); err != nil {
		t.Fatal(err)
	}
	if !getPayload.Success || getPayload.Data.Required != mediaServerTestID || len(getPayload.Data.Value) != 1 || getPayload.Data.Value[0].Config.APIKey != "" {
		t.Fatalf("GET payload = %#v", getPayload)
	}

	body := `[{"id":"` + mediaServerTestID + `","name":" home ","type":"EMBY","enabled":true,"sync_interval":6,"sync_libraries":["all"],"path_mapping":[],"config":{"host":"http://emby:8096/emby","apikey":""}}]`
	response = performDownloaderRequest(handler, http.MethodPost, "/api/v1/system/setting/MediaServers", body, token)
	if response.Code != http.StatusOK || strings.Contains(response.Body.String(), "secret-token") {
		t.Fatalf("POST status/body = %d %s", response.Code, response.Body.String())
	}
	if store.saved.Servers[0].Config.APIKey != "secret-token" || store.saved.RequiredID != mediaServerTestID {
		t.Fatalf("saved snapshot = %#v", store.saved)
	}
}

func TestMediaServerV2RoutesKeepRawShapes(t *testing.T) {
	t.Parallel()
	runtime := &fakeMediaServerRuntime{}
	handler, token := newMediaServerTestRouter(t, nil, runtime, runtime)
	tests := []struct {
		method string
		path   string
		body   string
		want   string
	}{
		{method: http.MethodGet, path: "/api/v1/mediaserver/clients", want: `[{"name":"home","type":"emby"}]`},
		{method: http.MethodGet, path: "/api/v1/mediaserver/library?server=home", want: `[{"name":"Movies","server":"home","type":"emby","collection_type":"movies","locations":["/media"],"item_id":"library-1"}]`},
		{method: http.MethodGet, path: "/api/v1/mediaserver/latest?server=home", want: `[]`},
		{method: http.MethodGet, path: "/api/v1/mediaserver/playing?server=home", want: `[]`},
		{method: http.MethodPost, path: "/api/v1/mediaserver/exists_remote", body: `{"title":"Show","season":1,"future_field":{"nested":true}}`, want: `{"1":[1,2]}`},
		{method: http.MethodPost, path: "/api/v1/mediaserver/notexists", body: `{"title":"Show","future_field":true}`, want: `[{"episodes":[3],"season":1,"total_episode":3}]`},
	}
	for _, tt := range tests {
		t.Run(tt.method+tt.path, func(t *testing.T) {
			response := performDownloaderRequest(handler, tt.method, tt.path, tt.body, token)
			if response.Code != http.StatusOK || strings.TrimSpace(response.Body.String()) != tt.want {
				t.Fatalf("response = %d %s, want %s", response.Code, response.Body.String(), tt.want)
			}
		})
	}
}

func TestMediaServerPlayCatchAllAndExistsShape(t *testing.T) {
	t.Parallel()
	runtime := &fakeMediaServerRuntime{}
	handler, token := newMediaServerTestRouter(t, nil, runtime, runtime)
	response := performDownloaderRequest(handler, http.MethodGet, "/api/v1/mediaserver/play/folder/item-1", "", token)
	if response.Code != http.StatusOK || !strings.Contains(response.Body.String(), `"item_id":"folder/item-1"`) {
		t.Fatalf("play response = %d %s", response.Code, response.Body.String())
	}
	response = performDownloaderRequest(handler, http.MethodGet, "/api/v1/mediaserver/exists?tmdbid=123&season=1", "", token)
	if response.Code != http.StatusOK || !strings.Contains(response.Body.String(), `"id":"indexed-1"`) {
		t.Fatalf("exists response = %d %s", response.Code, response.Body.String())
	}
}

func TestReadRemoteMediaInfoRequestBoundsAndSingleValue(t *testing.T) {
	t.Parallel()
	valid := `{"title":"Show","tmdbid":123,"future_field":true}`
	tests := []struct {
		name     string
		body     string
		maxBytes int64
		wantErr  bool
	}{
		{name: "accept exact limit and future fields", body: valid, maxBytes: int64(len(valid))},
		{name: "reject over limit", body: valid, maxBytes: int64(len(valid) - 1), wantErr: true},
		{name: "reject second JSON value", body: valid + ` {}`, maxBytes: int64(len(valid) + 3), wantErr: true},
		{name: "reject malformed JSON", body: `{"title":`, maxBytes: 64, wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			request := &http.Request{Body: io.NopCloser(strings.NewReader(tt.body))}
			got, err := readRemoteMediaInfoRequest(request, tt.maxBytes)
			if tt.wantErr {
				if err == nil {
					t.Fatal("readRemoteMediaInfoRequest() error = nil")
				}
				return
			}
			if err != nil {
				t.Fatalf("readRemoteMediaInfoRequest() error = %v", err)
			}
			if got.Title != "Show" || got.TMDBID != 123 {
				t.Fatalf("readRemoteMediaInfoRequest() = %#v", got)
			}
		})
	}
	if _, err := readRemoteMediaInfoRequest(nil, 64); err == nil {
		t.Fatal("nil request accepted")
	}
}

type fakeMediaServerSettingStore struct {
	found    bool
	snapshot mediaconfig.Snapshot
	saved    mediaconfig.Snapshot
}

func (s *fakeMediaServerSettingStore) LoadMediaServers(context.Context) (mediaconfig.Snapshot, bool, error) {
	return s.snapshot.Clone(), s.found, nil
}

func (s *fakeMediaServerSettingStore) SaveMediaServers(_ context.Context, servers []mediaconfig.Server) (mediaconfig.Snapshot, error) {
	snapshot, _, err := mediaconfig.Normalize(servers)
	if err != nil {
		return mediaconfig.Snapshot{}, err
	}
	s.saved = snapshot.Clone()
	s.snapshot = snapshot.Clone()
	s.found = true
	return snapshot, nil
}

type fakeMediaServerRuntime struct{}

func (*fakeMediaServerRuntime) ListMediaServerClients(context.Context) []MediaServerClient {
	return []MediaServerClient{{Name: "home", Type: "emby"}}
}

func (*fakeMediaServerRuntime) PlayMediaServerItem(_ context.Context, itemID string) (MediaServerPlayback, bool, error) {
	return MediaServerPlayback{URL: "http://emby/web/index.html#!/item?id=" + itemID, ItemID: itemID, ServerID: "server-1", ServerType: "emby"}, true, nil
}

func (*fakeMediaServerRuntime) ListMediaServerLibraries(context.Context, string, bool) ([]MediaServerLibrary, error) {
	return []MediaServerLibrary{{Name: "Movies", Server: "home", Type: "emby", Collection: "movies", Locations: []string{"/media"}, ItemID: "library-1"}}, nil
}

func (*fakeMediaServerRuntime) ListMediaServerLatest(context.Context, string, int) ([]MediaServerPlayItem, error) {
	return []MediaServerPlayItem{}, nil
}

func (*fakeMediaServerRuntime) ListMediaServerPlaying(context.Context, string, int) ([]MediaServerPlayItem, error) {
	return []MediaServerPlayItem{}, nil
}

func (*fakeMediaServerRuntime) ExistingEpisodes(context.Context, RemoteMediaInfo) (map[int][]int, error) {
	return map[int][]int{1: {1, 2}}, nil
}

func (*fakeMediaServerRuntime) MissingEpisodes(context.Context, RemoteMediaInfo) ([]map[string]any, error) {
	return []map[string]any{{"season": 1, "episodes": []int{3}, "total_episode": 3}}, nil
}

func (*fakeMediaServerRuntime) FindLocalMediaServerItem(context.Context, LocalMediaExistsQuery) (string, bool, error) {
	return "indexed-1", true, nil
}

func newMediaServerTestRouter(
	t *testing.T,
	store MediaServerSettingStore,
	runtime *fakeMediaServerRuntime,
	remote any,
) (http.Handler, string) {
	t.Helper()
	authService := auth.NewService(nil, "media-server-http-test-jwt-secret", "media-server-http-test-resource-secret", time.Hour)
	user := auth.User{ID: 1, Username: "admin", SuperUser: true}
	token, err := authService.AccessToken(user)
	if err != nil {
		t.Fatal(err)
	}
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	deps := Dependencies{
		Config:              config.Config{MaxRequestBody: 1 << 20, FrontendDir: t.TempDir()},
		MediaServerSettings: store, Users: fakeUserLookup{user: user}, Auth: authService, Logger: logger,
		MediaServerRemote: remote,
	}
	if runtime != nil {
		deps.MediaServerClients = runtime
		deps.MediaServerPlayback = runtime
		deps.MediaServerLibraries = runtime
		deps.LocalMediaIndex = runtime
	}
	return NewRouter(deps), token
}
