package downloader

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	qbittorrent "github.com/autobrr/go-qbittorrent"
)

func TestQBittorrentFindByHash(t *testing.T) {
	tests := []struct {
		name         string
		input        string
		response     string
		wantQuery    string
		wantHash     string
		wantFound    bool
		wantRequests int32
	}{
		{
			name:         "规范化 v1 hash 并找到任务",
			input:        "  aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa  ",
			response:     `[{"hash":"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"}]`,
			wantQuery:    "AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA",
			wantHash:     "AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA",
			wantFound:    true,
			wantRequests: 1,
		},
		{
			name:         "支持 v2 hash",
			input:        strings.Repeat("b", 64),
			response:     `[{"hash":"` + strings.Repeat("b", 64) + `"}]`,
			wantQuery:    strings.Repeat("B", 64),
			wantHash:     strings.Repeat("B", 64),
			wantFound:    true,
			wantRequests: 1,
		},
		{
			name:         "支持仅返回 infohash_v2 的任务",
			input:        strings.Repeat("d", 64),
			response:     `[{"hash":"","infohash_v2":"` + strings.Repeat("d", 64) + `"}]`,
			wantQuery:    strings.Repeat("D", 64),
			wantHash:     strings.Repeat("D", 64),
			wantFound:    true,
			wantRequests: 1,
		},
		{
			name:         "没有匹配任务",
			input:        strings.Repeat("c", 40),
			response:     `[]`,
			wantQuery:    strings.Repeat("C", 40),
			wantRequests: 1,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var requests atomic.Int32
			queryCh := make(chan string, 1)
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				requests.Add(1)
				queryCh <- r.URL.Query().Get("hashes")
				if r.Method != http.MethodGet || r.URL.Path != "/api/v2/torrents/info" {
					http.Error(w, "unexpected request", http.StatusNotFound)
					return
				}
				w.Header().Set("Content-Type", "application/json")
				_, _ = io.WriteString(w, tt.response)
			}))
			t.Cleanup(server.Close)

			q := newTestQBittorrent(server)
			gotHash, found, err := q.FindByHash(context.Background(), tt.input)
			if err != nil {
				t.Fatalf("FindByHash() error = %v", err)
			}
			if gotHash != tt.wantHash || found != tt.wantFound {
				t.Fatalf("FindByHash() = (%q, %v), want (%q, %v)", gotHash, found, tt.wantHash, tt.wantFound)
			}
			if gotQuery := <-queryCh; gotQuery != tt.wantQuery {
				t.Errorf("hashes query = %q, want %q", gotQuery, tt.wantQuery)
			}
			if got := requests.Load(); got != tt.wantRequests {
				t.Errorf("request count = %d, want %d", got, tt.wantRequests)
			}
		})
	}
}

func TestNewQBittorrentUsesAPIKeyWithoutCredentialLeak(t *testing.T) {
	t.Parallel()

	const apiKey = "fixture-secret-api-key"
	headerCh := make(chan string, 1)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		headerCh <- r.Header.Get("Authorization")
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `[]`)
	}))
	t.Cleanup(server.Close)

	client, err := NewQBittorrent(QBittorrentConfig{URL: server.URL, APIKey: apiKey})
	if err != nil {
		t.Fatal(err)
	}
	_, _, err = client.FindByHash(t.Context(), strings.Repeat("a", 40))
	if err != nil {
		t.Fatalf("FindByHash() error = %v", err)
	}
	if got := <-headerCh; got != "Bearer "+apiKey {
		t.Fatalf("Authorization = %q", got)
	}
}

func TestQBittorrentFindByHashRejectsInvalidInput(t *testing.T) {
	tests := []struct {
		name string
		hash string
	}{
		{name: "空字符串", hash: ""},
		{name: "只有空白", hash: " \t\r\n "},
		{name: "长度不足", hash: strings.Repeat("a", 39)},
		{name: "长度过长", hash: strings.Repeat("a", 41)},
		{name: "包含非十六进制字符", hash: strings.Repeat("g", 40)},
		{name: "无效 v2 长度", hash: strings.Repeat("a", 63)},
	}

	var requests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		requests.Add(1)
		w.WriteHeader(http.StatusInternalServerError)
	}))
	t.Cleanup(server.Close)
	q := newTestQBittorrent(server)

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, _, err := q.FindByHash(context.Background(), tt.hash)
			if err == nil || err.Error() != "种子 Hash 无效" {
				t.Fatalf("FindByHash() error = %v, want 种子 Hash 无效", err)
			}
		})
	}
	if got := requests.Load(); got != 0 {
		t.Fatalf("invalid hashes triggered %d HTTP requests", got)
	}
}

func TestQBittorrentListDownloading(t *testing.T) {
	const trackerSecret = "https://tracker.invalid/announce?passkey=TOPSECRET"
	labelA := "mp-" + strings.Repeat("a", 32)
	labelB := "mp-" + strings.Repeat("b", 32)
	labelC := "mp-" + strings.Repeat("c", 32)

	tests := []struct {
		name     string
		response string
		want     []Task
	}{
		{
			name: "筛选 MoviePilot 标签并映射中立任务",
			response: `[` +
				`{"hash":"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa","name":"Movie","total_size":1024,"progress":0.125,"state":"stalledDL","dlspeed":128,"upspeed":32,"eta":9,"tags":"other, ` + labelA + `","save_path":"/downloads","category":"movies","dl_limit":512,"up_limit":256,"ratio_limit":2.5,"seeding_time_limit":60,"tracker":"` + trackerSecret + `"},` +
				`{"hash":"bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb","name":"Negative","total_size":-1,"progress":-0.25,"state":"pausedDL","dlspeed":-2,"upspeed":-3,"eta":-4,"tags":"` + labelB + `, extra","content_path":"/ready/Negative","dl_limit":-1,"up_limit":-1,"ratio_limit":-2,"seeding_time_limit":-1},` +
				`{"hash":"cccccccccccccccccccccccccccccccccccccccc","name":"Clamped","progress":1.5,"state":"uploading","tags":"` + labelC + `, ` + labelA + `"},` +
				`{"hash":"dddddddddddddddddddddddddddddddddddddddd","name":"Uppercase tag","tags":"mp-AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA"},` +
				`{"hash":"eeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeee","name":"Short tag","tags":"mp-` + strings.Repeat("e", 31) + `"},` +
				`{"hash":"ffffffffffffffffffffffffffffffffffffffff","name":"Embedded tag","tags":"prefix-` + labelA + `"},` +
				`{"hash":"1111111111111111111111111111111111111111","name":"No tag","tags":"other"}` +
				`]`,
			want: []Task{
				{
					Hash: "AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA", Title: "Movie",
					SizeBytes: 1024, ProgressPercent: 12.5, State: TaskStateDownloading,
					DownSpeedBytes: 128, UpSpeedBytes: 32, ETASeconds: 9,
					Label: labelA, Tags: "other, " + labelA, SavePath: "/downloads",
					ContentPath: "/downloads/Movie", Category: "movies",
					DownloadLimitBytes: 512, UploadLimitBytes: 256,
					RatioLimit: 2.5, SeedingTimeLimit: 60,
				},
				{
					Hash: "BBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBB", Title: "Negative",
					ProgressPercent: 0, State: TaskStatePaused,
					Label: labelB, Tags: labelB + ", extra", ContentPath: "/ready/Negative",
					DownloadLimitBytes: -1, UploadLimitBytes: -1,
					RatioLimit: -2, SeedingTimeLimit: -1,
				},
				{
					Hash: "CCCCCCCCCCCCCCCCCCCCCCCCCCCCCCCCCCCCCCCC", Title: "Clamped",
					ProgressPercent: 100, State: TaskStateCompleted,
					Label: labelC, Tags: labelC + ", " + labelA, ContentPath: "Clamped",
				},
			},
		},
		{
			name:     "没有合法 MoviePilot 标签时返回空列表",
			response: `[{"hash":"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa","tags":"other"}]`,
			want:     []Task{},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			requestCh := make(chan *http.Request, 1)
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path == "/api/v2/app/webapiVersion" {
					_, _ = io.WriteString(w, "5.0.0")
					return
				}
				requestCh <- r.Clone(context.Background())
				w.Header().Set("Content-Type", "application/json")
				_, _ = io.WriteString(w, tt.response)
			}))
			t.Cleanup(server.Close)

			got, err := newTestQBittorrent(server).ListDownloading(t.Context())
			if err != nil {
				t.Fatalf("ListDownloading() error = %v", err)
			}
			if !reflect.DeepEqual(got, tt.want) {
				t.Fatalf("ListDownloading() = %#v, want %#v", got, tt.want)
			}

			request := <-requestCh
			if request.Method != http.MethodGet || request.URL.Path != "/api/v2/torrents/info" {
				t.Errorf("request = %s %s, want GET /api/v2/torrents/info", request.Method, request.URL.Path)
			}
			if gotFilter := request.URL.Query().Get("filter"); gotFilter != "downloading" {
				t.Errorf("filter = %q, want downloading", gotFilter)
			}
			if gotLimit := request.URL.Query().Get("limit"); gotLimit != "1000" {
				t.Errorf("limit = %q, want 1000", gotLimit)
			}
			if includeTrackers := request.URL.Query().Get("includeTrackers"); includeTrackers != "" {
				t.Errorf("includeTrackers = %q, want omitted", includeTrackers)
			}

			encoded, marshalErr := json.Marshal(got)
			if marshalErr != nil {
				t.Fatalf("marshal tasks: %v", marshalErr)
			}
			if strings.Contains(string(encoded), trackerSecret) {
				t.Fatalf("ListDownloading() exposed tracker URL: %s", encoded)
			}
		})
	}
}

func TestQBittorrentListDownloadingCapsResults(t *testing.T) {
	label := "mp-" + strings.Repeat("a", 32)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		torrents := make([]qbittorrent.Torrent, maxDownloadingTasks+1)
		for i := range torrents {
			torrents[i] = qbittorrent.Torrent{Hash: strings.Repeat("a", 40), Tags: label}
		}
		w.Header().Set("Content-Type", "application/json")
		if err := json.NewEncoder(w).Encode(torrents); err != nil {
			t.Errorf("encode response: %v", err)
		}
	}))
	t.Cleanup(server.Close)

	tasks, err := newTestQBittorrent(server).ListDownloading(t.Context())
	if err != nil {
		t.Fatalf("ListDownloading() error = %v", err)
	}
	if len(tasks) != maxDownloadingTasks {
		t.Fatalf("len(ListDownloading()) = %d, want %d", len(tasks), maxDownloadingTasks)
	}
}

func TestQBittorrentListDownloadingRedactsErrors(t *testing.T) {
	tests := []struct {
		name     string
		status   int
		response string
		secret   string
	}{
		{name: "HTTP 错误", status: http.StatusInternalServerError, response: "backend-passkey-secret", secret: "backend-passkey-secret"},
		{name: "无效 JSON", status: http.StatusOK, response: `{"tracker":"https://tracker.invalid/secret"`, secret: "https://tracker.invalid/secret"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(tt.status)
				_, _ = io.WriteString(w, tt.response)
			}))
			t.Cleanup(server.Close)

			_, err := newTestQBittorrent(server).ListDownloading(t.Context())
			if err == nil || err.Error() != "qBittorrent 下载任务查询失败" {
				t.Fatalf("ListDownloading() error = %v, want sanitized error", err)
			}
			for _, secret := range []string{tt.secret, server.URL} {
				if strings.Contains(err.Error(), secret) {
					t.Errorf("ListDownloading() error leaked %q: %q", secret, err)
				}
			}
		})
	}
}

func TestQBittorrentListDownloadingWaitHonorsContext(t *testing.T) {
	label := "mp-" + strings.Repeat("a", 32)
	started := make(chan struct{})
	release := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/v2/app/webapiVersion" {
			_, _ = io.WriteString(w, "5.0.0")
			return
		}
		close(started)
		<-release
		_, _ = io.WriteString(w, `[{"hash":"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa","tags":"`+label+`"}]`)
	}))
	t.Cleanup(server.Close)
	client := newTestQBittorrent(server)
	firstDone := make(chan error, 1)
	go func() {
		_, err := client.ListDownloading(context.Background())
		firstDone <- err
	}()
	<-started

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := client.ListDownloading(ctx); !errors.Is(err, context.Canceled) {
		t.Fatalf("等待查询许可 error = %v，want context.Canceled", err)
	}
	close(release)
	if err := <-firstDone; err != nil {
		t.Fatalf("第一个 ListDownloading() error = %v", err)
	}
}

func TestQBittorrentAddTorrentBytesMultipartRequest(t *testing.T) {
	tests := []struct {
		name       string
		paused     bool
		wantPaused string
	}{
		{name: "立即开始", paused: false, wantPaused: "false"},
		{name: "暂停添加", paused: true, wantPaused: "true"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			metainfo := []byte("d4:infod4:name7:fixtureee")
			requestCh := make(chan capturedAddRequest, 1)
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				captured := captureAddRequest(r)
				requestCh <- captured
				if captured.err != nil {
					http.Error(w, "invalid multipart", http.StatusBadRequest)
					return
				}
				w.Header().Set("Content-Type", "application/json")
				_, _ = io.WriteString(w, `{"success_count":1,"pending_count":0,"failure_count":0}`)
			}))
			t.Cleanup(server.Close)

			q := newTestQBittorrent(server)
			if err := q.AddTorrentBytes(context.Background(), metainfo, "/downloads/movies", "mp-fixture", tt.paused); err != nil {
				t.Fatalf("AddTorrentBytes() error = %v", err)
			}
			captured := <-requestCh
			if captured.err != nil {
				t.Fatalf("capture request: %v", captured.err)
			}
			if captured.method != http.MethodPost || captured.path != "/api/v2/torrents/add" {
				t.Errorf("request = %s %s, want POST /api/v2/torrents/add", captured.method, captured.path)
			}
			if !strings.HasPrefix(captured.contentType, "multipart/form-data;") {
				t.Errorf("Content-Type = %q, want multipart/form-data", captured.contentType)
			}
			if string(captured.metainfo) != string(metainfo) {
				t.Errorf("torrent field = %q, want %q", captured.metainfo, metainfo)
			}
			if got := captured.fields["savepath"]; got != "/downloads/movies" {
				t.Errorf("savepath = %q", got)
			}
			if got := captured.fields["tags"]; got != "mp-fixture" {
				t.Errorf("tags = %q", got)
			}
			if got := captured.fields["paused"]; got != tt.wantPaused {
				t.Errorf("paused = %q, want %q", got, tt.wantPaused)
			}
			if got := captured.fields["stopped"]; got != tt.wantPaused {
				t.Errorf("stopped = %q, want %q", got, tt.wantPaused)
			}
		})
	}
}

func TestQBittorrentAddTorrentBytesValidatesSize(t *testing.T) {
	tests := []struct {
		name     string
		metainfo []byte
		wantErr  string
	}{
		{name: "nil", metainfo: nil, wantErr: "种子文件内容为空"},
		{name: "空切片", metainfo: []byte{}, wantErr: "种子文件内容为空"},
		{name: "超过硬上限", metainfo: make([]byte, maxTorrentMetainfoSize+1), wantErr: "种子文件超过大小上限"},
	}

	var requests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		requests.Add(1)
		w.WriteHeader(http.StatusNoContent)
	}))
	t.Cleanup(server.Close)
	q := newTestQBittorrent(server)

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := q.AddTorrentBytes(context.Background(), tt.metainfo, "", "", false)
			if err == nil || err.Error() != tt.wantErr {
				t.Fatalf("AddTorrentBytes() error = %v, want %q", err, tt.wantErr)
			}
		})
	}
	if got := requests.Load(); got != 0 {
		t.Fatalf("invalid metainfo triggered %d HTTP requests", got)
	}
}

func TestQBittorrentAddTorrentBytesResponseSemantics(t *testing.T) {
	tests := []struct {
		name     string
		response string
		wantErr  string
	}{
		{
			name:     "全部失败视为拒绝",
			response: `{"success_count":0,"pending_count":0,"failure_count":1}`,
			wantErr:  "下载器明确拒绝了下载任务: qBittorrent 拒绝了下载任务",
		},
		{
			name:     "仍有待处理不视为拒绝",
			response: `{"success_count":0,"pending_count":1,"failure_count":1}`,
		},
		{
			name:     "成功",
			response: `{"success_count":1,"pending_count":0,"failure_count":0}`,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				_, _ = io.WriteString(w, tt.response)
			}))
			t.Cleanup(server.Close)
			q := newTestQBittorrent(server)

			err := q.AddTorrentBytes(context.Background(), []byte("fixture"), "", "", false)
			if tt.wantErr == "" {
				if err != nil {
					t.Fatalf("AddTorrentBytes() error = %v", err)
				}
				return
			}
			if err == nil || err.Error() != tt.wantErr {
				t.Fatalf("AddTorrentBytes() error = %v, want %q", err, tt.wantErr)
			}
			if tt.name == "全部失败视为拒绝" && !errors.Is(err, ErrDefinitiveAddFailure) {
				t.Fatalf("AddTorrentBytes() error = %v, want ErrDefinitiveAddFailure", err)
			}
		})
	}
}

func TestQBittorrentAddTorrentBytesRedactsErrors(t *testing.T) {
	const responseSecret = "response-body-secret"
	metainfo := []byte("announce=https://tracker.invalid/passkey/TOPSECRET")
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, responseSecret, http.StatusInternalServerError)
	}))
	t.Cleanup(server.Close)
	q := newTestQBittorrent(server)

	err := q.AddTorrentBytes(context.Background(), metainfo, "/downloads", "mp-secret", false)
	if err == nil {
		t.Fatal("AddTorrentBytes() error = nil")
	}
	if err.Error() != "qBittorrent 添加种子文件失败" {
		t.Fatalf("AddTorrentBytes() error = %q", err)
	}
	for _, secret := range []string{"TOPSECRET", responseSecret, server.URL, string(metainfo)} {
		if strings.Contains(err.Error(), secret) {
			t.Errorf("error leaked %q: %q", secret, err)
		}
	}
}

func TestQBittorrentAddRedactsErrorsAndMarksDefinitiveRejection(t *testing.T) {
	tests := []struct {
		name       string
		status     int
		response   string
		wantErr    string
		definitive bool
	}{
		{
			name:     "HTTP 错误脱敏",
			status:   http.StatusInternalServerError,
			response: "response-body-secret",
			wantErr:  "qBittorrent 添加下载任务失败",
		},
		{
			name:       "明确拒绝使用领域哨兵",
			status:     http.StatusOK,
			response:   `{"success_count":0,"pending_count":0,"failure_count":1}`,
			wantErr:    "下载器明确拒绝了下载任务: qBittorrent 拒绝了下载任务",
			definitive: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(tt.status)
				_, _ = io.WriteString(w, tt.response)
			}))
			t.Cleanup(server.Close)
			q := newTestQBittorrent(server)
			candidate := Candidate{DownloadURL: server.URL + "/download?passkey=TOPSECRET"}

			err := q.Add(context.Background(), candidate, "mp-fixture")
			if err == nil || err.Error() != tt.wantErr {
				t.Fatalf("Add() error = %v, want %q", err, tt.wantErr)
			}
			if errors.Is(err, ErrDefinitiveAddFailure) != tt.definitive {
				t.Fatalf("Add() definitive = %v, want %v", errors.Is(err, ErrDefinitiveAddFailure), tt.definitive)
			}
			for _, secret := range []string{"TOPSECRET", tt.response, server.URL} {
				if secret != "" && strings.Contains(err.Error(), secret) {
					t.Fatalf("Add() error 泄露 %q: %q", secret, err)
				}
			}
		})
	}
}

type capturedAddRequest struct {
	method      string
	path        string
	contentType string
	metainfo    []byte
	fields      map[string]string
	err         error
}

func captureAddRequest(r *http.Request) capturedAddRequest {
	captured := capturedAddRequest{
		method: r.Method, path: r.URL.Path, contentType: r.Header.Get("Content-Type"),
		fields: make(map[string]string),
	}
	if err := r.ParseMultipartForm(int64(maxTorrentMetainfoSize + (1 << 20))); err != nil {
		captured.err = err
		return captured
	}
	file, _, err := r.FormFile("torrents")
	if err != nil {
		captured.err = err
		return captured
	}
	captured.metainfo, captured.err = io.ReadAll(file)
	if closeErr := file.Close(); captured.err == nil && closeErr != nil {
		captured.err = closeErr
	}
	for _, key := range []string{"savepath", "tags", "paused", "stopped"} {
		captured.fields[key] = r.FormValue(key)
	}
	return captured
}

func newTestQBittorrent(server *httptest.Server) *QBittorrent {
	client := qbittorrent.NewClient(qbittorrent.Config{
		Host: server.URL, APIKey: "fixture-api-key", Timeout: 2, RetryAttempts: 1,
	})
	client.WithHTTPClient(server.Client())
	return &QBittorrent{client: client, queryPermit: make(chan struct{}, 1), filesRetryInterval: time.Millisecond}
}
