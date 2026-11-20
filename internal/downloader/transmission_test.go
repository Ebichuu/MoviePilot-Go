package downloader

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestNewTransmissionNormalizesConfig(t *testing.T) {
	if _, err := NewTransmission(TransmissionConfig{}); err == nil {
		t.Fatal("空 URL 应报错")
	}
	adapter, err := NewTransmission(TransmissionConfig{URL: " http://127.0.0.1:9091/transmission/rpc/ "})
	if err != nil {
		t.Fatalf("构造失败: %v", err)
	}
	// 尾部斜杠必须去掉，否则拼出来的 RPC 路径会带 `//`。
	if adapter.baseURL != "http://127.0.0.1:9091/transmission/rpc" {
		t.Fatalf("baseURL = %q", adapter.baseURL)
	}
	// 零值配置要落到安全默认值（30 秒超时、4 条连接）。
	if adapter.client.Timeout != 30*time.Second {
		t.Fatalf("默认超时 = %v", adapter.client.Timeout)
	}
	transport := adapter.client.Transport.(*http.Transport)
	if transport.MaxConnsPerHost != 4 || transport.MaxConnsPerHost != transport.MaxIdleConnsPerHost {
		t.Fatalf("默认连接数 = %d/%d", transport.MaxConnsPerHost, transport.MaxIdleConnsPerHost)
	}
	adapter.CloseIdleConnections()

	// 越界的连接数上限要收敛到默认值，不能把上游打爆。
	clamped, err := NewTransmission(TransmissionConfig{URL: "http://x", Timeout: time.Minute, MaxConnsPerHost: 99})
	if err != nil {
		t.Fatalf("构造失败: %v", err)
	}
	if clamped.client.Timeout != time.Minute {
		t.Fatalf("超时 = %v", clamped.client.Timeout)
	}
	if clamped.client.Transport.(*http.Transport).MaxConnsPerHost != 4 {
		t.Fatalf("连接数 = %d", clamped.client.Transport.(*http.Transport).MaxConnsPerHost)
	}
	clamped.CloseIdleConnections()

	// nil 适配器关闭空闲连接不能 panic。
	var nilAdapter *Transmission
	nilAdapter.CloseIdleConnections()
}

func TestTransmissionFindByLabel(t *testing.T) {
	transmission, fake := newTransmissionFake(t)
	fake.torrents = []any{
		map[string]any{"hashString": "bad-hash", "labels": []string{"目标标签"}},
		map[string]any{"hashString": strings.ToUpper(testTaskHash), "labels": []string{"其它", "  目标标签  "}},
	}
	hash, found, err := transmission.FindByLabel(context.Background(), " 目标标签 ")
	if err != nil || !found {
		t.Fatalf("查询 = %q %v %v", hash, found, err)
	}
	if !strings.EqualFold(hash, testTaskHash) {
		t.Fatalf("hash = %q", hash)
	}
	// 按标签查询要拉全量任务（不带 ids）。
	if _, arguments := fake.lastMethod(t); arguments["ids"] != nil {
		t.Fatalf("不应带 ids: %v", arguments)
	}

	if _, found, err := transmission.FindByLabel(context.Background(), "别的标签"); err != nil || found {
		t.Fatalf("未命中 = %v %v", found, err)
	}
	if _, _, err := transmission.FindByLabel(context.Background(), "   "); err == nil {
		t.Fatal("空标签应报错")
	}
}

func TestTransmissionFindByHash(t *testing.T) {
	transmission, fake := newTransmissionFake(t)
	fake.torrents = []any{map[string]any{"hashString": strings.ToUpper(testTaskHash), "labels": []string{}}}
	hash, found, err := transmission.FindByHash(context.Background(), testTaskHash)
	if err != nil || !found {
		t.Fatalf("查询 = %q %v %v", hash, found, err)
	}
	if !strings.EqualFold(hash, testTaskHash) {
		t.Fatalf("hash = %q", hash)
	}
	// 按 hash 查询要显式传 ids，避免拉回全部任务。
	if _, arguments := fake.lastMethod(t); arguments["ids"] == nil {
		t.Fatalf("应带 ids: %v", arguments)
	}
	if _, found, err := transmission.FindByHash(context.Background(), "1111111111111111111111111111111111111111"); err != nil || found {
		t.Fatalf("未命中 = %v %v", found, err)
	}
	if _, _, err := transmission.FindByHash(context.Background(), "not-a-hash"); err == nil {
		t.Fatal("非法 hash 应报错")
	}
}

func TestTransmissionAddTorrentBytes(t *testing.T) {
	transmission, fake := newTransmissionFake(t)
	metainfo := []byte("d8:announce4:http4:infod4:name4:testee")

	if err := transmission.AddTorrentBytes(context.Background(), nil, "", "", false); err == nil {
		t.Fatal("空种子应报错")
	}
	if fake.count() != 0 {
		t.Fatal("校验失败不应发请求")
	}
	oversize := make([]byte, maxTorrentMetainfoSize+1)
	if err := transmission.AddTorrentBytes(context.Background(), oversize, "", "", false); err == nil {
		t.Fatal("超大种子应报错")
	}

	if err := transmission.AddTorrentBytes(context.Background(), metainfo, " /downloads/电影 ", " 标签A ", true); err != nil {
		t.Fatalf("提交失败: %v", err)
	}
	method, arguments := fake.lastMethod(t)
	if method != "torrent-add" {
		t.Fatalf("方法 = %q", method)
	}
	// 种子必须以 base64 传，Transmission 不接受原始字节。
	if arguments["metainfo"] != base64.StdEncoding.EncodeToString(metainfo) {
		t.Fatalf("metainfo = %v", arguments["metainfo"])
	}
	if arguments["paused"] != true {
		t.Fatalf("paused = %v", arguments["paused"])
	}
	if arguments["download-dir"] != "/downloads/电影" {
		t.Fatalf("download-dir = %v", arguments["download-dir"])
	}
	labels, ok := arguments["labels"].([]any)
	if !ok || len(labels) != 1 || labels[0] != "标签A" {
		t.Fatalf("labels = %v", arguments["labels"])
	}

	// 空的保存路径与标签不应提交空字段。
	if err := transmission.AddTorrentBytes(context.Background(), metainfo, "  ", "", false); err != nil {
		t.Fatalf("提交失败: %v", err)
	}
	_, arguments = fake.lastMethod(t)
	if _, exists := arguments["download-dir"]; exists {
		t.Fatalf("空路径不应提交: %v", arguments)
	}
	if _, exists := arguments["labels"]; exists {
		t.Fatalf("空标签不应提交: %v", arguments)
	}
}

func TestTransmissionAddTorrentBytesPropagatesFailure(t *testing.T) {
	transmission, fake := newTransmissionFake(t)
	fake.result = "duplicate torrent"
	if err := transmission.AddTorrentBytes(context.Background(), []byte("x"), "", "", false); err == nil {
		t.Fatal("上游拒绝应报错")
	}
}

func TestTransmissionObserveTasks(t *testing.T) {
	transmission, fake := newTransmissionFake(t)
	pausedHash := strings.Repeat("A", 40)
	activeHash := strings.Repeat("B", 40)
	doneHash := strings.Repeat("C", 40)
	unknownHash := strings.Repeat("D", 40)
	fake.torrents = []any{
		map[string]any{"hashString": pausedHash, "status": 0, "percentDone": 0.5},
		map[string]any{"hashString": activeHash, "status": 3, "percentDone": 0.25},
		map[string]any{"hashString": doneHash, "status": 3, "percentDone": 1.0},
		map[string]any{"hashString": unknownHash, "status": 1, "percentDone": 0.1},
		map[string]any{"hashString": strings.Repeat("E", 40), "status": 3, "percentDone": 0.5},
	}
	missingHash := strings.Repeat("F", 40)
	observations, err := transmission.ObserveTasks(context.Background(), []string{
		pausedHash, activeHash, doneHash, unknownHash, missingHash,
		// 重复项必须去重（同一批里重复请求会让上游报错）。
		strings.ToLower(pausedHash),
	})
	if err != nil {
		t.Fatalf("对账失败: %v", err)
	}
	want := map[string]TaskObservationState{
		pausedHash:  TaskObservationPaused,
		activeHash:  TaskObservationActive,
		doneHash:    TaskObservationCompleted,
		unknownHash: TaskObservationUnknown,
		missingHash: TaskObservationMissing,
	}
	for hash, state := range want {
		got, ok := observations[hash]
		if !ok {
			t.Fatalf("缺少 %s 的观测结果", hash)
		}
		if got.State != state {
			t.Fatalf("%s 状态 = %q，期望 %q", hash, got.State, state)
		}
	}
	if len(observations) != len(want) {
		t.Fatalf("观测数量 = %d，期望 %d（不应包含未请求的 hash）", len(observations), len(want))
	}
	_, arguments := fake.lastMethod(t)
	ids, _ := arguments["ids"].([]any)
	if len(ids) != 5 {
		t.Fatalf("请求 ids = %v", arguments["ids"])
	}
}

func TestTransmissionObserveTasksGuards(t *testing.T) {
	transmission, fake := newTransmissionFake(t)
	if _, err := transmission.ObserveTasks(nil, []string{testTaskHash}); err == nil {
		t.Fatal("nil 上下文应报错")
	}
	if _, err := transmission.ObserveTasks(context.Background(), nil); err == nil {
		t.Fatal("空 hash 列表应报错")
	}
	tooMany := make([]string, transmissionMaxObserve+1)
	for index := range tooMany {
		tooMany[index] = strings.Repeat("a", 40)
	}
	if _, err := transmission.ObserveTasks(context.Background(), tooMany); err == nil {
		t.Fatal("超过上限应报错")
	}
	if _, err := transmission.ObserveTasks(context.Background(), []string{"bad"}); err == nil {
		t.Fatal("非法 hash 应报错")
	}
	if fake.count() != 0 {
		t.Fatalf("守卫失败不应发请求，实际 %d 次", fake.count())
	}
}

func TestTransmissionListDownloadingProjectsTasks(t *testing.T) {
	transmission, fake := newTransmissionFake(t)
	label := "mp-0123456789abcdef0123456789abcdef"
	fake.torrents = []any{
		map[string]any{
			"hashString": strings.ToUpper(testTaskHash), "name": "电影.2026.1080p", "status": 3,
			"percentDone": 0.5, "totalSize": 1024, "rateDownload": 2048, "rateUpload": 512,
			"eta": 60, "downloadDir": "/downloads/电影", "labels": []string{label},
		},
		map[string]any{"hashString": "bad", "name": "坏任务", "status": 3},
		map[string]any{
			"hashString": strings.Repeat("9", 40), "name": "已暂停", "status": 0,
			"percentDone": 0.1, "totalSize": -5, "rateDownload": -1, "eta": -1, "labels": []string{},
		},
	}
	tasks, err := transmission.ListDownloading(context.Background())
	if err != nil {
		t.Fatalf("查询失败: %v", err)
	}
	if len(tasks) != 2 {
		t.Fatalf("任务数 = %d（非法 hash 应被跳过）", len(tasks))
	}
	first := tasks[0]
	if !strings.EqualFold(first.Hash, testTaskHash) || first.Title != "电影.2026.1080p" {
		t.Fatalf("任务 = %+v", first)
	}
	if first.SizeBytes != 1024 || first.ProgressPercent != 50 || first.DownSpeedBytes != 2048 || first.UpSpeedBytes != 512 {
		t.Fatalf("投影 = %+v", first)
	}
	if first.ETASeconds != 60 || first.SavePath != "/downloads/电影" || first.ContentPath != "/downloads/电影" {
		t.Fatalf("投影 = %+v", first)
	}
	if first.Label != label || first.Tags != label {
		t.Fatalf("标签 = %q / %q", first.Label, first.Tags)
	}
	if first.State != TaskStateDownloading {
		t.Fatalf("状态 = %q", first.State)
	}
	// 负数必须被夹到 0，不能把 -1 当作真实速度/大小展示。
	paused := tasks[1]
	if paused.SizeBytes != 0 || paused.DownSpeedBytes != 0 || paused.ETASeconds != 0 {
		t.Fatalf("负数未夹紧: %+v", paused)
	}
	if paused.State != TaskStatePaused {
		t.Fatalf("状态 = %q", paused.State)
	}
}

func TestTransmissionListDownloadingCapsResults(t *testing.T) {
	transmission, fake := newTransmissionFake(t)
	torrents := make([]any, 0, maxDownloadingTasks+5)
	for index := 0; index < maxDownloadingTasks+5; index++ {
		torrents = append(torrents, map[string]any{
			"hashString": fmt.Sprintf("%040x", index+1),
			"status":     3, "percentDone": 0.1, "totalSize": 1,
		})
	}
	fake.torrents = torrents
	tasks, err := transmission.ListDownloading(context.Background())
	if err != nil {
		t.Fatalf("查询失败: %v", err)
	}
	if len(tasks) > maxDownloadingTasks {
		t.Fatalf("任务数 = %d 超过上限 %d", len(tasks), maxDownloadingTasks)
	}
}

func TestTransmissionListCompletedFiltersByState(t *testing.T) {
	transmission, fake := newTransmissionFake(t)
	fake.torrents = []any{
		map[string]any{"hashString": strings.Repeat("a", 40), "status": 5, "percentDone": 1.0, "name": "做种完成"},
		map[string]any{"hashString": strings.Repeat("b", 40), "status": 3, "percentDone": 1.0, "name": "下载完成"},
		map[string]any{"hashString": strings.Repeat("c", 40), "status": 3, "percentDone": 0.4, "name": "下载中"},
		map[string]any{"hashString": strings.Repeat("d", 40), "status": 0, "percentDone": 0.0, "name": "已暂停"},
	}
	completed, err := transmission.ListCompleted(context.Background())
	if err != nil {
		t.Fatalf("查询失败: %v", err)
	}
	if len(completed) != 2 {
		t.Fatalf("完成数 = %d", len(completed))
	}
	for _, task := range completed {
		if task.State != TaskStateCompleted {
			t.Fatalf("状态 = %q", task.State)
		}
	}
}

func TestObserveTransmissionStateMapping(t *testing.T) {
	cases := []struct {
		status  int
		percent float64
		want    TaskObservationState
	}{
		{0, 0.5, TaskObservationPaused},
		{1, 0.5, TaskObservationUnknown},
		{2, 0.5, TaskObservationUnknown},
		{3, 0.5, TaskObservationActive},
		{4, 0.5, TaskObservationActive},
		{3, 1.0, TaskObservationCompleted},
		{5, 0.9, TaskObservationCompleted},
		{6, 0.9, TaskObservationCompleted},
		{99, 0.5, TaskObservationUnknown},
	}
	for _, testCase := range cases {
		if got := observeTransmissionState(testCase.status, testCase.percent); got != testCase.want {
			t.Fatalf("status=%d percent=%v → %q，期望 %q", testCase.status, testCase.percent, got, testCase.want)
		}
	}
}

func TestTransmissionSelectEpisodeFiles(t *testing.T) {
	transmission, fake := newTransmissionFake(t)
	fake.torrents = []any{map[string]any{"files": []any{
		map[string]any{"name": "Show.S01E01.1080p.mkv"},
		map[string]any{"name": "Show.S01E02.1080p.mkv"},
		map[string]any{"name": "Show.S01E03.1080p.mkv"},
	}}}
	// 只要第 2 集：另外两集必须被标为不需要，然后启动任务。
	if err := transmission.SelectEpisodeFiles(context.Background(), testTaskHash, []Episode{{Season: 1, Episode: 2}}); err != nil {
		t.Fatalf("选集失败: %v", err)
	}
	methods := fake.methods
	if len(methods) != 3 {
		t.Fatalf("调用链 = %v", methods)
	}
	if methods[0] != "torrent-get" || methods[1] != "torrent-set" || methods[2] != "torrent-start" {
		t.Fatalf("调用链 = %v", methods)
	}
	unwanted, ok := fake.args[1]["files-unwanted"].([]any)
	if !ok || len(unwanted) != 2 {
		t.Fatalf("files-unwanted = %v", fake.args[1]["files-unwanted"])
	}
}

func TestTransmissionSelectEpisodeFilesGuards(t *testing.T) {
	transmission, fake := newTransmissionFake(t)
	if err := transmission.SelectEpisodeFiles(nil, testTaskHash, []Episode{{Season: 1, Episode: 1}}); !errors.Is(err, ErrEpisodeSelectionFailed) {
		t.Fatalf("nil 上下文 = %v", err)
	}
	// 没有目标集时不做任何选择（等价「整包下载」）。
	if err := transmission.SelectEpisodeFiles(context.Background(), testTaskHash, nil); err != nil {
		t.Fatalf("空目标集 = %v", err)
	}
	if fake.count() != 0 {
		t.Fatal("空目标集不应发请求")
	}
	if err := transmission.SelectEpisodeFiles(context.Background(), "bad", []Episode{{Season: 1, Episode: 1}}); !errors.Is(err, ErrEpisodeSelectionFailed) {
		t.Fatalf("非法 hash = %v", err)
	}

	// 任务还没出现在传输器里：返回可重试的 pending，而不是失败。
	fake.torrents = []any{}
	if err := transmission.SelectEpisodeFiles(context.Background(), testTaskHash, []Episode{{Season: 1, Episode: 1}}); !errors.Is(err, ErrEpisodeSelectionPending) {
		t.Fatalf("任务缺失 = %v", err)
	}
	// 有任务但没有文件列表：同样 pending。
	fake.torrents = []any{map[string]any{"files": []any{}}}
	if err := transmission.SelectEpisodeFiles(context.Background(), testTaskHash, []Episode{{Season: 1, Episode: 1}}); !errors.Is(err, ErrEpisodeSelectionPending) {
		t.Fatalf("文件缺失 = %v", err)
	}
}

func TestTransmissionSelectEpisodeFilesWithoutMatchOnlyStarts(t *testing.T) {
	transmission, fake := newTransmissionFake(t)
	fake.torrents = []any{map[string]any{"files": []any{
		map[string]any{"name": "Show.S01E01.1080p.mkv"},
	}}}
	// 目标集在文件列表里没有命中：不修改任何文件优先级，直接启动。
	if err := transmission.SelectEpisodeFiles(context.Background(), testTaskHash, []Episode{{Season: 5, Episode: 9}}); err != nil {
		t.Fatalf("选集失败: %v", err)
	}
	if len(fake.methods) != 2 || fake.methods[1] != "torrent-start" {
		t.Fatalf("调用链 = %v", fake.methods)
	}
}

func TestTransmissionRequestUsesBasicAuth(t *testing.T) {
	var gotUser, gotPassword string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotUser, gotPassword, _ = r.BasicAuth()
		_, _ = w.Write([]byte(`{"result":"success","arguments":{}}`))
	}))
	defer server.Close()
	transmission := &Transmission{
		client: server.Client(), baseURL: server.URL,
		username: "admin", password: "secret", queryPermit: make(chan struct{}, 1),
	}
	if err := transmission.ControlTask(context.Background(), testTaskHash, TaskControlStart); err != nil {
		t.Fatalf("控制失败: %v", err)
	}
	// 凭据走 Basic Auth，不能出现在 URL 查询串里。
	if gotUser != "admin" || gotPassword != "secret" {
		t.Fatalf("认证 = %q/%q", gotUser, gotPassword)
	}
}
