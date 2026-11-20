package httpapi

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"

	"github.com/moviepilot-go/moviepilot-go/internal/config"
	"github.com/moviepilot-go/moviepilot-go/internal/storage"
)

// fakeAuthProvider 只实现登录/配置能力与 TransType，用来驱动这些处理器的分支。
type fakeAuthProvider struct {
	storage.Provider
	data      any
	message   string
	err       error
	transType map[string]any
	saved     map[string]any
	reset     bool
}

func (f *fakeAuthProvider) QRCode(context.Context, string) (any, string, error) {
	return f.data, f.message, f.err
}

func (f *fakeAuthProvider) AuthURL(context.Context, string) (any, string, error) {
	return f.data, f.message, f.err
}

func (f *fakeAuthProvider) CheckLogin(context.Context, string, string, string) (any, string, error) {
	return f.data, f.message, f.err
}

func (f *fakeAuthProvider) SaveConfig(_ context.Context, _ string, conf map[string]any) error {
	f.saved = conf
	return f.err
}

func (f *fakeAuthProvider) ResetConfig(context.Context, string) error {
	f.reset = true
	return f.err
}

func (f *fakeAuthProvider) TransType(context.Context, string) (map[string]any, error) {
	return f.transType, nil
}

func chiRequest(method, body string, params map[string]string, query string) *http.Request {
	target := "/api/v1/storage"
	if query != "" {
		target += "?" + query
	}
	request := httptest.NewRequest(method, target, strings.NewReader(body))
	request.Header.Set("Content-Type", "application/json")
	routeContext := chi.NewRouteContext()
	for name, value := range params {
		routeContext.URLParams.Add(name, value)
	}
	return request.WithContext(context.WithValue(request.Context(), chi.RouteCtxKey, routeContext))
}

func localStorageDeps(t *testing.T) (Dependencies, string) {
	t.Helper()
	root := t.TempDir()
	return Dependencies{
		Config:  config.Config{Environment: "test", MaxRequestBody: 1 << 20},
		Storage: storage.NewLocalProvider(map[string]string{"local": root}),
	}, root
}

func TestStorageDownloadStreamsFile(t *testing.T) {
	deps, root := localStorageDeps(t)
	content := []byte("plain-text-content")
	if err := os.WriteFile(filepath.Join(root, "a.txt"), content, 0o644); err != nil {
		t.Fatalf("准备文件失败: %v", err)
	}

	recorder := httptest.NewRecorder()
	deps.storageDownload(recorder, chiRequest("POST", `{"storage":"local","path":"a.txt"}`, nil, ""))
	if recorder.Code != http.StatusOK {
		t.Fatalf("状态码 = %d: %s", recorder.Code, recorder.Body.String())
	}
	if got := recorder.Header().Get("Content-Type"); !strings.Contains(got, "text/plain") {
		t.Fatalf("Content-Type = %q，期望 text/plain", got)
	}
	if recorder.Body.String() != string(content) {
		t.Fatalf("响应体 = %q", recorder.Body.String())
	}
}

func TestStorageDownloadFailures(t *testing.T) {
	// 没有可打开能力的实现：返回非成功信封而不是 500。
	recorder := httptest.NewRecorder()
	Dependencies{Config: config.Config{Environment: "test", MaxRequestBody: 1 << 20}}.
		storageDownload(recorder, chiRequest("POST", `{}`, nil, ""))
	if recorder.Code != http.StatusOK {
		t.Fatalf("未接入存储时应返回 200，实际 %d", recorder.Code)
	}
	if envelope := decodeBody[Response](t, recorder); envelope.Success {
		t.Fatal("未接入存储时不应返回成功")
	}

	// 文件不存在：写入失败统一映射成 500。
	deps, _ := localStorageDeps(t)
	recorder = httptest.NewRecorder()
	deps.storageDownload(recorder, chiRequest("POST", `{"storage":"local","path":"missing.txt"}`, nil, ""))
	if recorder.Code != http.StatusInternalServerError {
		t.Fatalf("缺失文件应返回 500，实际 %d", recorder.Code)
	}
}

func TestStorageImageForcesJPEGContentType(t *testing.T) {
	deps, root := localStorageDeps(t)
	if err := os.WriteFile(filepath.Join(root, "cover.txt"), []byte("img"), 0o644); err != nil {
		t.Fatalf("准备文件失败: %v", err)
	}

	recorder := httptest.NewRecorder()
	deps.storageImage(recorder, chiRequest("POST", `{"storage":"local","path":"cover.txt"}`, nil, ""))
	if recorder.Code != http.StatusOK {
		t.Fatalf("状态码 = %d", recorder.Code)
	}
	// 图片接口固定声明 image/jpeg（前端按图片渲染缩略图）。
	if got := recorder.Header().Get("Content-Type"); got != "image/jpeg" {
		t.Fatalf("Content-Type = %q，期望 image/jpeg", got)
	}
	if recorder.Body.String() != "img" {
		t.Fatalf("响应体 = %q", recorder.Body.String())
	}

	// 读取失败必须 500 且带可读说明，不能返回空图。
	recorder = httptest.NewRecorder()
	deps.storageImage(recorder, chiRequest("POST", `{"storage":"local","path":"missing.txt"}`, nil, ""))
	if recorder.Code != http.StatusInternalServerError {
		t.Fatalf("缺失文件应返回 500，实际 %d", recorder.Code)
	}
	if !strings.Contains(recorder.Body.String(), "图片读取出错") {
		t.Fatalf("错误信息不对: %s", recorder.Body.String())
	}
}

func TestStorageAuthHandlersWithoutManager(t *testing.T) {
	deps := Dependencies{Config: config.Config{Environment: "test", MaxRequestBody: 1 << 20}}

	recorder := httptest.NewRecorder()
	deps.storageQRCode(recorder, chiRequest("GET", "", map[string]string{"name": "local"}, ""))
	envelope := decodeBody[Response](t, recorder)
	if envelope.Success || !strings.Contains(envelope.Message, "未接入") {
		t.Fatalf("没有登录能力时应返回未接入提示: %+v", envelope)
	}

	// 保存/重置在没有登录能力时是无操作成功。
	recorder = httptest.NewRecorder()
	deps.storageSave(recorder, chiRequest("POST", `{"k":"v"}`, map[string]string{"name": "local"}, ""))
	if envelope := decodeBody[Response](t, recorder); !envelope.Success {
		t.Fatal("无登录能力时保存配置应视为成功")
	}
	recorder = httptest.NewRecorder()
	deps.storageReset(recorder, chiRequest("POST", "", map[string]string{"name": "local"}, ""))
	if envelope := decodeBody[Response](t, recorder); !envelope.Success {
		t.Fatal("无登录能力时重置配置应视为成功")
	}

	// 分发方式为空时返回空对象而不是 null。
	recorder = httptest.NewRecorder()
	deps.storageTransType(recorder, chiRequest("GET", "", map[string]string{"name": "local"}, ""))
	if body := strings.TrimSpace(recorder.Body.String()); !strings.Contains(body, `"transtype":{}`) {
		t.Fatalf("空分发方式应返回空对象: %s", body)
	}
}

func TestStorageAuthHandlersWithManager(t *testing.T) {
	provider := &fakeAuthProvider{data: map[string]any{"qrcode": "data:image/png;base64,xx"}, message: "扫码"}
	deps := Dependencies{Config: config.Config{Environment: "test", MaxRequestBody: 1 << 20}, Storage: provider}

	recorder := httptest.NewRecorder()
	deps.storageQRCode(recorder, chiRequest("GET", "", map[string]string{"name": "alist"}, ""))
	envelope := decodeBody[Response](t, recorder)
	if !envelope.Success || envelope.Message != "扫码" {
		t.Fatalf("QRCode 信封 = %+v", envelope)
	}

	recorder = httptest.NewRecorder()
	deps.storageAuthURL(recorder, chiRequest("GET", "", map[string]string{"name": "alist"}, ""))
	if envelope := decodeBody[Response](t, recorder); !envelope.Success {
		t.Fatalf("AuthURL 应成功: %+v", envelope)
	}

	recorder = httptest.NewRecorder()
	deps.storageCheck(recorder, chiRequest("GET", "", map[string]string{"name": "alist"}, "ck=abc&t=xyz"))
	if envelope := decodeBody[Response](t, recorder); !envelope.Success {
		t.Fatalf("CheckLogin 应成功: %+v", envelope)
	}

	recorder = httptest.NewRecorder()
	deps.storageSave(recorder, chiRequest("POST", `{"root":"/media"}`, map[string]string{"name": "alist"}, ""))
	if envelope := decodeBody[Response](t, recorder); !envelope.Success {
		t.Fatal("保存配置应成功")
	}
	if provider.saved["root"] != "/media" {
		t.Fatalf("配置未透传: %+v", provider.saved)
	}

	recorder = httptest.NewRecorder()
	deps.storageReset(recorder, chiRequest("POST", "", map[string]string{"name": "alist"}, ""))
	if !provider.reset {
		t.Fatal("重置应调用实现")
	}

	provider.transType = map[string]any{"trans_type": "115"}
	recorder = httptest.NewRecorder()
	deps.storageTransType(recorder, chiRequest("GET", "", map[string]string{"name": "alist"}, ""))
	if body := recorder.Body.String(); !strings.Contains(body, "115") {
		t.Fatalf("分发方式应透传: %s", body)
	}
}

func TestStorageAuthHandlersBranchOnDataAndError(t *testing.T) {
	base := Dependencies{Config: config.Config{Environment: "test", MaxRequestBody: 1 << 20}}

	// data 为 nil + ErrOperationFailed：视为「暂时还没有二维码」，200 且非成功。
	provider := &fakeAuthProvider{data: nil, message: "尚未生成", err: storage.ErrOperationFailed}
	base.Storage = provider
	recorder := httptest.NewRecorder()
	base.storageQRCode(recorder, chiRequest("GET", "", map[string]string{"name": "alist"}, ""))
	if recorder.Code != http.StatusOK {
		t.Fatalf("ErrOperationFailed 不应变成 500，实际 %d", recorder.Code)
	}
	envelope := decodeBody[Response](t, recorder)
	if envelope.Success || envelope.Message != "尚未生成" {
		t.Fatalf("信封 = %+v", envelope)
	}

	// 未接入的存储名：200 + 未接入说明。
	provider = &fakeAuthProvider{err: storage.ErrUnsupportedStorage}
	base.Storage = provider
	recorder = httptest.NewRecorder()
	base.storageAuthURL(recorder, chiRequest("GET", "", map[string]string{"name": "nope"}, ""))
	if recorder.Code != http.StatusOK {
		t.Fatalf("未注册存储应返回 200，实际 %d", recorder.Code)
	}

	// 真正的内部错误：500。
	provider = &fakeAuthProvider{err: errors.New("boom")}
	base.Storage = provider
	recorder = httptest.NewRecorder()
	base.storageCheck(recorder, chiRequest("GET", "", map[string]string{"name": "alist"}, ""))
	if recorder.Code != http.StatusInternalServerError {
		t.Fatalf("内部错误应返回 500，实际 %d", recorder.Code)
	}
}

func TestStorageSaveRejectsInvalidBody(t *testing.T) {
	provider := &fakeAuthProvider{data: map[string]any{"k": "v"}}
	deps := Dependencies{Config: config.Config{Environment: "test", MaxRequestBody: 1 << 20}, Storage: provider}

	recorder := httptest.NewRecorder()
	deps.storageSave(recorder, chiRequest("POST", "not-json", map[string]string{"name": "alist"}, ""))
	if recorder.Code != http.StatusBadRequest {
		t.Fatalf("非法配置体应返回 400，实际 %d", recorder.Code)
	}
	if provider.saved != nil {
		t.Fatal("解析失败时不应调用实现")
	}
}
