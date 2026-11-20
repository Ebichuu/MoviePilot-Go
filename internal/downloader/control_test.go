package downloader

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

const testTaskHash = "0123456789abcdef0123456789abcdef01234567"

type transmissionFake struct {
	mu       sync.Mutex
	methods  []string
	args     []map[string]any
	withSess []bool
	result   string
	status   int
	torrents []any
}

func (f *transmissionFake) handler(w http.ResponseWriter, r *http.Request) {
	if r.Header.Get(transmissionSessionHeader) == "" {
		w.Header().Set(transmissionSessionHeader, "sess-1")
		w.WriteHeader(http.StatusConflict)
		return
	}
	payload, _ := io.ReadAll(r.Body)
	var request struct {
		Method    string         `json:"method"`
		Arguments map[string]any `json:"arguments"`
	}
	_ = json.Unmarshal(payload, &request)
	f.mu.Lock()
	f.methods = append(f.methods, request.Method)
	f.args = append(f.args, request.Arguments)
	f.withSess = append(f.withSess, true)
	result, status, torrents := f.result, f.status, f.torrents
	f.mu.Unlock()

	if status != 0 {
		w.WriteHeader(status)
		return
	}
	if result == "" {
		result = "success"
	}
	arguments := map[string]any{}
	if request.Method == "torrent-get" {
		arguments["torrents"] = torrents
	}
	_ = json.NewEncoder(w).Encode(map[string]any{"result": result, "arguments": arguments})
}

func (f *transmissionFake) lastMethod(t *testing.T) (string, map[string]any) {
	t.Helper()
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.methods) == 0 {
		return "", nil
	}
	return f.methods[len(f.methods)-1], f.args[len(f.args)-1]
}

func (f *transmissionFake) count() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.methods)
}

func newTransmissionFake(t *testing.T) (*Transmission, *transmissionFake) {
	t.Helper()
	fake := &transmissionFake{}
	server := httptest.NewServer(http.HandlerFunc(fake.handler))
	t.Cleanup(server.Close)
	return &Transmission{
		client: server.Client(), baseURL: server.URL,
		queryPermit: make(chan struct{}, 1),
	}, fake
}

func TestControlTaskRejectsInvalidHashForEveryAdapter(t *testing.T) {
	badHashes := []string{"", "  ", "not-a-hash", "abcd"}

	qbServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	}))
	defer qbServer.Close()
	qb := newQBAdapter(t, qbServer)
	transmission, _ := newTransmissionFake(t)
	rtorrent := &RTorrent{client: qbServer.Client(), endpoint: qbServer.URL + "/RPC2", queryPermit: make(chan struct{}, 1)}

	for _, hash := range badHashes {
		if _, _, err := qb.InspectControlTask(context.Background(), hash); err == nil {
			t.Fatalf("qBittorrent 应拒绝非法 hash %q", hash)
		}
		if err := qb.ControlTask(context.Background(), hash, TaskControlStart); err == nil {
			t.Fatalf("qBittorrent 应拒绝非法 hash %q", hash)
		}
		if _, _, err := transmission.InspectControlTask(context.Background(), hash); err == nil {
			t.Fatalf("Transmission 应拒绝非法 hash %q", hash)
		}
		if err := transmission.ControlTask(context.Background(), hash, TaskControlStart); err == nil {
			t.Fatalf("Transmission 应拒绝非法 hash %q", hash)
		}
		if _, _, err := rtorrent.InspectControlTask(context.Background(), hash); err == nil {
			t.Fatalf("rTorrent 应拒绝非法 hash %q", hash)
		}
		if err := rtorrent.ControlTask(context.Background(), hash, TaskControlStart); err == nil {
			t.Fatalf("rTorrent 应拒绝非法 hash %q", hash)
		}
	}
}

func TestQBittorrentControlTaskGuards(t *testing.T) {
	// 未初始化的适配器必须在做任何 I/O 前拒绝。
	var nilAdapter *QBittorrent
	if _, _, err := nilAdapter.InspectControlTask(context.Background(), testTaskHash); err == nil {
		t.Fatal("nil 适配器应拒绝查询")
	}
	if err := nilAdapter.ControlTask(context.Background(), testTaskHash, TaskControlStart); err == nil {
		t.Fatal("nil 适配器应拒绝控制")
	}
	if _, _, err := nilAdapter.InspectControlTask(nil, testTaskHash); err == nil {
		t.Fatal("nil 上下文应拒绝查询")
	}
	// 未知动作：合法 hash 也必须在任何 HTTP 请求前拒绝。
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		t.Error("未知动作不应发起 HTTP 请求")
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer server.Close()
	adapter := newQBAdapter(t, server)
	if err := adapter.ControlTask(context.Background(), testTaskHash, TaskControlAction("restart")); err == nil {
		t.Fatal("未知动作应报错")
	}
}

func TestTransmissionControlTaskDispatchesAndReusesSession(t *testing.T) {
	transmission, fake := newTransmissionFake(t)
	ctx := context.Background()

	if err := transmission.ControlTask(ctx, testTaskHash, TaskControlStart); err != nil {
		t.Fatalf("开始失败: %v", err)
	}
	method, arguments := fake.lastMethod(t)
	if method != "torrent-start" {
		t.Fatalf("开始方法 = %q", method)
	}
	ids, _ := arguments["ids"].([]any)
	if len(ids) != 1 || !strings.EqualFold(ids[0].(string), testTaskHash) {
		t.Fatalf("ids = %v", arguments["ids"])
	}

	if err := transmission.ControlTask(ctx, testTaskHash, TaskControlStop); err != nil {
		t.Fatalf("暂停失败: %v", err)
	}
	if method, _ := fake.lastMethod(t); method != "torrent-stop" {
		t.Fatalf("暂停方法 = %q", method)
	}

	if err := transmission.ControlTask(ctx, testTaskHash, TaskControlDelete); err != nil {
		t.Fatalf("删除失败: %v", err)
	}
	method, arguments = fake.lastMethod(t)
	if method != "torrent-remove" {
		t.Fatalf("删除方法 = %q", method)
	}
	// 删除必须同时删本地数据，否则用户界面「删除」会留下垃圾文件。
	if arguments["delete-local-data"] != true {
		t.Fatalf("删除参数 = %v", arguments)
	}

	// 会话 ID 在第一次 409 之后被记住，后续请求都带上了。
	fake.mu.Lock()
	sessions := len(fake.withSess)
	fake.mu.Unlock()
	if sessions != 3 {
		t.Fatalf("带会话头的请求数 = %d", sessions)
	}
	if transmission.loadSessionID() != "sess-1" {
		t.Fatalf("会话 ID = %q", transmission.loadSessionID())
	}

	// 未知动作在本地拒绝，不产生多余请求。
	before := fake.count()
	if err := transmission.ControlTask(ctx, testTaskHash, TaskControlAction("restart")); err == nil {
		t.Fatal("未知动作应报错")
	}
	if fake.count() != before {
		t.Fatalf("未知动作不应发起请求，实际多了 %d 次", fake.count()-before)
	}
}

func TestTransmissionInspectControlTaskMatchesHash(t *testing.T) {
	transmission, fake := newTransmissionFake(t)
	label := "mp-0123456789abcdef0123456789abcdef"
	fake.torrents = []any{
		map[string]any{"hashString": strings.ToUpper(testTaskHash), "labels": []string{label}, "name": "影片",
			"status": 6, "percentDone": 1.0},
		map[string]any{"hashString": "ffffffffffffffffffffffffffffffffffffffff", "labels": []string{}, "name": "别的"},
	}

	task, found, err := transmission.InspectControlTask(context.Background(), testTaskHash)
	if err != nil || !found {
		t.Fatalf("查询 = %v %v", found, err)
	}
	if !strings.EqualFold(task.Hash, testTaskHash) {
		t.Fatalf("hash = %q", task.Hash)
	}
	// hash 比较不区分大小写，但标签要按本项目的确定性标签识别。
	if task.Label != label {
		t.Fatalf("标签 = %q", task.Label)
	}
	if task.State == "" {
		t.Fatal("状态不应为空")
	}

	// 库里没有该 hash 时返回 found=false 而不是错误。
	if _, found, err := transmission.InspectControlTask(context.Background(), "1111111111111111111111111111111111111111"); err != nil || found {
		t.Fatalf("未命中 = %v %v", found, err)
	}
}

func TestTransmissionControlFailuresAreTranslated(t *testing.T) {
	transmission, fake := newTransmissionFake(t)
	fake.status = http.StatusInternalServerError
	if err := transmission.ControlTask(context.Background(), testTaskHash, TaskControlStart); err == nil {
		t.Fatal("HTTP 500 应报错")
	}

	transmission, fake = newTransmissionFake(t)
	fake.result = "torrent not found"
	if err := transmission.ControlTask(context.Background(), testTaskHash, TaskControlStop); err == nil {
		t.Fatal("result 非 success 应报错")
	}
}

// rTorrent 的删除语义：先取 base_path，`d.erase` 之后再删宿主上的文件。
func TestRTorrentControlTaskDeleteRemovesFiles(t *testing.T) {
	mediaDir := filepath.Join(t.TempDir(), "影片目录")
	if err := os.MkdirAll(mediaDir, 0o755); err != nil {
		t.Fatalf("准备目录失败: %v", err)
	}
	if err := os.WriteFile(filepath.Join(mediaDir, "a.mkv"), []byte("x"), 0o644); err != nil {
		t.Fatalf("准备文件失败: %v", err)
	}

	var methods []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		payload, _ := io.ReadAll(r.Body)
		body := string(payload)
		switch {
		case strings.Contains(body, "d.base_path"):
			methods = append(methods, "d.base_path")
			writeXMLRPCString(w, mediaDir)
		case strings.Contains(body, "d.erase"):
			methods = append(methods, "d.erase")
			writeXMLRPCString(w, "")
		default:
			methods = append(methods, "other")
			writeXMLRPCString(w, "")
		}
	}))
	defer server.Close()

	rtorrent := &RTorrent{
		client: server.Client(), endpoint: server.URL + "/RPC2",
		queryPermit: make(chan struct{}, 1),
	}
	if err := rtorrent.ControlTask(context.Background(), testTaskHash, TaskControlDelete); err != nil {
		t.Fatalf("删除失败: %v", err)
	}
	if _, err := os.Stat(mediaDir); !os.IsNotExist(err) {
		t.Fatalf("目录应被删除，stat = %v", err)
	}
	if len(methods) < 2 || methods[0] != "d.base_path" || methods[1] != "d.erase" {
		t.Fatalf("调用顺序 = %v", methods)
	}
}

func TestRTorrentControlTaskUnknownActionAndGuards(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		t.Error("未知动作不应发起请求")
		writeXMLRPCString(w, "")
	}))
	defer server.Close()
	rtorrent := &RTorrent{client: server.Client(), endpoint: server.URL + "/RPC2", queryPermit: make(chan struct{}, 1)}

	if err := rtorrent.ControlTask(context.Background(), testTaskHash, TaskControlAction("restart")); err == nil {
		t.Fatal("未知动作应报错")
	}

	var nilAdapter *RTorrent
	if _, _, err := nilAdapter.InspectControlTask(context.Background(), testTaskHash); err == nil {
		t.Fatal("nil 适配器应拒绝查询")
	}
	if err := nilAdapter.ControlTask(context.Background(), testTaskHash, TaskControlStart); err == nil {
		t.Fatal("nil 适配器应拒绝控制")
	}
}

func writeXMLRPCString(w http.ResponseWriter, value string) {
	_, _ = w.Write([]byte(`<?xml version="1.0"?><methodResponse><params><param><value><string>` +
		value + `</string></value></param></params></methodResponse>`))
}
