package httpapi

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"

	"github.com/moviepilot-go/moviepilot-go/internal/command"
	"github.com/moviepilot-go/moviepilot-go/internal/config"
	"github.com/moviepilot-go/moviepilot-go/internal/database"
	"github.com/moviepilot-go/moviepilot-go/internal/subscription"
)

func newCommandTestDependencies() Dependencies {
	return Dependencies{Config: config.Config{MaxRequestBody: 4096}}
}

// serveCommandName 通过 chi 路由注入 `{name}` 路径参数后执行命令。
func serveCommandName(handler http.HandlerFunc, name, body string) *httptest.ResponseRecorder {
	router := chi.NewRouter()
	router.Post("/api/v1/command/{name}", handler)
	recorder := httptest.NewRecorder()
	router.ServeHTTP(recorder, httptest.NewRequest(
		http.MethodPost, "/api/v1/command/"+name, strings.NewReader(body),
	))
	return recorder
}

// TestNewCommandRegistry 覆盖预设命令装配与只读命令的行为。
func TestNewCommandRegistry(t *testing.T) {
	registry := newCommandRegistry(newCommandTestDependencies())
	if registry == nil {
		t.Fatal("newCommandRegistry() = nil")
	}
	descriptors := registry.List()
	if len(descriptors) != 5 {
		t.Fatalf("命令数 = %d, want 5", len(descriptors))
	}

	help, err := registry.ExecuteName(context.Background(), "help", command.Request{})
	if err != nil {
		t.Fatalf("help error = %v", err)
	}
	if !strings.Contains(help.Text, "共 5 条命令") || !strings.Contains(help.Text, "/subscribes") {
		t.Fatalf("help 文本 = %q", help.Text)
	}

	// doctor 未接入时返回错误。
	if _, err := registry.ExecuteName(context.Background(), "doctor", command.Request{}); err == nil {
		t.Fatal("未接入 doctor 时应返回错误")
	}
	// sites 未接入站点存储时返回错误。
	if _, err := registry.ExecuteName(context.Background(), "sites", command.Request{}); err == nil {
		t.Fatal("未接入站点存储时 sites 应返回错误")
	}
	// subscribes 未接入订阅存储时返回错误。
	if _, err := registry.ExecuteName(context.Background(), "subscribes", command.Request{}); err == nil {
		t.Fatal("未接入订阅存储时 subscribes 应返回错误")
	}
	// downloads 未接入任务源时返回错误。
	if _, err := registry.ExecuteName(context.Background(), "downloads", command.Request{}); err == nil {
		t.Fatal("未接入任务源时 downloads 应返回错误")
	}
}

// TestExecuteCommandName 覆盖按命令名执行：注册表缺失、请求体损坏、空请求体、
// 未知命令、执行失败与管理员专属命令。
func TestExecuteCommandName(t *testing.T) {
	dependencies := newCommandTestDependencies()
	registry := newCommandRegistry(dependencies)
	handler := dependencies.executeCommandName(registry)

	if recorder := serveCommandName(dependencies.executeCommandName(nil), "help", ""); recorder.Code != http.StatusServiceUnavailable {
		t.Fatalf("nil registry 状态码 = %d, want 503", recorder.Code)
	}
	if recorder := serveCommandName(handler, "help", "{bad json"); recorder.Code != http.StatusBadRequest {
		t.Fatalf("损坏请求体状态码 = %d, want 400", recorder.Code)
	}
	if recorder := serveCommandName(handler, "help", ""); recorder.Code != http.StatusOK {
		t.Fatalf("空请求体执行 help 状态码 = %d, want 200", recorder.Code)
	}
	if recorder := serveCommandName(handler, "nope", `{"args":[]}`); recorder.Code != http.StatusNotFound {
		t.Fatalf("未知命令状态码 = %d, want 404", recorder.Code)
	}
	// doctor 未接入 → dispatch 默认分支 500。
	if recorder := serveCommandName(handler, "doctor", `{"args":["x"]}`); recorder.Code != http.StatusInternalServerError {
		t.Fatalf("执行失败状态码 = %d, want 500", recorder.Code)
	}

	adminOnly, err := command.NewRegistry(command.Command{
		Name: "admin", Title: "管理员命令", AdminOnly: true,
		Handler: func(context.Context, command.Request) (command.Result, error) {
			return command.Result{Text: "ok"}, nil
		},
	})
	if err != nil {
		t.Fatalf("构建管理员命令注册表: %v", err)
	}
	if recorder := serveCommandName(dependencies.executeCommandName(adminOnly), "admin", ""); recorder.Code != http.StatusForbidden {
		t.Fatalf("非管理员执行状态码 = %d, want 403", recorder.Code)
	}
}

// TestExecuteCommandText 覆盖聊天文本形式的命令执行。
func TestExecuteCommandText(t *testing.T) {
	dependencies := newCommandTestDependencies()
	handler := dependencies.executeCommand(newCommandRegistry(dependencies))

	post := func(body string) *httptest.ResponseRecorder {
		recorder := httptest.NewRecorder()
		handler.ServeHTTP(recorder, httptest.NewRequest(http.MethodPost, "/api/v1/command", strings.NewReader(body)))
		return recorder
	}

	recorder := httptest.NewRecorder()
	dependencies.executeCommand(nil).ServeHTTP(recorder, httptest.NewRequest(http.MethodPost, "/api/v1/command", nil))
	if recorder.Code != http.StatusServiceUnavailable {
		t.Fatalf("nil registry 状态码 = %d, want 503", recorder.Code)
	}
	if got := post("{bad").Code; got != http.StatusBadRequest {
		t.Fatalf("损坏请求体状态码 = %d, want 400", got)
	}
	if got := post(`{"command":"/help"}`).Code; got != http.StatusOK {
		t.Fatalf("执行 help 状态码 = %d, want 200", got)
	}
	if got := post(`{"command":"/help","args":["extra"]}`).Code; got != http.StatusOK {
		t.Fatalf("带参数执行状态码 = %d, want 200", got)
	}
	if got := post(`{"command":""}`).Code; got != http.StatusBadRequest {
		t.Fatalf("空命令状态码 = %d, want 400", got)
	}
	if got := post(`{"command":"/nope"}`).Code; got != http.StatusNotFound {
		t.Fatalf("未知命令状态码 = %d, want 404", got)
	}
}

// TestListCommands 覆盖命令列表端点。
func TestListCommands(t *testing.T) {
	dependencies := newCommandTestDependencies()
	recorder := httptest.NewRecorder()
	dependencies.listCommands(nil).ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/api/v1/command", nil))
	if recorder.Code != http.StatusServiceUnavailable {
		t.Fatalf("nil registry 状态码 = %d, want 503", recorder.Code)
	}

	listed := httptest.NewRecorder()
	dependencies.listCommands(newCommandRegistry(dependencies)).ServeHTTP(
		listed, httptest.NewRequest(http.MethodGet, "/api/v1/command", nil))
	if listed.Code != http.StatusOK || !strings.Contains(listed.Body.String(), "help") {
		t.Fatalf("命令列表 = (%d, %q)", listed.Code, listed.Body.String())
	}
}

// TestFormatSubscriptionSummary 覆盖订阅摘要的计数、排序与截断提示。
func TestFormatSubscriptionSummary(t *testing.T) {
	text, details := formatSubscriptionSummary(nil)
	if text != "共 0 个订阅" || details["total"] != 0 {
		t.Fatalf("空订阅摘要 = (%q, %#v)", text, details)
	}

	items := make([]database.Subscription, 0, 12)
	for index := range 12 {
		state := subscription.Active
		if index%2 == 1 {
			state = subscription.Paused
		}
		items = append(items, database.Subscription{
			Name: "订阅" + string(rune('A'+index)), MediaType: "电视剧", InternalState: state,
		})
	}
	text, details = formatSubscriptionSummary(items)
	if !strings.Contains(text, "共 12 个订阅") || !strings.Contains(text, "…还有 2 个未显示") {
		t.Fatalf("截断摘要 = %q", text)
	}
	if !strings.Contains(text, "（电视剧）") {
		t.Fatalf("摘要缺少媒体类型: %q", text)
	}
	if details["total"] != 12 {
		t.Fatalf("摘要 total = %v", details["total"])
	}

	// 纯函数外的存储守卫。
	if _, err := (Dependencies{}).runSubscribeCommand(context.Background(), command.Request{}); err == nil {
		t.Fatal("缺少订阅存储时应报错")
	}
	if _, err := (Dependencies{}).runSitesCommand(context.Background()); err == nil {
		t.Fatal("缺少站点存储时应报错")
	}
	if _, err := (Dependencies{}).runDownloadsCommand(context.Background(), command.Request{}); err == nil {
		t.Fatal("缺少下载任务源时应报错")
	}
	if _, err := runDoctorCommand(context.Background(), nil); err == nil {
		t.Fatal("缺少 doctor 时应报错")
	}
	if request := requestFromClaims(httptest.NewRequest(http.MethodGet, "/", nil)); request.Username != "" || request.Admin {
		t.Fatalf("无 claims 请求 = %#v", request)
	}
}
