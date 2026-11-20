package httpapi

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"

	"github.com/moviepilot-go/moviepilot-go/internal/auth"
	"github.com/moviepilot-go/moviepilot-go/internal/downloader"
)

const (
	downloadControlHash40 = "0123456789abcdef0123456789abcdef01234567"
	downloadControlHash64 = "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"
)

type downloadControlCall struct {
	Downloader string
	Hash       string
	Action     downloader.TaskControlAction
}

type fakeDownloadControlSource struct {
	task       NamedDownloadTask
	found      bool
	findErr    error
	controlErr error
	findCalls  int
	controls   []downloadControlCall
}

func (s *fakeDownloadControlSource) ListDownloading(context.Context, string) ([]NamedDownloadTask, error) {
	return nil, nil
}

func (s *fakeDownloadControlSource) FindDownloadTask(
	_ context.Context,
	_ string,
	_ string,
) (NamedDownloadTask, bool, error) {
	s.findCalls++
	if s.findErr != nil {
		return NamedDownloadTask{}, false, s.findErr
	}
	if !s.found {
		return NamedDownloadTask{}, false, nil
	}
	return s.task, true, nil
}

func (s *fakeDownloadControlSource) ControlDownloadTask(
	_ context.Context,
	downloaderName string,
	hash string,
	action downloader.TaskControlAction,
) error {
	s.controls = append(s.controls, downloadControlCall{Downloader: downloaderName, Hash: hash, Action: action})
	return s.controlErr
}

// fakeListOnlyDownloadSource 只实现列表查询，用来验证控制接口的类型断言失败分支。
type fakeListOnlyDownloadSource struct{}

func (fakeListOnlyDownloadSource) ListDownloading(context.Context, string) ([]NamedDownloadTask, error) {
	return nil, nil
}

func usableDownloadControlSource() *fakeDownloadControlSource {
	return &fakeDownloadControlSource{
		found: true,
		task: NamedDownloadTask{
			Downloader: "qbittorrent",
			Task:       downloader.Task{Label: "某剧", Hash: strings.ToUpper(downloadControlHash40)},
		},
	}
}

func TestDownloadControlEndpointsMapActions(t *testing.T) {
	tests := []struct {
		name       string
		hash       string
		method     string
		pathPrefix string
		action     downloader.TaskControlAction
	}{
		{name: "开始任务", hash: downloadControlHash40, method: http.MethodGet, pathPrefix: "/download/start/", action: downloader.TaskControlStart},
		{name: "停止任务", hash: downloadControlHash40, method: http.MethodGet, pathPrefix: "/download/stop/", action: downloader.TaskControlStop},
		{name: "删除任务", hash: downloadControlHash64, method: http.MethodDelete, pathPrefix: "/download/", action: downloader.TaskControlDelete},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			source := usableDownloadControlSource()
			user := auth.User{ID: 7, Username: "alice"}
			router := chi.NewRouter()
			Dependencies{
				Users:         fakeUserLookup{user: user},
				DownloadTasks: source,
			}.registerProtected(router)

			request := downloadListRequest(test.pathPrefix+test.hash+"?name=qbittorrent", user)
			request.Method = test.method
			response := httptest.NewRecorder()
			router.ServeHTTP(response, request)

			if response.Code != http.StatusOK {
				t.Fatalf("status = %d, body=%s", response.Code, response.Body.String())
			}
			if !downloadControlSuccess(t, response) {
				t.Fatalf("success = false, body=%s", response.Body.String())
			}
			// hash 交给下载器前统一大写。
			wantHash := strings.ToUpper(test.hash)
			if len(source.controls) != 1 || source.controls[0].Action != test.action ||
				source.controls[0].Hash != wantHash || source.controls[0].Downloader != "qbittorrent" {
				t.Fatalf("控制调用 = %#v", source.controls)
			}
		})
	}
}

func TestDownloadControlRejectsUnusableInput(t *testing.T) {
	blankLabel := usableDownloadControlSource()
	blankLabel.task.Task.Label = "  "

	controlFailure := usableDownloadControlSource()
	controlFailure.controlErr = errors.New("下载器拒绝")

	tests := []struct {
		name         string
		tasks        DownloadTaskSource
		hash         string
		wantControls int
	}{
		{
			name:  "非法 hash 直接失败",
			tasks: usableDownloadControlSource(),
			hash:  "not-a-hash",
		},
		{
			name:  "任务源不支持控制",
			tasks: fakeListOnlyDownloadSource{},
			hash:  downloadControlHash40,
		},
		{
			name:  "查找失败",
			tasks: &fakeDownloadControlSource{findErr: errors.New("下载器不可达")},
			hash:  downloadControlHash40,
		},
		{
			name:  "任务不存在",
			tasks: &fakeDownloadControlSource{found: false},
			hash:  downloadControlHash40,
		},
		{
			name:  "任务标签为空不触发控制",
			tasks: blankLabel,
			hash:  downloadControlHash40,
		},
		{
			name:         "控制动作失败",
			tasks:        controlFailure,
			hash:         downloadControlHash40,
			wantControls: 1,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			user := auth.User{ID: 7, Username: "alice"}
			router := chi.NewRouter()
			Dependencies{Users: fakeUserLookup{user: user}, DownloadTasks: test.tasks}.registerProtected(router)

			request := downloadListRequest("/download/start/"+test.hash, user)
			response := httptest.NewRecorder()
			router.ServeHTTP(response, request)

			if response.Code != http.StatusOK {
				t.Fatalf("status = %d, body=%s", response.Code, response.Body.String())
			}
			if downloadControlSuccess(t, response) {
				t.Fatalf("预期失败但 success=true, body=%s", response.Body.String())
			}
			source, _ := test.tasks.(*fakeDownloadControlSource)
			if source != nil && len(source.controls) != test.wantControls {
				t.Fatalf("控制调用 = %#v, want %d 次", source.controls, test.wantControls)
			}
		})
	}
}

func TestDownloadControlRequiresClaims(t *testing.T) {
	router := chi.NewRouter()
	Dependencies{DownloadTasks: usableDownloadControlSource()}.registerProtected(router)

	request := httptest.NewRequest(http.MethodGet, "/download/start/"+downloadControlHash40, nil)
	response := httptest.NewRecorder()
	router.ServeHTTP(response, request)

	if response.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403; body=%s", response.Code, response.Body.String())
	}
	if !strings.Contains(response.Body.String(), "用户身份无效") {
		t.Fatalf("body = %s", response.Body.String())
	}
}

func downloadControlSuccess(t *testing.T, response *httptest.ResponseRecorder) bool {
	t.Helper()
	var body struct {
		Success bool `json:"success"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &body); err != nil {
		t.Fatalf("解析响应: %v; body=%s", err, response.Body.String())
	}
	return body.Success
}
