package httpapi

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/moviepilot-go/moviepilot-go/internal/auth"
	"github.com/moviepilot-go/moviepilot-go/internal/config"
	"github.com/moviepilot-go/moviepilot-go/internal/mediaconfig"
)

const mediaServersSettingPath = "/api/v1/system/setting/MediaServers"

func newMediaServerBranchRouter(t *testing.T, configure func(*Dependencies)) (http.Handler, string) {
	t.Helper()
	authService := auth.NewService(nil, "media-server-branch-jwt", "media-server-branch-resource", time.Hour)
	user := auth.User{ID: 1, Username: "admin", SuperUser: true}
	token, err := authService.AccessToken(user)
	if err != nil {
		t.Fatal(err)
	}
	deps := Dependencies{
		Config: config.Config{MaxRequestBody: 1 << 20, FrontendDir: t.TempDir()},
		Users:  fakeUserLookup{user: user}, Auth: authService,
		Logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
	}
	if configure != nil {
		configure(&deps)
	}
	return NewRouter(deps), token
}

// errorMediaServerStore 让加载/保存固定失败。
type errorMediaServerStore struct {
	loadErr   error
	saveErr   error
	found     bool
	snapshot  mediaconfig.Snapshot
	saveCalls int
}

func (s *errorMediaServerStore) LoadMediaServers(context.Context) (mediaconfig.Snapshot, bool, error) {
	if s.loadErr != nil {
		return mediaconfig.Snapshot{}, false, s.loadErr
	}
	return s.snapshot.Clone(), s.found, nil
}

func (s *errorMediaServerStore) SaveMediaServers(_ context.Context, servers []mediaconfig.Server) (mediaconfig.Snapshot, error) {
	s.saveCalls++
	if s.saveErr != nil {
		return mediaconfig.Snapshot{}, s.saveErr
	}
	snapshot, _, err := mediaconfig.Normalize(servers)
	if err != nil {
		return mediaconfig.Snapshot{}, err
	}
	return snapshot, nil
}

// brokenMediaServerRuntime 让所有媒体服务器来源返回错误。
type brokenMediaServerRuntime struct{}

var errBrokenMediaServer = errors.New("media server down")

func (brokenMediaServerRuntime) ListMediaServerClients(context.Context) []MediaServerClient {
	return []MediaServerClient{}
}

func (brokenMediaServerRuntime) PlayMediaServerItem(context.Context, string) (MediaServerPlayback, bool, error) {
	return MediaServerPlayback{}, false, errBrokenMediaServer
}

func (brokenMediaServerRuntime) ListMediaServerLibraries(context.Context, string, bool) ([]MediaServerLibrary, error) {
	return nil, errBrokenMediaServer
}

func (brokenMediaServerRuntime) ListMediaServerLatest(context.Context, string, int) ([]MediaServerPlayItem, error) {
	return nil, errBrokenMediaServer
}

func (brokenMediaServerRuntime) ListMediaServerPlaying(context.Context, string, int) ([]MediaServerPlayItem, error) {
	return nil, errBrokenMediaServer
}

func (brokenMediaServerRuntime) ExistingEpisodes(context.Context, RemoteMediaInfo) (map[int][]int, error) {
	return nil, errBrokenMediaServer
}

func (brokenMediaServerRuntime) MissingEpisodes(context.Context, RemoteMediaInfo) ([]map[string]any, error) {
	return nil, errBrokenMediaServer
}

func (brokenMediaServerRuntime) FindLocalMediaServerItem(context.Context, LocalMediaExistsQuery) (string, bool, error) {
	return "", false, errBrokenMediaServer
}

// missMediaServerRuntime 只覆盖「本地索引未收录」分支。
type missMediaServerRuntime struct{ fakeMediaServerRuntime }

func (*missMediaServerRuntime) FindLocalMediaServerItem(context.Context, LocalMediaExistsQuery) (string, bool, error) {
	return "", false, nil
}

func TestMediaServerSettingStoreUnavailable(t *testing.T) {
	handler, token := newMediaServerBranchRouter(t, nil)

	response := performDownloaderRequest(handler, http.MethodGet, mediaServersSettingPath, "", token)
	if response.Code != http.StatusInternalServerError {
		t.Fatalf("GET 无存储 status = %d", response.Code)
	}
	response = performDownloaderRequest(handler, http.MethodPost, mediaServersSettingPath, `[]`, token)
	if response.Code != http.StatusInternalServerError {
		t.Fatalf("POST 无存储 status = %d", response.Code)
	}
}

func TestMediaServersSettingLoadErrorAndNotFound(t *testing.T) {
	loadErr := &errorMediaServerStore{loadErr: errors.New("db down")}
	handler, token := newMediaServerBranchRouter(t, func(deps *Dependencies) {
		deps.MediaServerSettings = loadErr
	})
	if response := performDownloaderRequest(handler, http.MethodGet, mediaServersSettingPath, "", token); response.Code != http.StatusInternalServerError {
		t.Fatalf("GET 读取失败 status = %d", response.Code)
	}
	valid := `[{"name":"home","type":"emby","enabled":true,"config":{"host":"http://emby:8096/emby","apikey":"secret"}}]`
	if response := performDownloaderRequest(handler, http.MethodPost, mediaServersSettingPath, valid, token); response.Code != http.StatusInternalServerError {
		t.Fatalf("POST 读取失败 status = %d", response.Code)
	}

	empty := &errorMediaServerStore{}
	handler, token = newMediaServerBranchRouter(t, func(deps *Dependencies) {
		deps.MediaServerSettings = empty
	})
	response := performDownloaderRequest(handler, http.MethodGet, mediaServersSettingPath, "", token)
	if response.Code != http.StatusOK || !strings.Contains(response.Body.String(), `"value":null`) {
		t.Fatalf("GET 未设置 = %d %s", response.Code, response.Body.String())
	}
}

func TestSetMediaServersSettingValidationAndSaveErrors(t *testing.T) {
	valid := `[{"name":"home","type":"emby","enabled":true,"config":{"host":"http://emby:8096/emby","apikey":"secret"}}]`
	cases := []struct {
		name   string
		store  *errorMediaServerStore
		body   string
		status int
		substr string
	}{
		{
			name: "非法 JSON", store: &errorMediaServerStore{}, body: `{`,
			status: http.StatusBadRequest, substr: "媒体服务器设置无效",
		},
		{
			name: "非法 ID", store: &errorMediaServerStore{},
			body:   `[{"id":"not-a-uuid","name":"home","type":"emby"}]`,
			status: http.StatusBadRequest, substr: "媒体服务器设置无效",
		},
		{
			name: "非法类型", store: &errorMediaServerStore{},
			body:   `[{"name":"home","type":"unknown"}]`,
			status: http.StatusBadRequest, substr: "媒体服务器设置无效",
		},
		{
			name: "启用但缺 host", store: &errorMediaServerStore{},
			body:   `[{"id":"2cc1444f-854d-4adb-8cd8-2a6bb63585df","name":"home","type":"emby","enabled":true}]`,
			status: http.StatusBadRequest, substr: "媒体服务器设置无效",
		},
		{
			name: "旧快照非法导致合并失败",
			store: &errorMediaServerStore{found: true, snapshot: mediaconfig.Snapshot{
				Servers: []mediaconfig.Server{{ID: "bad", Name: "home", Type: "emby"}},
			}},
			body: valid, status: http.StatusBadRequest, substr: "媒体服务器设置无效",
		},
		{
			name: "保存失败", store: &errorMediaServerStore{saveErr: errors.New("disk full")},
			body: valid, status: http.StatusInternalServerError, substr: "保存媒体服务器设置失败",
		},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			handler, token := newMediaServerBranchRouter(t, func(deps *Dependencies) {
				deps.MediaServerSettings = testCase.store
			})
			response := performDownloaderRequest(handler, http.MethodPost, mediaServersSettingPath, testCase.body, token)
			if response.Code != testCase.status || !strings.Contains(response.Body.String(), testCase.substr) {
				t.Fatalf("status/body = %d %s，期望 %d 含 %q",
					response.Code, response.Body.String(), testCase.status, testCase.substr)
			}
		})
	}
}

func TestSetMediaServersSettingReloadBranches(t *testing.T) {
	body := `[{"name":"home","type":"emby","enabled":true,"config":{"host":"http://emby:8096/emby","apikey":"secret"}}]`

	// 重载失败且没有 logger 时也要返回 503 而不是 panic。
	handler, token := newMediaServerBranchRouter(t, func(deps *Dependencies) {
		deps.MediaServerSettings = &errorMediaServerStore{}
		deps.Logger = nil
		deps.ReloadMediaServers = func(context.Context) error { return errors.New("reload failed") }
	})
	response := performDownloaderRequest(handler, http.MethodPost, mediaServersSettingPath, body, token)
	if response.Code != http.StatusServiceUnavailable || !strings.Contains(response.Body.String(), `"reload":false`) {
		t.Fatalf("重载失败 = %d %s", response.Code, response.Body.String())
	}

	var reloadCalls int
	handler, token = newMediaServerBranchRouter(t, func(deps *Dependencies) {
		deps.MediaServerSettings = &errorMediaServerStore{}
		deps.ReloadMediaServers = func(ctx context.Context) error {
			reloadCalls++
			if ctx.Err() != nil {
				return ctx.Err()
			}
			return nil
		}
	})
	response = performDownloaderRequest(handler, http.MethodPost, mediaServersSettingPath, body, token)
	if response.Code != http.StatusOK || reloadCalls != 1 {
		t.Fatalf("重载成功 = %d %s（调用 %d 次）", response.Code, response.Body.String(), reloadCalls)
	}
}

func TestMediaServerRoutesWithoutRuntime(t *testing.T) {
	handler, token := newMediaServerBranchRouter(t, nil)
	cases := []struct {
		method, path, body, want string
		status                   int
	}{
		{http.MethodGet, "/api/v1/mediaserver/clients", "", `[]`, http.StatusOK},
		{http.MethodGet, "/api/v1/mediaserver/library", "", `[]`, http.StatusOK},
		{http.MethodGet, "/api/v1/mediaserver/latest", "", `[]`, http.StatusOK},
		{http.MethodGet, "/api/v1/mediaserver/playing", "", `[]`, http.StatusOK},
		{http.MethodPost, "/api/v1/mediaserver/exists_remote", `{"title":"Show"}`, `{}`, http.StatusOK},
		{http.MethodPost, "/api/v1/mediaserver/notexists", `{"title":"Show"}`, `[]`, http.StatusOK},
	}
	for _, testCase := range cases {
		response := performDownloaderRequest(handler, testCase.method, testCase.path, testCase.body, token)
		if response.Code != testCase.status || strings.TrimSpace(response.Body.String()) != testCase.want {
			t.Fatalf("%s %s = %d %s", testCase.method, testCase.path, response.Code, response.Body.String())
		}
	}

	// 没有本地索引时 exists 返回 200 + 失败消息。
	response := performDownloaderRequest(handler, http.MethodGet, "/api/v1/mediaserver/exists?title=Show", "", token)
	if response.Code != http.StatusOK || !strings.Contains(response.Body.String(), "未找到媒体") {
		t.Fatalf("exists 无索引 = %d %s", response.Code, response.Body.String())
	}

	// 空 body 的 JSON 请求直接 400。
	response = performDownloaderRequest(handler, http.MethodPost, "/api/v1/mediaserver/exists_remote", ``, token)
	if response.Code != http.StatusBadRequest {
		t.Fatalf("exists_remote 空 body = %d", response.Code)
	}
	response = performDownloaderRequest(handler, http.MethodPost, "/api/v1/mediaserver/notexists", `{`, token)
	if response.Code != http.StatusBadRequest {
		t.Fatalf("notexists 非法 body = %d", response.Code)
	}
}

func TestMediaServerRouteErrorsFromRuntime(t *testing.T) {
	broken := brokenMediaServerRuntime{}
	handler, token := newMediaServerBranchRouter(t, func(deps *Dependencies) {
		deps.MediaServerClients = broken
		deps.MediaServerPlayback = broken
		deps.MediaServerLibraries = broken
		deps.MediaServerRemote = broken
		deps.LocalMediaIndex = broken
	})
	cases := []struct {
		method, path, body string
	}{
		{http.MethodGet, "/api/v1/mediaserver/library", ""},
		{http.MethodGet, "/api/v1/mediaserver/latest", ""},
		{http.MethodGet, "/api/v1/mediaserver/playing", ""},
		{http.MethodGet, "/api/v1/mediaserver/exists?title=Show", ""},
		{http.MethodPost, "/api/v1/mediaserver/exists_remote", `{"title":"Show"}`},
		{http.MethodPost, "/api/v1/mediaserver/notexists", `{"title":"Show"}`},
	}
	for _, testCase := range cases {
		response := performDownloaderRequest(handler, testCase.method, testCase.path, testCase.body, token)
		if response.Code != http.StatusBadGateway {
			t.Fatalf("%s %s = %d %s", testCase.method, testCase.path, response.Code, response.Body.String())
		}
	}
	// clients 接口本身不返回错误，只返回空列表。
	response := performDownloaderRequest(handler, http.MethodGet, "/api/v1/mediaserver/clients", "", token)
	if response.Code != http.StatusOK || strings.TrimSpace(response.Body.String()) != `[]` {
		t.Fatalf("clients = %d %s", response.Code, response.Body.String())
	}

	// 本地索引未收录时返回空 item 而不是错误。
	miss := &missMediaServerRuntime{}
	handler, token = newMediaServerBranchRouter(t, func(deps *Dependencies) {
		deps.LocalMediaIndex = miss
	})
	response = performDownloaderRequest(handler, http.MethodGet, "/api/v1/mediaserver/exists?title=Show", "", token)
	if response.Code != http.StatusOK || !strings.Contains(response.Body.String(), `"success":false`) {
		t.Fatalf("exists 未收录 = %d %s", response.Code, response.Body.String())
	}
}

func TestMediaServerCountBounds(t *testing.T) {
	cases := map[string]int{
		"":     20,
		"abc":  20,
		"0":    20,
		"-3":   20,
		"7":    7,
		"500":  100,
		" 12 ": 12,
	}
	for raw, want := range cases {
		request := httptest.NewRequest(http.MethodGet, "/api/v1/mediaserver/latest?count="+url.QueryEscape(raw), nil)
		if got := mediaServerCount(request, 20, 100); got != want {
			t.Fatalf("mediaServerCount(%q) = %d，期望 %d", raw, got, want)
		}
	}
}

func TestReadMediaServersRequestBounds(t *testing.T) {
	valid := `[{"name":"home","type":"emby"}]`
	request := func(body string) *http.Request {
		return httptest.NewRequest(http.MethodPost, mediaServersSettingPath, strings.NewReader(body))
	}
	if _, err := readMediaServersRequest(request(valid), 0); err != nil {
		t.Fatalf("默认上限下合法请求 = %v", err)
	}
	if _, err := readMediaServersRequest(request(valid), int64(len(valid))-1); err == nil {
		t.Fatal("超出上限的请求应报错")
	}
	if _, err := readMediaServersRequest(request(`{`), 64); err == nil {
		t.Fatal("非法 JSON 应报错")
	}
}
