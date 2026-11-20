//go:build integration

package httpapi

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"

	"github.com/moviepilot-go/moviepilot-go/internal/auth"
	"github.com/moviepilot-go/moviepilot-go/internal/cache"
	"github.com/moviepilot-go/moviepilot-go/internal/config"
)

func newTestAuthService(store auth.UserStore) *auth.Service {
	return auth.NewService(store, "integration-jwt-secret", "integration-resource-secret", time.Hour)
}

func TestLoginAndCurrentUserIntegration(t *testing.T) {
	deps := newHTTPAPIIntegrationDeps(t)
	alice := seedHTTPAPIUser(t, deps, "alice", false)
	deps.Auth = newTestAuthService(deps.Store)

	loginRequest := func(body string) *http.Request {
		request := httptest.NewRequest(http.MethodPost, "/api/v1/login", strings.NewReader(body))
		request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		return request
	}

	// 空凭据与错误密码都是 401（`登录参数无效` 分支要求 ParseForm 失败，
	// 但 ParseMultipartForm 已先行解析，该分支在当前实现下不可达）。
	recorder := httptest.NewRecorder()
	deps.login(recorder, loginRequest(""))
	if recorder.Code != http.StatusUnauthorized {
		t.Fatalf("空凭据 status = %d, body=%s", recorder.Code, recorder.Body.String())
	}

	recorder = httptest.NewRecorder()
	deps.login(recorder, loginRequest("username=alice&password=wrong-password"))
	if recorder.Code != http.StatusUnauthorized {
		t.Fatalf("密码错误 status = %d, body=%s", recorder.Code, recorder.Body.String())
	}

	// 登录成功：返回令牌与用户信息，并写入资源令牌 Cookie。
	recorder = httptest.NewRecorder()
	deps.login(recorder, loginRequest("username=alice&password=abcd1234"))
	if recorder.Code != http.StatusOK {
		t.Fatalf("登录 status = %d, body=%s", recorder.Code, recorder.Body.String())
	}
	var payload map[string]any
	if err := json.Unmarshal(recorder.Body.Bytes(), &payload); err != nil {
		t.Fatalf("解析登录响应: %v", err)
	}
	if payload["token_type"] != "bearer" || payload["user_name"] != "alice" ||
		payload["user_id"] != float64(alice.ID) || payload["super_user"] != false {
		t.Fatalf("登录响应 = %#v", payload)
	}
	if token, _ := payload["access_token"].(string); token == "" {
		t.Fatalf("缺少 access_token: %#v", payload)
	}
	var cookie *http.Cookie
	for _, candidate := range recorder.Result().Cookies() {
		if candidate.Name == "MoviePilot" {
			cookie = candidate
		}
	}
	if cookie == nil || cookie.Value == "" || !cookie.HttpOnly || cookie.SameSite != http.SameSiteLaxMode {
		t.Fatalf("资源令牌 Cookie = %#v", cookie)
	}

	// 当前用户：正常、非法 subject、未知用户。
	recorder = httptest.NewRecorder()
	deps.currentUser(recorder, httpapiClaimsRequest(http.MethodGet, "/user/current",
		nil, claimsForUser(alice)))
	var user map[string]any
	if err := json.Unmarshal(recorder.Body.Bytes(), &user); err != nil || user["name"] != "alice" {
		t.Fatalf("当前用户 = (%s, %v)", recorder.Body.String(), err)
	}
	recorder = httptest.NewRecorder()
	deps.currentUser(recorder, httpapiClaimsRequest(http.MethodGet, "/user/current", nil,
		auth.Claims{Username: "alice", RegisteredClaims: jwt.RegisteredClaims{Subject: "abc"}}))
	if recorder.Code != http.StatusForbidden {
		t.Fatalf("非法 subject status = %d", recorder.Code)
	}
	recorder = httptest.NewRecorder()
	deps.currentUser(recorder, httpapiClaimsRequest(http.MethodGet, "/user/current", nil,
		auth.Claims{Username: "ghost", RegisteredClaims: jwt.RegisteredClaims{Subject: "99999"}}))
	if recorder.Code != http.StatusNotFound {
		t.Fatalf("未知用户 status = %d", recorder.Code)
	}
}

func TestSystemGlobalAndReadyIntegration(t *testing.T) {
	deps := newHTTPAPIIntegrationDeps(t)

	recorder := httptest.NewRecorder()
	deps.systemGlobal(recorder, httptest.NewRequest(http.MethodGet, "/system/global", nil))
	var global struct {
		Success bool `json:"success"`
		Data    struct {
			AppVersion      string `json:"APP_VERSION"`
			FrontendVersion string `json:"FRONTEND_VERSION"`
			ProjectName     string `json:"PROJECT_NAME"`
			Env             string `json:"ENV"`
		} `json:"data"`
	}
	if err := json.Unmarshal(recorder.Body.Bytes(), &global); err != nil {
		t.Fatalf("解析 global: %v; body=%s", err, recorder.Body.String())
	}
	if !global.Success || global.Data.AppVersion != backendVersion ||
		global.Data.FrontendVersion != frontendVersion ||
		global.Data.ProjectName != "MoviePilot" || global.Data.Env != "test" {
		t.Fatalf("system/global = %s", recorder.Body.String())
	}

	// DB 未初始化 → 503。
	recorder = httptest.NewRecorder()
	Dependencies{Config: deps.Config}.ready(recorder, httptest.NewRequest(http.MethodGet, "/ready", nil))
	if recorder.Code != http.StatusServiceUnavailable || !strings.Contains(recorder.Body.String(), "not_ready") {
		t.Fatalf("无 DB ready = (%d, %s)", recorder.Code, recorder.Body.String())
	}

	decodeReady := func(recorder *httptest.ResponseRecorder) (string, map[string]any) {
		t.Helper()
		var body struct {
			Status string         `json:"status"`
			Checks map[string]any `json:"checks"`
		}
		if err := json.Unmarshal(recorder.Body.Bytes(), &body); err != nil {
			t.Fatalf("解析 ready: %v; body=%s", err, recorder.Body.String())
		}
		return body.Status, body.Checks
	}

	// DB 可用、未配置缓存 → ready，redis=false。
	recorder = httptest.NewRecorder()
	deps.ready(recorder, httptest.NewRequest(http.MethodGet, "/ready", nil))
	status, checks := decodeReady(recorder)
	if recorder.Code != http.StatusOK || status != "ready" || checks["database"] != true || checks["redis"] != false {
		t.Fatalf("ready = (%d, %s)", recorder.Code, recorder.Body.String())
	}

	// 缓存对象存在但未初始化客户端 → redis=false，仍 ready。
	recorder = httptest.NewRecorder()
	withBrokenCache := deps
	withBrokenCache.Cache = &cache.Redis{}
	withBrokenCache.ready(recorder, httptest.NewRequest(http.MethodGet, "/ready", nil))
	status, checks = decodeReady(recorder)
	if recorder.Code != http.StatusOK || status != "ready" || checks["redis"] != false {
		t.Fatalf("坏缓存 ready = (%d, %s)", recorder.Code, recorder.Body.String())
	}

	// 真实 Redis（可选）：MOVIEPILOT_TEST_REDIS_URL 存在时校验 redis=true。
	if redisURL := strings.TrimSpace(os.Getenv("MOVIEPILOT_TEST_REDIS_URL")); redisURL != "" {
		client, err := cache.Open(t.Context(), config.Config{RedisURL: redisURL, RedisPoolSize: 4, MaxCacheValue: 1 << 20})
		if err != nil {
			t.Fatalf("连接测试 Redis: %v", err)
		}
		t.Cleanup(func() { _ = client.Close() })
		recorder = httptest.NewRecorder()
		withCache := deps
		withCache.Cache = client
		withCache.ready(recorder, httptest.NewRequest(http.MethodGet, "/ready", nil))
		status, checks = decodeReady(recorder)
		if recorder.Code != http.StatusOK || status != "ready" || checks["redis"] != true {
			t.Fatalf("真实 Redis ready = (%d, %s)", recorder.Code, recorder.Body.String())
		}
	}
}

func TestMonitorRuntimeIntegration(t *testing.T) {
	deps := newHTTPAPIIntegrationDeps(t)
	ctx := t.Context()

	// River 的迁移落在 public schema，业务连接只看得到隔离 schema，
	// 这里建一张同形替身表，只为验证状态聚合的读取映射。
	if _, err := deps.DB.Pool.Exec(ctx, `CREATE TABLE river_job (state text NOT NULL)`); err != nil {
		t.Fatalf("创建 river_job 替身表: %v", err)
	}
	if _, err := deps.DB.Pool.Exec(ctx, `INSERT INTO river_job (state) VALUES ('available'),('available'),('running')`); err != nil {
		t.Fatalf("写入 river_job 数据: %v", err)
	}

	states, err := deps.riverJobStates(ctx)
	if err != nil {
		t.Fatalf("riverJobStates() error = %v", err)
	}
	if states["available"] != 2 || states["running"] != 1 {
		t.Fatalf("riverJobStates() = %#v", states)
	}

	// monitor 透出 database 与 river_jobs；未配置缓存时没有 redis 字段。
	recorder := httptest.NewRecorder()
	deps.monitorRuntime(recorder, httptest.NewRequest(http.MethodGet, "/system/monitor", nil))
	var monitor struct {
		Success bool `json:"success"`
		Data    struct {
			Environment string         `json:"environment"`
			Goroutines  int            `json:"goroutines"`
			Database    map[string]any `json:"database"`
			RiverJobs   map[string]any `json:"river_jobs"`
			RiverError  string         `json:"river_jobs_error"`
			Redis       map[string]any `json:"redis"`
		} `json:"data"`
	}
	if err := json.Unmarshal(recorder.Body.Bytes(), &monitor); err != nil {
		t.Fatalf("解析 monitor: %v; body=%s", err, recorder.Body.String())
	}
	if !monitor.Success || monitor.Data.Environment != "test" || monitor.Data.Goroutines <= 0 {
		t.Fatalf("monitor = %s", recorder.Body.String())
	}
	if monitor.Data.Database["ok"] != true {
		t.Fatalf("monitor.database = %#v", monitor.Data.Database)
	}
	if monitor.Data.RiverJobs["available"] != float64(2) || monitor.Data.RiverError != "" {
		t.Fatalf("monitor.river_jobs = (%#v, %q)", monitor.Data.RiverJobs, monitor.Data.RiverError)
	}
	if monitor.Data.Redis != nil {
		t.Fatalf("未配置缓存时不应出现 redis: %#v", monitor.Data.Redis)
	}

	// 队列查询失败时降级成 river_jobs_error，整体仍是成功响应。
	if _, err := deps.DB.Pool.Exec(ctx, `DROP TABLE river_job`); err != nil {
		t.Fatalf("删除 river_job 替身表: %v", err)
	}
	if _, err := deps.riverJobStates(ctx); err == nil {
		t.Fatal("表不存在时 riverJobStates 应报错")
	}
	recorder = httptest.NewRecorder()
	deps.monitorRuntime(recorder, httptest.NewRequest(http.MethodGet, "/system/monitor", nil))
	monitor = struct {
		Success bool `json:"success"`
		Data    struct {
			Environment string         `json:"environment"`
			Goroutines  int            `json:"goroutines"`
			Database    map[string]any `json:"database"`
			RiverJobs   map[string]any `json:"river_jobs"`
			RiverError  string         `json:"river_jobs_error"`
			Redis       map[string]any `json:"redis"`
		} `json:"data"`
	}{}
	if err := json.Unmarshal(recorder.Body.Bytes(), &monitor); err != nil {
		t.Fatalf("解析降级 monitor: %v", err)
	}
	if !monitor.Success || monitor.Data.RiverJobs != nil || monitor.Data.RiverError == "" {
		t.Fatalf("降级 monitor = %s", recorder.Body.String())
	}
}
