package httpapi

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/moviepilot-go/moviepilot-go/internal/storage"
)

// fakeStorageProvider 只实现手动整理需要的 Rename，其余能力返回零值。
type fakeStorageProvider struct {
	renameOK   bool
	renameErr  error
	renameArgs []struct {
		Storage   string
		Path      string
		Target    string
		Recursive bool
	}
}

func (f *fakeStorageProvider) List(context.Context, string, string, string, string) ([]storage.Item, error) {
	return nil, nil
}

func (f *fakeStorageProvider) Mkdir(context.Context, string, string, string) (bool, error) {
	return false, nil
}

func (f *fakeStorageProvider) Delete(context.Context, string, string) (bool, error) {
	return false, nil
}

func (f *fakeStorageProvider) Rename(_ context.Context, storageName, path, newName string, recursive bool) (bool, error) {
	f.renameArgs = append(f.renameArgs, struct {
		Storage   string
		Path      string
		Target    string
		Recursive bool
	}{storageName, path, newName, recursive})
	return f.renameOK, f.renameErr
}

func (f *fakeStorageProvider) Usage(context.Context, string) (storage.Usage, error) {
	return storage.Usage{}, nil
}

func (f *fakeStorageProvider) TransType(context.Context, string) (map[string]any, error) {
	return map[string]any{}, nil
}

func manualTransferRequest(body string) *http.Request {
	return httptest.NewRequest(http.MethodPost, "/transfer/manual", strings.NewReader(body))
}

func TestManualTransferValidatesInputAndProvider(t *testing.T) {
	// 坏 JSON。
	recorder := httptest.NewRecorder()
	Dependencies{}.manualTransfer(recorder, manualTransferRequest(`{"source_path":`))
	if body := decodeResponseBody(t, recorder.Body.Bytes()); body.Success || body.Message != "请求体不是合法 JSON" {
		t.Fatalf("坏 JSON = %+v", body)
	}

	// 缺路径（含只有空白的情况）。
	for _, payload := range []string{
		`{"source_path":"","target_path":"/x"}`,
		`{"source_path":"/a","target_path":"   "}`,
		`{}`,
	} {
		recorder = httptest.NewRecorder()
		Dependencies{}.manualTransfer(recorder, manualTransferRequest(payload))
		if body := decodeResponseBody(t, recorder.Body.Bytes()); body.Success ||
			body.Message != "source_path 与 target_path 不能为空" {
			t.Fatalf("%s = %+v", payload, body)
		}
	}

	// 没有存储实现。
	recorder = httptest.NewRecorder()
	Dependencies{}.manualTransfer(recorder, manualTransferRequest(`{"source_path":"/a","target_path":"/b"}`))
	if body := decodeResponseBody(t, recorder.Body.Bytes()); body.Success || body.Message != "未配置存储，无法手动整理" {
		t.Fatalf("无存储 = %+v", body)
	}
}

func TestManualTransferUsesProviderAndSkipsRecordingWithoutStore(t *testing.T) {
	provider := &fakeStorageProvider{renameOK: true}
	deps := Dependencies{Storage: provider}

	recorder := httptest.NewRecorder()
	deps.manualTransfer(recorder, manualTransferRequest(`{"source_path":" /src/a.mkv ","target_path":"/dst/a.mkv"}`))
	var body struct {
		Success bool   `json:"success"`
		Message string `json:"message"`
		Data    struct {
			SourcePath string `json:"source_path"`
			TargetPath string `json:"target_path"`
		} `json:"data"`
	}
	if err := json.Unmarshal(recorder.Body.Bytes(), &body); err != nil {
		t.Fatalf("解析响应: %v; body=%s", err, recorder.Body.String())
	}
	if !body.Success || body.Message != "整理完成" {
		t.Fatalf("成功响应 = %s", recorder.Body.String())
	}
	if body.Data.SourcePath != "/src/a.mkv" || body.Data.TargetPath != "/dst/a.mkv" {
		t.Fatalf("data = %+v", body.Data)
	}
	if len(provider.renameArgs) != 1 {
		t.Fatalf("Rename 调用次数 = %d", len(provider.renameArgs))
	}
	call := provider.renameArgs[0]
	if call.Storage != "local" || call.Path != "/src/a.mkv" || call.Target != "/dst/a.mkv" || call.Recursive {
		t.Fatalf("Rename 参数 = %+v", call)
	}

	// 显式 storage 透传。
	recorder = httptest.NewRecorder()
	deps.manualTransfer(recorder, manualTransferRequest(`{"source_path":"/s","target_path":"/t","storage":"alist"}`))
	if provider.renameArgs[1].Storage != "alist" {
		t.Fatalf("storage 未透传: %+v", provider.renameArgs[1])
	}
}

func TestManualTransferReportsProviderFailures(t *testing.T) {
	failing := &fakeStorageProvider{renameErr: errors.New("磁盘只读")}
	recorder := httptest.NewRecorder()
	Dependencies{Storage: failing}.manualTransfer(recorder, manualTransferRequest(`{"source_path":"/s","target_path":"/t"}`))
	if body := decodeResponseBody(t, recorder.Body.Bytes()); body.Success ||
		body.Message != "整理失败：磁盘只读" {
		t.Fatalf("Rename 失败 = %+v", body)
	}

	noop := &fakeStorageProvider{renameOK: false}
	recorder = httptest.NewRecorder()
	Dependencies{Storage: noop}.manualTransfer(recorder, manualTransferRequest(`{"source_path":"/s","target_path":"/t"}`))
	if body := decodeResponseBody(t, recorder.Body.Bytes()); body.Success ||
		body.Message != "整理失败：存储未执行该操作" {
		t.Fatalf("Rename 未执行 = %+v", body)
	}
}

func TestManualTransferHistoryAndTargetPath(t *testing.T) {
	// 没有存储：历史直接返回空数组。
	recorder := httptest.NewRecorder()
	Dependencies{}.manualTransferHistory(recorder, httptest.NewRequest(http.MethodPost, "/transfer/manual/history?count=5", nil))
	if strings.TrimSpace(recorder.Body.String()) != "[]" {
		t.Fatalf("历史 = %s", recorder.Body.String())
	}

	// 空请求体返回参数校验提示；合法参数用命名模板计算目标路径。
	recorder = httptest.NewRecorder()
	Dependencies{}.manualTransferTargetPath(recorder, httptest.NewRequest(http.MethodPost, "/transfer/manual/target-path", nil))
	body := decodeResponseBody(t, recorder.Body.Bytes())
	if body.Success {
		t.Fatalf("空请求体不应成功 = %+v", body)
	}

	recorder = httptest.NewRecorder()
	req := manualTransferRequest(`{"source_path":"/downloads/Dune.2021.2160p.mkv","target_path":"/media/movies","media_type":"电影","title":"沙丘","year":"2021"}`)
	Dependencies{}.manualTransferTargetPath(recorder, req)
	var targetResp struct {
		Success bool           `json:"success"`
		Message string         `json:"message"`
		Data    map[string]any `json:"data"`
	}
	if err := json.Unmarshal(recorder.Body.Bytes(), &targetResp); err != nil {
		t.Fatalf("解析目标路径响应失败: %v", err)
	}
	if !targetResp.Success {
		t.Fatalf("目标路径计算失败 = %+v", targetResp)
	}
	if got := targetResp.Data["target_path"]; got != "/media/movies/沙丘 (2021)/沙丘 (2021) - 2160p.mkv" {
		t.Fatalf("target_path = %v", got)
	}
}
