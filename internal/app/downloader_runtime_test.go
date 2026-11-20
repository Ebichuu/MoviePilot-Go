package app

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/moviepilot-go/moviepilot-go/internal/downloadconfig"
	"github.com/moviepilot-go/moviepilot-go/internal/downloader"
	"github.com/moviepilot-go/moviepilot-go/internal/downloadflow"
)

func TestLoadDownloadRuntime(t *testing.T) {
	t.Parallel()
	const enabledID = "12e2fe2b-2ed7-4c6a-b5ba-1c1c0f7a3ad8"
	tests := []struct {
		name       string
		store      downloaderRuntimeTestStore
		resolveID  string
		wantFound  bool
		wantErr    bool
		wantLogOut string
	}{
		{name: "未配置时返回空注册表", store: downloaderRuntimeTestStore{}, resolveID: enabledID},
		{name: "禁用的损坏 qBittorrent 配置被忽略", store: downloaderRuntimeTestStore{found: true, settings: []downloadconfig.Downloader{{
			ID: enabledID, Name: "disabled", Type: "qbittorrent", Config: map[string]any{"password": []string{"password-secret"}},
		}}}, resolveID: enabledID},
		{name: "只装载启用的 qBittorrent", store: downloaderRuntimeTestStore{found: true, settings: []downloadconfig.Downloader{
			{ID: enabledID, Name: "qB", Type: "qbittorrent", Enabled: true, Config: map[string]any{"host": "http://127.0.0.1:8080", "username": "alice", "password": "password-secret"}},
			{ID: "58b08ab7-c5c1-43da-82af-16d3a3f4912e", Name: "disabled", Type: "qbittorrent", Config: map[string]any{"host": "http://127.0.0.1:8081"}},
		}}, resolveID: enabledID, wantFound: true, wantLogOut: "已装载下载器"},
		{name: "未支持类型安全跳过", store: downloaderRuntimeTestStore{found: true, settings: []downloadconfig.Downloader{{ID: enabledID, Name: "Deluge", Type: "deluge", Enabled: true, Config: map[string]any{"host": "http://127.0.0.1:9091", "password": "password-secret"}}}}, resolveID: enabledID, wantLogOut: "跳过尚未支持的下载器"},
		{name: "损坏认证配置拒绝启动", store: downloaderRuntimeTestStore{found: true, settings: []downloadconfig.Downloader{{ID: enabledID, Name: "qB", Type: "qbittorrent", Enabled: true, Config: map[string]any{"host": "http://127.0.0.1:8080", "password": []string{"password-secret"}}}}}, resolveID: enabledID, wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			var logs bytes.Buffer
			runtime, err := loadDownloadRuntime(t.Context(), &tt.store, 5*time.Second, slog.New(slog.NewTextHandler(&logs, nil)))
			if tt.wantErr {
				if err == nil {
					t.Fatal("loadDownloadRuntime() error = nil")
				}
				if strings.Contains(err.Error(), "password-secret") || strings.Contains(logs.String(), "password-secret") {
					t.Fatal("错误或日志泄露下载器秘密")
				}
				return
			}
			if err != nil {
				t.Fatalf("loadDownloadRuntime() error = %v", err)
			}
			defer runtime.CloseIdleConnections()
			_, resolveErr := runtime.submitters.ResolveSubmitter(tt.resolveID)
			if tt.wantFound && resolveErr != nil {
				t.Fatalf("ResolveSubmitter() error = %v", resolveErr)
			}
			if !tt.wantFound && !errors.Is(resolveErr, downloadflow.ErrDownloaderUnavailable) {
				t.Fatalf("ResolveSubmitter() error = %v", resolveErr)
			}
			if tt.wantLogOut != "" && !strings.Contains(logs.String(), tt.wantLogOut) {
				t.Fatalf("日志 %q 不包含 %q", logs.String(), tt.wantLogOut)
			}
			if strings.Contains(logs.String(), "password-secret") {
				t.Fatal("结构化日志泄露下载器秘密")
			}
		})
	}
}

func TestLoadDownloadRuntimeRejectsStoreFailure(t *testing.T) {
	t.Parallel()
	store := &downloaderRuntimeTestStore{err: errors.New("database password-secret")}
	_, err := loadDownloadRuntime(t.Context(), store, time.Second, nil)
	if err == nil {
		t.Fatal("loadDownloadRuntime() error = nil")
	}
	if strings.Contains(err.Error(), "password-secret") {
		t.Fatal("存储错误泄露底层秘密")
	}
}

func TestDownloadRuntimeReloadReplacesSnapshot(t *testing.T) {
	t.Parallel()
	const (
		oldID = "12e2fe2b-2ed7-4c6a-b5ba-1c1c0f7a3ad8"
		newID = "58b08ab7-c5c1-43da-82af-16d3a3f4912e"
	)
	store := &downloaderRuntimeTestStore{found: true, settings: []downloadconfig.Downloader{
		{ID: oldID, Name: "old", Type: "qbittorrent", Enabled: true, Config: map[string]any{"host": "http://127.0.0.1:8080"}},
	}}
	runtime, err := loadDownloadRuntime(t.Context(), store, time.Second, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer runtime.CloseIdleConnections()

	store.settings = []downloadconfig.Downloader{
		{ID: newID, Name: "new", Type: "qbittorrent", Enabled: true, Config: map[string]any{"host": "http://127.0.0.1:8081"}},
	}
	if err := runtime.Reload(t.Context(), store, time.Second, nil); err != nil {
		t.Fatalf("Reload() error = %v", err)
	}
	if _, err := runtime.submitters.ResolveSubmitter(oldID); !errors.Is(err, downloadflow.ErrDownloaderUnavailable) {
		t.Fatalf("旧下载器仍然可用: %v", err)
	}
	if _, err := runtime.submitters.ResolveSubmitter(newID); err != nil {
		t.Fatalf("新下载器不可用: %v", err)
	}
}

func TestDownloadRuntimeReloadFailureKeepsSnapshot(t *testing.T) {
	t.Parallel()
	const stableID = "12e2fe2b-2ed7-4c6a-b5ba-1c1c0f7a3ad8"
	store := &downloaderRuntimeTestStore{found: true, settings: []downloadconfig.Downloader{
		{ID: stableID, Name: "stable", Type: "qbittorrent", Enabled: true, Config: map[string]any{"host": "http://127.0.0.1:8080"}},
	}}
	runtime, err := loadDownloadRuntime(t.Context(), store, time.Second, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer runtime.CloseIdleConnections()

	store.err = errors.New("database unavailable")
	if err := runtime.Reload(t.Context(), store, time.Second, nil); err == nil {
		t.Fatal("Reload() error = nil")
	}
	if _, err := runtime.submitters.ResolveSubmitter(stableID); err != nil {
		t.Fatalf("失败重载破坏了旧快照: %v", err)
	}
}

func TestDownloadRuntimeReloadSerializesSnapshotLoads(t *testing.T) {
	t.Parallel()
	const (
		initialID = "12e2fe2b-2ed7-4c6a-b5ba-1c1c0f7a3ad8"
		firstID   = "58b08ab7-c5c1-43da-82af-16d3a3f4912e"
		secondID  = "4b981b3b-0d3c-4919-868d-2dedf4693c80"
	)
	runtime, err := loadDownloadRuntime(context.Background(), &downloaderRuntimeTestStore{
		found: true,
		settings: []downloadconfig.Downloader{{
			ID: initialID, Name: "initial", Type: "qbittorrent", Enabled: true,
			Config: map[string]any{"host": "http://127.0.0.1:8080"},
		}},
	}, time.Second, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer runtime.CloseIdleConnections()

	store := &sequencedDownloaderRuntimeStore{
		firstStarted:  make(chan struct{}),
		secondStarted: make(chan struct{}),
		releaseFirst:  make(chan struct{}),
		settings: [][]downloadconfig.Downloader{
			{{ID: firstID, Name: "first", Type: "qbittorrent", Enabled: true, Config: map[string]any{"host": "http://127.0.0.1:8081"}}},
			{{ID: secondID, Name: "second", Type: "qbittorrent", Enabled: true, Config: map[string]any{"host": "http://127.0.0.1:8082"}}},
		},
	}
	firstDone := make(chan error, 1)
	go func() {
		firstDone <- runtime.Reload(context.Background(), store, time.Second, nil)
	}()
	<-store.firstStarted

	secondDone := make(chan error, 1)
	go func() {
		secondDone <- runtime.Reload(context.Background(), store, time.Second, nil)
	}()
	secondStartedBeforeFirstRelease := false
	select {
	case <-store.secondStarted:
		secondStartedBeforeFirstRelease = true
	case <-time.After(100 * time.Millisecond):
	}

	close(store.releaseFirst)
	if err := <-firstDone; err != nil {
		t.Fatalf("first Reload() error = %v", err)
	}
	if !secondStartedBeforeFirstRelease {
		<-store.secondStarted
	}
	if err := <-secondDone; err != nil {
		t.Fatalf("second Reload() error = %v", err)
	}
	if secondStartedBeforeFirstRelease {
		t.Fatal("并发 Reload 在前一快照提交前开始了后续加载")
	}
	if _, err := runtime.submitters.ResolveSubmitter(firstID); !errors.Is(err, downloadflow.ErrDownloaderUnavailable) {
		t.Fatalf("较早快照仍然可用: %v", err)
	}
	if _, err := runtime.submitters.ResolveSubmitter(secondID); err != nil {
		t.Fatalf("最终快照不是最后一次加载的配置: %v", err)
	}
}

func TestDownloadRuntimeListDownloadingSelectsNamedClient(t *testing.T) {
	t.Parallel()
	first := &downloadClientTestDouble{tasks: []downloader.Task{{Hash: "FIRST"}}}
	second := &downloadClientTestDouble{tasks: []downloader.Task{{Hash: "SECOND"}}}
	runtime := &downloadRuntime{clients: []downloadClientEntry{
		{name: "客厅 qB", client: first},
		{name: "Archive", client: second},
	}}

	tasks, err := runtime.ListDownloading(t.Context(), "  archive ")
	if err != nil {
		t.Fatalf("ListDownloading() error = %v", err)
	}
	if len(tasks) != 1 || tasks[0].Downloader != "Archive" || tasks[0].Task.Hash != "SECOND" {
		t.Fatalf("ListDownloading() = %#v", tasks)
	}
	if first.calls.Load() != 0 || second.calls.Load() != 1 {
		t.Fatalf("查询次数 first=%d second=%d", first.calls.Load(), second.calls.Load())
	}
}

func TestDownloadRuntimeListDownloadingBoundsConcurrency(t *testing.T) {
	t.Parallel()
	const clientCount = 5
	started := make(chan struct{}, clientCount)
	release := make(chan struct{})
	var active atomic.Int32
	var peak atomic.Int32
	entries := make([]downloadClientEntry, 0, clientCount)
	for index := 0; index < clientCount; index++ {
		client := &downloadClientTestDouble{list: func(ctx context.Context) ([]downloader.Task, error) {
			current := active.Add(1)
			for {
				previous := peak.Load()
				if current <= previous || peak.CompareAndSwap(previous, current) {
					break
				}
			}
			started <- struct{}{}
			select {
			case <-release:
				active.Add(-1)
				return nil, nil
			case <-ctx.Done():
				active.Add(-1)
				return nil, ctx.Err()
			}
		}}
		entries = append(entries, downloadClientEntry{name: fmt.Sprintf("qb-%d", index), client: client})
	}
	runtime := &downloadRuntime{clients: entries}
	done := make(chan error, 1)
	go func() {
		_, err := runtime.ListDownloading(context.Background(), "")
		done <- err
	}()

	for index := 0; index < 4; index++ {
		<-started
	}
	select {
	case <-started:
		t.Fatal("第五个下载器在并发槽释放前启动")
	case <-time.After(100 * time.Millisecond):
	}
	close(release)
	if err := <-done; err != nil {
		t.Fatalf("ListDownloading() error = %v", err)
	}
	if peak.Load() != 4 {
		t.Fatalf("并发峰值 = %d，want 4", peak.Load())
	}
}

func TestDownloadRuntimeListDownloadingCapsAggregate(t *testing.T) {
	t.Parallel()
	firstTasks := make([]downloader.Task, 1001)
	first := &downloadClientTestDouble{tasks: firstTasks}
	second := &downloadClientTestDouble{tasks: []downloader.Task{{Hash: "SECOND"}}}
	runtime := &downloadRuntime{clients: []downloadClientEntry{
		{name: "first", client: first},
		{name: "second", client: second},
	}}

	tasks, err := runtime.ListDownloading(t.Context(), "")
	if err != nil {
		t.Fatalf("ListDownloading() error = %v", err)
	}
	if len(tasks) != 1000 {
		t.Fatalf("任务数 = %d，want 1000", len(tasks))
	}
}

func TestDownloadRuntimeListDownloadingSanitizesClientFailure(t *testing.T) {
	t.Parallel()
	runtime := &downloadRuntime{clients: []downloadClientEntry{{
		name: "qb", client: &downloadClientTestDouble{err: errors.New("host password-secret")},
	}}}

	_, err := runtime.ListDownloading(t.Context(), "qb")
	if err == nil {
		t.Fatal("ListDownloading() error = nil")
	}
	if strings.Contains(err.Error(), "password-secret") {
		t.Fatalf("ListDownloading() 泄漏底层错误: %v", err)
	}
}

type downloadClientTestDouble struct {
	tasks  []downloader.Task
	err    error
	list   func(context.Context) ([]downloader.Task, error)
	calls  atomic.Int32
	closed atomic.Int32
}

func (c *downloadClientTestDouble) ListDownloading(ctx context.Context) ([]downloader.Task, error) {
	c.calls.Add(1)
	if c.list != nil {
		return c.list(ctx)
	}
	return c.tasks, c.err
}

func (c *downloadClientTestDouble) CloseIdleConnections() {
	c.closed.Add(1)
}

type downloaderRuntimeTestStore struct {
	settings []downloadconfig.Downloader
	found    bool
	err      error
}

func (s *downloaderRuntimeTestStore) LoadDownloaders(context.Context) ([]downloadconfig.Downloader, bool, error) {
	return s.settings, s.found, s.err
}

type sequencedDownloaderRuntimeStore struct {
	mu            sync.Mutex
	calls         int
	firstStarted  chan struct{}
	secondStarted chan struct{}
	releaseFirst  chan struct{}
	settings      [][]downloadconfig.Downloader
}

func (s *sequencedDownloaderRuntimeStore) LoadDownloaders(context.Context) ([]downloadconfig.Downloader, bool, error) {
	s.mu.Lock()
	call := s.calls
	s.calls++
	s.mu.Unlock()
	if call == 0 {
		close(s.firstStarted)
		<-s.releaseFirst
	} else if call == 1 {
		close(s.secondStarted)
	}
	return s.settings[call], true, nil
}
