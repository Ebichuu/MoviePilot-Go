package httpapi

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"
)

// doubanSubjectFake 只实现详情端点，用不同 payload 区分电影/电视剧分支。
func doubanSubjectFake(t *testing.T) (*httptest.Server, *[]string) {
	t.Helper()
	var paths []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		paths = append(paths, r.URL.Path)
		switch r.URL.Path {
		case "/movie/123":
			_, _ = w.Write([]byte(`{
				"id":"123","title":"某电影","year":"2021","card_subtitle":"2021 / 中国大陆",
				"pic":{"large":"https://img/123.jpg"},"rating":{"value":8.1}
			}`))
		case "/tv/123":
			_, _ = w.Write([]byte(`{
				"id":"123","title":"某剧","year":"2022","card_subtitle":"2022 / 中国大陆",
				"episodes_count":12,"pic":{"large":"https://img/tv.jpg"}
			}`))
		case "/tv/789":
			_, _ = w.Write([]byte(`{"id":"789","title":"前缀剧","year":"2023","card_subtitle":"2023 / 日本"}`))
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(server.Close)
	return server, &paths
}

func TestDoubanSubjectResolvesTypeAndPayload(t *testing.T) {
	server, paths := doubanSubjectFake(t)
	router := chi.NewRouter()
	router.Get("/douban/{doubanid}", doubanDeps(t, server.URL).doubanSubject)

	tests := []struct {
		name     string
		target   string
		wantPath string
		wantType string
		wantName string
		wantYear string
	}{
		{
			name: "纯数字默认电影", target: "/douban/123",
			wantPath: "/movie/123", wantType: "电影", wantName: "某电影", wantYear: "2021",
		},
		{
			name: "type_name 指定电视剧", target: "/douban/123?type_name=%E7%94%B5%E8%A7%86%E5%89%A7",
			wantPath: "/tv/123", wantType: "电视剧", wantName: "某剧", wantYear: "2022",
		},
		{
			name: "tv 前缀优先", target: "/douban/tv:789",
			wantPath: "/tv/789", wantType: "电视剧", wantName: "前缀剧", wantYear: "2023",
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			*paths = nil
			recorder := httptest.NewRecorder()
			router.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, test.target, nil))
			if recorder.Code != http.StatusOK {
				t.Fatalf("status = %d, body=%s", recorder.Code, recorder.Body.String())
			}
			if len(*paths) != 1 || (*paths)[0] != test.wantPath {
				t.Fatalf("请求路径 = %v, want %s", *paths, test.wantPath)
			}
			info := decodeMediaInfo(t, recorder.Body.Bytes())
			if info["type"] != test.wantType || info["title"] != test.wantName || info["year"] != test.wantYear {
				t.Fatalf("详情 = %#v", info)
			}
		})
	}
}

func TestDoubanSubjectDegradesWithoutMetadata(t *testing.T) {
	router := chi.NewRouter()
	router.Get("/douban/{doubanid}", Dependencies{}.doubanSubject)

	recorder := httptest.NewRecorder()
	router.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/douban/123", nil))
	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d", recorder.Code)
	}
	var body struct {
		Success bool   `json:"success"`
		Message string `json:"message"`
	}
	if err := json.Unmarshal(recorder.Body.Bytes(), &body); err != nil {
		t.Fatalf("解析响应: %v; body=%s", err, recorder.Body.String())
	}
	if body.Success || body.Message != "未配置元数据源" {
		t.Fatalf("body = %s", recorder.Body.String())
	}
}

func TestDoubanSubjectFallsBackToEmptyInfo(t *testing.T) {
	server, _ := doubanSubjectFake(t)
	router := chi.NewRouter()
	router.Get("/douban/{doubanid}", doubanDeps(t, server.URL).doubanSubject)

	// 空豆瓣 ID 不触发上游请求，直接返回空状态。
	recorder := httptest.NewRecorder()
	router.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/douban/%20", nil))
	if recorder.Code != http.StatusOK {
		t.Fatalf("空 ID status = %d", recorder.Code)
	}
	empty := decodeMediaInfo(t, recorder.Body.Bytes())
	if len(empty) != len(emptyMediaInfo()) {
		t.Fatalf("空 ID 详情字段数 = %d", len(empty))
	}

	// 上游 404 时同样降级为空状态，不报 5xx、不透出上游错误。
	recorder = httptest.NewRecorder()
	router.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/douban/999", nil))
	if recorder.Code != http.StatusOK {
		t.Fatalf("失败详情 status = %d", recorder.Code)
	}
	if strings.Contains(recorder.Body.String(), "404") || strings.Contains(recorder.Body.String(), "not found") {
		t.Fatalf("泄漏上游错误: %s", recorder.Body.String())
	}
	failed := decodeMediaInfo(t, recorder.Body.Bytes())
	if len(failed) != len(emptyMediaInfo()) {
		t.Fatalf("失败详情字段数 = %d", len(failed))
	}
}

func decodeMediaInfo(t *testing.T, body []byte) map[string]any {
	t.Helper()
	var info map[string]any
	if err := json.Unmarshal(body, &info); err != nil {
		t.Fatalf("解析媒体详情: %v; body=%s", err, body)
	}
	return info
}
