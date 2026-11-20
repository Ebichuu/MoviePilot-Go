package httpapi

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
)

func submitDownloadBody(enclosure string, site int, downloader, savePath string) string {
	body := `{"torrent_in":{"site":` + strconv.Itoa(site) + `,"enclosure":"` + enclosure + `"}`
	if downloader != "" {
		body += `,"downloader":"` + downloader + `"`
	}
	if savePath != "" {
		body += `,"save_path":"` + savePath + `"`
	}
	return body + `}`
}

const validDownloadEnclosure = "/api/v1/resource/download/7/abc"

// TestSubmitDownloadRejectsInvalidRequests 覆盖提交下载前的各项校验与
// 「下载服务不可用 / 没有下载器 / 路径不在白名单」三档降级。
func TestSubmitDownloadRejectsInvalidRequests(t *testing.T) {
	submission := &fakeDownloadSubmission{}
	valid := standardDownloadDependencies(standardDownloadRequestStore(), submission)

	cases := []struct {
		name     string
		deps     Dependencies
		body     string
		username string
		code     int
		message  string
	}{
		{name: "请求体不是 JSON", deps: valid, body: "nope", username: "alice",
			code: http.StatusUnprocessableEntity, message: "下载请求 JSON 无效"},
		{name: "没有登录态", deps: valid, body: submitDownloadBody(validDownloadEnclosure, 7, "", ""),
			code: http.StatusOK, message: "用户身份无效"},
		{name: "资源地址非法", deps: valid, body: submitDownloadBody("/other/path", 7, "", ""),
			code: http.StatusOK, message: "下载来源无效"},
		{name: "站点与地址不一致", deps: valid, body: submitDownloadBody(validDownloadEnclosure, 8, "", ""),
			code: http.StatusOK, message: "下载来源无效"},
		{name: "站点没有解析器", deps: valid,
			body: submitDownloadBody("/api/v1/resource/download/99/abc", 99, "", ""),
			code: http.StatusOK, message: "下载来源无效"},
		{name: "下载请求存储不可用", deps: Dependencies{
			Config: valid.Config, SearchRegistry: downloadHTTPRegistry(), DownloadSubmissions: submission,
		}, body: submitDownloadBody(validDownloadEnclosure, 7, "", ""),
			code: http.StatusOK, message: "下载服务暂不可用"},
		{name: "没有可用下载器", deps: standardDownloadDependencies(&fakeDownloadRequestStore{}, submission),
			body: submitDownloadBody(validDownloadEnclosure, 7, "", ""),
			code: http.StatusOK, message: "没有可用的下载器"},
		{name: "保存路径不在白名单", deps: valid,
			body: submitDownloadBody(validDownloadEnclosure, 7, "", "/etc/passwd"),
			code: http.StatusOK, message: "保存路径不在允许的下载目录中"},
		{name: "下载提交能力缺失", deps: Dependencies{
			Config: valid.Config, SearchRegistry: downloadHTTPRegistry(),
			DownloadRequests: standardDownloadRequestStore(),
		}, body: submitDownloadBody(validDownloadEnclosure, 7, "", ""),
			code: http.StatusOK, message: "下载服务暂不可用"},
		{name: "提交失败", deps: standardDownloadDependencies(standardDownloadRequestStore(),
			&fakeDownloadSubmission{err: errors.New("boom")}),
			body: submitDownloadBody(validDownloadEnclosure, 7, "", ""),
			code: http.StatusOK, message: "任务添加失败"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			username := tc.username
			if username == "" && tc.message != "用户身份无效" {
				username = "alice"
			}
			recorder := httptest.NewRecorder()
			tc.deps.submitDownload(recorder, downloadHTTPRequest(tc.body, username))
			if recorder.Code != tc.code {
				t.Fatalf("状态码 = %d，期望 %d（%s）", recorder.Code, tc.code, recorder.Body.String())
			}
			if !strings.Contains(recorder.Body.String(), tc.message) {
				t.Fatalf("响应 = %s，期望包含 %q", recorder.Body.String(), tc.message)
			}
		})
	}
}

// TestSubmitDownloadSavePathUnavailable 覆盖「保存路径校验时存储不可用」这条与
// 「没有可下载器」不同的错误映射分支。
func TestSubmitDownloadSavePathUnavailable(t *testing.T) {
	store := &fakeDownloadRequestStore{
		downloaders:      standardDownloaders(),
		downloadersFound: true,
		settingErr:       errors.New("boom"),
	}
	deps := standardDownloadDependencies(store, &fakeDownloadSubmission{})
	recorder := httptest.NewRecorder()
	deps.submitDownload(recorder, downloadHTTPRequest(
		submitDownloadBody(validDownloadEnclosure, 7, "", "/downloads"), "alice"))
	if recorder.Code != http.StatusOK || !strings.Contains(recorder.Body.String(), "下载服务暂不可用") {
		t.Fatalf("保存路径读取失败 = %d %s", recorder.Code, recorder.Body.String())
	}
}

// TestParseDownloadEnclosureBranches 覆盖资源地址解析的各条拒绝理由。
func TestParseDownloadEnclosureBranches(t *testing.T) {
	valid := []string{
		"/api/v1/resource/download/7/abc",
		"/api/v1/resource/download/7/a%2Fb",
	}
	for _, value := range valid {
		if siteID, locator, err := parseDownloadEnclosure(value); err != nil || siteID != 7 || locator == "" {
			t.Fatalf("%q = (%d,%q,%v)", value, siteID, locator, err)
		}
	}
	invalid := []string{
		"",
		"/api/v1/resource/other/7/abc",
		"https://example.com/api/v1/resource/download/7/abc",
		"/api/v1/resource/download/7",
		"/api/v1/resource/download/0/abc",
		"/api/v1/resource/download/abc/abc",
		"/api/v1/resource/download/7/%2e",
		"/api/v1/resource/download/7/abc?x=1",
	}
	for _, value := range invalid {
		if _, _, err := parseDownloadEnclosure(value); err == nil {
			t.Fatalf("%q 应被拒绝", value)
		}
	}
}

// TestDecodeDownloadRequestBranches 覆盖请求体读取与「多个 JSON 值」的拒绝。
func TestDecodeDownloadRequestBranches(t *testing.T) {
	if _, err := decodeDownloadRequest(nil, 0); err == nil {
		t.Fatal("空请求应报错")
	}
	if _, err := decodeDownloadRequest(httptest.NewRequest(http.MethodPost, "/download", nil), 0); err == nil {
		t.Fatal("空请求体应报错")
	}
	if _, err := decodeDownloadRequest(
		httptest.NewRequest(http.MethodPost, "/download", strings.NewReader(`{"a":1}{"b":2}`)), 0); err == nil {
		t.Fatal("多个 JSON 值应报错")
	}
	if _, err := decodeDownloadRequest(
		httptest.NewRequest(http.MethodPost, "/download", strings.NewReader(strings.Repeat("x", 64))), 8); err == nil {
		t.Fatal("超出长度限制应报错")
	}
}
