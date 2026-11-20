//go:build integration

package httpapi

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"

	"github.com/moviepilot-go/moviepilot-go/internal/auth"
)

type fakeWebPushDispatcher struct {
	errs   []error
	titles []string
	bodies []string
	links  []string
}

func (f *fakeWebPushDispatcher) SendWebPush(_ context.Context, title, body, link string) error {
	f.titles = append(f.titles, title)
	f.bodies = append(f.bodies, body)
	f.links = append(f.links, link)
	if len(f.errs) > 0 {
		err := f.errs[0]
		f.errs = f.errs[1:]
		return err
	}
	return nil
}

func claimsForUser(user auth.User) auth.Claims {
	return auth.Claims{
		Username: user.Username, SuperUser: user.SuperUser,
		RegisteredClaims: jwt.RegisteredClaims{Subject: strconv.FormatInt(user.ID, 10)},
	}
}

func TestWebPushStoreIntegration(t *testing.T) {
	deps := newHTTPAPIIntegrationDeps(t)
	alice := seedHTTPAPIUser(t, deps, "alice", false)
	claims := claimsForUser(alice)

	// 订阅守卫：无认证 → 401，坏 JSON → 400，缺字段 → 422。
	recorder := httptest.NewRecorder()
	deps.subscribeWebPush(recorder, httptest.NewRequest(http.MethodPost, "/message/webpush/subscribe", strings.NewReader(`{}`)))
	if recorder.Code != http.StatusUnauthorized {
		t.Fatalf("无认证 status = %d", recorder.Code)
	}
	recorder = httptest.NewRecorder()
	deps.subscribeWebPush(recorder, httpapiJSONRequest(http.MethodPost,
		"/message/webpush/subscribe", nil, claims, `{"endpoint":`))
	if recorder.Code != http.StatusBadRequest {
		t.Fatalf("坏 JSON status = %d", recorder.Code)
	}
	for _, payload := range []string{
		`{"endpoint":"https://push.test/1"}`,
		`{"endpoint":"   ","keys":{"p256dh":"k","auth":"a"}}`,
		`{"endpoint":"https://push.test/1","keys":{"p256dh":"  ","auth":"a"}}`,
	} {
		recorder = httptest.NewRecorder()
		deps.subscribeWebPush(recorder, httpapiJSONRequest(http.MethodPost,
			"/message/webpush/subscribe", nil, claims, payload))
		if recorder.Code != http.StatusUnprocessableEntity {
			t.Fatalf("%s status = %d", payload, recorder.Code)
		}
	}

	// 正常订阅落库。
	recorder = httptest.NewRecorder()
	deps.subscribeWebPush(recorder, httpapiJSONRequest(http.MethodPost, "/message/webpush/subscribe", nil, claims,
		`{"endpoint":" https://push.test/1 ","keys":{"p256dh":"p256dh-key","auth":"auth-key"},"user_agent":"UA"}`))
	if body := decodeResponseBody(t, recorder.Body.Bytes()); !body.Success {
		t.Fatalf("订阅 = %+v", body)
	}

	// 发送：无认证 401、坏 JSON 400、无分发器直接成功、分发失败仍成功。
	recorder = httptest.NewRecorder()
	deps.sendWebPush(recorder, httptest.NewRequest(http.MethodPost, "/message/webpush/send", strings.NewReader(`{}`)))
	if recorder.Code != http.StatusUnauthorized {
		t.Fatalf("发送无认证 status = %d", recorder.Code)
	}
	recorder = httptest.NewRecorder()
	Dependencies{Users: deps.Store}.sendWebPush(recorder, httpapiJSONRequest(http.MethodPost,
		"/message/webpush/send", nil, claims, `{}`))
	if body := decodeResponseBody(t, recorder.Body.Bytes()); !body.Success {
		t.Fatalf("无分发器 = %+v", body)
	}
	recorder = httptest.NewRecorder()
	Dependencies{Store: deps.Store, WebPush: &fakeWebPushDispatcher{}}.sendWebPush(recorder,
		httpapiJSONRequest(http.MethodPost, "/message/webpush/send", nil, claims, `{"title":`))
	if recorder.Code != http.StatusBadRequest {
		t.Fatalf("发送坏 JSON status = %d, body=%s", recorder.Code, recorder.Body.String())
	}
	dispatcher := &fakeWebPushDispatcher{errs: []error{errors.New("boom")}}
	recorder = httptest.NewRecorder()
	Dependencies{Store: deps.Store, Users: deps.Store, WebPush: dispatcher}.sendWebPush(recorder,
		httpapiJSONRequest(http.MethodPost, "/message/webpush/send", nil, claims,
			`{"title":" 通知 ","body":"内容","url":" https://moviepilot.test/x "}`))
	if body := decodeResponseBody(t, recorder.Body.Bytes()); !body.Success {
		t.Fatalf("分发失败应仍成功 = %+v", body)
	}
	if len(dispatcher.titles) != 1 || dispatcher.titles[0] != "通知" || dispatcher.links[0] != "https://moviepilot.test/x" {
		t.Fatalf("分发参数 = (%v, %v)", dispatcher.titles, dispatcher.links)
	}

	// 成功分发。
	dispatcher = &fakeWebPushDispatcher{}
	recorder = httptest.NewRecorder()
	Dependencies{Store: deps.Store, WebPush: dispatcher}.sendWebPush(recorder,
		httpapiJSONRequest(http.MethodPost, "/message/webpush/send", nil, claims, `{"title":"t","body":"b"}`))
	if body := decodeResponseBody(t, recorder.Body.Bytes()); !body.Success || dispatcher.bodies[0] != "b" {
		t.Fatalf("成功分发 = (%+v, %v)", body, dispatcher.bodies)
	}
}

func TestMFAOTPStoreIntegration(t *testing.T) {
	deps := newHTTPAPIIntegrationDeps(t)
	seedHTTPAPIUser(t, deps, "alice", false)
	seedHTTPAPIUser(t, deps, "bob", false)

	// 用户名为空：三个写接口与状态接口都要拒绝。
	for _, handler := range []http.HandlerFunc{deps.mfaOTPGenerate, deps.mfaOTPVerify, deps.mfaOTPDisable} {
		recorder := httptest.NewRecorder()
		handler(recorder, httpapiJSONRequest(http.MethodPost, "/mfa/otp", nil, auth.Claims{}, `{}`))
		if body := decodeResponseBody(t, recorder.Body.Bytes()); body.Success || body.Message != "用户名不能为空" {
			t.Fatalf("空用户名 = %+v", body)
		}
	}
	recorder := httptest.NewRecorder()
	deps.mfaStatus(recorder, httpapiClaimsRequest(http.MethodGet, "/mfa/status/", nil, auth.Claims{}))
	if body := decodeResponseBody(t, recorder.Body.Bytes()); body.Success || body.Message != "用户名不能为空" {
		t.Fatalf("状态空用户名 = %+v", body)
	}

	// 未生成密钥时校验/关闭都要提示先配置。
	recorder = httptest.NewRecorder()
	deps.mfaOTPVerify(recorder, httpapiJSONRequest(http.MethodPost, "/mfa/otp/verify", nil, auth.Claims{},
		`{"username":"alice","code":"123456"}`))
	if body := decodeResponseBody(t, recorder.Body.Bytes()); body.Success || body.Message != "请先生成 MFA 密钥" {
		t.Fatalf("未生成校验 = %+v", body)
	}
	recorder = httptest.NewRecorder()
	deps.mfaOTPDisable(recorder, httpapiJSONRequest(http.MethodPost, "/mfa/otp/disable", nil, auth.Claims{},
		`{"username":"bob","code":"123456"}`))
	if body := decodeResponseBody(t, recorder.Body.Bytes()); body.Success || body.Message != "未启用 MFA" {
		t.Fatalf("未启用关闭 = %+v", body)
	}

	// 生成：走 query 参数分支，返回 32 位 base32 种子与 otpauth URI。
	recorder = httptest.NewRecorder()
	deps.mfaOTPGenerate(recorder, httpapiJSONRequest(http.MethodPost, "/mfa/otp/generate?username=alice", nil, auth.Claims{}, ``))
	var generated struct {
		Success bool           `json:"success"`
		Data    map[string]any `json:"data"`
	}
	if err := json.Unmarshal(recorder.Body.Bytes(), &generated); err != nil {
		t.Fatalf("解析生成响应: %v", err)
	}
	secret, _ := generated.Data["secret"].(string)
	if !generated.Success || len(secret) != 32 || generated.Data["enabled"] != false {
		t.Fatalf("生成 MFA = %s", recorder.Body.String())
	}
	if uri, _ := generated.Data["otpauth_url"].(string); !strings.Contains(uri, secret) || !strings.HasPrefix(uri, "otpauth://totp/MoviePilot:alice") {
		t.Fatalf("otpauth url = %q", generated.Data["otpauth_url"])
	}

	// 状态：未校验前 otp.enabled=false。
	statusOtp := func() bool {
		recorder := httptest.NewRecorder()
		deps.mfaStatus(recorder, httpapiClaimsRequest(http.MethodGet, "/mfa/status/alice",
			map[string]string{"username": "alice"}, auth.Claims{}))
		var body struct {
			Success bool `json:"success"`
			Data    struct {
				OTP struct {
					Enabled bool `json:"enabled"`
				} `json:"otp"`
				Passkey struct {
					Enabled bool `json:"enabled"`
					Count   int  `json:"count"`
				} `json:"passkey"`
			} `json:"data"`
		}
		if err := json.Unmarshal(recorder.Body.Bytes(), &body); err != nil || !body.Success {
			t.Fatalf("状态解析 = (%s, %v)", recorder.Body.String(), err)
		}
		if body.Data.Passkey.Enabled || body.Data.Passkey.Count != 0 {
			t.Fatalf("passkey 形状 = %+v", body.Data.Passkey)
		}
		return body.Data.OTP.Enabled
	}
	if statusOtp() {
		t.Fatal("生成后不应启用")
	}

	// 校验：错误验证码失败，正确验证码启用。
	recorder = httptest.NewRecorder()
	deps.mfaOTPVerify(recorder, httpapiJSONRequest(http.MethodPost, "/mfa/otp/verify", nil, auth.Claims{},
		`{"username":"alice","code":"000000"}`))
	if body := decodeResponseBody(t, recorder.Body.Bytes()); body.Success || body.Message != "验证码错误" {
		t.Fatalf("错误验证码 = %+v", body)
	}
	code := mfaCodeAt(secret, time.Now().Unix()/30)
	recorder = httptest.NewRecorder()
	deps.mfaOTPVerify(recorder, httpapiJSONRequest(http.MethodPost, "/mfa/otp/verify", nil, auth.Claims{},
		`{"username":"alice","code":"`+code+`"}`))
	if body := decodeResponseBody(t, recorder.Body.Bytes()); !body.Success {
		t.Fatalf("正确验证码 = %+v", body)
	}
	if !statusOtp() {
		t.Fatal("校验后应启用")
	}

	// 关闭：错误验证码失败，正确验证码关闭。
	recorder = httptest.NewRecorder()
	deps.mfaOTPDisable(recorder, httpapiJSONRequest(http.MethodPost, "/mfa/otp/disable", nil, auth.Claims{},
		`{"username":"alice","code":"000000"}`))
	if body := decodeResponseBody(t, recorder.Body.Bytes()); body.Success || body.Message != "验证码错误" {
		t.Fatalf("关闭错误验证码 = %+v", body)
	}
	recorder = httptest.NewRecorder()
	deps.mfaOTPDisable(recorder, httpapiJSONRequest(http.MethodPost, "/mfa/otp/disable", nil, auth.Claims{},
		`{"username":"alice","code":"`+code+`"}`))
	if body := decodeResponseBody(t, recorder.Body.Bytes()); !body.Success {
		t.Fatalf("关闭 = %+v", body)
	}
	if statusOtp() {
		t.Fatal("关闭后不应启用")
	}

	// 关闭后记录里没有种子，再校验要提示重新生成。
	recorder = httptest.NewRecorder()
	deps.mfaOTPVerify(recorder, httpapiJSONRequest(http.MethodPost, "/mfa/otp/verify", nil, auth.Claims{},
		`{"username":"alice","code":"`+code+`"}`))
	if body := decodeResponseBody(t, recorder.Body.Bytes()); body.Success || body.Message != "请先生成 MFA 密钥" {
		t.Fatalf("关闭后校验 = %+v", body)
	}

	// 未知用户的统计不报错。
	recorder = httptest.NewRecorder()
	deps.mfaStatus(recorder, httpapiClaimsRequest(http.MethodGet, "/mfa/status/ghost",
		map[string]string{"username": "ghost"}, auth.Claims{}))
	if body := decodeResponseBody(t, recorder.Body.Bytes()); !body.Success {
		t.Fatalf("未知用户状态 = %+v", body)
	}

	// 纯函数：非法种子与长度不符的验证码都判失败。
	if mfaCodeAt("not-base32!!", 0) != "" {
		t.Fatal("非法种子应返回空串")
	}
	if mfaValidateCode(secret, "12345", time.Now()) || mfaValidateCode(secret, "1234567", time.Now()) {
		t.Fatal("长度不符的验证码必须失败")
	}
	if mfaValidateCode(secret, mfaCodeAt(secret, time.Now().Unix()/30-2), time.Now()) {
		t.Fatal("超出 ±1 窗口的验证码必须失败")
	}
	if _, err := mfaNewSecret(); err != nil {
		t.Fatalf("生成种子失败: %v", err)
	}
}
