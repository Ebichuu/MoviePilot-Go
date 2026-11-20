package downloader

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

func rtxString(value string) string {
	return "<value><string>" + value + "</string></value>"
}

func rtxInt(value int64) string {
	return "<value><i4>" + strconv.FormatInt(value, 10) + "</i4></value>"
}

func rtxArray(items ...string) string {
	return "<value><array><data>" + strings.Join(items, "") + "</data></array></value>"
}

func rtxResponse(inner string) string {
	return `<?xml version="1.0"?><methodResponse><params><param>` + inner + `</param></params></methodResponse>`
}

type rtorrentFake struct {
	mu       sync.Mutex
	methods  []string
	bodies   []string
	rows     [][]string
	files    []string
	failOn   string
	status   int
	override string
}

func (f *rtorrentFake) handler(w http.ResponseWriter, r *http.Request) {
	payload := new(strings.Builder)
	if _, err := io.Copy(payload, r.Body); err != nil {
		w.WriteHeader(http.StatusInternalServerError)
		return
	}
	body := payload.String()
	method := ""
	if start := strings.Index(body, "<methodName>"); start >= 0 {
		rest := body[start+len("<methodName>"):]
		if end := strings.Index(rest, "</methodName>"); end >= 0 {
			method = rest[:end]
		}
	}
	f.mu.Lock()
	f.methods = append(f.methods, method)
	f.bodies = append(f.bodies, body)
	rows, files := f.rows, f.files
	failOn, status, override := f.failOn, f.status, f.override
	f.mu.Unlock()

	if status != 0 {
		w.WriteHeader(status)
		return
	}
	if failOn != "" && method == failOn {
		w.WriteHeader(http.StatusInternalServerError)
		return
	}
	switch method {
	case "d.multicall2":
		entries := make([]string, 0, len(rows))
		for _, row := range rows {
			entries = append(entries, rtxArray(row...))
		}
		_, _ = w.Write([]byte(rtxResponse(rtxArray(entries...))))
	case "f.multicall":
		entries := make([]string, 0, len(files))
		for _, path := range files {
			entries = append(entries, rtxArray(rtxString(path)))
		}
		_, _ = w.Write([]byte(rtxResponse(rtxArray(entries...))))
	default:
		if override != "" {
			_, _ = w.Write([]byte(rtxResponse(override)))
			return
		}
		_, _ = w.Write([]byte(rtxResponse(rtxInt(0))))
	}
}

func (f *rtorrentFake) lastBody(t *testing.T) (string, string) {
	t.Helper()
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.methods) == 0 {
		return "", ""
	}
	return f.methods[len(f.methods)-1], f.bodies[len(f.bodies)-1]
}

func (f *rtorrentFake) callCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.methods)
}

func newRTorrentFake(t *testing.T) (*RTorrent, *rtorrentFake) {
	t.Helper()
	fake := &rtorrentFake{}
	server := httptest.NewServer(http.HandlerFunc(fake.handler))
	t.Cleanup(server.Close)
	return &RTorrent{client: server.Client(), endpoint: server.URL + "/RPC2", queryPermit: make(chan struct{}, 1)}, fake
}

// rtxRow 按 d.multicall2 的字段顺序构造一行。
func rtxRow(hash, label, name string, state, complete, done, size, down, up int64, directory string) []string {
	return []string{
		rtxString(hash), rtxString(label), rtxString(name), rtxInt(state), rtxInt(complete),
		rtxInt(done), rtxInt(size), rtxInt(down), rtxInt(up), rtxString(directory),
	}
}

func TestNewRTorrentConfig(t *testing.T) {
	if _, err := NewRTorrent(RTorrentConfig{}); err == nil {
		t.Fatal("空 URL 应报错")
	}
	adapter, err := NewRTorrent(RTorrentConfig{URL: "  http://127.0.0.1:5000/RPC2  "})
	if err != nil {
		t.Fatalf("构造失败: %v", err)
	}
	// rTorrent 的端点就是完整 RPC2 路径，只裁剪空白，不做路径改写。
	if adapter.endpoint != "http://127.0.0.1:5000/RPC2" {
		t.Fatalf("endpoint = %q", adapter.endpoint)
	}
	if adapter.client.Timeout != 30*time.Second {
		t.Fatalf("默认超时 = %v", adapter.client.Timeout)
	}
	transport := adapter.client.Transport.(*http.Transport)
	if transport.MaxConnsPerHost != 4 || transport.MaxIdleConnsPerHost != 4 {
		t.Fatalf("默认连接数 = %d/%d", transport.MaxConnsPerHost, transport.MaxIdleConnsPerHost)
	}
	adapter.CloseIdleConnections()

	clamped, err := NewRTorrent(RTorrentConfig{URL: "http://x/RPC2", Timeout: time.Minute, MaxConnsPerHost: 99})
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

	var nilAdapter *RTorrent
	nilAdapter.CloseIdleConnections()
}

func TestRTorrentFindByLabelAndHash(t *testing.T) {
	adapter, fake := newRTorrentFake(t)
	fake.rows = [][]string{
		rtxRow("bad-hash", "目标标签", "坏任务", 1, 0, 0, 0, 0, 0, "/d"),
		rtxRow(strings.ToUpper(testTaskHash), " 目标标签 ", "电影", 1, 0, 500, 1000, 10, 0, "/downloads"),
		rtxRow(strings.Repeat("B", 40), "其它", "别的", 1, 0, 0, 0, 0, 0, "/d"),
	}
	hash, found, err := adapter.FindByLabel(context.Background(), " 目标标签 ")
	if err != nil || !found || !strings.EqualFold(hash, testTaskHash) {
		t.Fatalf("按标签查询 = %q %v %v", hash, found, err)
	}
	if _, _, err := adapter.FindByLabel(context.Background(), "  "); err == nil {
		t.Fatal("空标签应报错")
	}
	if _, found, err := adapter.FindByLabel(context.Background(), "不存在的标签"); err != nil || found {
		t.Fatalf("未命中 = %v %v", found, err)
	}

	hash, found, err = adapter.FindByHash(context.Background(), strings.ToLower(testTaskHash))
	if err != nil || !found || !strings.EqualFold(hash, testTaskHash) {
		t.Fatalf("按 hash 查询 = %q %v %v", hash, found, err)
	}
	if _, found, err := adapter.FindByHash(context.Background(), strings.Repeat("F", 40)); err != nil || found {
		t.Fatalf("未命中 = %v %v", found, err)
	}
	if _, _, err := adapter.FindByHash(context.Background(), "bad"); err == nil {
		t.Fatal("非法 hash 应报错")
	}
}

func TestRTorrentAddTorrentBytes(t *testing.T) {
	adapter, fake := newRTorrentFake(t)
	metainfo := []byte("d8:announce4:http4:infod4:name4:testee")

	if err := adapter.AddTorrentBytes(context.Background(), nil, "", "", false); err == nil {
		t.Fatal("空种子应报错")
	}
	if err := adapter.AddTorrentBytes(context.Background(), make([]byte, maxTorrentMetainfoSize+1), "", "", false); err == nil {
		t.Fatal("超大种子应报错")
	}
	if fake.callCount() != 0 {
		t.Fatal("校验失败不应发请求")
	}

	// 非暂停走 load.raw_start（装载后立即开始）。
	if err := adapter.AddTorrentBytes(context.Background(), metainfo, "/downloads/电影", "标签A", false); err != nil {
		t.Fatalf("提交失败: %v", err)
	}
	method, body := fake.lastBody(t)
	if method != "load.raw_start" {
		t.Fatalf("方法 = %q", method)
	}
	if !strings.Contains(body, xmlrpcEncodeBase64(metainfo)) {
		t.Fatalf("种子未按 base64 提交: %s", body)
	}
	if !strings.Contains(body, `d.directory.set=&quot;/downloads/电影&quot;`) {
		t.Fatalf("缺少目录参数: %s", body)
	}
	if !strings.Contains(body, `d.custom1.set=&quot;标签A&quot;`) {
		t.Fatalf("缺少标签参数: %s", body)
	}

	// 暂停走 load.raw，并且空的目录/标签不提交。
	if err := adapter.AddTorrentBytes(context.Background(), metainfo, "  ", "", true); err != nil {
		t.Fatalf("提交失败: %v", err)
	}
	method, body = fake.lastBody(t)
	if method != "load.raw" {
		t.Fatalf("暂停方法 = %q", method)
	}
	if strings.Contains(body, "d.directory.set") || strings.Contains(body, "d.custom1.set") {
		t.Fatalf("空目录/标签不应提交: %s", body)
	}
}

func TestRTorrentAddTorrentBytesPropagatesFailure(t *testing.T) {
	adapter, fake := newRTorrentFake(t)
	fake.status = http.StatusInternalServerError
	if err := adapter.AddTorrentBytes(context.Background(), []byte("x"), "", "", false); err == nil {
		t.Fatal("上游失败应报错")
	}
}

func TestRTorrentObserveTasks(t *testing.T) {
	adapter, fake := newRTorrentFake(t)
	completed := strings.Repeat("A", 40)
	paused := strings.Repeat("B", 40)
	active := strings.Repeat("C", 40)
	missing := strings.Repeat("D", 40)
	fake.rows = [][]string{
		rtxRow(completed, "", "完成", 1, 1, 1000, 1000, 0, 0, "/d"),
		rtxRow(paused, "", "暂停", 0, 0, 100, 1000, 0, 0, "/d"),
		rtxRow(active, "", "下载中", 1, 0, 100, 1000, 5, 0, "/d"),
		rtxRow(strings.Repeat("E", 40), "", "无关", 1, 0, 0, 0, 0, 0, "/d"),
	}
	observations, err := adapter.ObserveTasks(context.Background(), []string{completed, paused, active, missing})
	if err != nil {
		t.Fatalf("对账失败: %v", err)
	}
	want := map[string]TaskObservationState{
		completed: TaskObservationCompleted,
		paused:    TaskObservationPaused,
		active:    TaskObservationActive,
		missing:   TaskObservationMissing,
	}
	for hash, state := range want {
		got, ok := observations[hash]
		if !ok {
			t.Fatalf("缺少 %s", hash)
		}
		if got.State != state {
			t.Fatalf("%s 状态 = %q，期望 %q", hash, got.State, state)
		}
	}
	// 请求之外的 hash 不能混进结果。
	if len(observations) != len(want) {
		t.Fatalf("观测数量 = %d", len(observations))
	}
}

func TestRTorrentObserveTasksGuards(t *testing.T) {
	adapter, fake := newRTorrentFake(t)
	if _, err := adapter.ObserveTasks(nil, []string{testTaskHash}); err == nil {
		t.Fatal("nil 上下文应报错")
	}
	if _, err := adapter.ObserveTasks(context.Background(), nil); err == nil {
		t.Fatal("空列表应报错")
	}
	tooMany := make([]string, maxObservedTasks+1)
	for index := range tooMany {
		tooMany[index] = strings.Repeat("A", 40)
	}
	if _, err := adapter.ObserveTasks(context.Background(), tooMany); err == nil {
		t.Fatal("超过上限应报错")
	}
	if _, err := adapter.ObserveTasks(context.Background(), []string{"bad"}); err == nil {
		t.Fatal("非法 hash 应报错")
	}
	if fake.callCount() != 0 {
		t.Fatalf("守卫失败不应发请求，实际 %d 次", fake.callCount())
	}
}

func TestRTorrentListDownloadingProjectsTasks(t *testing.T) {
	adapter, fake := newRTorrentFake(t)
	label := "mp-0123456789abcdef0123456789abcdef"
	fake.rows = [][]string{
		rtxRow(strings.ToUpper(testTaskHash), label, "电影.2026.1080p", 1, 0, 500, 1000, 2048, 512, "/downloads/电影"),
		rtxRow("bad-hash", label, "坏任务", 1, 0, 0, 0, 0, 0, "/d"),
		rtxRow(strings.Repeat("B", 40), "", "已完成", 1, 1, 1000, 1000, 0, 0, "/d"),
	}
	tasks, err := adapter.ListDownloading(context.Background())
	if err != nil {
		t.Fatalf("查询失败: %v", err)
	}
	if len(tasks) != 2 {
		t.Fatalf("任务数 = %d", len(tasks))
	}
	first := tasks[0]
	if !strings.EqualFold(first.Hash, testTaskHash) || first.Title != "电影.2026.1080p" {
		t.Fatalf("任务 = %+v", first)
	}
	if first.SizeBytes != 1000 || first.ProgressPercent != 50 {
		t.Fatalf("投影 = %+v", first)
	}
	if first.DownSpeedBytes != 2048 || first.UpSpeedBytes != 512 {
		t.Fatalf("速度 = %+v", first)
	}
	if first.SavePath != "/downloads/电影" || first.ContentPath != "/downloads/电影" {
		t.Fatalf("路径 = %+v", first)
	}
	if first.Label != label || first.Tags != label {
		t.Fatalf("标签 = %q / %q", first.Label, first.Tags)
	}
	if first.State != TaskStateDownloading {
		t.Fatalf("状态 = %q", first.State)
	}
	if tasks[1].State != TaskStateCompleted || tasks[1].ProgressPercent != 100 {
		t.Fatalf("完成任务 = %+v", tasks[1])
	}
}

func TestRTorrentListDownloadingCapsResults(t *testing.T) {
	adapter, fake := newRTorrentFake(t)
	rows := make([][]string, 0, maxDownloadingTasks+5)
	for index := 0; index < maxDownloadingTasks+5; index++ {
		rows = append(rows, rtxRow(fmt.Sprintf("%040X", index+1), "", "任务", 1, 0, 1, 10, 0, 0, "/d"))
	}
	fake.rows = rows
	tasks, err := adapter.ListDownloading(context.Background())
	if err != nil {
		t.Fatalf("查询失败: %v", err)
	}
	if len(tasks) > maxDownloadingTasks {
		t.Fatalf("任务数 = %d 超过上限", len(tasks))
	}
}

func TestRTorrentListCompleted(t *testing.T) {
	adapter, fake := newRTorrentFake(t)
	fake.rows = [][]string{
		rtxRow(strings.Repeat("A", 40), "", "完成", 1, 1, 1000, 1000, 0, 0, "/d"),
		rtxRow(strings.Repeat("B", 40), "", "下载中", 1, 0, 100, 1000, 1, 0, "/d"),
		rtxRow(strings.Repeat("C", 40), "", "暂停", 0, 0, 100, 1000, 0, 0, "/d"),
	}
	completed, err := adapter.ListCompleted(context.Background())
	if err != nil {
		t.Fatalf("查询失败: %v", err)
	}
	if len(completed) != 1 || completed[0].Title != "完成" {
		t.Fatalf("完成列表 = %+v", completed)
	}
}

func TestRTorrentStateHelpers(t *testing.T) {
	cases := []struct {
		row   rTorrentTorrentRow
		state string
	}{
		{rTorrentTorrentRow{State: 1, Complete: 1}, TaskStateCompleted},
		{rTorrentTorrentRow{State: 0, Complete: 0}, TaskStatePaused},
		{rTorrentTorrentRow{State: 1, Complete: 0}, TaskStateDownloading},
		// 「已完成」优先于「暂停」：做种中的任务不应被当成暂停。
		{rTorrentTorrentRow{State: 0, Complete: 1}, TaskStateCompleted},
	}
	for _, testCase := range cases {
		if got := normalizeRTorrentTaskState(testCase.row); got != testCase.state {
			t.Fatalf("%+v → %q，期望 %q", testCase.row, got, testCase.state)
		}
	}

	progress := []struct {
		row  rTorrentTorrentRow
		want float64
	}{
		{rTorrentTorrentRow{SizeBytes: 0, BytesDone: 10}, 0},
		{rTorrentTorrentRow{SizeBytes: 1000, BytesDone: 500}, 50},
		{rTorrentTorrentRow{SizeBytes: 1000, BytesDone: 1000}, 100},
		// 下载量超过总大小（元数据修正中）时夹到 100，不能出现 120%。
		{rTorrentTorrentRow{SizeBytes: 1000, BytesDone: 1200}, 100},
	}
	for _, testCase := range progress {
		if got := rTorrentProgress(testCase.row); got != testCase.want {
			t.Fatalf("%+v → %v，期望 %v", testCase.row, got, testCase.want)
		}
	}

	if got := observeRTorrentState(rTorrentTorrentRow{State: 1, Complete: 1}); got != TaskObservationCompleted {
		t.Fatalf("完成观测 = %q", got)
	}
	if got := observeRTorrentState(rTorrentTorrentRow{State: 0}); got != TaskObservationPaused {
		t.Fatalf("暂停观测 = %q", got)
	}
	if got := observeRTorrentState(rTorrentTorrentRow{State: 1}); got != TaskObservationActive {
		t.Fatalf("活动观测 = %q", got)
	}
}

func TestRTorrentSelectEpisodeFiles(t *testing.T) {
	adapter, fake := newRTorrentFake(t)
	fake.files = []string{
		"Show.S01E01.1080p.mkv",
		"Show.S01E02.1080p.mkv",
		"Show.S01E03.1080p.mkv",
	}
	if err := adapter.SelectEpisodeFiles(context.Background(), testTaskHash, []Episode{{Season: 1, Episode: 2}}); err != nil {
		t.Fatalf("选集失败: %v", err)
	}
	fake.mu.Lock()
	methods := append([]string(nil), fake.methods...)
	bodies := append([]string(nil), fake.bodies...)
	fake.mu.Unlock()
	// f.multicall 取文件 → 对每个非目标文件 f.priority.set=0 → d.update_priorities → d.start
	if len(methods) != 5 {
		t.Fatalf("调用链 = %v", methods)
	}
	if methods[0] != "f.multicall" || methods[4] != "d.start" {
		t.Fatalf("调用链 = %v", methods)
	}
	priorities := 0
	for index, method := range methods {
		if method != "f.priority.set" {
			continue
		}
		priorities++
		// 索引必须指向非目标文件（第 1 集与第 3 集 → f0/f2），且优先级为 0。
		if !strings.Contains(bodies[index], ":f0") && !strings.Contains(bodies[index], ":f2") {
			t.Fatalf("优先级参数 = %s", bodies[index])
		}
		if !strings.Contains(bodies[index], "<string>0</string>") {
			t.Fatalf("优先级应为 0: %s", bodies[index])
		}
	}
	if priorities != 2 {
		t.Fatalf("设置了 %d 个文件优先级", priorities)
	}
}

func TestRTorrentSelectEpisodeFilesGuards(t *testing.T) {
	adapter, fake := newRTorrentFake(t)
	if err := adapter.SelectEpisodeFiles(nil, testTaskHash, []Episode{{Season: 1, Episode: 1}}); !errors.Is(err, ErrEpisodeSelectionFailed) {
		t.Fatalf("nil 上下文 = %v", err)
	}
	if err := adapter.SelectEpisodeFiles(context.Background(), testTaskHash, nil); err != nil {
		t.Fatalf("空目标集 = %v", err)
	}
	if fake.callCount() != 0 {
		t.Fatal("空目标集不应发请求")
	}
	if err := adapter.SelectEpisodeFiles(context.Background(), "bad", []Episode{{Season: 1, Episode: 1}}); !errors.Is(err, ErrEpisodeSelectionFailed) {
		t.Fatalf("非法 hash = %v", err)
	}

	fake.files = nil
	if err := adapter.SelectEpisodeFiles(context.Background(), testTaskHash, []Episode{{Season: 1, Episode: 1}}); !errors.Is(err, ErrEpisodeSelectionPending) {
		t.Fatalf("文件未就绪 = %v", err)
	}
}

func TestRTorrentSelectEpisodeFilesWithoutMatchOnlyStarts(t *testing.T) {
	adapter, fake := newRTorrentFake(t)
	fake.files = []string{"Show.S01E01.1080p.mkv"}
	if err := adapter.SelectEpisodeFiles(context.Background(), testTaskHash, []Episode{{Season: 9, Episode: 9}}); err != nil {
		t.Fatalf("选集失败: %v", err)
	}
	fake.mu.Lock()
	methods := append([]string(nil), fake.methods...)
	fake.mu.Unlock()
	if len(methods) != 2 || methods[1] != "d.start" {
		t.Fatalf("调用链 = %v", methods)
	}
}

func TestRTorrentRequestFailureIsTranslated(t *testing.T) {
	adapter, fake := newRTorrentFake(t)
	fake.status = http.StatusBadGateway
	if _, err := adapter.ListDownloading(context.Background()); err == nil {
		t.Fatal("上游失败应报错")
	}
	if _, _, err := adapter.InspectControlTask(context.Background(), testTaskHash); err == nil {
		t.Fatal("查询失败应报错")
	}
}
