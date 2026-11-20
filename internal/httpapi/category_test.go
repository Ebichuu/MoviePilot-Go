package httpapi

import (
	"bytes"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/moviepilot-go/moviepilot-go/internal/auth"
	"github.com/moviepilot-go/moviepilot-go/internal/config"
	"github.com/moviepilot-go/moviepilot-go/internal/metadata"
)

func TestCategoryRoutesAuthorizationAndContract(t *testing.T) {
	configService := &fakeCategoryConfigService{config: metadata.CategoryConfig{
		Movie: []metadata.CategoryRule{{Name: "首项"}, {Name: "次项"}},
	}}
	tests := []struct {
		name       string
		method     string
		path       string
		body       string
		authorized bool
		admin      bool
		wantStatus int
		wantText   []string
	}{
		{name: "未认证读取", method: http.MethodGet, path: "/api/v1/media/category/config", wantStatus: http.StatusUnauthorized},
		{name: "普通用户读取配置", method: http.MethodGet, path: "/api/v1/media/category/config", authorized: true, wantStatus: http.StatusOK, wantText: []string{`"success":true`, `"首项"`, `"次项"`}},
		{name: "普通用户读取列表", method: http.MethodGet, path: "/api/v1/media/category", authorized: true, wantStatus: http.StatusOK, wantText: []string{`"电影":["首项","次项"]`}},
		{name: "普通用户不能保存", method: http.MethodPost, path: "/api/v1/media/category/config", body: `{}`, authorized: true, wantStatus: http.StatusForbidden},
		{name: "管理员保存", method: http.MethodPost, path: "/api/v1/media/category/config", body: `{"movie":{"自定义":null},"tv":null}`, authorized: true, admin: true, wantStatus: http.StatusOK, wantText: []string{`"success":true`, `"message":"保存成功"`}},
		{name: "管理员提交非法结构", method: http.MethodPost, path: "/api/v1/media/category/config", body: `{"movie":[]}`, authorized: true, admin: true, wantStatus: http.StatusUnprocessableEntity},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			user := auth.User{ID: 1, Username: "alice", SuperUser: test.admin}
			handler, token := newCategoryTestRouter(t, configService, user)
			if !test.authorized {
				token = ""
			}
			request := httptest.NewRequest(test.method, test.path, strings.NewReader(test.body))
			if token != "" {
				request.Header.Set("Authorization", "Bearer "+token)
			}
			response := httptest.NewRecorder()
			handler.ServeHTTP(response, request)
			if response.Code != test.wantStatus {
				t.Fatalf("status = %d, want %d; body=%s", response.Code, test.wantStatus, response.Body.String())
			}
			for _, want := range test.wantText {
				if !strings.Contains(response.Body.String(), want) {
					t.Fatalf("body = %s, want %q", response.Body.String(), want)
				}
			}
			if test.path == "/api/v1/media/category" && test.wantStatus == http.StatusOK &&
				strings.Contains(response.Body.String(), `"success"`) {
				t.Fatalf("V2 /media/category 应返回裸对象: %s", response.Body.String())
			}
		})
	}
	if got := configService.config.Movie; len(got) != 1 || got[0].Name != "自定义" {
		t.Fatalf("saved config = %#v", configService.config)
	}
}

func TestSaveCategoryConfigReportsPersistenceFailureAsV2Response(t *testing.T) {
	service := &fakeCategoryConfigService{saveErr: errors.New("disk full")}
	handler, token := newCategoryTestRouter(t, service, auth.User{ID: 1, SuperUser: true})
	request := httptest.NewRequest(http.MethodPost, "/api/v2/media/category/config", strings.NewReader(`{"movie":null,"tv":null}`))
	request.Header.Set("Authorization", "Bearer "+token)
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusOK || !strings.Contains(response.Body.String(), `"success":false`) ||
		!strings.Contains(response.Body.String(), `"message":"保存失败"`) {
		t.Fatalf("response = %d %s", response.Code, response.Body.String())
	}
}

func TestSaveCategoryConfigRejectsOneToFourMiBRequestWhoseYAMLExpandsPastLimit(t *testing.T) {
	directory := t.TempDir()
	service := metadata.NewCategoryService(directory)
	if err := service.SaveCategoryConfig(metadata.CategoryConfig{Movie: []metadata.CategoryRule{{Name: "原分类"}}}); err != nil {
		t.Fatal(err)
	}
	path := directory + string(os.PathSeparator) + "category.yaml"
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	handler, token := newCategoryTestRouterWithLimit(t, service, auth.User{ID: 1, SuperUser: true}, 4<<20)
	body := `{"movie":{"超限":{"original_language":"` + strings.Repeat("'", (1<<20)+1024) + `"}},"tv":{}}`
	if len(body) <= 1<<20 || len(body) >= 4<<20 {
		t.Fatalf("测试请求大小 = %d，不在 (1MiB, 4MiB) 内", len(body))
	}
	request := httptest.NewRequest(http.MethodPost, "/api/v1/media/category/config", strings.NewReader(body))
	request.Header.Set("Authorization", "Bearer "+token)
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusOK || !strings.Contains(response.Body.String(), `"success":false`) {
		t.Fatalf("response = %d %s", response.Code, response.Body.String())
	}
	after, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(after, before) {
		t.Fatal("YAML 膨胀超限请求改变了磁盘配置")
	}
	if got := service.Category("movie", metadata.Media{}); got != "原分类" {
		t.Fatalf("YAML 膨胀超限请求改变了运行快照: %q", got)
	}
}

type fakeCategoryConfigService struct {
	config  metadata.CategoryConfig
	saveErr error
}

func (s *fakeCategoryConfigService) LoadCategoryConfig() metadata.CategoryConfig { return s.config }
func (s *fakeCategoryConfigService) SaveCategoryConfig(config metadata.CategoryConfig) error {
	if s.saveErr != nil {
		return s.saveErr
	}
	s.config = config
	return nil
}
func (s *fakeCategoryConfigService) MediaCategories() map[string][]string {
	result := map[string][]string{"电影": {}, "电视剧": {}}
	for _, rule := range s.config.Movie {
		result["电影"] = append(result["电影"], rule.Name)
	}
	for _, rule := range s.config.TV {
		result["电视剧"] = append(result["电视剧"], rule.Name)
	}
	return result
}

func newCategoryTestRouter(t *testing.T, categories CategoryConfigService, user auth.User) (http.Handler, string) {
	return newCategoryTestRouterWithLimit(t, categories, user, 1<<20)
}

func newCategoryTestRouterWithLimit(t *testing.T, categories CategoryConfigService, user auth.User, maxRequestBody int64) (http.Handler, string) {
	t.Helper()
	authService := auth.NewService(nil, "category-http-test-jwt-secret", "category-http-test-resource-secret", time.Hour)
	token, err := authService.AccessToken(user)
	if err != nil {
		t.Fatal(err)
	}
	return NewRouter(Dependencies{
		Config:     config.Config{MaxRequestBody: maxRequestBody, FrontendDir: t.TempDir()},
		Categories: categories,
		Users:      fakeUserLookup{user: user},
		Auth:       authService,
		Logger:     slog.New(slog.NewTextHandler(io.Discard, nil)),
	}), token
}
