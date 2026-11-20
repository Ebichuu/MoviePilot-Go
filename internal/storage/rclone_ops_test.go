package storage

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"sync"
	"testing"
)

// rcloneRequest 是假 rclone rc 服务端收到的一次调用。
type rcloneRequest struct {
	Path string
	Body map[string]any
	Auth bool
}

// rcloneFake 模拟 rclone RC 的 /operations/* 子集：默认成功、按端点登记失败状态码，
// 以便验证「先按文件操作、失败再按目录操作」的回退链。
type rcloneFake struct {
	mu        sync.Mutex
	requests  []rcloneRequest
	fail      map[string]int
	aboutBody string
	listBody  string
}

func (f *rcloneFake) handler(w http.ResponseWriter, r *http.Request) {
	payload, _ := io.ReadAll(r.Body)
	var body map[string]any
	_ = json.Unmarshal(payload, &body)
	_, hasAuth := r.Header["Authorization"]

	f.mu.Lock()
	f.requests = append(f.requests, rcloneRequest{Path: r.URL.Path, Body: body, Auth: hasAuth})
	status := f.fail[r.URL.Path]
	aboutBody := f.aboutBody
	listBody := f.listBody
	f.mu.Unlock()

	if status != 0 {
		w.WriteHeader(status)
		return
	}
	if r.URL.Path == "/operations/about" {
		if aboutBody != "" {
			_, _ = io.WriteString(w, aboutBody)
			return
		}
		_, _ = io.WriteString(w, `{"total":1099511627776,"free":549755813888}`)
		return
	}
	if r.URL.Path == "/operations/list" && listBody != "" {
		_, _ = io.WriteString(w, listBody)
		return
	}
	_, _ = io.WriteString(w, `{}`)
}

func (f *rcloneFake) recorded() []rcloneRequest {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]rcloneRequest(nil), f.requests...)
}

func (f *rcloneFake) reset() {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.requests = nil
}

func (f *rcloneFake) setFail(path string, status int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.fail[path] = status
}

func (f *rcloneFake) setAboutBody(body string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.aboutBody = body
}

func (f *rcloneFake) setListBody(body string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.listBody = body
}

func (f *rcloneFake) endpoints() []string {
	requests := f.recorded()
	result := make([]string, 0, len(requests))
	for _, request := range requests {
		result = append(result, request.Path)
	}
	return result
}

func newRcloneFake(t *testing.T, config RcloneConfig) (*RcloneProvider, *rcloneFake) {
	t.Helper()

	fake := &rcloneFake{fail: map[string]int{}}
	server := httptest.NewServer(http.HandlerFunc(fake.handler))
	t.Cleanup(server.Close)

	config.URL = server.URL
	if config.Name == "" {
		config.Name = "rclone"
	}
	if config.FileSystem == "" {
		config.FileSystem = "gdrive:media"
	}
	provider, err := NewRcloneProvider(config)
	if err != nil {
		t.Fatalf("构造 rclone 适配器失败: %v", err)
	}
	return provider, fake
}

// rcloneOperations 把五个写/读能力统一成同一种签名，守卫类断言才能逐项跑。
func rcloneOperations() map[string]func(*RcloneProvider, context.Context, string) error {
	return map[string]func(*RcloneProvider, context.Context, string) error{
		"mkdir": func(p *RcloneProvider, ctx context.Context, name string) error {
			_, err := p.Mkdir(ctx, name, "", "child")
			return err
		},
		"delete": func(p *RcloneProvider, ctx context.Context, name string) error {
			_, err := p.Delete(ctx, name, "dir/file.mkv")
			return err
		},
		"rename": func(p *RcloneProvider, ctx context.Context, name string) error {
			_, err := p.Rename(ctx, name, "dir/file.mkv", "new.mkv", false)
			return err
		},
		"usage": func(p *RcloneProvider, ctx context.Context, name string) error {
			_, err := p.Usage(ctx, name)
			return err
		},
		"transtype": func(p *RcloneProvider, ctx context.Context, name string) error {
			_, err := p.TransType(ctx, name)
			return err
		},
	}
}

func TestRcloneOperationGuards(t *testing.T) {
	t.Parallel()

	provider, fake := newRcloneFake(t, RcloneConfig{})
	for name, operation := range rcloneOperations() {
		t.Run(name+" 存储名不匹配", func(t *testing.T) {
			fake.reset()
			if err := operation(provider, context.Background(), "other"); !errors.Is(err, ErrUnsupportedStorage) {
				t.Fatalf("%s(存储名不匹配) error = %v, want ErrUnsupportedStorage", name, err)
			}
			// 本地守卫必须在发请求之前生效，否则会拿错误存储名去打远端。
			if got := fake.endpoints(); len(got) != 0 {
				t.Fatalf("%s(存储名不匹配) 发出了请求: %v", name, got)
			}
		})
		t.Run(name+" nil 上下文", func(t *testing.T) {
			fake.reset()
			if err := operation(provider, nil, "rclone"); !errors.Is(err, ErrInvalidRequest) {
				t.Fatalf("%s(nil ctx) error = %v, want ErrInvalidRequest", name, err)
			}
			if got := fake.endpoints(); len(got) != 0 {
				t.Fatalf("%s(nil ctx) 发出了请求: %v", name, got)
			}
		})
		t.Run(name+" 已取消上下文", func(t *testing.T) {
			fake.reset()
			cancelled, cancel := context.WithCancel(context.Background())
			cancel()
			if err := operation(provider, cancelled, "rclone"); !errors.Is(err, context.Canceled) {
				t.Fatalf("%s(已取消) error = %v, want context.Canceled", name, err)
			}
			if got := fake.endpoints(); len(got) != 0 {
				t.Fatalf("%s(已取消) 发出了请求: %v", name, got)
			}
		})
	}

	// nil 适配器不能崩溃，必须先于任何字段访问拒绝。
	var nilProvider *RcloneProvider
	for name, operation := range rcloneOperations() {
		if err := operation(nilProvider, context.Background(), "rclone"); !errors.Is(err, ErrUnsupportedStorage) {
			t.Fatalf("nil 适配器 %s error = %v, want ErrUnsupportedStorage", name, err)
		}
	}
	if nilProvider.Name() != "" {
		t.Fatalf("nil 适配器 Name() = %q", nilProvider.Name())
	}
}

func TestRcloneMkdirJoinsRemoteAndValidatesName(t *testing.T) {
	t.Parallel()

	provider, fake := newRcloneFake(t, RcloneConfig{})
	tests := []struct {
		name      string
		requested string
		dirName   string
		want      string
	}{
		{name: "根目录", requested: "", dirName: "新目录", want: "新目录"},
		{name: "子目录", requested: "剧集", dirName: "新目录", want: "剧集/新目录"},
		{name: "父目录尾斜杠归一", requested: "剧集/", dirName: "新 目录", want: "剧集/新 目录"},
		{name: "多级父目录", requested: "a/b", dirName: "c", want: "a/b/c"},
		{name: "名字两端空白裁剪", requested: "", dirName: "  新目录  ", want: "新目录"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			fake.reset()
			ok, err := provider.Mkdir(context.Background(), "rclone", tt.requested, tt.dirName)
			if err != nil || !ok {
				t.Fatalf("Mkdir(%q, %q) = %v %v", tt.requested, tt.dirName, ok, err)
			}
			requests := fake.recorded()
			if len(requests) != 1 || requests[0].Path != "/operations/mkdir" {
				t.Fatalf("请求 = %v", fake.endpoints())
			}
			if requests[0].Body["fs"] != "gdrive:media" {
				t.Fatalf("fs = %v", requests[0].Body["fs"])
			}
			if requests[0].Body["remote"] != tt.want {
				t.Fatalf("remote = %v, want %q", requests[0].Body["remote"], tt.want)
			}
		})
	}

	// 名字里的分隔符必须在本地拒绝：它会把 mkdir 变成对另一层目录的写入。
	for _, dirName := range []string{"", "   ", "a/b", `a\b`} {
		fake.reset()
		ok, err := provider.Mkdir(context.Background(), "rclone", "", dirName)
		if !errors.Is(err, ErrInvalidRequest) || ok {
			t.Fatalf("Mkdir(名字=%q) = %v %v, want ErrInvalidRequest", dirName, ok, err)
		}
		if got := fake.endpoints(); len(got) != 0 {
			t.Fatalf("Mkdir(名字=%q) 发出了请求: %v", dirName, got)
		}
	}
}

func TestRcloneMkdirMapsUpstreamFailure(t *testing.T) {
	t.Parallel()

	provider, fake := newRcloneFake(t, RcloneConfig{})
	fake.setFail("/operations/mkdir", http.StatusInternalServerError)
	ok, err := provider.Mkdir(context.Background(), "rclone", "", "新目录")
	if ok || !errors.Is(err, ErrOperationFailed) {
		t.Fatalf("Mkdir(上游 500) = %v %v, want ErrOperationFailed", ok, err)
	}
}

func TestRcloneDeleteFallsBackToPurge(t *testing.T) {
	t.Parallel()

	provider, fake := newRcloneFake(t, RcloneConfig{})
	ctx := context.Background()

	// 文件删除成功时不应再按目录清理。
	ok, err := provider.Delete(ctx, "rclone", "/剧集/第2集.mp4/")
	if err != nil || !ok {
		t.Fatalf("Delete() = %v %v", ok, err)
	}
	requests := fake.recorded()
	if endpoints := fake.endpoints(); !reflect.DeepEqual(endpoints, []string{"/operations/deletefile"}) {
		t.Fatalf("端点 = %v", endpoints)
	}
	// 远端路径两端斜杠被裁剪，否则 rclone 会把它当成绝对路径之外的另一处。
	if requests[0].Body["remote"] != "剧集/第2集.mp4" {
		t.Fatalf("remote = %v", requests[0].Body["remote"])
	}
	if requests[0].Body["fs"] != "gdrive:media" {
		t.Fatalf("fs = %v", requests[0].Body["fs"])
	}

	// 文件删除失败（目标其实是目录）时，必须改用 purge 递归清理。
	fake.reset()
	fake.setFail("/operations/deletefile", http.StatusInternalServerError)
	ok, err = provider.Delete(ctx, "rclone", "剧集")
	if err != nil || !ok {
		t.Fatalf("Delete(回退) = %v %v", ok, err)
	}
	if endpoints := fake.endpoints(); !reflect.DeepEqual(endpoints, []string{"/operations/deletefile", "/operations/purge"}) {
		t.Fatalf("回退端点 = %v", endpoints)
	}
	requests = fake.recorded()
	if requests[1].Body["remote"] != "剧集" {
		t.Fatalf("purge remote = %v", requests[1].Body["remote"])
	}

	// 两条路径都失败才是真失败，不能谎报成功。
	fake.reset()
	fake.setFail("/operations/purge", http.StatusInternalServerError)
	ok, err = provider.Delete(ctx, "rclone", "剧集")
	if ok || !errors.Is(err, ErrOperationFailed) {
		t.Fatalf("Delete(全失败) = %v %v, want ErrOperationFailed", ok, err)
	}

	// 空路径与纯分隔符会删掉远端根目录，必须在本地拒绝。
	for _, requested := range []string{"", "/", "  /  ", "   "} {
		fake.reset()
		if ok, err := provider.Delete(ctx, "rclone", requested); !errors.Is(err, ErrInvalidRequest) || ok {
			t.Fatalf("Delete(%q) = %v %v, want ErrInvalidRequest", requested, ok, err)
		}
		if got := fake.endpoints(); len(got) != 0 {
			t.Fatalf("Delete(%q) 发出了请求: %v", requested, got)
		}
	}
}

func TestRcloneRenameMovesFileOrDirectory(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name      string
		requested string
		newName   string
		recursive bool
		fail      string
		wantCalls []string
		wantSrc   string
		wantDst   string
	}{
		{
			name: "文件走 movefile", requested: "剧集/第2集.mp4", newName: "第3集.mp4",
			wantCalls: []string{"/operations/movefile"},
			wantSrc:   "剧集/第2集.mp4", wantDst: "剧集/第3集.mp4",
		},
		{
			name: "文件失败回退 move", requested: "剧集/第2集.mp4", newName: "第3集.mp4",
			fail:      "/operations/movefile",
			wantCalls: []string{"/operations/movefile", "/operations/move"},
			wantSrc:   "剧集/第2集.mp4", wantDst: "剧集/第3集.mp4",
		},
		{
			name: "根级文件目标同级", requested: "Movie.mkv", newName: "Movie2.mkv",
			wantCalls: []string{"/operations/movefile"},
			wantSrc:   "Movie.mkv", wantDst: "Movie2.mkv",
		},
		{
			name: "递归重命名走 move", requested: "剧集", newName: "新剧集", recursive: true,
			wantCalls: []string{"/operations/move"},
			wantSrc:   "剧集", wantDst: "新剧集",
		},
		{
			name: "递归失败回退 movefile", requested: "剧集", newName: "新剧集", recursive: true,
			fail:      "/operations/move",
			wantCalls: []string{"/operations/move", "/operations/movefile"},
			wantSrc:   "剧集", wantDst: "新剧集",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			provider, fake := newRcloneFake(t, RcloneConfig{})
			if tt.fail != "" {
				fake.setFail(tt.fail, http.StatusNotFound)
			}
			ok, err := provider.Rename(context.Background(), "rclone", tt.requested, tt.newName, tt.recursive)
			if err != nil || !ok {
				t.Fatalf("Rename() = %v %v", ok, err)
			}
			if endpoints := fake.endpoints(); !reflect.DeepEqual(endpoints, tt.wantCalls) {
				t.Fatalf("端点 = %v, want %v", endpoints, tt.wantCalls)
			}
			for _, request := range fake.recorded() {
				if request.Body["srcFs"] != "gdrive:media" || request.Body["dstFs"] != "gdrive:media" {
					t.Fatalf("Fs 参数 = %v", request.Body)
				}
				if request.Body["srcRemote"] != tt.wantSrc || request.Body["dstRemote"] != tt.wantDst {
					t.Fatalf("Remote 参数 = %v / %v, want %q / %q",
						request.Body["srcRemote"], request.Body["dstRemote"], tt.wantSrc, tt.wantDst)
				}
			}
		})
	}

	// 新旧名字都必须经过本地校验，不能把斜杠拼进远端路径。
	provider, fake := newRcloneFake(t, RcloneConfig{})
	for _, newName := range []string{"", "   ", "a/b", `a\b`} {
		if ok, err := provider.Rename(context.Background(), "rclone", "a.mkv", newName, false); !errors.Is(err, ErrInvalidRequest) || ok {
			t.Fatalf("Rename(新名=%q) = %v %v, want ErrInvalidRequest", newName, ok, err)
		}
	}
	for _, requested := range []string{"", "/", "  /  "} {
		if ok, err := provider.Rename(context.Background(), "rclone", requested, "b.mkv", false); !errors.Is(err, ErrInvalidRequest) || ok {
			t.Fatalf("Rename(源=%q) = %v %v, want ErrInvalidRequest", requested, ok, err)
		}
	}
	if got := fake.endpoints(); len(got) != 0 {
		t.Fatalf("非法参数发出了请求: %v", got)
	}

	// 两条路径都失败时不能返回成功（调用方会据此更新整理记录）。
	fake.setFail("/operations/movefile", http.StatusInternalServerError)
	fake.setFail("/operations/move", http.StatusInternalServerError)
	if ok, err := provider.Rename(context.Background(), "rclone", "a.mkv", "b.mkv", false); ok || !errors.Is(err, ErrOperationFailed) {
		t.Fatalf("Rename(全失败) = %v %v, want ErrOperationFailed", ok, err)
	}
}

func TestRcloneUsageReadsRemoteCapacity(t *testing.T) {
	t.Parallel()

	provider, fake := newRcloneFake(t, RcloneConfig{})
	usage, err := provider.Usage(context.Background(), "rclone")
	if err != nil {
		t.Fatalf("Usage() error = %v", err)
	}
	want := Usage{Total: 1099511627776, Available: 549755813888}
	if usage != want {
		t.Fatalf("Usage() = %+v, want %+v", usage, want)
	}
	requests := fake.recorded()
	if len(requests) != 1 || requests[0].Path != "/operations/about" {
		t.Fatalf("端点 = %v", fake.endpoints())
	}
	if requests[0].Body["fs"] != "gdrive:media" {
		t.Fatalf("fs = %v", requests[0].Body["fs"])
	}

	fake.setFail("/operations/about", http.StatusServiceUnavailable)
	if _, err := provider.Usage(context.Background(), "rclone"); !errors.Is(err, ErrOperationFailed) {
		t.Fatalf("Usage(上游 503) error = %v, want ErrOperationFailed", err)
	}

	// 上游回了 2xx 但正文读不懂，必须报错，不能把 0/0 当成真实容量。
	fake.setAboutBody("total: nope")
	if _, err := provider.Usage(context.Background(), "rclone"); !errors.Is(err, ErrOperationFailed) {
		t.Fatalf("Usage(非法 JSON) error = %v, want ErrOperationFailed", err)
	}
}

func TestRcloneTransTypeDeclaresNoLink(t *testing.T) {
	t.Parallel()

	provider, fake := newRcloneFake(t, RcloneConfig{})
	transType, err := provider.TransType(context.Background(), "rclone")
	if err != nil {
		t.Fatalf("TransType() error = %v", err)
	}
	want := map[string]any{"move": true, "copy": true, "link": false}
	if !reflect.DeepEqual(transType, want) {
		t.Fatalf("TransType() = %v, want %v", transType, want)
	}
	// 这是纯能力声明，不该产生网络请求。
	if got := fake.endpoints(); len(got) != 0 {
		t.Fatalf("TransType() 发出了请求: %v", got)
	}
}

func TestRcloneCallSendsBasicAuthOnlyWhenConfigured(t *testing.T) {
	t.Parallel()

	provider, fake := newRcloneFake(t, RcloneConfig{})
	if _, err := provider.Mkdir(context.Background(), "rclone", "", "dir"); err != nil {
		t.Fatalf("Mkdir() error = %v", err)
	}
	if requests := fake.recorded(); len(requests) != 1 || requests[0].Auth {
		t.Fatalf("无凭据时不应发送 Authorization 头: %+v", requests)
	}

	withAuth, authFake := newRcloneFake(t, RcloneConfig{Username: "user", Password: "pass"})
	if _, err := withAuth.Mkdir(context.Background(), "rclone", "", "dir"); err != nil {
		t.Fatalf("Mkdir() error = %v", err)
	}
	if requests := authFake.recorded(); len(requests) != 1 || !requests[0].Auth {
		t.Fatalf("有凭据时必须发送 Authorization 头: %+v", requests)
	}
}
