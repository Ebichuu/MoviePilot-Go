package cookiecloud

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func writeLocalPayload(t *testing.T, dir string, payload map[string]any) {
	t.Helper()
	encoded, err := json.Marshal(payload)
	if err != nil {
		t.Fatalf("编码载荷失败: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "key.json"), encoded, 0o600); err != nil {
		t.Fatalf("写入载荷失败: %v", err)
	}
}

func encryptedPayload(t *testing.T, plaintext string) map[string]any {
	t.Helper()
	encrypted, err := Encrypt([]byte(plaintext), CryptKey("key", "pass"))
	if err != nil {
		t.Fatalf("加密失败: %v", err)
	}
	return map[string]any{"encrypted": encrypted}
}

func TestDownloadRequiresCompleteConfig(t *testing.T) {
	cases := map[string]Config{
		"缺 key":        {Server: "http://cloud.local", Password: "pass"},
		"缺 password":   {Server: "http://cloud.local", Key: "key"},
		"远端模式缺 server": {Key: "key", Password: "pass"},
	}
	for name, cfg := range cases {
		if _, err := NewHelper(cfg, nil).Download(context.Background()); !errors.Is(err, ErrConfig) {
			t.Fatalf("%s 应返回 ErrConfig，实际 %v", name, err)
		}
	}
	// 本地模式不需要 server，但仍要 key 与 password。
	if _, err := NewHelper(Config{EnableLocal: true, LocalDir: t.TempDir(), Password: "pass"}, nil).
		Download(context.Background()); !errors.Is(err, ErrConfig) {
		t.Fatal("本地模式缺 key 也应返回 ErrConfig")
	}
}

func TestDownloadReportsMissingOrBrokenLocalPayload(t *testing.T) {
	missing := NewHelper(Config{EnableLocal: true, LocalDir: t.TempDir(), Key: "key", Password: "pass"}, nil)
	if _, err := missing.Download(context.Background()); !errors.Is(err, ErrRequest) {
		t.Fatalf("本地文件缺失应返回 ErrRequest，实际 %v", err)
	}

	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "key.json"), []byte("not-json"), 0o600); err != nil {
		t.Fatalf("写入损坏载荷失败: %v", err)
	}
	broken := NewHelper(Config{EnableLocal: true, LocalDir: dir, Key: "key", Password: "pass"}, nil)
	if _, err := broken.Download(context.Background()); !errors.Is(err, ErrRequest) {
		t.Fatalf("损坏载荷应返回 ErrRequest，实际 %v", err)
	}
}

func TestDownloadRejectsMissingCipherAndWrongPassword(t *testing.T) {
	dir := t.TempDir()
	writeLocalPayload(t, dir, map[string]any{"other": "x"})
	helper := NewHelper(Config{EnableLocal: true, LocalDir: dir, Key: "key", Password: "pass"}, nil)
	if _, err := helper.Download(context.Background()); !errors.Is(err, ErrNoCipher) {
		t.Fatalf("缺少密文应返回 ErrNoCipher，实际 %v", err)
	}

	dir = t.TempDir()
	writeLocalPayload(t, dir, map[string]any{"encrypted": "   "})
	helper = NewHelper(Config{EnableLocal: true, LocalDir: dir, Key: "key", Password: "pass"}, nil)
	if _, err := helper.Download(context.Background()); !errors.Is(err, ErrNoCipher) {
		t.Fatalf("空白密文应返回 ErrNoCipher，实际 %v", err)
	}

	// 用错误口令加密：解密必须失败，不能返回半截结果。
	dir = t.TempDir()
	wrong, err := Encrypt([]byte(`{"cookie_data":{"a":[{"domain":"a.com","name":"x","value":"1"}]}}`), CryptKey("key", "wrong"))
	if err != nil {
		t.Fatalf("加密失败: %v", err)
	}
	writeLocalPayload(t, dir, map[string]any{"encrypted": wrong})
	helper = NewHelper(Config{EnableLocal: true, LocalDir: dir, Key: "key", Password: "pass"}, nil)
	if _, err := helper.Download(context.Background()); err == nil {
		t.Fatal("错误口令必须解密失败")
	}
}

func TestDownloadRejectsEmptyPlaintext(t *testing.T) {
	for name, plaintext := range map[string]string{
		"空对象":    "{}",
		"非 JSON": "definitely-not-json",
	} {
		dir := t.TempDir()
		writeLocalPayload(t, dir, encryptedPayload(t, plaintext))
		helper := NewHelper(Config{EnableLocal: true, LocalDir: dir, Key: "key", Password: "pass"}, nil)
		if _, err := helper.Download(context.Background()); !errors.Is(err, ErrEmpty) {
			t.Fatalf("%s 应返回 ErrEmpty，实际 %v", name, err)
		}
	}
}

func TestDownloadGroupsCookiesByDomain(t *testing.T) {
	plaintext := `{
		"cookie_data": {
			"https://www.example.com": [
				{"domain": ".example.com", "name": "uid", "value": "1"},
				{"domain": ".example.com", "name": "cid", "value": "2"},
				{"domain": ".example.com", "name": "CookieAutoDeleteBrowsingDataCleanup", "value": "drop"},
				{"domain": ".example.com", "name": "", "value": "empty-name"},
				"not-an-object",
				{"domain": "", "name": "nodomain", "value": "x"}
			],
			"mikan": [
				{"domain": ".mikanani.me", "name": "cf_clearance", "value": "cf"}
			]
		}
	}`
	dir := t.TempDir()
	writeLocalPayload(t, dir, encryptedPayload(t, plaintext))
	helper := NewHelper(Config{EnableLocal: true, LocalDir: dir, Key: "key", Password: "pass"}, nil)

	result, err := helper.Download(context.Background())
	if err != nil {
		t.Fatalf("下载失败: %v", err)
	}

	exampleKey := URLDomain(".example.com")
	if exampleKey == "" {
		t.Fatal("URLDomain 未返回可用域名")
	}
	cookies, ok := result[exampleKey]
	if !ok {
		t.Fatalf("缺少域名 %q 的 cookie，实际 %+v", exampleKey, result)
	}
	// 内置忽略项与空名字必须被跳过，其余按 name=value 用分号连接。
	if cookies != "uid=1;cid=2" {
		t.Fatalf("cookie 拼接 = %q，期望 uid=1;cid=2", cookies)
	}
	// 纯 cf_clearance 的域不产出条目：没有它反而让站点用密码登录。
	for domain, value := range result {
		if strings.HasPrefix(value, "cf_clearance=") {
			t.Fatalf("纯 cf_clearance 组不应产出: %s=%s", domain, value)
		}
	}
}

func TestDownloadAcceptsRootLevelCookieMap(t *testing.T) {
	// 有些版本没有 cookie_data 包装，直接就是「域 → cookie 列表」。
	plaintext := `{"a.example.org": [{"domain": "a.example.org", "name": "sid", "value": "9"}]}`
	dir := t.TempDir()
	writeLocalPayload(t, dir, encryptedPayload(t, plaintext))
	result, err := NewHelper(Config{EnableLocal: true, LocalDir: dir, Key: "key", Password: "pass"}, nil).
		Download(context.Background())
	if err != nil {
		t.Fatalf("下载失败: %v", err)
	}
	key := URLDomain("a.example.org")
	if result[key] != "sid=9" {
		t.Fatalf("根级 cookie 映射未生效: %+v", result)
	}
}

func TestDownloadOverHTTP(t *testing.T) {
	payload := encryptedPayload(t, `{"cookie_data":{"b.example.net":[{"domain":"b.example.net","name":"uid","value":"7"}]}}`)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/get/key" {
			t.Errorf("请求路径 = %q，期望 /get/key", r.URL.Path)
		}
		_ = json.NewEncoder(w).Encode(payload)
	}))
	defer server.Close()

	helper := NewHelper(Config{Server: server.URL + "/", Key: "key", Password: "pass"}, server.Client())
	result, err := helper.Download(context.Background())
	if err != nil {
		t.Fatalf("下载失败: %v", err)
	}
	if result[URLDomain("b.example.net")] != "uid=7" {
		t.Fatalf("远端下载结果不对: %+v", result)
	}

	// 非 200 与非法 JSON 都必须映射成 ErrRequest。
	bad := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer bad.Close()
	if _, err := NewHelper(Config{Server: bad.URL, Key: "key", Password: "pass"}, bad.Client()).
		Download(context.Background()); !errors.Is(err, ErrRequest) {
		t.Fatalf("非 200 应返回 ErrRequest，实际 %v", err)
	}

	garbage := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("not-json"))
	}))
	defer garbage.Close()
	if _, err := NewHelper(Config{Server: garbage.URL, Key: "key", Password: "pass"}, garbage.Client()).
		Download(context.Background()); !errors.Is(err, ErrRequest) {
		t.Fatalf("非法 JSON 应返回 ErrRequest，实际 %v", err)
	}
}

func TestDecodeCookieEntry(t *testing.T) {
	entry, ok := decodeCookieEntry(map[string]any{"domain": "a.com", "name": "uid", "value": "1"})
	if !ok || entry.Domain != "a.com" || entry.Name != "uid" || entry.Value != "1" {
		t.Fatalf("解析失败: %+v %v", entry, ok)
	}
	// 字段缺失时保留空串，但仍然算一个合法条目（由上层决定丢弃）。
	entry, ok = decodeCookieEntry(map[string]any{"name": "x", "value": 123})
	if !ok || entry.Domain != "" || entry.Value != "" {
		t.Fatalf("缺字段/null 解析错误: %+v %v", entry, ok)
	}
	if _, ok := decodeCookieEntry("not-an-object"); ok {
		t.Fatal("非对象应返回 false")
	}
}

func TestNewHelperDefaultsToHTTPClient(t *testing.T) {
	helper := NewHelper(Config{Server: "http://x", Key: "k", Password: "p"}, nil)
	if helper.httpClient == nil {
		t.Fatal("nil 客户端应回落到默认客户端")
	}
	if helper.readFile == nil {
		t.Fatal("readFile 必须初始化")
	}
}
