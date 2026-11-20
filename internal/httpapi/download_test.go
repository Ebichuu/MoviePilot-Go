package httpapi

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"
	"github.com/golang-jwt/jwt/v5"
	"github.com/google/uuid"

	"github.com/moviepilot-go/moviepilot-go/internal/auth"
	"github.com/moviepilot-go/moviepilot-go/internal/config"
	"github.com/moviepilot-go/moviepilot-go/internal/database"
	"github.com/moviepilot-go/moviepilot-go/internal/downloadconfig"
	"github.com/moviepilot-go/moviepilot-go/internal/downloader"
	"github.com/moviepilot-go/moviepilot-go/internal/indexer"
	"github.com/moviepilot-go/moviepilot-go/internal/organization"
)

const (
	downloadHTTPPrimaryID   = "11111111-1111-4111-8111-111111111111"
	downloadHTTPSecondaryID = "22222222-2222-4222-8222-222222222222"
)

func TestListDownloadsReturnsBareEmptyArray(t *testing.T) {
	tests := []struct {
		name string
		path string
	}{
		{name: "无尾斜杠", path: "/download"},
		{name: "有尾斜杠", path: "/download/"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			user := auth.User{ID: 11, Username: "alice"}
			router := chi.NewRouter()
			Dependencies{
				Users:          fakeUserLookup{user: user},
				DownloadTasks:  &fakeDownloadTaskSource{},
				DownloadOwners: &fakeDownloadIntentOwnerStore{},
			}.registerProtected(router)
			response := httptest.NewRecorder()
			router.ServeHTTP(response, downloadListRequest(tt.path, user))

			if response.Code != http.StatusOK {
				t.Fatalf("status = %d, want %d; body=%s", response.Code, http.StatusOK, response.Body.String())
			}
			var items []any
			if err := json.Unmarshal(response.Body.Bytes(), &items); err != nil {
				t.Fatalf("响应不是裸数组: %v; body=%s", err, response.Body.String())
			}
			if items == nil || len(items) != 0 {
				t.Fatalf("items = %#v, want non-nil empty array", items)
			}
		})
	}
}

func TestDownloadPathsReturnsOriginalFieldsAndOnlyLocalDirectories(t *testing.T) {
	store := &fakeDownloadRequestStore{directories: []downloadDirectorySetting{
		{
			Name:          downloadStringPointer("电影下载"),
			Storage:       downloadStringPointer("local"),
			DownloadPath:  downloadStringPointer("/downloads/movies"),
			Priority:      downloadIntPointer(3),
			MediaType:     downloadStringPointer("电影"),
			MediaCategory: downloadStringPointer("华语"),
		},
		{
			Name:         downloadStringPointer("默认下载"),
			Storage:      downloadStringPointer(""),
			DownloadPath: downloadStringPointer("/downloads/default"),
		},
		{
			Name:         downloadStringPointer("远端"),
			Storage:      downloadStringPointer("rclone"),
			DownloadPath: downloadStringPointer("/remote"),
		},
		{Name: downloadStringPointer("无路径")},
	}}
	store.directoriesFound = true
	router := chi.NewRouter()
	Dependencies{DownloadRequests: store}.registerProtected(router)
	response := httptest.NewRecorder()
	router.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/download/paths", nil))

	if response.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d; body=%s", response.Code, http.StatusOK, response.Body.String())
	}
	var paths []map[string]any
	if err := json.Unmarshal(response.Body.Bytes(), &paths); err != nil {
		t.Fatalf("解析路径响应: %v", err)
	}
	if len(paths) != 2 {
		t.Fatalf("paths = %#v, want 2 local paths", paths)
	}
	wantKeys := []string{"name", "storage", "download_path", "save_path", "priority", "media_type", "media_category"}
	for index, path := range paths {
		for _, key := range wantKeys {
			if _, ok := path[key]; !ok {
				t.Fatalf("paths[%d] 缺少原版字段 %q: %#v", index, key, path)
			}
		}
		if path["storage"] != "local" || path["download_path"] != path["save_path"] {
			t.Fatalf("paths[%d] = %#v", index, path)
		}
	}
	if paths[0]["priority"] != float64(3) || paths[0]["media_type"] != "电影" || paths[0]["media_category"] != "华语" {
		t.Fatalf("first path = %#v", paths[0])
	}
	if paths[1]["priority"] != float64(0) {
		t.Fatalf("default priority = %#v, want 0", paths[1]["priority"])
	}
}

func TestSubmitDownloadBuildsOwnerScopedImmutablePlanAndIgnoresBrowserCredentials(t *testing.T) {
	store := standardDownloadRequestStore()
	submission := &fakeDownloadSubmission{}
	deps := standardDownloadDependencies(store, submission)
	body := `{
		"torrent_in": {
			"site": 7,
			"site_downloader": "Secondary",
			"title": "Example Show WEB-DL",
			"description": "S02E03-S02E04 HDR 2160p",
			"enclosure": "/api/v1/resource/download/7/123",
			"site_cookie": "cookie-must-not-be-used",
			"site_ua": "ua-must-not-be-used",
			"site_proxy": true,
			"proxy": "http://proxy-secret.invalid",
			"downloader": {"host":"http://browser-secret.invalid","password":"password-secret"}
		},
		"media_in": {
			"source": "tmdb",
			"media_id": "42",
			"tmdb_id": 999,
			"title": "测试剧",
			"year": "2026",
			"title_year": "不可信的派生标题",
			"type": "电视剧",
			"category": "国产剧",
			"vote_average": 8.7,
			"poster_path": "https://image.invalid/poster-original.jpg",
			"backdrop_path": "https://image.invalid/backdrop-original.jpg",
			"cookie": "media-cookie-secret",
			"token": "media-token-secret",
			"enclosure": "media-enclosure-secret",
			"locator": "media-locator-secret",
			"save_path": "media-save-path-secret"
		},
		"downloader": " qb PRIMARY ",
		"save_path": "/downloads"
	}`
	response := httptest.NewRecorder()
	deps.submitDownload(response, downloadHTTPRequest(body, "alice"))

	payload := decodeDownloadResponse(t, response, http.StatusOK)
	if !payload.Success {
		t.Fatalf("response = %#v", payload)
	}
	data, ok := payload.Data.(map[string]any)
	if !ok || data["download_id"] != submission.intent.ID.String() {
		t.Fatalf("data = %#v, intent=%#v", payload.Data, submission.intent)
	}
	if submission.calls != 1 || submission.username != "alice" {
		t.Fatalf("submission = %#v", submission)
	}
	if submission.intent.ID == uuid.Nil || submission.intent.State != downloader.IntentPlanned || submission.intent.CandidateKey == "" || submission.intent.Label == "" {
		t.Fatalf("intent = %#v", submission.intent)
	}
	if submission.plan.IntentID != submission.intent.ID || submission.plan.SiteID != 7 ||
		submission.plan.DownloaderID != downloadHTTPPrimaryID || submission.plan.ResolverKind != "test" ||
		submission.plan.ResolverVersion != 1 || submission.plan.ResourceID != "123" ||
		submission.plan.ResourceSecret != "" || submission.plan.SavePath != "/downloads" || submission.plan.Paused {
		t.Fatalf("plan = %#v", submission.plan)
	}
	wantSuccessInput := organization.SuccessInput{
		IntentID: submission.intent.ID, Version: 1, Username: "alice", Background: true,
		Media: organization.MediaContext{
			Source: "themoviedb", MediaID: "42", TitleYear: "测试剧 (2026)", MediaType: "电视剧",
			Category: "国产剧", VoteAverage: 8.7, PosterURL: "https://image.invalid/poster-original.jpg",
			BackdropURL: "https://image.invalid/backdrop-original.jpg",
		},
		ResourceTerm: "WEB-DL HDR 2160p",
		Episodes:     []organization.Episode{{Season: 2, Episode: 3}, {Season: 2, Episode: 4}},
	}
	if encoded, wantEncoded := downloadJSON(t, submission.successInput), downloadJSON(t, wantSuccessInput); encoded != wantEncoded {
		t.Fatalf("success input = %s, want %s", encoded, wantEncoded)
	}
	for _, secret := range []string{
		"cookie-must-not-be-used", "ua-must-not-be-used", "proxy-secret", "browser-secret", "password-secret",
		"media-cookie-secret", "media-token-secret", "media-enclosure-secret", "media-locator-secret", "media-save-path-secret",
	} {
		if strings.Contains(response.Body.String(), secret) || strings.Contains(submission.plan.ResourceSecret, secret) ||
			strings.Contains(downloadJSON(t, submission.successInput), secret) {
			t.Fatalf("浏览器字段 %q 泄露到响应、计划或 success input", secret)
		}
	}
}

func TestSubmitDownloadProtectsNonNumericResourceLocator(t *testing.T) {
	store := standardDownloadRequestStore()
	submission := &fakeDownloadSubmission{}
	deps := standardDownloadDependencies(store, submission)
	locator := "https://tracker.invalid/download.php?id=42&token=resource-secret"
	body := downloadRequestBody(t, map[string]any{
		"torrent_in": map[string]any{
			"site":      7,
			"enclosure": "/api/v1/resource/download/7/" + url.PathEscape(locator),
		},
	})
	response := httptest.NewRecorder()
	deps.submitDownload(response, downloadHTTPRequest(body, "alice"))

	payload := decodeDownloadResponse(t, response, http.StatusOK)
	if !payload.Success {
		t.Fatalf("response = %#v", payload)
	}
	if !strings.HasPrefix(submission.plan.ResourceID, "sha256:") || submission.plan.ResourceSecret != locator {
		t.Fatalf("plan resource = (%q, %q)", submission.plan.ResourceID, submission.plan.ResourceSecret)
	}
	if strings.Contains(response.Body.String(), locator) || strings.Contains(response.Body.String(), "resource-secret") {
		t.Fatalf("响应泄露资源定位秘密: %s", response.Body.String())
	}
}

func TestSubmitDownloadRoutes(t *testing.T) {
	tests := []struct {
		name string
		path string
	}{
		{name: "无尾斜杠", path: "/download"},
		{name: "有尾斜杠", path: "/download/"},
		{name: "add 兼容入口", path: "/download/add"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			submission := &fakeDownloadSubmission{}
			deps := standardDownloadDependencies(standardDownloadRequestStore(), submission)
			router := chi.NewRouter()
			deps.registerProtected(router)
			response := httptest.NewRecorder()
			request := downloadHTTPRequest(`{"torrent_in":{"site":7,"enclosure":"/api/v1/resource/download/7/123"}}`, "alice")
			request.URL.Path = tt.path
			router.ServeHTTP(response, request)

			payload := decodeDownloadResponse(t, response, http.StatusOK)
			if !payload.Success || submission.calls != 1 {
				t.Fatalf("response=%#v submission=%#v", payload, submission)
			}
			if submission.successInput.Version != 1 || submission.successInput.Legacy ||
				submission.successInput.Manual || !submission.successInput.Background ||
				submission.successInput.Username != "alice" {
				t.Fatalf("success input=%#v", submission.successInput)
			}
		})
	}
}

func TestSubmitDownloadCreatesBoundedSuccessInputForMissingEmptyAndUnknownMedia(t *testing.T) {
	tests := []struct {
		name         string
		media        any
		includeMedia bool
		title        string
		description  string
		wantTerm     string
		wantEpisodes []organization.Episode
	}{
		{name: "media_in 缺省", wantEpisodes: []organization.Episode{}},
		{name: "media_in 空对象", media: map[string]any{}, includeMedia: true, wantEpisodes: []organization.Episode{}},
		{
			name: "未知媒体字段被忽略且副标题参与解析",
			media: map[string]any{
				"cookie": "must-not-persist", "token": "must-not-persist",
				"nested": map[string]any{"save_path": "/must/not/persist"},
			},
			includeMedia: true,
			title:        "Example Show WEB-DL",
			description:  "S03E05 HDR10+ 4K",
			wantTerm:     "WEB-DL HDR10+ 2160p",
			wantEpisodes: []organization.Episode{{Season: 3, Episode: 5}},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			requestBody := map[string]any{
				"torrent_in": map[string]any{
					"site": 7, "enclosure": "/api/v1/resource/download/7/123",
					"title": tt.title, "description": tt.description,
				},
			}
			if tt.includeMedia {
				requestBody["media_in"] = tt.media
			}
			submission := &fakeDownloadSubmission{}
			deps := standardDownloadDependencies(standardDownloadRequestStore(), submission)
			response := httptest.NewRecorder()
			deps.submitDownload(response, downloadHTTPRequest(downloadRequestBody(t, requestBody), "alice"))

			payload := decodeDownloadResponse(t, response, http.StatusOK)
			if !payload.Success || submission.calls != 1 {
				t.Fatalf("response=%#v submission=%#v", payload, submission)
			}
			input := submission.successInput
			if input.Version != 1 || input.Legacy || input.Manual || !input.Background || input.Username != "alice" {
				t.Fatalf("success input=%#v", input)
			}
			if input.Media != (organization.MediaContext{}) || input.ResourceTerm != tt.wantTerm ||
				downloadJSON(t, input.Episodes) != downloadJSON(t, tt.wantEpisodes) {
				t.Fatalf("success input=%#v", input)
			}
			for _, forbidden := range []string{"must-not-persist", "/must/not/persist"} {
				if strings.Contains(downloadJSON(t, input), forbidden) {
					t.Fatalf("未知字段 %q 进入 success input: %#v", forbidden, input)
				}
			}
		})
	}
}

func TestSubmitDownloadRejectsMalformedOrMultipleJSONValues(t *testing.T) {
	tests := []struct {
		name string
		body string
	}{
		{name: "非法 JSON", body: `{"torrent_in":`},
		{name: "多个 JSON 值", body: `{"torrent_in":{}} {}`},
		{name: "字段类型错误", body: `{"torrent_in":{"site":"7"}}`},
		{name: "media_in 不是对象", body: `{"media_in":[],"torrent_in":{"site":7}}`},
		{name: "media_in 字符串字段类型错误", body: `{"media_in":{"source":7},"torrent_in":{"site":7}}`},
		{name: "media_in ID 字段类型错误", body: `{"media_in":{"tmdb_id":"7"},"torrent_in":{"site":7}}`},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			response := httptest.NewRecorder()
			Dependencies{Config: config.Config{MaxRequestBody: 1 << 20}}.submitDownload(
				response,
				downloadHTTPRequest(tt.body, "alice"),
			)
			payload := decodeDownloadResponse(t, response, http.StatusUnprocessableEntity)
			if payload.Success || payload.Message != "下载请求 JSON 无效" {
				t.Fatalf("response = %#v", payload)
			}
		})
	}
}

func TestSubmitDownloadRejectsInvalidEnclosureAndSite(t *testing.T) {
	tests := []struct {
		name      string
		torrentIn map[string]any
		registry  *indexer.Registry
	}{
		{name: "外部 URL", torrentIn: map[string]any{"site": 7, "enclosure": "https://evil.invalid/api/v1/resource/download/7/123"}},
		{name: "首尾空白", torrentIn: map[string]any{"site": 7, "enclosure": " /api/v1/resource/download/7/123 "}},
		{name: "错误路径", torrentIn: map[string]any{"site": 7, "enclosure": "/api/v1/resource/other/7/123"}},
		{name: "查询参数", torrentIn: map[string]any{"site": 7, "enclosure": "/api/v1/resource/download/7/123?token=secret"}},
		{name: "额外路径段", torrentIn: map[string]any{"site": 7, "enclosure": "/api/v1/resource/download/7/123/extra"}},
		{name: "编码上级目录", torrentIn: map[string]any{"site": 7, "enclosure": "/api/v1/resource/download/7/%2e%2e"}},
		{name: "缺少 site", torrentIn: map[string]any{"enclosure": "/api/v1/resource/download/7/123"}},
		{name: "site 不匹配", torrentIn: map[string]any{"site": 8, "enclosure": "/api/v1/resource/download/7/123"}},
		{
			name:      "site 未注册",
			torrentIn: map[string]any{"site": 9, "enclosure": "/api/v1/resource/download/9/123"},
			registry:  indexer.NewRegistry(nil),
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			registry := tt.registry
			if registry == nil {
				registry = downloadHTTPRegistry()
			}
			submission := &fakeDownloadSubmission{}
			deps := standardDownloadDependencies(standardDownloadRequestStore(), submission)
			deps.SearchRegistry = registry
			response := httptest.NewRecorder()
			deps.submitDownload(response, downloadHTTPRequest(downloadRequestBody(t, map[string]any{"torrent_in": tt.torrentIn}), "alice"))

			payload := decodeDownloadResponse(t, response, http.StatusOK)
			if payload.Success || payload.Message != "下载来源无效" || submission.calls != 0 {
				t.Fatalf("response=%#v submission calls=%d", payload, submission.calls)
			}
		})
	}
}

func TestSubmitDownloadSelectsTopLevelSiteAndDefaultDownloader(t *testing.T) {
	tests := []struct {
		name           string
		topLevel       string
		siteDownloader string
		wantID         string
	}{
		{name: "顶层选择优先且忽略大小写", topLevel: "SECONDARY", siteDownloader: "QB Primary", wantID: downloadHTTPSecondaryID},
		{name: "站点下载器回退", siteDownloader: "secondary", wantID: downloadHTTPSecondaryID},
		{name: "默认下载器回退", wantID: downloadHTTPPrimaryID},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			submission := &fakeDownloadSubmission{}
			deps := standardDownloadDependencies(standardDownloadRequestStore(), submission)
			body := downloadRequestBody(t, map[string]any{
				"torrent_in": map[string]any{
					"site":            7,
					"site_downloader": tt.siteDownloader,
					"enclosure":       "/api/v1/resource/download/7/123",
				},
				"downloader": tt.topLevel,
			})
			response := httptest.NewRecorder()
			deps.submitDownload(response, downloadHTTPRequest(body, "alice"))

			payload := decodeDownloadResponse(t, response, http.StatusOK)
			if !payload.Success || submission.plan.DownloaderID != tt.wantID {
				t.Fatalf("response=%#v downloader=%q, want %q", payload, submission.plan.DownloaderID, tt.wantID)
			}
		})
	}
}

func TestSubmitDownloadRejectsUnavailableDownloader(t *testing.T) {
	tests := []struct {
		name        string
		selector    string
		downloaders []downloadconfig.Downloader
		found       bool
	}{
		{name: "名称不存在", selector: "missing", downloaders: standardDownloaders(), found: true},
		{
			name:     "已禁用",
			selector: "disabled",
			downloaders: []downloadconfig.Downloader{{
				ID: downloadHTTPPrimaryID, Name: "disabled", Type: "qbittorrent", Enabled: false, Default: true,
			}},
			found: true,
		},
		{
			name:     "非 qBittorrent",
			selector: "deluge",
			downloaders: []downloadconfig.Downloader{{
				ID: downloadHTTPPrimaryID, Name: "deluge", Type: "deluge", Enabled: true, Default: true,
			}},
			found: true,
		},
		{name: "未配置", found: false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			store := standardDownloadRequestStore()
			store.downloaders = tt.downloaders
			store.downloadersFound = tt.found
			submission := &fakeDownloadSubmission{}
			deps := standardDownloadDependencies(store, submission)
			body := downloadRequestBody(t, map[string]any{
				"torrent_in": map[string]any{"site": 7, "enclosure": "/api/v1/resource/download/7/123"},
				"downloader": tt.selector,
			})
			response := httptest.NewRecorder()
			deps.submitDownload(response, downloadHTTPRequest(body, "alice"))

			payload := decodeDownloadResponse(t, response, http.StatusOK)
			if payload.Success || payload.Message != "没有可用的下载器" || submission.calls != 0 {
				t.Fatalf("response=%#v submission calls=%d", payload, submission.calls)
			}
		})
	}
}

func TestSubmitDownloadRequiresExactAllowedLocalSavePath(t *testing.T) {
	tests := []struct {
		name        string
		savePath    string
		wantSuccess bool
	}{
		{name: "空路径", savePath: "", wantSuccess: true},
		{name: "精确本地路径", savePath: "/downloads", wantSuccess: true},
		{name: "子目录不允许", savePath: "/downloads/movies"},
		{name: "未知路径", savePath: "/unknown"},
		{name: "远端目录不允许", savePath: "/remote"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			store := standardDownloadRequestStore()
			store.directories = append(store.directories, downloadDirectorySetting{
				Storage: downloadStringPointer("rclone"), DownloadPath: downloadStringPointer("/remote"),
			})
			submission := &fakeDownloadSubmission{}
			deps := standardDownloadDependencies(store, submission)
			body := downloadRequestBody(t, map[string]any{
				"torrent_in": map[string]any{"site": 7, "enclosure": "/api/v1/resource/download/7/123"},
				"save_path":  tt.savePath,
			})
			response := httptest.NewRecorder()
			deps.submitDownload(response, downloadHTTPRequest(body, "alice"))

			payload := decodeDownloadResponse(t, response, http.StatusOK)
			if payload.Success != tt.wantSuccess {
				t.Fatalf("response = %#v", payload)
			}
			if tt.wantSuccess {
				if submission.calls != 1 || submission.plan.SavePath != tt.savePath {
					t.Fatalf("submission=%#v", submission)
				}
			} else if payload.Message != "保存路径不在允许的下载目录中" || submission.calls != 0 {
				t.Fatalf("response=%#v submission calls=%d", payload, submission.calls)
			}
		})
	}
}

func TestSubmitDownloadDoesNotLeakDependencyErrors(t *testing.T) {
	secrets := []string{"cookie-secret", "http://internal-downloader.invalid", "/downloads/private"}
	for _, secret := range secrets {
		t.Run(secret, func(t *testing.T) {
			submission := &fakeDownloadSubmission{err: errors.New("dependency failed: " + secret)}
			deps := standardDownloadDependencies(standardDownloadRequestStore(), submission)
			response := httptest.NewRecorder()
			deps.submitDownload(response, downloadHTTPRequest(
				`{"torrent_in":{"site":7,"enclosure":"/api/v1/resource/download/7/123"}}`,
				"alice",
			))

			payload := decodeDownloadResponse(t, response, http.StatusOK)
			if payload.Success || payload.Message != "任务添加失败" || strings.Contains(response.Body.String(), secret) {
				t.Fatalf("response = %#v; body=%s", payload, response.Body.String())
			}
		})
	}
}

func TestSubmitDownloadUsesOwnerClaims(t *testing.T) {
	tests := []struct {
		name     string
		username string
		wantCall bool
	}{
		{name: "owner 传递", username: "bob", wantCall: true},
		{name: "缺少 owner", username: "", wantCall: false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			submission := &fakeDownloadSubmission{}
			deps := standardDownloadDependencies(standardDownloadRequestStore(), submission)
			response := httptest.NewRecorder()
			deps.submitDownload(response, downloadHTTPRequest(
				`{"torrent_in":{"site":7,"enclosure":"/api/v1/resource/download/7/123"}}`,
				tt.username,
			))

			payload := decodeDownloadResponse(t, response, http.StatusOK)
			if (submission.calls == 1) != tt.wantCall {
				t.Fatalf("submission calls=%d response=%#v", submission.calls, payload)
			}
			if tt.wantCall && submission.username != tt.username {
				t.Fatalf("username = %q, want %q", submission.username, tt.username)
			}
			if !tt.wantCall && (payload.Success || payload.Message != "用户身份无效") {
				t.Fatalf("response = %#v", payload)
			}
		})
	}
}

type fakeDownloadRequestStore struct {
	downloaders      []downloadconfig.Downloader
	downloadersFound bool
	directories      []downloadDirectorySetting
	directoriesFound bool
	loadErr          error
	settingErr       error
}

func (s *fakeDownloadRequestStore) LoadDownloaders(context.Context) ([]downloadconfig.Downloader, bool, error) {
	if s.loadErr != nil {
		return nil, false, s.loadErr
	}
	return append([]downloadconfig.Downloader(nil), s.downloaders...), s.downloadersFound, nil
}

func (s *fakeDownloadRequestStore) GetSetting(_ context.Context, key string, target any) (bool, error) {
	if s.settingErr != nil {
		return false, s.settingErr
	}
	if key != directoriesSettingKey || !s.directoriesFound {
		return false, nil
	}
	encoded, err := json.Marshal(s.directories)
	if err != nil {
		return false, err
	}
	if err := json.Unmarshal(encoded, target); err != nil {
		return false, err
	}
	return true, nil
}

type fakeDownloadSubmission struct {
	username     string
	intent       downloader.Intent
	plan         database.DownloadIntentPlan
	successInput organization.SuccessInput
	calls        int
	err          error
}

func (s *fakeDownloadSubmission) Submit(
	_ context.Context,
	username string,
	intent downloader.Intent,
	plan database.DownloadIntentPlan,
	successInput organization.SuccessInput,
) (uuid.UUID, error) {
	s.calls++
	s.username = username
	s.intent = intent
	s.plan = plan
	s.successInput = successInput
	if s.err != nil {
		return uuid.Nil, s.err
	}
	return intent.ID, nil
}

type downloadHTTPAdapter struct {
	siteID  int
	kind    string
	version int16
}

func (a downloadHTTPAdapter) Name() string { return "download-http-test" }

func (a downloadHTTPAdapter) SiteID() int { return a.siteID }

func (a downloadHTTPAdapter) ResolverKind() string { return a.kind }

func (a downloadHTTPAdapter) ResolverVersion() int16 { return a.version }

func (a downloadHTTPAdapter) Search(context.Context, indexer.Query) ([]indexer.Resource, error) {
	return nil, nil
}

func (a downloadHTTPAdapter) ResolveDownload(context.Context, string) (indexer.ResolvedDownload, error) {
	return indexer.ResolvedDownload{}, nil
}

func standardDownloadDependencies(store *fakeDownloadRequestStore, submission *fakeDownloadSubmission) Dependencies {
	return Dependencies{
		Config:              config.Config{MaxRequestBody: 1 << 20},
		SearchRegistry:      downloadHTTPRegistry(),
		DownloadRequests:    store,
		DownloadSubmissions: submission,
	}
}

func downloadHTTPRegistry() *indexer.Registry {
	return indexer.NewRegistry([]indexer.Adapter{
		downloadHTTPAdapter{siteID: 7, kind: "test", version: 1},
	})
}

func standardDownloadRequestStore() *fakeDownloadRequestStore {
	return &fakeDownloadRequestStore{
		downloaders:      standardDownloaders(),
		downloadersFound: true,
		directories: []downloadDirectorySetting{{
			Name: downloadStringPointer("下载"), Storage: downloadStringPointer("local"), DownloadPath: downloadStringPointer("/downloads"),
		}},
		directoriesFound: true,
	}
}

func standardDownloaders() []downloadconfig.Downloader {
	return []downloadconfig.Downloader{
		{ID: downloadHTTPPrimaryID, Name: "QB Primary", Type: "qbittorrent", Enabled: true, Default: true},
		{ID: downloadHTTPSecondaryID, Name: "Secondary", Type: "qbittorrent", Enabled: true},
	}
}

func downloadHTTPRequest(body, username string) *http.Request {
	request := httptest.NewRequest(http.MethodPost, "/download", strings.NewReader(body))
	if username == "" {
		return request
	}
	claims := auth.Claims{
		Username: username,
		RegisteredClaims: jwt.RegisteredClaims{
			Subject: "1",
		},
	}
	return request.WithContext(context.WithValue(request.Context(), claimsContextKey, claims))
}

func downloadRequestBody(t *testing.T, value map[string]any) string {
	t.Helper()
	encoded, err := json.Marshal(value)
	if err != nil {
		t.Fatalf("编码下载请求: %v", err)
	}
	return string(encoded)
}

func downloadJSON(t *testing.T, value any) string {
	t.Helper()
	encoded, err := json.Marshal(value)
	if err != nil {
		t.Fatalf("编码测试值: %v", err)
	}
	return string(encoded)
}

func decodeDownloadResponse(t *testing.T, response *httptest.ResponseRecorder, wantStatus int) Response {
	t.Helper()
	if response.Code != wantStatus {
		t.Fatalf("status = %d, want %d; body=%s", response.Code, wantStatus, response.Body.String())
	}
	var payload Response
	if err := json.Unmarshal(response.Body.Bytes(), &payload); err != nil {
		t.Fatalf("解析响应: %v; body=%s", err, response.Body.String())
	}
	return payload
}

func downloadStringPointer(value string) *string { return &value }

func downloadIntPointer(value int) *int { return &value }

var _ DownloadRequestStore = (*fakeDownloadRequestStore)(nil)
var _ DownloadSubmission = (*fakeDownloadSubmission)(nil)
var _ indexer.Adapter = downloadHTTPAdapter{}
var _ indexer.SiteIDAdapter = downloadHTTPAdapter{}
var _ indexer.VersionedDownloadResolver = downloadHTTPAdapter{}
