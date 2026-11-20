package storage

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
)

// alistFake 模拟 Alist/OpenList 的开放 API 子集，记录收到的请求以便断言契约。
type alistFake struct {
	mu       sync.Mutex
	logins   int
	requests []map[string]any
	failCode int
	token    string
}

func (f *alistFake) handler(w http.ResponseWriter, r *http.Request) {
	payload, _ := io.ReadAll(r.Body)
	var body map[string]any
	_ = json.Unmarshal(payload, &body)
	f.mu.Lock()
	f.requests = append(f.requests, map[string]any{"path": r.URL.Path, "body": body})
	f.mu.Unlock()

	if r.URL.Path == "/api/auth/login" {
		f.mu.Lock()
		f.logins++
		f.mu.Unlock()
		if f.token == "" {
			f.token = "token-1"
		}
		writeAlist(w, 200, map[string]any{"token": f.token})
		return
	}
	if f.failCode != 0 {
		writeAlist(w, f.failCode, nil)
		return
	}
	switch r.URL.Path {
	case "/api/fs/list":
		page, _ := body["page"].(float64)
		if page > 1 {
			writeAlist(w, 200, map[string]any{"content": []any{}})
			return
		}
		writeAlist(w, 200, map[string]any{"content": []any{
			map[string]any{"name": "第2集.mp4", "is_dir": false, "size": 2048, "modified": "2026-11-19T10:00:00Z"},
			map[string]any{"name": "第10集.mp4", "is_dir": false, "size": 4096, "modified": "2026-11-18T10:00:00Z"},
			map[string]any{"name": "Subs", "is_dir": true},
		}})
	default:
		writeAlistCode(w, 200)
	}
}

func writeAlist(w http.ResponseWriter, code int, data map[string]any) {
	_ = json.NewEncoder(w).Encode(map[string]any{"code": code, "message": "", "data": data})
}

func writeAlistCode(w http.ResponseWriter, code int) {
	_ = json.NewEncoder(w).Encode(map[string]any{"code": code, "message": ""})
}

func (f *alistFake) last() (string, map[string]any) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.requests) == 0 {
		return "", nil
	}
	last := f.requests[len(f.requests)-1]
	path, _ := last["path"].(string)
	body, _ := last["body"].(map[string]any)
	return path, body
}

func newAlistFake(t *testing.T) (*AlistProvider, *alistFake) {
	t.Helper()
	fake := &alistFake{}
	server := httptest.NewServer(http.HandlerFunc(fake.handler))
	t.Cleanup(server.Close)
	provider, err := NewAlistProvider(AlistConfig{
		Name: "alist", URL: server.URL + "/", Username: "user", Password: "pass", Root: "/media",
	})
	if err != nil {
		t.Fatalf("构造 Alist 适配器失败: %v", err)
	}
	return provider, fake
}

func TestNewAlistProviderDefaultsAndValidation(t *testing.T) {
	if _, err := NewAlistProvider(AlistConfig{}); !errors.Is(err, ErrInvalidRequest) {
		t.Fatalf("空 URL 应报参数错误，实际 %v", err)
	}
	provider, err := NewAlistProvider(AlistConfig{URL: " https://alist.example/ ", Root: "media"})
	if err != nil {
		t.Fatalf("构造失败: %v", err)
	}
	if provider.Name() != "alist" {
		t.Fatalf("默认名 = %q", provider.Name())
	}
	// URL 去尾斜杠、root 补前导斜杠，都不该出现双斜杠。
	if provider.config.URL != "https://alist.example" {
		t.Fatalf("URL = %q", provider.config.URL)
	}
	if provider.config.Root != "/media" {
		t.Fatalf("Root = %q", provider.config.Root)
	}
	if provider.client.Timeout <= 0 {
		t.Fatal("必须设置超时")
	}

	var nilProvider *AlistProvider
	if nilProvider.Name() != "" {
		t.Fatal("nil 适配器名称为空串")
	}
}

func TestAlistRequireGuards(t *testing.T) {
	provider, _ := newAlistFake(t)
	if err := provider.require(context.Background(), "other"); !errors.Is(err, ErrUnsupportedStorage) {
		t.Fatalf("存储名不匹配错误 = %v", err)
	}
	if err := provider.require(nil, "alist"); !errors.Is(err, ErrInvalidRequest) {
		t.Fatalf("nil 上下文错误 = %v", err)
	}
	cancelled, cancel := context.WithCancel(context.Background())
	cancel()
	if err := provider.require(cancelled, "alist"); !errors.Is(err, context.Canceled) {
		t.Fatalf("已取消上下文错误 = %v", err)
	}
	var nilProvider *AlistProvider
	if err := nilProvider.require(context.Background(), "alist"); !errors.Is(err, ErrUnsupportedStorage) {
		t.Fatalf("nil 适配器错误 = %v", err)
	}
}

func TestAlistListLogsInOnceAndProjectsEntries(t *testing.T) {
	provider, fake := newAlistFake(t)

	items, err := provider.List(context.Background(), "alist", "剧集", "name", "")
	if err != nil {
		t.Fatalf("列目录失败: %v", err)
	}
	if len(items) != 3 {
		t.Fatalf("条目数 = %d: %+v", len(items), items)
	}
	// 用名字建索引，避免把「按名称排序」与条目投影两件事混在一个下标断言里。
	byName := make(map[string]Item, len(items))
	for _, item := range items {
		byName[item.Name] = item
	}

	// 名称排序是「数字感知」的：第2集 必须排在 第10集 之前。
	firstEpisode, secondEpisode := -1, -1
	for index, item := range items {
		switch item.Name {
		case "第2集.mp4":
			firstEpisode = index
		case "第10集.mp4":
			secondEpisode = index
		}
	}
	if firstEpisode < 0 || secondEpisode < 0 || firstEpisode > secondEpisode {
		t.Fatalf("自然排序错误: 第2集@%d 第10集@%d (%v)", firstEpisode, secondEpisode, items)
	}

	file := byName["第2集.mp4"]
	if file.Type != "file" || file.Extension != "mp4" || file.Basename != "第2集" {
		t.Fatalf("文件条目投影错误: %+v", file)
	}
	if file.Size == nil || *file.Size != 2048 {
		t.Fatalf("大小 = %v", file.Size)
	}
	if file.ModifyTime == nil {
		t.Fatal("应解析 RFC3339 修改时间")
	}
	if file.Path != "剧集/第2集.mp4" {
		t.Fatalf("路径 = %q", file.Path)
	}
	directory := byName["Subs"]
	if directory.Type != "dir" || directory.Size != nil {
		t.Fatalf("目录条目投影错误: %+v", directory)
	}
	// 目录条目不应带大小/扩展名。
	if directory.Extension != "" {
		t.Fatalf("目录不应有扩展名: %+v", directory)
	}

	// 第一次调用登录，第二次复用 token。
	if _, err := provider.List(context.Background(), "alist", "剧集", "name", ""); err != nil {
		t.Fatalf("第二次列目录失败: %v", err)
	}
	fake.mu.Lock()
	logins := fake.logins
	fake.mu.Unlock()
	if logins != 1 {
		t.Fatalf("登录次数 = %d，期望 1（应复用 token）", logins)
	}

	// 请求里必须带 Root 前缀与分页参数。
	path, body := fake.last()
	if path != "/api/fs/list" {
		t.Fatalf("末次请求 = %q", path)
	}
	if body["path"] != "/media/剧集" {
		t.Fatalf("远端路径 = %v", body["path"])
	}
	if body["per_page"] == nil || body["page"] == nil {
		t.Fatalf("缺少分页参数: %v", body)
	}
}

func TestAlistListFiltersKeyword(t *testing.T) {
	provider, _ := newAlistFake(t)
	items, err := provider.List(context.Background(), "alist", "", "", "*.mp4")
	if err != nil {
		t.Fatalf("列目录失败: %v", err)
	}
	if len(items) != 2 {
		t.Fatalf("关键字过滤后条目数 = %d: %+v", len(items), items)
	}
	for _, item := range items {
		if item.Type != "file" {
			t.Fatalf("过滤后不应包含目录: %+v", item)
		}
	}
}

func TestAlistSkipsLoginWithoutCredentials(t *testing.T) {
	fake := &alistFake{}
	server := httptest.NewServer(http.HandlerFunc(fake.handler))
	defer server.Close()
	provider, err := NewAlistProvider(AlistConfig{URL: server.URL})
	if err != nil {
		t.Fatalf("构造失败: %v", err)
	}
	if _, err := provider.List(context.Background(), "alist", "", "", ""); err != nil {
		t.Fatalf("匿名列出失败: %v", err)
	}
	fake.mu.Lock()
	logins := fake.logins
	fake.mu.Unlock()
	if logins != 0 {
		t.Fatalf("无凭据不应尝试登录，实际 %d 次", logins)
	}
}

func TestAlistRequestErrorMapping(t *testing.T) {
	provider, fake := newAlistFake(t)
	if _, err := provider.List(context.Background(), "alist", "", "", ""); err != nil {
		t.Fatalf("首次列出失败: %v", err)
	}
	// 业务 code 非 200：统一映射为操作失败。
	fake.mu.Lock()
	fake.failCode = 500
	fake.mu.Unlock()
	if _, err := provider.List(context.Background(), "alist", "", "", ""); !errors.Is(err, ErrOperationFailed) {
		t.Fatalf("code 500 错误 = %v", err)
	}
}

func TestAlistUnauthorizedClearsToken(t *testing.T) {
	provider, _ := newAlistFake(t)
	if _, err := provider.List(context.Background(), "alist", "", "", ""); err != nil {
		t.Fatalf("首次列出失败: %v", err)
	}
	provider.storeToken("stale-token")

	unauthorized := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
	}))
	defer unauthorized.Close()
	provider.config.URL = unauthorized.URL
	provider.client = unauthorized.Client()

	if _, err := provider.List(context.Background(), "alist", "", "", ""); !errors.Is(err, ErrOperationFailed) {
		t.Fatalf("401 错误 = %v", err)
	}
	// 401 必须把旧 token 丢掉，否则后续请求会一直带失效凭据。
	if token := provider.loadToken(); token != "" {
		t.Fatalf("401 后 token 应清空，实际 %q", token)
	}
}

func TestAlistMkdirDeleteRenamePayloads(t *testing.T) {
	provider, fake := newAlistFake(t)
	ctx := context.Background()

	ok, err := provider.Mkdir(ctx, "alist", "剧集", "新目录")
	if err != nil || !ok {
		t.Fatalf("Mkdir = %v %v", ok, err)
	}
	path, body := fake.last()
	if path != "/api/fs/mkdir" || body["path"] != "/media/剧集/新目录" {
		t.Fatalf("Mkdir 请求 = %q %v", path, body)
	}

	ok, err = provider.Delete(ctx, "alist", "/剧集/第2集.mp4")
	if err != nil || !ok {
		t.Fatalf("Delete = %v %v", ok, err)
	}
	path, body = fake.last()
	if path != "/api/fs/remove" {
		t.Fatalf("Delete 端点 = %q", path)
	}
	// 删除要拆成 dir + names，而不是把整条路径丢给接口。
	if body["dir"] != "/media/剧集" {
		t.Fatalf("Delete dir = %v", body["dir"])
	}
	names, _ := body["names"].([]any)
	if len(names) != 1 || names[0] != "第2集.mp4" {
		t.Fatalf("Delete names = %v", body["names"])
	}

	ok, err = provider.Rename(ctx, "alist", "剧集/第2集.mp4", "第3集.mp4", false)
	if err != nil || !ok {
		t.Fatalf("Rename = %v %v", ok, err)
	}
	path, body = fake.last()
	if path != "/api/fs/rename" || body["name"] != "第3集.mp4" || body["path"] != "/media/剧集/第2集.mp4" {
		t.Fatalf("Rename 请求 = %q %v", path, body)
	}

	// 非法名字必须在本地就拒绝，不能把 "/" 送进远端。
	if _, err := provider.Mkdir(ctx, "alist", "", "a/b"); !errors.Is(err, ErrInvalidRequest) {
		t.Fatalf("含斜杠的目录名错误 = %v", err)
	}
	if _, err := provider.Rename(ctx, "alist", "a", "", false); !errors.Is(err, ErrInvalidRequest) {
		t.Fatalf("空新名字错误 = %v", err)
	}
	if _, err := provider.Delete(ctx, "alist", "  /  "); !errors.Is(err, ErrInvalidRequest) {
		t.Fatalf("删除根路径错误 = %v", err)
	}
}

func TestAlistUsageAndTransType(t *testing.T) {
	provider, _ := newAlistFake(t)
	usage, err := provider.Usage(context.Background(), "alist")
	if err != nil {
		t.Fatalf("Usage 失败: %v", err)
	}
	// Alist 没有统一容量接口：返回零值而不是编造数字。
	if usage.Total != 0 || usage.Available != 0 {
		t.Fatalf("Usage = %+v", usage)
	}
	transType, err := provider.TransType(context.Background(), "alist")
	if err != nil {
		t.Fatalf("TransType 失败: %v", err)
	}
	if transType["move"] != true || transType["copy"] != true || transType["link"] != false {
		t.Fatalf("TransType = %v", transType)
	}
	// 存储名不匹配时同样报错。
	if _, err := provider.Usage(context.Background(), "other"); !errors.Is(err, ErrUnsupportedStorage) {
		t.Fatalf("错误存储名 Usage = %v", err)
	}
}

func TestAlistRemotePath(t *testing.T) {
	provider, _ := newAlistFake(t)
	cases := map[string]string{
		"":            "/media",
		"/":           "/media",
		"剧集":          "/media/剧集",
		"/剧集/第1集.mp4": "/media/剧集/第1集.mp4",
		"  剧集  ":      "/media/剧集",
	}
	for requested, want := range cases {
		if got := provider.remotePath(requested); got != want {
			t.Fatalf("remotePath(%q) = %q，期望 %q", requested, got, want)
		}
	}
}

func TestAlistListStopsOnCancellation(t *testing.T) {
	provider, _ := newAlistFake(t)
	cancelled, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := provider.List(cancelled, "alist", "", "", ""); !errors.Is(err, context.Canceled) {
		t.Fatalf("已取消上下文错误 = %v", err)
	}
}
