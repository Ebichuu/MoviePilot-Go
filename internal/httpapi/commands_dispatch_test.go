package httpapi

import (
	"errors"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/moviepilot-go/moviepilot-go/internal/command"
	"github.com/moviepilot-go/moviepilot-go/internal/config"
)

// 命令执行的错误必须映射成稳定状态码：V2 的命令面板按状态码区分「没权限」和「不存在」。
func TestDispatchCommandStatusMapping(t *testing.T) {
	cases := []struct {
		name   string
		result command.Result
		err    error
		status int
	}{
		{"成功", command.Result{Command: "help", Text: "ok"}, nil, 200},
		{"空命令", command.Result{}, command.ErrEmpty, 400},
		{"未知命令", command.Result{}, command.ErrUnknown, 404},
		{"无权限", command.Result{}, command.ErrForbidden, 403},
		{"执行失败", command.Result{}, errors.New("boom"), 500},
	}
	deps := Dependencies{Config: config.Config{Environment: "test", MaxRequestBody: 1 << 20}}
	for _, tc := range cases {
		recorder := httptest.NewRecorder()
		deps.dispatchCommand(recorder, tc.result, tc.err)
		if recorder.Code != tc.status {
			t.Fatalf("%s: 状态码 = %d，期望 %d", tc.name, recorder.Code, tc.status)
		}
	}
}

func TestDispatchCommandErrorDoesNotLeakSuccessEnvelope(t *testing.T) {
	deps := Dependencies{Config: config.Config{Environment: "test", MaxRequestBody: 1 << 20}}
	recorder := httptest.NewRecorder()
	deps.dispatchCommand(recorder, command.Result{}, command.ErrForbidden)
	body := recorder.Body.String()
	if strings.Contains(body, `"success":true`) {
		t.Fatalf("失败时不应返回成功信封: %s", body)
	}
	if !strings.Contains(body, "没有权限执行该命令") {
		t.Fatalf("缺少可读错误信息: %s", body)
	}
}

func TestRequestFromClaimsCarriesIdentityAndAdmin(t *testing.T) {
	request := httptest.NewRequest("POST", "/api/v1/command", nil)
	anonymous := requestFromClaims(request)
	if anonymous.Username != "" || anonymous.Admin {
		t.Fatalf("没有 claims 时应当是匿名: %+v", anonymous)
	}

	authed := request.WithContext(authedContext())
	identity := requestFromClaims(authed)
	if identity.Username != "admin" || !identity.Admin {
		t.Fatalf("未把身份/管理员标记带给命令: %+v", identity)
	}
}
