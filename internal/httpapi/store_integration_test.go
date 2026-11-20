//go:build integration

package httpapi

import (
	"bytes"
	"context"
	"encoding/json"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/moviepilot-go/moviepilot-go/internal/auth"
	"github.com/moviepilot-go/moviepilot-go/internal/config"
	"github.com/moviepilot-go/moviepilot-go/internal/database"
)

const httpapiIntegrationDatabaseEnv = "MOVIEPILOT_TEST_DATABASE_URL"
const httpapiIntegrationSchemaPrefix = "moviepilot_httpapi_it_"

// newHTTPAPIIntegrationDeps 在独立 schema 里跑完整迁移并返回带真实 Store 的依赖。
func newHTTPAPIIntegrationDeps(t *testing.T) Dependencies {
	t.Helper()

	databaseURL := strings.TrimSpace(os.Getenv(httpapiIntegrationDatabaseEnv))
	if databaseURL == "" {
		t.Skipf("未设置 %s，跳过 PostgreSQL 集成测试", httpapiIntegrationDatabaseEnv)
	}

	setupCtx, cancel := context.WithTimeout(t.Context(), 60*time.Second)
	defer cancel()

	adminConfig, err := pgxpool.ParseConfig(databaseURL)
	if err != nil {
		t.Fatalf("解析 %s: %v", httpapiIntegrationDatabaseEnv, err)
	}
	adminConfig.MaxConns = 2
	adminConfig.MinConns = 0
	adminPool, err := pgxpool.NewWithConfig(setupCtx, adminConfig)
	if err != nil {
		t.Fatalf("创建 PostgreSQL 管理连接池: %v", err)
	}
	t.Cleanup(adminPool.Close)
	if err := adminPool.Ping(setupCtx); err != nil {
		t.Fatalf("连接 PostgreSQL 集成测试数据库: %v", err)
	}

	schema := httpapiIntegrationSchemaPrefix + strings.ReplaceAll(uuid.NewString(), "-", "")
	quotedSchema := pgx.Identifier{schema}.Sanitize()
	if _, err := adminPool.Exec(setupCtx, "CREATE SCHEMA "+quotedSchema); err != nil {
		t.Fatalf("创建隔离 schema %q: %v", schema, err)
	}

	var storePool *pgxpool.Pool
	t.Cleanup(func() {
		if storePool != nil {
			storePool.Close()
		}
		cleanupCtx, cleanupCancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cleanupCancel()
		if _, err := adminPool.Exec(cleanupCtx, "DROP SCHEMA "+quotedSchema+" CASCADE"); err != nil {
			t.Errorf("清理隔离 schema %q: %v", schema, err)
		}
	})

	migrationURL, err := url.Parse(databaseURL)
	if err != nil {
		t.Fatalf("解析 PostgreSQL URL: %v", err)
	}
	query := migrationURL.Query()
	query.Set("search_path", schema)
	query.Set("application_name", "moviepilot-httpapi-integration")
	migrationURL.RawQuery = query.Encode()
	if err := database.Migrate(setupCtx, config.Config{DatabaseURL: migrationURL.String()}); err != nil {
		t.Fatalf("在隔离 schema %q 执行迁移: %v", schema, err)
	}

	poolConfig, err := pgxpool.ParseConfig(databaseURL)
	if err != nil {
		t.Fatalf("解析业务连接池配置: %v", err)
	}
	poolConfig.MaxConns = 8
	poolConfig.MinConns = 0
	poolConfig.ConnConfig.RuntimeParams["search_path"] = schema
	poolConfig.ConnConfig.RuntimeParams["application_name"] = "moviepilot-httpapi-integration"
	storePool, err = pgxpool.NewWithConfig(setupCtx, poolConfig)
	if err != nil {
		t.Fatalf("创建隔离业务连接池: %v", err)
	}
	if err := storePool.Ping(setupCtx); err != nil {
		t.Fatalf("连接隔离 schema %q: %v", schema, err)
	}
	var currentSchema string
	if err := storePool.QueryRow(setupCtx, "SELECT current_schema()").Scan(&currentSchema); err != nil {
		t.Fatalf("校验当前 schema: %v", err)
	}
	if currentSchema != schema {
		t.Fatalf("业务连接当前 schema = %q, want %q", currentSchema, schema)
	}

	box, err := auth.NewSecretBox("moviepilot-httpapi-integration-key")
	if err != nil {
		t.Fatalf("创建敏感配置加密器: %v", err)
	}
	db := &database.DB{Pool: storePool}
	return Dependencies{
		Store:  database.NewStore(db, box),
		DB:     db,
		Config: config.Config{Environment: "test", MaxRequestBody: 1 << 20},
	}
}

// httpapiClaimsRequest 给请求挂上认证声明与 chi 路由参数。
func httpapiClaimsRequest(method, target string, params map[string]string, claims auth.Claims) *http.Request {
	request := httptest.NewRequest(method, target, nil)
	routeContext := chi.NewRouteContext()
	for name, value := range params {
		routeContext.URLParams.Add(name, value)
	}
	ctx := context.WithValue(request.Context(), chi.RouteCtxKey, routeContext)
	ctx = context.WithValue(ctx, claimsContextKey, claims)
	return request.WithContext(ctx)
}

func httpapiJSONRequest(method, target string, params map[string]string, claims auth.Claims, body string) *http.Request {
	request := httpapiClaimsRequest(method, target, params, claims)
	request.Body = http.NoBody
	if body != "" {
		request = request.WithContext(request.Context())
		request.Body = ioNopCloser(strings.NewReader(body))
		request.ContentLength = int64(len(body))
	}
	return request
}

type nopCloser struct{ *strings.Reader }

func (nopCloser) Close() error { return nil }

func ioNopCloser(reader *strings.Reader) nopCloser { return nopCloser{reader} }

func seedHTTPAPIUser(t *testing.T, deps Dependencies, username string, superUser bool) auth.User {
	t.Helper()
	hash, err := auth.HashPassword("abcd1234")
	if err != nil {
		t.Fatalf("生成密码哈希: %v", err)
	}
	user, err := deps.Store.CreateUser(t.Context(), username, hash, superUser, map[string]any{})
	if err != nil {
		t.Fatalf("创建用户 %q: %v", username, err)
	}
	return user
}

func TestUsersStoreIntegration(t *testing.T) {
	deps := newHTTPAPIIntegrationDeps(t)
	admin := seedHTTPAPIUser(t, deps, "admin", true)
	adminClaims := auth.Claims{Username: "admin", SuperUser: true}

	// 列表：超级管理员可见全部用户。
	recorder := httptest.NewRecorder()
	deps.listUsers(recorder, httpapiClaimsRequest(http.MethodGet, "/user/", nil, adminClaims))
	var users []map[string]any
	if err := json.Unmarshal(recorder.Body.Bytes(), &users); err != nil {
		t.Fatalf("解析用户列表: %v; body=%s", err, recorder.Body.String())
	}
	if len(users) != 1 || users[0]["name"] != "admin" {
		t.Fatalf("用户列表 = %#v", users)
	}

	// 创建：正常、重名、弱密码、空名、坏 JSON。
	recorder = httptest.NewRecorder()
	deps.createUser(recorder, httpapiJSONRequest(http.MethodPost, "/user/", nil, adminClaims,
		`{"name":" alice ","password":"abcd1234"}`))
	if body := decodeResponseBody(t, recorder.Body.Bytes()); !body.Success {
		t.Fatalf("创建用户 = %+v", body)
	}
	alice, err := deps.Store.UserByName(t.Context(), "alice")
	if err != nil || alice.SuperUser {
		t.Fatalf("新建用户 = (%+v, %v)", alice, err)
	}

	cases := []struct {
		payload string
		message string
	}{
		{`{"name":"alice","password":"abcd1234"}`, "用户已存在"},
		{`{"name":"bob","password":"abcdefgh"}`, userPasswordRule},
		{`{"name":"   ","password":"abcd1234"}`, "用户名不能为空"},
		{`{"name":`, "参数错误"},
	}
	for _, test := range cases {
		recorder = httptest.NewRecorder()
		deps.createUser(recorder, httpapiJSONRequest(http.MethodPost, "/user/", nil, adminClaims, test.payload))
		if body := decodeResponseBody(t, recorder.Body.Bytes()); body.Success || body.Message != test.message {
			t.Fatalf("%s = %+v", test.payload, body)
		}
	}
	if _, err := deps.Store.UserByName(t.Context(), "bob"); err == nil {
		t.Fatal("弱密码用户不应落库")
	}

	// 详情：命中与未命中。
	recorder = httptest.NewRecorder()
	deps.getUser(recorder, httpapiClaimsRequest(http.MethodGet, "/user/alice", map[string]string{"username": "alice"}, adminClaims))
	var detail map[string]any
	if err := json.Unmarshal(recorder.Body.Bytes(), &detail); err != nil {
		t.Fatalf("解析用户详情: %v", err)
	}
	if detail["name"] != "alice" {
		t.Fatalf("用户详情 = %#v", detail)
	}
	recorder = httptest.NewRecorder()
	deps.getUser(recorder, httpapiClaimsRequest(http.MethodGet, "/user/missing", map[string]string{"username": "missing"}, adminClaims))
	if recorder.Code != http.StatusNotFound {
		t.Fatalf("未知用户 status = %d", recorder.Code)
	}

	// 更新：改名 + 改密码；非法 ID、未知 ID、重名。
	recorder = httptest.NewRecorder()
	deps.updateUser(recorder, httpapiJSONRequest(http.MethodPut, "/user/", nil, adminClaims,
		`{"id":`+strconv.FormatInt(alice.ID, 10)+`,"name":"alice2","password":"abcd1234!","is_superuser":false}`))
	if body := decodeResponseBody(t, recorder.Body.Bytes()); !body.Success {
		t.Fatalf("更新用户 = %+v", body)
	}
	renamed, err := deps.Store.UserByName(t.Context(), "alice2")
	if err != nil || renamed.ID != alice.ID {
		t.Fatalf("改名结果 = (%+v, %v)", renamed, err)
	}

	updateCases := []struct {
		payload string
		message string
	}{
		{`{"id":0,"name":"x"}`, "用户ID不能为空"},
		{`{"id":99999,"name":"x"}`, "用户不存在"},
		{`{"id":` + strconv.FormatInt(alice.ID, 10) + `,"name":"admin"}`, "用户已存在"},
		{`{"id":` + strconv.FormatInt(alice.ID, 10) + `,"password":"abc"}`, userPasswordRule},
	}
	for _, test := range updateCases {
		recorder = httptest.NewRecorder()
		deps.updateUser(recorder, httpapiJSONRequest(http.MethodPut, "/user/", nil, adminClaims, test.payload))
		if body := decodeResponseBody(t, recorder.Body.Bytes()); body.Success || body.Message != test.message {
			t.Fatalf("%s = %+v", test.payload, body)
		}
	}

	// 头像：缺少文件、非本人且非超管、本人上传成功。
	recorder = httptest.NewRecorder()
	deps.uploadUserAvatar(recorder, httpapiClaimsRequest(http.MethodPost, "/user/avatar/1",
		map[string]string{"user_id": strconv.FormatInt(alice.ID, 10)}, auth.Claims{Username: "alice2"}))
	if body := decodeResponseBody(t, recorder.Body.Bytes()); body.Success || body.Message != "缺少上传文件" {
		t.Fatalf("缺少文件 = %+v", body)
	}

	recorder = httptest.NewRecorder()
	avatarRequest := httpapiClaimsRequest(http.MethodPost, "/user/avatar/1",
		map[string]string{"user_id": strconv.FormatInt(alice.ID, 10)}, auth.Claims{Username: "alice2"})
	var upload bytes.Buffer
	writer := multipart.NewWriter(&upload)
	part, err := writer.CreateFormFile("file", "avatar.png")
	if err != nil {
		t.Fatalf("构建上传体: %v", err)
	}
	if _, err := part.Write([]byte("png-bytes")); err != nil {
		t.Fatalf("写入上传体: %v", err)
	}
	if err := writer.Close(); err != nil {
		t.Fatalf("关闭 multipart: %v", err)
	}
	avatarRequest.Body = ioNopCloser(strings.NewReader(upload.String()))
	avatarRequest.Header.Set("Content-Type", writer.FormDataContentType())
	avatarRequest.ContentLength = int64(upload.Len())
	avatarRequest = avatarRequest.WithContext(avatarRequest.Context())
	deps.uploadUserAvatar(recorder, avatarRequest)
	if body := decodeResponseBody(t, recorder.Body.Bytes()); !body.Success {
		t.Fatalf("上传头像 = %+v", body)
	}
	updated, err := deps.Store.UserByID(t.Context(), alice.ID)
	if err != nil || updated.Avatar == nil || !strings.HasPrefix(*updated.Avatar, "data:image/ico;base64,") {
		t.Fatalf("头像字段 = (%v, %v)", updated.Avatar, err)
	}

	// 头像守卫：无身份、非法 ID、未知用户、非本人的普通用户。
	recorder = httptest.NewRecorder()
	deps.uploadUserAvatar(recorder, httptest.NewRequest(http.MethodPost, "/user/avatar/1", nil))
	if recorder.Code != http.StatusForbidden {
		t.Fatalf("无身份 status = %d", recorder.Code)
	}
	recorder = httptest.NewRecorder()
	deps.uploadUserAvatar(recorder, httpapiClaimsRequest(http.MethodPost, "/user/avatar/abc",
		map[string]string{"user_id": "abc"}, adminClaims))
	if body := decodeResponseBody(t, recorder.Body.Bytes()); body.Success || body.Message != "用户ID无效" {
		t.Fatalf("头像非法 ID = %+v", body)
	}
	recorder = httptest.NewRecorder()
	deps.uploadUserAvatar(recorder, httpapiClaimsRequest(http.MethodPost, "/user/avatar/99999",
		map[string]string{"user_id": "99999"}, adminClaims))
	if body := decodeResponseBody(t, recorder.Body.Bytes()); body.Success || body.Message != "用户不存在" {
		t.Fatalf("头像未知用户 = %+v", body)
	}
	recorder = httptest.NewRecorder()
	deps.uploadUserAvatar(recorder, httpapiClaimsRequest(http.MethodPost, "/user/avatar/"+strconv.FormatInt(admin.ID, 10),
		map[string]string{"user_id": strconv.FormatInt(admin.ID, 10)}, auth.Claims{Username: "alice2"}))
	if recorder.Code != http.StatusForbidden || !strings.Contains(recorder.Body.String(), "用户权限不足") {
		t.Fatalf("越权上传 = (%d, %s)", recorder.Code, recorder.Body.String())
	}

	// 删除：非法 ID、未知 ID、按名删除未知/已知、最后一个超管保护。
	recorder = httptest.NewRecorder()
	deps.deleteUserByID(recorder, httpapiClaimsRequest(http.MethodDelete, "/user/id/0",
		map[string]string{"user_id": "0"}, adminClaims))
	if body := decodeResponseBody(t, recorder.Body.Bytes()); body.Success || body.Message != "用户ID无效" {
		t.Fatalf("非法 ID = %+v", body)
	}
	recorder = httptest.NewRecorder()
	deps.deleteUserByID(recorder, httpapiClaimsRequest(http.MethodDelete, "/user/id/99999",
		map[string]string{"user_id": "99999"}, adminClaims))
	if body := decodeResponseBody(t, recorder.Body.Bytes()); body.Success || body.Message != "用户不存在" {
		t.Fatalf("未知 ID = %+v", body)
	}
	recorder = httptest.NewRecorder()
	deps.deleteUserByName(recorder, httpapiClaimsRequest(http.MethodDelete, "/user/name/ghost",
		map[string]string{"user_name": "ghost"}, adminClaims))
	if body := decodeResponseBody(t, recorder.Body.Bytes()); body.Success || body.Message != "用户不存在" {
		t.Fatalf("未知用户名 = %+v", body)
	}
	recorder = httptest.NewRecorder()
	deps.deleteUserByID(recorder, httpapiClaimsRequest(http.MethodDelete, "/user/id/"+strconv.FormatInt(admin.ID, 10),
		map[string]string{"user_id": strconv.FormatInt(admin.ID, 10)}, adminClaims))
	if body := decodeResponseBody(t, recorder.Body.Bytes()); body.Success ||
		body.Message != "不能删除最后一个超级管理员" {
		t.Fatalf("最后超管 = %+v", body)
	}
	recorder = httptest.NewRecorder()
	deps.deleteUserByName(recorder, httpapiClaimsRequest(http.MethodDelete, "/user/name/alice2",
		map[string]string{"user_name": "alice2"}, adminClaims))
	if body := decodeResponseBody(t, recorder.Body.Bytes()); !body.Success {
		t.Fatalf("按名删除 = %+v", body)
	}
	if _, err := deps.Store.UserByID(t.Context(), alice.ID); err == nil {
		t.Fatal("删除后仍能查到用户")
	}
}

func TestSiteByIDStoreIntegration(t *testing.T) {
	deps := newHTTPAPIIntegrationDeps(t)
	adminClaims := auth.Claims{Username: "admin", SuperUser: true}

	id, err := deps.Store.UpsertSite(t.Context(), database.Site{
		Name: "集成站", Domain: "site.example", URL: "https://site.example", Adapter: "rss", Enabled: true,
		Config: map[string]any{"public": true},
	})
	if err != nil {
		t.Fatalf("新增站点: %v", err)
	}

	recorder := httptest.NewRecorder()
	deps.getSite(recorder, httpapiClaimsRequest(http.MethodGet, "/site/"+strconv.FormatInt(int64(id), 10),
		map[string]string{"siteid": strconv.FormatInt(int64(id), 10)}, adminClaims))
	var site map[string]any
	if err := json.Unmarshal(recorder.Body.Bytes(), &site); err != nil {
		t.Fatalf("解析站点详情: %v; body=%s", err, recorder.Body.String())
	}
	if site["name"] != "集成站" || site["domain"] != "site.example" {
		t.Fatalf("站点详情 = %#v", site)
	}

	recorder = httptest.NewRecorder()
	deps.getSite(recorder, httpapiClaimsRequest(http.MethodGet, "/site/abc",
		map[string]string{"siteid": "abc"}, adminClaims))
	if recorder.Code != http.StatusBadRequest {
		t.Fatalf("非法 ID status = %d, body=%s", recorder.Code, recorder.Body.String())
	}

	recorder = httptest.NewRecorder()
	deps.getSite(recorder, httpapiClaimsRequest(http.MethodGet, "/site/99999",
		map[string]string{"siteid": "99999"}, adminClaims))
	if recorder.Code != http.StatusNotFound {
		t.Fatalf("未知站点 status = %d, body=%s", recorder.Code, recorder.Body.String())
	}
}
