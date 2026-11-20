package httpapi

import (
	"context"
	"encoding/json"
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

func storageTestDeps(t *testing.T) (Dependencies, string) {
	t.Helper()
	root := t.TempDir()
	provider := storage.NewLocalProvider(map[string]string{"local": root})
	deps := Dependencies{
		Config:  config.Config{Environment: "test", MaxRequestBody: 1 << 20},
		Storage: provider,
	}
	return deps, root
}

func storageRequest(method, body, query string) *http.Request {
	request := httptest.NewRequest(method, "/api/v1/storage/list?"+query, strings.NewReader(body))
	request.Header.Set("Content-Type", "application/json")
	return request
}

func decodeBody[T any](t *testing.T, recorder *httptest.ResponseRecorder) T {
	t.Helper()
	var value T
	if err := json.Unmarshal(recorder.Body.Bytes(), &value); err != nil {
		t.Fatalf("响应不是合法 JSON（%v）: %s", err, recorder.Body.String())
	}
	return value
}

func TestStorageHandlerFailureMapping(t *testing.T) {
	cases := []struct {
		err    error
		status int
	}{
		{storage.ErrUnsupportedStorage, http.StatusOK},
		{storage.ErrPathRejected, http.StatusBadRequest},
		{storage.ErrInvalidRequest, http.StatusBadRequest},
		{errors.New("boom"), http.StatusInternalServerError},
	}
	for _, testCase := range cases {
		recorder := httptest.NewRecorder()
		writeStorageFailure(recorder, testCase.err)
		if recorder.Code != testCase.status {
			t.Fatalf("%v 应映射为 %d，实际 %d", testCase.err, testCase.status, recorder.Code)
		}
		envelope := decodeBody[Response](t, recorder)
		if envelope.Success {
			t.Fatalf("%v 不应返回成功信封", testCase.err)
		}
		if strings.TrimSpace(envelope.Message) == "" {
			t.Fatalf("%v 的错误响应必须带说明", testCase.err)
		}
	}
}

func TestDecodeFileItemRejectsBadBody(t *testing.T) {
	recorder := httptest.NewRecorder()
	if _, ok := decodeFileItem(recorder, storageRequest("POST", "not-json", "")); ok {
		t.Fatal("非法 JSON 应被拒绝")
	}
	if recorder.Code != http.StatusBadRequest {
		t.Fatalf("非法 JSON 应返回 400，实际 %d", recorder.Code)
	}

	recorder = httptest.NewRecorder()
	input, ok := decodeFileItem(recorder, storageRequest("POST", `{"storage":"local","path":"sub"}`, ""))
	if !ok || input.Storage != "local" || input.Path != "sub" {
		t.Fatalf("合法请求解析失败: %+v %v", input, ok)
	}
}

func TestStorageListWithoutProviderReturnsEmptyArray(t *testing.T) {
	deps := Dependencies{Config: config.Config{Environment: "test", MaxRequestBody: 1 << 20}}
	recorder := httptest.NewRecorder()
	deps.storageList(recorder, storageRequest("POST", `{}`, ""))
	if recorder.Code != http.StatusOK {
		t.Fatalf("未接入存储时应返回 200，实际 %d", recorder.Code)
	}
	if got := strings.TrimSpace(recorder.Body.String()); got != "[]" {
		t.Fatalf("未接入存储时应返回空数组而不是 null: %s", got)
	}
}

func TestStorageListProjectsRealDirectory(t *testing.T) {
	deps, root := storageTestDeps(t)
	if err := os.MkdirAll(filepath.Join(root, "电影"), 0o755); err != nil {
		t.Fatalf("准备目录失败: %v", err)
	}
	if err := os.WriteFile(filepath.Join(root, "a.mkv"), []byte("x"), 0o644); err != nil {
		t.Fatalf("准备文件失败: %v", err)
	}

	recorder := httptest.NewRecorder()
	deps.storageList(recorder, storageRequest("POST", `{"storage":"local","path":""}`, "sort=name"))
	if recorder.Code != http.StatusOK {
		t.Fatalf("状态码 = %d: %s", recorder.Code, recorder.Body.String())
	}
	items := decodeBody[[]storage.Item](t, recorder)
	if len(items) != 2 {
		t.Fatalf("应返回 2 个条目，实际 %d: %+v", len(items), items)
	}
	if items[0].Name != "a.mkv" || items[0].Type != "file" {
		t.Fatalf("按名称排序结果不对: %+v", items[0])
	}

	// 越界路径必须被拒成 400，而不是把根目录之外的内容列出来。
	recorder = httptest.NewRecorder()
	deps.storageList(recorder, storageRequest("POST", `{"storage":"local","path":"../etc"}`, ""))
	if recorder.Code != http.StatusBadRequest {
		t.Fatalf("越界路径应返回 400，实际 %d", recorder.Code)
	}

	// 未注册的存储名走「未接入」分支：200 + 非成功信封。
	recorder = httptest.NewRecorder()
	deps.storageList(recorder, storageRequest("POST", `{"storage":"nope","path":""}`, ""))
	if recorder.Code != http.StatusOK {
		t.Fatalf("未注册存储应返回 200，实际 %d", recorder.Code)
	}
}

func TestStorageMkdirHandler(t *testing.T) {
	deps, root := storageTestDeps(t)

	// 缺少 name 时不能假装建成功。
	recorder := httptest.NewRecorder()
	deps.storageMkdir(recorder, storageRequest("POST", `{"storage":"local"}`, ""))
	if envelope := decodeBody[Response](t, recorder); envelope.Success {
		t.Fatal("缺少 name 时不应返回成功")
	}

	recorder = httptest.NewRecorder()
	deps.storageMkdir(recorder, storageRequest("POST", `{"storage":"local"}`, "name=新目录"))
	if envelope := decodeBody[Response](t, recorder); !envelope.Success {
		t.Fatalf("创建目录应成功: %s", recorder.Body.String())
	}
	if info, err := os.Stat(filepath.Join(root, "新目录")); err != nil || !info.IsDir() {
		t.Fatalf("目录未真正创建: %v", err)
	}

	// 名字里带分隔符一律拒绝。
	recorder = httptest.NewRecorder()
	deps.storageMkdir(recorder, storageRequest("POST", `{"storage":"local"}`, "name=a/b"))
	if recorder.Code != http.StatusBadRequest {
		t.Fatalf("非法目录名应返回 400，实际 %d", recorder.Code)
	}
}

func TestStorageDeleteAndRenameHandlers(t *testing.T) {
	deps, root := storageTestDeps(t)
	target := filepath.Join(root, "a.txt")
	if err := os.WriteFile(target, []byte("x"), 0o644); err != nil {
		t.Fatalf("准备文件失败: %v", err)
	}

	recorder := httptest.NewRecorder()
	deps.storageRename(recorder, storageRequest("POST", `{"storage":"local","path":"a.txt"}`, "new_name=b.txt"))
	if envelope := decodeBody[Response](t, recorder); !envelope.Success {
		t.Fatalf("重命名应成功: %s", recorder.Body.String())
	}
	if _, err := os.Stat(filepath.Join(root, "b.txt")); err != nil {
		t.Fatalf("重命名后文件不存在: %v", err)
	}

	recorder = httptest.NewRecorder()
	deps.storageRename(recorder, storageRequest("POST", `{"storage":"local","path":"b.txt"}`, ""))
	if envelope := decodeBody[Response](t, recorder); envelope.Success {
		t.Fatal("缺少 new_name 时不应返回成功")
	}

	recorder = httptest.NewRecorder()
	deps.storageDelete(recorder, storageRequest("POST", `{"storage":"local","path":"b.txt"}`, ""))
	if envelope := decodeBody[Response](t, recorder); !envelope.Success {
		t.Fatalf("删除应成功: %s", recorder.Body.String())
	}
	if _, err := os.Stat(filepath.Join(root, "b.txt")); !os.IsNotExist(err) {
		t.Fatalf("文件应已被删除: %v", err)
	}

	// 删除根目录是危险操作，必须 400。
	recorder = httptest.NewRecorder()
	deps.storageDelete(recorder, storageRequest("POST", `{"storage":"local","path":"."}`, ""))
	if recorder.Code != http.StatusBadRequest {
		t.Fatalf("删除根目录应返回 400，实际 %d", recorder.Code)
	}
}

func TestStorageUsageHandler(t *testing.T) {
	deps, _ := storageTestDeps(t)
	request := httptest.NewRequest("GET", "/api/v1/storage/usage/local", nil)
	routeContext := chi.NewRouteContext()
	routeContext.URLParams.Add("name", "local")
	request = request.WithContext(context.WithValue(request.Context(), chi.RouteCtxKey, routeContext))

	recorder := httptest.NewRecorder()
	deps.storageUsage(recorder, request)
	if recorder.Code != http.StatusOK {
		t.Fatalf("状态码 = %d: %s", recorder.Code, recorder.Body.String())
	}
	usage := decodeBody[storage.Usage](t, recorder)
	if usage.Total < 0 || usage.Available < 0 {
		t.Fatalf("容量不应为负: %+v", usage)
	}

	// 未接入存储时返回零值容量，而不是 null。
	recorder = httptest.NewRecorder()
	Dependencies{}.storageUsage(recorder, request)
	if recorder.Code != http.StatusOK {
		t.Fatalf("未接入存储应返回 200，实际 %d", recorder.Code)
	}
	if body := strings.TrimSpace(recorder.Body.String()); strings.HasPrefix(body, "null") {
		t.Fatalf("未接入存储应返回零值对象: %s", body)
	}
}
