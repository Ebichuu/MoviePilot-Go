package httpapi

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/moviepilot-go/moviepilot-go/internal/storage"
)

// storageFake 只实现 Provider 的六项能力，故障与返回值都可注入。
type storageFake struct {
	list            []storage.Item
	listErr         error
	mkdirOK         bool
	mkdirErr        error
	deleteOK        bool
	deleteErr       error
	renameOK        bool
	renameErr       error
	renameRecursive bool
	usageValue      storage.Usage
	usageErr        error
	transType       map[string]any
	transTypeErr    error
}

func (f *storageFake) List(context.Context, string, string, string, string) ([]storage.Item, error) {
	return f.list, f.listErr
}

func (f *storageFake) Mkdir(context.Context, string, string, string) (bool, error) {
	return f.mkdirOK, f.mkdirErr
}

func (f *storageFake) Delete(context.Context, string, string) (bool, error) {
	return f.deleteOK, f.deleteErr
}

func (f *storageFake) Rename(_ context.Context, _, _, _ string, recursive bool) (bool, error) {
	f.renameRecursive = recursive
	return f.renameOK, f.renameErr
}

func (f *storageFake) Usage(context.Context, string) (storage.Usage, error) {
	return f.usageValue, f.usageErr
}

func (f *storageFake) TransType(context.Context, string) (map[string]any, error) {
	return f.transType, f.transTypeErr
}

// storageFileFake 追加文件读取能力，用来区分「支持下载」与「不支持下载」两种装配。
type storageFileFake struct {
	storageFake
	content     string
	contentType string
	openErr     error
}

func (f *storageFileFake) Open(context.Context, string, string) (io.ReadCloser, string, error) {
	if f.openErr != nil {
		return nil, "", f.openErr
	}
	return io.NopCloser(strings.NewReader(f.content)), f.contentType, nil
}

// storageAuthFake 追加远端存储登录能力。
type storageAuthFake struct {
	storageFake
	qrData         any
	qrMessage      string
	qrErr          error
	authURLData    any
	authURLMessage string
	authURLErr     error
	checkData      any
	checkMessage   string
	checkErr       error
	saveErr        error
	resetErr       error
}

func (f *storageAuthFake) QRCode(context.Context, string) (any, string, error) {
	return f.qrData, f.qrMessage, f.qrErr
}

func (f *storageAuthFake) AuthURL(context.Context, string) (any, string, error) {
	return f.authURLData, f.authURLMessage, f.authURLErr
}

func (f *storageAuthFake) CheckLogin(context.Context, string, string, string) (any, string, error) {
	return f.checkData, f.checkMessage, f.checkErr
}

func (f *storageAuthFake) SaveConfig(context.Context, string, map[string]any) error {
	return f.saveErr
}

func (f *storageAuthFake) ResetConfig(context.Context, string) error {
	return f.resetErr
}

const storageItemBody = `{"storage":"local","path":"/电影"}`

func storageNameRequest(method, target, name, body string) *http.Request {
	return siteParamRequest(method, target, "name", name, body)
}

// TestStorageProviderBranches 覆盖列表/建目录/删除/重命名/容量/类型转换的出口。
func TestStorageProviderBranches(t *testing.T) {
	recorder := httptest.NewRecorder()
	Dependencies{}.storageList(recorder, storageNameRequest(http.MethodPost, "/api/v1/storage/list", "", storageItemBody))
	if strings.TrimSpace(recorder.Body.String()) != "[]" {
		t.Fatalf("无存储列表 = %s", recorder.Body.String())
	}
	recorder = httptest.NewRecorder()
	Dependencies{Storage: &storageFake{}}.
		storageList(recorder, storageNameRequest(http.MethodPost, "/api/v1/storage/list", "", "nope"))
	if recorder.Code != http.StatusBadRequest {
		t.Fatalf("列表请求体非法 = %d", recorder.Code)
	}
	recorder = httptest.NewRecorder()
	Dependencies{Storage: &storageFake{listErr: storage.ErrPathRejected}}.
		storageList(recorder, storageNameRequest(http.MethodPost, "/api/v1/storage/list", "", storageItemBody))
	if recorder.Code != http.StatusBadRequest {
		t.Fatalf("路径被拒 = %d", recorder.Code)
	}
	recorder = httptest.NewRecorder()
	Dependencies{Storage: &storageFake{listErr: errors.New("boom")}}.
		storageList(recorder, storageNameRequest(http.MethodPost, "/api/v1/storage/list?sort=name&keyword=x", "", storageItemBody))
	if recorder.Code != http.StatusInternalServerError {
		t.Fatalf("列表失败 = %d", recorder.Code)
	}
	// 存储返回 nil 切片时必须归一成空数组。
	recorder = httptest.NewRecorder()
	Dependencies{Storage: &storageFake{}}.
		storageList(recorder, storageNameRequest(http.MethodPost, "/api/v1/storage/list", "", storageItemBody))
	if strings.TrimSpace(recorder.Body.String()) != "[]" {
		t.Fatalf("空列表归一 = %s", recorder.Body.String())
	}
	recorder = httptest.NewRecorder()
	Dependencies{Storage: &storageFake{list: []storage.Item{{Name: "电影", Type: "dir"}}}}.
		storageList(recorder, storageNameRequest(http.MethodPost, "/api/v1/storage/list", "", storageItemBody))
	if !strings.Contains(recorder.Body.String(), "电影") {
		t.Fatalf("列表正常 = %s", recorder.Body.String())
	}

	// mkdir：无存储、请求体非法、缺名称、失败、成功。
	recorder = httptest.NewRecorder()
	Dependencies{}.storageMkdir(recorder, storageNameRequest(http.MethodPost, "/x", "", storageItemBody))
	if !strings.Contains(recorder.Body.String(), `"success":false`) {
		t.Fatalf("无存储 mkdir = %s", recorder.Body.String())
	}
	recorder = httptest.NewRecorder()
	Dependencies{Storage: &storageFake{}}.
		storageMkdir(recorder, storageNameRequest(http.MethodPost, "/x", "", "nope"))
	if recorder.Code != http.StatusBadRequest {
		t.Fatalf("mkdir 请求体非法 = %d", recorder.Code)
	}
	recorder = httptest.NewRecorder()
	Dependencies{Storage: &storageFake{}}.
		storageMkdir(recorder, storageNameRequest(http.MethodPost, "/x", "", storageItemBody))
	if !strings.Contains(recorder.Body.String(), `"success":false`) {
		t.Fatalf("缺名称 mkdir = %s", recorder.Body.String())
	}
	recorder = httptest.NewRecorder()
	Dependencies{Storage: &storageFake{mkdirErr: storage.ErrUnsupportedStorage}}.
		storageMkdir(recorder, storageNameRequest(http.MethodPost, "/x?name=新目录", "", storageItemBody))
	if !strings.Contains(recorder.Body.String(), "存储实现未接入") {
		t.Fatalf("mkdir 不支持 = %s", recorder.Body.String())
	}
	recorder = httptest.NewRecorder()
	Dependencies{Storage: &storageFake{mkdirOK: true}}.
		storageMkdir(recorder, storageNameRequest(http.MethodPost, "/x?name=新目录", "", storageItemBody))
	if !strings.Contains(recorder.Body.String(), `"success":true`) {
		t.Fatalf("mkdir 成功 = %s", recorder.Body.String())
	}

	// delete：无存储、请求体非法、失败、成功。
	recorder = httptest.NewRecorder()
	Dependencies{}.storageDelete(recorder, storageNameRequest(http.MethodPost, "/x", "", storageItemBody))
	if !strings.Contains(recorder.Body.String(), `"success":false`) {
		t.Fatalf("无存储 delete = %s", recorder.Body.String())
	}
	recorder = httptest.NewRecorder()
	Dependencies{Storage: &storageFake{}}.storageDelete(recorder, storageNameRequest(http.MethodPost, "/x", "", "nope"))
	if recorder.Code != http.StatusBadRequest {
		t.Fatalf("delete 请求体非法 = %d", recorder.Code)
	}
	recorder = httptest.NewRecorder()
	Dependencies{Storage: &storageFake{deleteErr: errors.New("boom")}}.
		storageDelete(recorder, storageNameRequest(http.MethodPost, "/x", "", storageItemBody))
	if recorder.Code != http.StatusInternalServerError {
		t.Fatalf("delete 失败 = %d", recorder.Code)
	}
	recorder = httptest.NewRecorder()
	Dependencies{Storage: &storageFake{deleteOK: true}}.
		storageDelete(recorder, storageNameRequest(http.MethodPost, "/x", "", storageItemBody))
	if !strings.Contains(recorder.Body.String(), `"success":true`) {
		t.Fatalf("delete 成功 = %s", recorder.Body.String())
	}

	// rename：无存储、请求体非法、缺新名称、失败、递归成功。
	recorder = httptest.NewRecorder()
	Dependencies{}.storageRename(recorder, storageNameRequest(http.MethodPost, "/x", "", storageItemBody))
	if !strings.Contains(recorder.Body.String(), `"success":false`) {
		t.Fatalf("无存储 rename = %s", recorder.Body.String())
	}
	recorder = httptest.NewRecorder()
	Dependencies{Storage: &storageFake{}}.storageRename(recorder, storageNameRequest(http.MethodPost, "/x", "", "nope"))
	if recorder.Code != http.StatusBadRequest {
		t.Fatalf("rename 请求体非法 = %d", recorder.Code)
	}
	recorder = httptest.NewRecorder()
	Dependencies{Storage: &storageFake{}}.
		storageRename(recorder, storageNameRequest(http.MethodPut, "/x", "", storageItemBody))
	if !strings.Contains(recorder.Body.String(), "新名称为空") {
		t.Fatalf("缺新名称 rename = %s", recorder.Body.String())
	}
	recorder = httptest.NewRecorder()
	Dependencies{Storage: &storageFake{renameErr: storage.ErrInvalidRequest}}.
		storageRename(recorder, storageNameRequest(http.MethodPut, "/x?new_name=新", "", storageItemBody))
	if recorder.Code != http.StatusBadRequest {
		t.Fatalf("rename 参数无效 = %d", recorder.Code)
	}
	renameFake := &storageFake{renameOK: true}
	recorder = httptest.NewRecorder()
	Dependencies{Storage: renameFake}.
		storageRename(recorder, storageNameRequest(http.MethodPut, "/x?new_name=新&recursive=true", "", storageItemBody))
	if !strings.Contains(recorder.Body.String(), `"success":true`) || !renameFake.renameRecursive {
		t.Fatalf("rename 成功 = %s（recursive=%v）", recorder.Body.String(), renameFake.renameRecursive)
	}

	// usage：无存储、失败、正常。
	recorder = httptest.NewRecorder()
	Dependencies{}.storageUsage(recorder, storageNameRequest(http.MethodGet, "/x", "local", ""))
	if !strings.Contains(recorder.Body.String(), `"total":0`) {
		t.Fatalf("无存储 usage = %s", recorder.Body.String())
	}
	recorder = httptest.NewRecorder()
	Dependencies{Storage: &storageFake{usageErr: errors.New("boom")}}.
		storageUsage(recorder, storageNameRequest(http.MethodGet, "/x", "local", ""))
	if recorder.Code != http.StatusInternalServerError {
		t.Fatalf("usage 失败 = %d", recorder.Code)
	}
	recorder = httptest.NewRecorder()
	Dependencies{Storage: &storageFake{usageValue: storage.Usage{Total: 100, Available: 40}}}.
		storageUsage(recorder, storageNameRequest(http.MethodGet, "/x", "local", ""))
	if !strings.Contains(recorder.Body.String(), `"total":100`) {
		t.Fatalf("usage 正常 = %s", recorder.Body.String())
	}

	// transtype：无存储、失败、nil 归一、正常。
	recorder = httptest.NewRecorder()
	Dependencies{}.storageTransType(recorder, storageNameRequest(http.MethodGet, "/x", "local", ""))
	if !strings.Contains(recorder.Body.String(), `"transtype":{}`) {
		t.Fatalf("无存储 transtype = %s", recorder.Body.String())
	}
	recorder = httptest.NewRecorder()
	Dependencies{Storage: &storageFake{transTypeErr: errors.New("boom")}}.
		storageTransType(recorder, storageNameRequest(http.MethodGet, "/x", "local", ""))
	if recorder.Code != http.StatusInternalServerError {
		t.Fatalf("transtype 失败 = %d", recorder.Code)
	}
	recorder = httptest.NewRecorder()
	Dependencies{Storage: &storageFake{}}.
		storageTransType(recorder, storageNameRequest(http.MethodGet, "/x", "local", ""))
	if !strings.Contains(recorder.Body.String(), `"transtype":{}`) {
		t.Fatalf("transtype 空值 = %s", recorder.Body.String())
	}
	recorder = httptest.NewRecorder()
	Dependencies{Storage: &storageFake{transType: map[string]any{"local": "本地"}}}.
		storageTransType(recorder, storageNameRequest(http.MethodGet, "/x", "local", ""))
	if !strings.Contains(recorder.Body.String(), "本地") {
		t.Fatalf("transtype 正常 = %s", recorder.Body.String())
	}
}

// TestStorageFileBranches 覆盖下载与图片读取：不支持、请求体非法、打开失败、成功。
func TestStorageFileBranches(t *testing.T) {
	recorder := httptest.NewRecorder()
	Dependencies{Storage: &storageFake{}}.
		storageDownload(recorder, storageNameRequest(http.MethodPost, "/x", "", storageItemBody))
	if !strings.Contains(recorder.Body.String(), `"success":false`) {
		t.Fatalf("不支持下载 = %s", recorder.Body.String())
	}
	recorder = httptest.NewRecorder()
	Dependencies{Storage: &storageFileFake{}}.
		storageDownload(recorder, storageNameRequest(http.MethodPost, "/x", "", "nope"))
	if recorder.Code != http.StatusBadRequest {
		t.Fatalf("下载请求体非法 = %d", recorder.Code)
	}
	recorder = httptest.NewRecorder()
	Dependencies{Storage: &storageFileFake{openErr: errors.New("boom")}}.
		storageDownload(recorder, storageNameRequest(http.MethodPost, "/x", "", storageItemBody))
	if recorder.Code != http.StatusInternalServerError {
		t.Fatalf("下载打开失败 = %d", recorder.Code)
	}
	recorder = httptest.NewRecorder()
	Dependencies{Storage: &storageFileFake{content: "文件内容", contentType: "text/plain"}}.
		storageDownload(recorder, storageNameRequest(http.MethodPost, "/x", "", storageItemBody))
	if recorder.Body.String() != "文件内容" || recorder.Header().Get("Content-Type") != "text/plain" {
		t.Fatalf("下载正常 = %q（%s）", recorder.Body.String(), recorder.Header().Get("Content-Type"))
	}

	recorder = httptest.NewRecorder()
	Dependencies{Storage: &storageFake{}}.
		storageImage(recorder, storageNameRequest(http.MethodPost, "/x", "", storageItemBody))
	if recorder.Code != http.StatusInternalServerError {
		t.Fatalf("不支持图片 = %d", recorder.Code)
	}
	recorder = httptest.NewRecorder()
	Dependencies{Storage: &storageFileFake{}}.
		storageImage(recorder, storageNameRequest(http.MethodPost, "/x", "", "nope"))
	if recorder.Code != http.StatusBadRequest {
		t.Fatalf("图片请求体非法 = %d", recorder.Code)
	}
	recorder = httptest.NewRecorder()
	Dependencies{Storage: &storageFileFake{openErr: errors.New("boom")}}.
		storageImage(recorder, storageNameRequest(http.MethodPost, "/x", "", storageItemBody))
	if recorder.Code != http.StatusInternalServerError {
		t.Fatalf("图片打开失败 = %d", recorder.Code)
	}
	recorder = httptest.NewRecorder()
	Dependencies{Storage: &storageFileFake{content: "图片", contentType: "image/png"}}.
		storageImage(recorder, storageNameRequest(http.MethodPost, "/x", "", storageItemBody))
	if recorder.Body.String() != "图片" || recorder.Header().Get("Content-Type") != "image/jpeg" {
		t.Fatalf("图片正常 = %q（%s）", recorder.Body.String(), recorder.Header().Get("Content-Type"))
	}
}

// TestStorageAuthBranches 覆盖扫码/OAuth/登录检查/保存/重置的出口。
func TestStorageAuthBranches(t *testing.T) {
	// 未接入：四个端点都要给出稳定响应而不是报错。
	recorder := httptest.NewRecorder()
	Dependencies{Storage: &storageFake{}}.
		storageQRCode(recorder, storageNameRequest(http.MethodGet, "/x", "alist", ""))
	if !strings.Contains(recorder.Body.String(), "存储实现未接入") {
		t.Fatalf("扫码未接入 = %s", recorder.Body.String())
	}
	recorder = httptest.NewRecorder()
	Dependencies{Storage: &storageFake{}}.
		storageAuthURL(recorder, storageNameRequest(http.MethodGet, "/x", "alist", ""))
	if !strings.Contains(recorder.Body.String(), "存储实现未接入") {
		t.Fatalf("OAuth 未接入 = %s", recorder.Body.String())
	}
	recorder = httptest.NewRecorder()
	Dependencies{Storage: &storageFake{}}.
		storageCheck(recorder, storageNameRequest(http.MethodGet, "/x", "alist", ""))
	if !strings.Contains(recorder.Body.String(), "存储实现未接入") {
		t.Fatalf("登录检查未接入 = %s", recorder.Body.String())
	}
	recorder = httptest.NewRecorder()
	Dependencies{Storage: &storageFake{}}.
		storageSave(recorder, storageNameRequest(http.MethodPost, "/x", "alist", `{"a":1}`))
	if !strings.Contains(recorder.Body.String(), `"success":true`) {
		t.Fatalf("保存未接入 = %s", recorder.Body.String())
	}
	recorder = httptest.NewRecorder()
	Dependencies{Storage: &storageFake{}}.
		storageReset(recorder, storageNameRequest(http.MethodPost, "/x", "alist", ""))
	if !strings.Contains(recorder.Body.String(), `"success":true`) {
		t.Fatalf("重置未接入 = %s", recorder.Body.String())
	}

	// 扫码：操作失败按业务失败返回、一般错误 500、正常 200。
	recorder = httptest.NewRecorder()
	Dependencies{Storage: &storageAuthFake{qrErr: storage.ErrOperationFailed, qrMessage: "扫码失败"}}.
		storageQRCode(recorder, storageNameRequest(http.MethodGet, "/x", "alist", ""))
	if !strings.Contains(recorder.Body.String(), "扫码失败") {
		t.Fatalf("扫码业务失败 = %s", recorder.Body.String())
	}
	recorder = httptest.NewRecorder()
	Dependencies{Storage: &storageAuthFake{qrErr: errors.New("boom")}}.
		storageQRCode(recorder, storageNameRequest(http.MethodGet, "/x", "alist", ""))
	if recorder.Code != http.StatusInternalServerError {
		t.Fatalf("扫码一般失败 = %d", recorder.Code)
	}
	recorder = httptest.NewRecorder()
	Dependencies{Storage: &storageAuthFake{qrData: map[string]any{"qr": "base64"}, qrMessage: "ok"}}.
		storageQRCode(recorder, storageNameRequest(http.MethodGet, "/x", "alist", ""))
	if !strings.Contains(recorder.Body.String(), "base64") {
		t.Fatalf("扫码正常 = %s", recorder.Body.String())
	}

	// OAuth：无数据、一般错误、正常。
	recorder = httptest.NewRecorder()
	Dependencies{Storage: &storageAuthFake{authURLMessage: "需要先登录"}}.
		storageAuthURL(recorder, storageNameRequest(http.MethodGet, "/x", "alist", ""))
	if !strings.Contains(recorder.Body.String(), "需要先登录") {
		t.Fatalf("OAuth 无数据 = %s", recorder.Body.String())
	}
	recorder = httptest.NewRecorder()
	Dependencies{Storage: &storageAuthFake{authURLErr: errors.New("boom")}}.
		storageAuthURL(recorder, storageNameRequest(http.MethodGet, "/x", "alist", ""))
	if recorder.Code != http.StatusInternalServerError {
		t.Fatalf("OAuth 一般失败 = %d", recorder.Code)
	}
	recorder = httptest.NewRecorder()
	Dependencies{Storage: &storageAuthFake{authURLData: map[string]any{"url": "https://auth.example.com"}}}.
		storageAuthURL(recorder, storageNameRequest(http.MethodGet, "/x", "alist", ""))
	if !strings.Contains(recorder.Body.String(), "https://auth.example.com") {
		t.Fatalf("OAuth 正常 = %s", recorder.Body.String())
	}

	// 登录检查：无数据、一般错误、正常。
	recorder = httptest.NewRecorder()
	Dependencies{Storage: &storageAuthFake{checkMessage: "尚未登录"}}.
		storageCheck(recorder, storageNameRequest(http.MethodGet, "/x?ck=a&t=b", "alist", ""))
	if !strings.Contains(recorder.Body.String(), "尚未登录") {
		t.Fatalf("登录检查无数据 = %s", recorder.Body.String())
	}
	recorder = httptest.NewRecorder()
	Dependencies{Storage: &storageAuthFake{checkErr: errors.New("boom")}}.
		storageCheck(recorder, storageNameRequest(http.MethodGet, "/x", "alist", ""))
	if recorder.Code != http.StatusInternalServerError {
		t.Fatalf("登录检查一般失败 = %d", recorder.Code)
	}
	recorder = httptest.NewRecorder()
	Dependencies{Storage: &storageAuthFake{checkData: map[string]any{"logged_in": true}}}.
		storageCheck(recorder, storageNameRequest(http.MethodGet, "/x", "alist", ""))
	if !strings.Contains(recorder.Body.String(), "logged_in") {
		t.Fatalf("登录检查正常 = %s", recorder.Body.String())
	}

	// 保存配置：请求体非法、保存失败、成功。
	recorder = httptest.NewRecorder()
	Dependencies{Storage: &storageAuthFake{}}.
		storageSave(recorder, storageNameRequest(http.MethodPost, "/x", "alist", "nope"))
	if recorder.Code != http.StatusBadRequest {
		t.Fatalf("保存请求体非法 = %d", recorder.Code)
	}
	recorder = httptest.NewRecorder()
	Dependencies{Storage: &storageAuthFake{saveErr: storage.ErrUnsupportedStorage}}.
		storageSave(recorder, storageNameRequest(http.MethodPost, "/x", "alist", `{"a":1}`))
	if !strings.Contains(recorder.Body.String(), "存储实现未接入") {
		t.Fatalf("保存失败 = %s", recorder.Body.String())
	}
	recorder = httptest.NewRecorder()
	Dependencies{Storage: &storageAuthFake{}}.
		storageSave(recorder, storageNameRequest(http.MethodPost, "/x", "alist", `{"a":1}`))
	if !strings.Contains(recorder.Body.String(), `"success":true`) {
		t.Fatalf("保存成功 = %s", recorder.Body.String())
	}

	// 重置配置：失败与成功。
	recorder = httptest.NewRecorder()
	Dependencies{Storage: &storageAuthFake{resetErr: errors.New("boom")}}.
		storageReset(recorder, storageNameRequest(http.MethodPost, "/x", "alist", ""))
	if recorder.Code != http.StatusInternalServerError {
		t.Fatalf("重置失败 = %d", recorder.Code)
	}
	recorder = httptest.NewRecorder()
	Dependencies{Storage: &storageAuthFake{}}.
		storageReset(recorder, storageNameRequest(http.MethodPost, "/x", "alist", ""))
	if !strings.Contains(recorder.Body.String(), `"success":true`) {
		t.Fatalf("重置成功 = %s", recorder.Body.String())
	}
}
