package httpapi

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// TestValidUserPassword 覆盖密码强度规则的类别与长度边界。
func TestValidUserPassword(t *testing.T) {
	cases := map[string]bool{
		"":                       false,
		"1234567":                false, // 太短
		"12345678":               false, // 只有数字
		"abcdefgh":               false, // 只有字母
		"!!!!!!!!":               false, // 只有符号
		"abcd1234":               true,  // 字母 + 数字
		"abcd!!!!":               true,  // 字母 + 符号
		"1234!!!!":               true,  // 数字 + 符号
		"ab 1234!":               true,  // 空格不计入类别
		strings.Repeat("a", 51):  false,
		strings.Repeat("a1", 25): true,
	}
	for password, want := range cases {
		if got := validUserPassword(password); got != want {
			t.Fatalf("validUserPassword(%q) = %v，期望 %v", password, got, want)
		}
	}
}

// TestDecodeJSONBodyBranches 覆盖请求体缺失与正常解码。
func TestDecodeJSONBodyBranches(t *testing.T) {
	var target map[string]any
	if err := decodeJSONBody(nil, &target); err == nil {
		t.Fatal("nil 请求应报错")
	}
	if err := decodeJSONBody(httptest.NewRequest(http.MethodPost, "/x", nil), &target); err == nil {
		t.Fatal("空请求体应报错")
	}
	request := httptest.NewRequest(http.MethodPost, "/x", strings.NewReader(`{"a":1}`))
	if err := decodeJSONBody(request, &target); err != nil {
		t.Fatalf("正常解码: %v", err)
	}
}

func userRequest(method, body string, params map[string]string) *http.Request {
	request := chiRequest(method, body, params, "")
	return request
}

// TestUserHandlerGuards 覆盖用户端点里所有「还没走到存储」的校验分支。
func TestUserHandlerGuards(t *testing.T) {
	deps := Dependencies{}

	cases := []struct {
		name   string
		call   func(http.ResponseWriter, *http.Request)
		body   string
		params map[string]string
		code   int
		substr string
	}{
		{name: "创建用户：JSON 非法", call: deps.createUser, body: "nope", code: http.StatusOK, substr: "参数错误"},
		{name: "创建用户：用户名为空", call: deps.createUser, body: `{"name":"  "}`, code: http.StatusOK,
			substr: "用户名不能为空"},
		{name: "更新用户：JSON 非法", call: deps.updateUser, body: "nope", code: http.StatusOK, substr: "参数错误"},
		{name: "更新用户：ID 缺失", call: deps.updateUser, body: `{"id":0}`, code: http.StatusOK,
			substr: "用户ID不能为空"},
		{name: "按 ID 删除：ID 非法", call: deps.deleteUserByID, params: map[string]string{"user_id": "abc"},
			code: http.StatusOK, substr: "用户ID无效"},
		{name: "按 ID 删除：ID 为负", call: deps.deleteUserByID, params: map[string]string{"user_id": "-3"},
			code: http.StatusOK, substr: "用户ID无效"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			recorder := httptest.NewRecorder()
			tc.call(recorder, userRequest(http.MethodPost, tc.body, tc.params))
			if recorder.Code != tc.code {
				t.Fatalf("状态码 = %d，期望 %d（%s）", recorder.Code, tc.code, recorder.Body.String())
			}
			if !strings.Contains(recorder.Body.String(), tc.substr) {
				t.Fatalf("响应 = %s，期望包含 %q", recorder.Body.String(), tc.substr)
			}
		})
	}

	// 上传头像：没有登录态 → 403。
	recorder := httptest.NewRecorder()
	deps.uploadUserAvatar(recorder, userRequest(http.MethodPost, "", map[string]string{"user_id": "1"}))
	if recorder.Code != http.StatusForbidden {
		t.Fatalf("无登录态 = %d", recorder.Code)
	}
}
