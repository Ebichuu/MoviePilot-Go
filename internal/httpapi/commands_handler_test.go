package httpapi

import (
	"context"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/moviepilot-go/moviepilot-go/internal/auth"
	"github.com/moviepilot-go/moviepilot-go/internal/command"
	"github.com/moviepilot-go/moviepilot-go/internal/config"
)

func newCommandTestHandler(t *testing.T) func(ctx context.Context, body string) *httptest.ResponseRecorder {
	t.Helper()
	registry, err := command.NewRegistry(command.Command{
		Name:  "ping",
		Title: "测试命令",
		Handler: func(context.Context, command.Request) (command.Result, error) {
			return command.Result{Command: "ping", Text: "pong"}, nil
		},
	})
	if err != nil {
		t.Fatalf("构造命令注册表失败: %v", err)
	}
	deps := Dependencies{Config: config.Config{Environment: "test", MaxRequestBody: 1 << 20}}
	handler := deps.executeCommand(registry)
	return func(ctx context.Context, body string) *httptest.ResponseRecorder {
		request := httptest.NewRequest("POST", "/api/v1/command", strings.NewReader(body)).
			WithContext(ctx)
		recorder := httptest.NewRecorder()
		handler(recorder, request)
		return recorder
	}
}

// 鉴权在路由中间件层（`/api/v1/command` 挂在 requireUser 下），处理器本身不重复判权；
// 这里断言「没有 claims 时非管理命令仍可执行」，避免将来误以为处理器漏了鉴权。
func TestExecuteCommandLeavesAuthToRouteMiddleware(t *testing.T) {
	call := newCommandTestHandler(t)
	if code := call(context.Background(), `{"command":"/ping"}`).Code; code != 200 {
		t.Fatalf("状态码 = %d，期望 200", code)
	}
}

func authedContext() context.Context {
	return context.WithValue(context.Background(), claimsContextKey, auth.Claims{Username: "admin", SuperUser: true})
}

func TestExecuteCommandRejectsEmptyText(t *testing.T) {
	call := newCommandTestHandler(t)
	if code := call(authedContext(), `{"command":"   "}`).Code; code != 400 {
		t.Fatalf("空命令状态码 = %d，期望 400", code)
	}
}

func TestExecuteCommandRejectsUnknownName(t *testing.T) {
	call := newCommandTestHandler(t)
	if code := call(authedContext(), `{"command":"/不存在的命令"}`).Code; code != 404 {
		t.Fatalf("未知命令状态码 = %d，期望 404", code)
	}
}
