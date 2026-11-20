package httpapi

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/moviepilot-go/moviepilot-go/internal/auth"
)

const downloadersSettingPath = "/api/v1/system/setting/Downloaders"

var downloaderBranchAdmin = auth.User{ID: 1, Username: "admin", SuperUser: true}

func downloaderBranchStore(value string) *fakeDownloaderSettingStore {
	return &fakeDownloaderSettingStore{found: true, value: json.RawMessage(value)}
}

const validDownloaderBody = `[{"name":"qb","type":"qbittorrent","enabled":false,"config":{}}]`

func TestDownloaderSettingStoreUnavailable(t *testing.T) {
	handler, token := newDownloaderTestRouter(t, nil, downloaderBranchAdmin)

	response := performDownloaderRequest(handler, http.MethodGet, downloadersSettingPath, "", token)
	if response.Code != http.StatusInternalServerError || !strings.Contains(response.Body.String(), "读取下载器设置失败") {
		t.Fatalf("GET 无存储 = %d %s", response.Code, response.Body.String())
	}
	response = performDownloaderRequest(handler, http.MethodPost, downloadersSettingPath, validDownloaderBody, token)
	if response.Code != http.StatusInternalServerError || !strings.Contains(response.Body.String(), "保存下载器设置失败") {
		t.Fatalf("POST 无存储 = %d %s", response.Code, response.Body.String())
	}
	response = performDownloaderRequest(handler, http.MethodGet, "/api/v1/download/clients", "", token)
	if response.Code != http.StatusInternalServerError || !strings.Contains(response.Body.String(), "读取下载器列表失败") {
		t.Fatalf("clients 无存储 = %d %s", response.Code, response.Body.String())
	}
}

func TestDownloaderSettingsLoadErrorBranches(t *testing.T) {
	store := &fakeDownloaderSettingStore{loadErr: errors.New("db down")}
	handler, token := newDownloaderTestRouter(t, store, downloaderBranchAdmin)

	response := performDownloaderRequest(handler, http.MethodGet, downloadersSettingPath, "", token)
	if response.Code != http.StatusInternalServerError {
		t.Fatalf("GET 读取失败 = %d", response.Code)
	}
	response = performDownloaderRequest(handler, http.MethodPost, downloadersSettingPath, validDownloaderBody, token)
	if response.Code != http.StatusInternalServerError {
		t.Fatalf("POST 读取失败 = %d", response.Code)
	}
	response = performDownloaderRequest(handler, http.MethodGet, "/api/v1/download/clients", "", token)
	if response.Code != http.StatusInternalServerError {
		t.Fatalf("clients 读取失败 = %d", response.Code)
	}
}

func TestDownloaderSettingsNotFound(t *testing.T) {
	handler, token := newDownloaderTestRouter(t, &fakeDownloaderSettingStore{}, downloaderBranchAdmin)

	response := performDownloaderRequest(handler, http.MethodGet, downloadersSettingPath, "", token)
	if response.Code != http.StatusOK || !strings.Contains(response.Body.String(), `"value":null`) {
		t.Fatalf("GET 未设置 = %d %s", response.Code, response.Body.String())
	}
	response = performDownloaderRequest(handler, http.MethodGet, "/api/v1/download/clients", "", token)
	if response.Code != http.StatusOK || strings.TrimSpace(response.Body.String()) != "[]" {
		t.Fatalf("clients 未设置 = %d %s", response.Code, response.Body.String())
	}
}

func TestGetDownloadersSettingErrorBranches(t *testing.T) {
	cases := []struct {
		name       string
		value      string
		saveErr    error
		wantSubstr string
	}{
		{
			name: "归一化失败", value: `[{"name":"","type":"qbittorrent","enabled":false,"config":{}}]`,
			wantSubstr: "读取下载器设置失败",
		},
		{
			name: "派生字段校验失败", value: `[{"name":"qb","type":"qbittorrent","enabled":false,"config":{"password":123}}]`,
			wantSubstr: "读取下载器设置失败",
		},
		{
			name: "升级写回失败", value: `[{"name":"qb","type":"qbittorrent","enabled":false,"config":{}}]`,
			saveErr: errors.New("disk full"), wantSubstr: "保存下载器设置失败",
		},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			store := downloaderBranchStore(testCase.value)
			store.saveErr = testCase.saveErr
			handler, token := newDownloaderTestRouter(t, store, downloaderBranchAdmin)
			response := performDownloaderRequest(handler, http.MethodGet, downloadersSettingPath, "", token)
			if response.Code != http.StatusInternalServerError || !strings.Contains(response.Body.String(), testCase.wantSubstr) {
				t.Fatalf("status/body = %d %s", response.Code, response.Body.String())
			}
		})
	}
}

func TestDownloadClientsBranches(t *testing.T) {
	store := downloaderBranchStore(`[
		{"name":"qb","type":"qbittorrent","enabled":true,"config":{"host":"http://qb:8080","username":"u","password":"p"}},
		{"name":"qb2","type":"qbittorrent","enabled":false,"config":{}}]`)
	handler, token := newDownloaderTestRouter(t, store, downloaderBranchAdmin)
	response := performDownloaderRequest(handler, http.MethodGet, "/api/v1/download/clients", "", token)
	if response.Code != http.StatusOK {
		t.Fatalf("clients status = %d", response.Code)
	}
	body := response.Body.String()
	if !strings.Contains(body, `"name":"qb"`) || strings.Contains(body, `"name":"qb2"`) {
		t.Fatalf("clients 只应包含启用的下载器: %s", body)
	}

	broken := downloaderBranchStore(`[{"name":"","type":"qbittorrent","enabled":false,"config":{}}]`)
	handler, token = newDownloaderTestRouter(t, broken, downloaderBranchAdmin)
	response = performDownloaderRequest(handler, http.MethodGet, "/api/v1/download/clients", "", token)
	if response.Code != http.StatusInternalServerError || !strings.Contains(response.Body.String(), "读取下载器列表失败") {
		t.Fatalf("clients 归一化失败 = %d %s", response.Code, response.Body.String())
	}
}

func TestSetDownloadersSettingErrorBranches(t *testing.T) {
	cases := []struct {
		name       string
		storeValue string
		saveErr    error
		body       string
		status     int
		wantSubstr string
	}{
		{
			name: "非法 JSON", body: `{`, status: http.StatusBadRequest,
			wantSubstr: "下载器设置无效",
		},
		{
			name: "入参归一化失败", body: `[{"name":"","type":"qbittorrent","enabled":false,"config":{}}]`,
			status: http.StatusBadRequest, wantSubstr: "下载器设置无效",
		},
		{
			name: "秘密合并失败", body: `[{"name":"qb","type":"qbittorrent","enabled":false,"config":{"clear_password":"yes"}}]`,
			status: http.StatusBadRequest, wantSubstr: "下载器设置无效",
		},
		{
			name: "入参派生字段非法", body: `[{"name":"qb","type":"qbittorrent","enabled":false,"config":{"password":123}}]`,
			status: http.StatusBadRequest, wantSubstr: "下载器设置无效",
		},
		{
			name: "保存失败", body: validDownloaderBody, saveErr: errors.New("disk full"),
			status: http.StatusInternalServerError, wantSubstr: "保存下载器设置失败",
		},
		{
			name:       "旧值归一化失败",
			storeValue: `[{"name":"","type":"qbittorrent","enabled":false,"config":{}}]`,
			body:       validDownloaderBody, status: http.StatusInternalServerError,
			wantSubstr: "保存下载器设置失败",
		},
		{
			name:       "旧值派生字段非法",
			storeValue: `[{"name":"qb","type":"qbittorrent","enabled":false,"config":{"password":123}}]`,
			body:       validDownloaderBody, status: http.StatusInternalServerError,
			wantSubstr: "保存下载器设置失败",
		},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			store := &fakeDownloaderSettingStore{saveErr: testCase.saveErr}
			if testCase.storeValue != "" {
				store.value = json.RawMessage(testCase.storeValue)
				store.found = true
			}
			handler, token := newDownloaderTestRouter(t, store, downloaderBranchAdmin)
			response := performDownloaderRequest(handler, http.MethodPost, downloadersSettingPath, testCase.body, token)
			if response.Code != testCase.status || !strings.Contains(response.Body.String(), testCase.wantSubstr) {
				t.Fatalf("status/body = %d %s，期望 %d 含 %q",
					response.Code, response.Body.String(), testCase.status, testCase.wantSubstr)
			}
		})
	}
}

func TestSetDownloadersSettingReloadSuccess(t *testing.T) {
	store := &fakeDownloaderSettingStore{}
	reloadCalls := 0
	handler, token := newDownloaderTestRouterWithReload(t, store, downloaderBranchAdmin, func(context.Context) error {
		reloadCalls++
		return nil
	})
	response := performDownloaderRequest(handler, http.MethodPost, downloadersSettingPath, validDownloaderBody, token)
	if response.Code != http.StatusOK || reloadCalls != 1 {
		t.Fatalf("POST = %d %s（重载 %d 次）", response.Code, response.Body.String(), reloadCalls)
	}
}

func TestReadDownloaderSettingsRequestLimits(t *testing.T) {
	request := func(body string) *http.Request {
		return httptest.NewRequest(http.MethodPost, downloadersSettingPath, strings.NewReader(body))
	}
	if _, err := readDownloaderSettingsRequest(request(validDownloaderBody), 0); err != nil {
		t.Fatalf("默认上限下合法请求 = %v", err)
	}
	if _, err := readDownloaderSettingsRequest(request(validDownloaderBody), int64(len(validDownloaderBody))-1); err == nil {
		t.Fatal("超出上限的请求应报错")
	}
	if _, err := readDownloaderSettingsRequest(request(`{`), 64); err == nil {
		t.Fatal("非法 JSON 应报错")
	}
}
