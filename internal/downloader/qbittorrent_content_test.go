package downloader

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"math"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	qbittorrent "github.com/autobrr/go-qbittorrent"
)

func TestQBittorrentInspectTaskContent(t *testing.T) {
	hash := strings.Repeat("a", 40)
	label := "mp-" + strings.Repeat("b", 32)
	trackerSecret := "https://tracker.invalid/announce?passkey=TOPSECRET"

	tests := []struct {
		name          string
		files         qbittorrent.TorrentFiles
		wantFiles     []TaskContentFile
		wantTotalSize int64
	}{
		{
			name: "单文件",
			files: qbittorrent.TorrentFiles{
				{Name: "Fixture.mkv", Priority: 1, Progress: 1, Size: 1024},
			},
			wantFiles: []TaskContentFile{
				{RelativePath: "Fixture.mkv", SizeBytes: 1024},
			},
			wantTotalSize: 1024,
		},
		{
			name: "多文件忽略未选择文件",
			files: qbittorrent.TorrentFiles{
				{Name: `Fixture\Video.mkv`, Priority: 1, Progress: 1, Size: 2048},
				{Name: "Fixture/Subtitles.srt", Priority: 7, Progress: 1, Size: 512},
				{Name: "Fixture/Unselected.nfo", Priority: 0, Progress: 1, Size: 64},
				{Name: "Fixture/Incomplete.mkv", Priority: 0, Progress: 0.5, Size: 4096},
			},
			wantFiles: []TaskContentFile{
				{RelativePath: "Fixture/Video.mkv", SizeBytes: 2048},
				{RelativePath: "Fixture/Subtitles.srt", SizeBytes: 512},
			},
			wantTotalSize: 6720,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			requests := make(chan *http.Request, 3)
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				switch r.URL.Path {
				case "/api/v2/torrents/info":
					requests <- r.Clone(context.Background())
					_, _ = io.WriteString(w, `[{"hash":"`+hash+`","name":"Fixture","total_size":`+
						int64String(tt.wantTotalSize)+`,"progress":1,"state":"uploading","tags":"`+label+
						`","save_path":"/downloads","tracker":"`+trackerSecret+`"}]`)
				case "/api/v2/torrents/files":
					requests <- r.Clone(context.Background())
					if err := json.NewEncoder(w).Encode(tt.files); err != nil {
						t.Errorf("编码文件响应: %v", err)
					}
				case "/api/v2/app/webapiVersion":
					_, _ = io.WriteString(w, "2.11.0")
				default:
					http.NotFound(w, r)
				}
			}))
			t.Cleanup(server.Close)

			got, found, err := newTestQBittorrent(server).InspectTaskContent(t.Context(), "", hash)
			if err != nil {
				t.Fatalf("InspectTaskContent() error = %v", err)
			}
			if !found {
				t.Fatal("InspectTaskContent() found = false")
			}
			if got.Task.Hash != strings.ToUpper(hash) || got.Task.State != TaskStateCompleted || got.Task.SavePath != "/downloads" {
				t.Fatalf("InspectTaskContent().Task = %#v", got.Task)
			}
			if !reflect.DeepEqual(got.Files, tt.wantFiles) {
				t.Fatalf("InspectTaskContent().Files = %#v, want %#v", got.Files, tt.wantFiles)
			}

			infoRequest := <-requests
			filesRequest := <-requests
			verificationRequest := <-requests
			if infoRequest.URL.Path != "/api/v2/torrents/info" || infoRequest.URL.Query().Get("hashes") != strings.ToUpper(hash) {
				t.Fatalf("任务请求 = %s?%s", infoRequest.URL.Path, infoRequest.URL.RawQuery)
			}
			if filesRequest.URL.Path != "/api/v2/torrents/files" || filesRequest.URL.Query().Get("hash") != strings.ToUpper(hash) {
				t.Fatalf("文件请求 = %s?%s", filesRequest.URL.Path, filesRequest.URL.RawQuery)
			}
			if verificationRequest.URL.Path != "/api/v2/torrents/info" || verificationRequest.URL.Query().Get("hashes") != strings.ToUpper(hash) {
				t.Fatalf("复查请求 = %s?%s", verificationRequest.URL.Path, verificationRequest.URL.RawQuery)
			}

			encoded, marshalErr := json.Marshal(got)
			if marshalErr != nil {
				t.Fatalf("编码快照: %v", marshalErr)
			}
			if strings.Contains(string(encoded), trackerSecret) {
				t.Fatalf("InspectTaskContent() 泄露 tracker: %s", encoded)
			}
		})
	}
}

func TestQBittorrentInspectTaskContentUsesDeterministicIdentity(t *testing.T) {
	hash := strings.Repeat("a", 40)
	normalizedHash := strings.ToUpper(hash)
	decoyHash := strings.Repeat("b", 40)
	label := "mp-" + strings.Repeat("6", 32)

	matching := completedTaskContentTorrent(hash)
	matching.Tags = label
	decoy := completedTaskContentTorrent(decoyHash)
	decoy.Tags = label
	withoutLabel := matching
	withoutLabel.Tags = "user-tag"

	labelRequest := taskContentIdentityRequest{
		path: "/api/v2/torrents/info", tag: label, filter: "all", limit: "20",
	}
	hashRequest := taskContentIdentityRequest{
		path: "/api/v2/torrents/info", hashes: normalizedHash,
	}
	filesRequest := taskContentIdentityRequest{
		path: "/api/v2/torrents/files", hash: normalizedHash,
	}

	tests := []struct {
		name           string
		labelResponses [][]qbittorrent.Torrent
		hashResponses  [][]qbittorrent.Torrent
		wantRequests   []taskContentIdentityRequest
		wantTaskTags   string
	}{
		{
			name: "两次标签查询都用 Hash 排除同标签干扰项",
			labelResponses: [][]qbittorrent.Torrent{
				{decoy, matching},
				{decoy, matching},
			},
			wantRequests: []taskContentIdentityRequest{
				labelRequest,
				filesRequest,
				labelRequest,
			},
			wantTaskTags: label,
		},
		{
			name: "标签从首次查询起被移除时两次都按 Hash 回退",
			labelResponses: [][]qbittorrent.Torrent{
				{withoutLabel},
				{withoutLabel},
			},
			hashResponses: [][]qbittorrent.Torrent{
				{withoutLabel},
				{withoutLabel},
			},
			wantRequests: []taskContentIdentityRequest{
				labelRequest,
				hashRequest,
				filesRequest,
				labelRequest,
				hashRequest,
			},
			wantTaskTags: "user-tag",
		},
		{
			name: "文件查询后标签被移除时复查按 Hash 回退",
			labelResponses: [][]qbittorrent.Torrent{
				{matching},
				{withoutLabel},
			},
			hashResponses: [][]qbittorrent.Torrent{
				{withoutLabel},
			},
			wantRequests: []taskContentIdentityRequest{
				labelRequest,
				filesRequest,
				labelRequest,
				hashRequest,
			},
			wantTaskTags: label,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			server, requests := newIdentityTaskContentServer(t, taskContentIdentityScript{
				labelResponses: tt.labelResponses,
				hashResponses:  tt.hashResponses,
				files: qbittorrent.TorrentFiles{
					{Name: "Fixture.mkv", Priority: 1, Progress: 1, Size: 1},
				},
			})

			got, found, err := newTestQBittorrent(server).InspectTaskContent(t.Context(), label, hash)
			if err != nil {
				t.Fatalf("InspectTaskContent() error = %v", err)
			}
			if !found {
				t.Fatal("InspectTaskContent() found = false")
			}
			if got.Task.Hash != normalizedHash || got.Task.Label != label || got.Task.Tags != tt.wantTaskTags {
				t.Fatalf("InspectTaskContent().Task identity = %#v", got.Task)
			}
			wantFiles := []TaskContentFile{{RelativePath: "Fixture.mkv", SizeBytes: 1}}
			if !reflect.DeepEqual(got.Files, wantFiles) {
				t.Fatalf("InspectTaskContent().Files = %#v, want %#v", got.Files, wantFiles)
			}
			if gotRequests := requests.snapshot(); !reflect.DeepEqual(gotRequests, tt.wantRequests) {
				t.Fatalf("qBittorrent requests = %#v, want %#v", gotRequests, tt.wantRequests)
			}
		})
	}
}

func TestQBittorrentInspectTaskContentRejectsIdentityMismatches(t *testing.T) {
	hash := strings.Repeat("c", 40)
	normalizedHash := strings.ToUpper(hash)
	changedHashValue := strings.Repeat("d", 40)
	label := "mp-" + strings.Repeat("9", 32)
	changedLabel := "mp-" + strings.Repeat("a", 32)
	const secret = "TOPSECRET-IDENTITY"

	initial := completedTaskContentTorrent(hash)
	initial.Tags = label
	changedHash := completedTaskContentTorrent(changedHashValue)
	changedHash.Tags = label
	changedHash.Name = secret
	changedHash.Tracker = secret
	changedLabelTask := initial
	changedLabelTask.Tags = changedLabel
	changedLabelTask.Name = secret
	changedLabelTask.Tracker = secret
	missingLabelTask := initial
	missingLabelTask.Tags = ""
	missingLabelTask.Name = secret
	missingLabelTask.Tracker = secret

	labelRequest := taskContentIdentityRequest{
		path: "/api/v2/torrents/info", tag: label, filter: "all", limit: "20",
	}
	hashRequest := taskContentIdentityRequest{
		path: "/api/v2/torrents/info", hashes: normalizedHash,
	}
	filesRequest := taskContentIdentityRequest{
		path: "/api/v2/torrents/files", hash: normalizedHash,
	}

	tests := []struct {
		name           string
		labelResponses [][]qbittorrent.Torrent
		hashResponses  [][]qbittorrent.Torrent
		wantErr        bool
		wantRequests   []taskContentIdentityRequest
	}{
		{
			name:           "首次查询返回错误 Hash",
			labelResponses: [][]qbittorrent.Torrent{{changedHash}},
			hashResponses:  [][]qbittorrent.Torrent{{changedHash}},
			wantRequests:   []taskContentIdentityRequest{labelRequest, hashRequest},
		},
		{
			name:           "首次标签响应缺失标签且 Hash 回退查不到",
			labelResponses: [][]qbittorrent.Torrent{{missingLabelTask}},
			hashResponses:  [][]qbittorrent.Torrent{{}},
			wantRequests:   []taskContentIdentityRequest{labelRequest, hashRequest},
		},
		{
			name:           "首次 Hash 回退返回另一个确定性标签",
			labelResponses: [][]qbittorrent.Torrent{{changedLabelTask}, {changedLabelTask}},
			hashResponses:  [][]qbittorrent.Torrent{{changedLabelTask}, {changedLabelTask}},
			wantRequests:   []taskContentIdentityRequest{labelRequest, hashRequest},
		},
		{
			name: "二次确认 Hash 变化",
			labelResponses: [][]qbittorrent.Torrent{
				{initial},
				{changedHash},
			},
			hashResponses: [][]qbittorrent.Torrent{{changedHash}},
			wantErr:       true,
			wantRequests:  []taskContentIdentityRequest{labelRequest, filesRequest, labelRequest, hashRequest},
		},
		{
			name: "二次确认标签变成另一个确定性标签",
			labelResponses: [][]qbittorrent.Torrent{
				{initial},
				{changedLabelTask},
			},
			hashResponses: [][]qbittorrent.Torrent{{changedLabelTask}},
			wantErr:       true,
			wantRequests:  []taskContentIdentityRequest{labelRequest, filesRequest, labelRequest, hashRequest},
		},
		{
			name: "二次确认标签响应缺失且 Hash 回退查不到",
			labelResponses: [][]qbittorrent.Torrent{
				{initial},
				{missingLabelTask},
			},
			hashResponses: [][]qbittorrent.Torrent{{}},
			wantErr:       true,
			wantRequests:  []taskContentIdentityRequest{labelRequest, filesRequest, labelRequest, hashRequest},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			server, requests := newIdentityTaskContentServer(t, taskContentIdentityScript{
				labelResponses: tt.labelResponses,
				hashResponses:  tt.hashResponses,
				files: qbittorrent.TorrentFiles{
					{Name: "Fixture.mkv", Priority: 1, Progress: 1, Size: 1},
				},
			})

			_, found, err := newTestQBittorrent(server).InspectTaskContent(t.Context(), label, hash)
			if found {
				t.Fatal("InspectTaskContent() found = true for mismatched identity")
			}
			if tt.wantErr {
				if err == nil || err.Error() != "qBittorrent 任务内容查询失败" {
					t.Fatalf("InspectTaskContent() error = %v, want stable sanitized error", err)
				}
			} else if err != nil {
				t.Fatalf("InspectTaskContent() error = %v, want nil for an unresolvable identity", err)
			}
			if err != nil {
				for _, forbidden := range []string{secret, server.URL, "fixture-api-key"} {
					if strings.Contains(err.Error(), forbidden) {
						t.Fatalf("InspectTaskContent() error 泄露 %q: %q", forbidden, err)
					}
				}
			}
			if gotRequests := requests.snapshot(); !reflect.DeepEqual(gotRequests, tt.wantRequests) {
				t.Fatalf("qBittorrent requests = %#v, want %#v", gotRequests, tt.wantRequests)
			}
		})
	}
}

func TestQBittorrentInspectTaskContentRejectsIncompleteSnapshots(t *testing.T) {
	hash := strings.Repeat("1", 40)
	completed := completedTaskContentTorrent(hash)
	active := completed
	active.Progress = 0.5
	active.State = qbittorrent.TorrentState("downloading")

	tests := []struct {
		name             string
		task             qbittorrent.Torrent
		files            qbittorrent.TorrentFiles
		wantTaskRequests int32
		wantFileRequests int32
	}{
		{
			name:             "任务尚未完成",
			task:             active,
			files:            qbittorrent.TorrentFiles{{Name: "Fixture.mkv", Priority: 1, Progress: 1, Size: 1}},
			wantTaskRequests: 1,
		},
		{
			name:             "已选择文件尚未完成",
			task:             completed,
			files:            qbittorrent.TorrentFiles{{Name: "Fixture.mkv", Priority: 1, Progress: 0.5, Size: 1}},
			wantTaskRequests: 1,
			wantFileRequests: 1,
		},
		{
			name:             "没有已选择文件",
			task:             completed,
			files:            qbittorrent.TorrentFiles{{Name: "Fixture.mkv", Priority: 0, Progress: 1, Size: 1}},
			wantTaskRequests: 1,
			wantFileRequests: 1,
		},
		{
			name:             "文件列表为空",
			task:             completed,
			files:            qbittorrent.TorrentFiles{},
			wantTaskRequests: 1,
			wantFileRequests: 1,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			server, requests := newScriptedTaskContentServer(
				t,
				[][]qbittorrent.Torrent{{tt.task}},
				tt.files,
			)
			_, found, err := newTestQBittorrent(server).InspectTaskContent(t.Context(), "", hash)
			if found {
				t.Fatal("InspectTaskContent() found = true on error")
			}
			if err == nil || err.Error() != "qBittorrent 任务内容查询失败" {
				t.Fatalf("InspectTaskContent() error = %v, want stable sanitized error", err)
			}
			if got := requests.task.Load(); got != tt.wantTaskRequests {
				t.Fatalf("任务请求数 = %d, want %d", got, tt.wantTaskRequests)
			}
			if got := requests.files.Load(); got != tt.wantFileRequests {
				t.Fatalf("文件请求数 = %d, want %d", got, tt.wantFileRequests)
			}
		})
	}
}

func TestQBittorrentInspectTaskContentRejectsTaskChanges(t *testing.T) {
	hash := strings.Repeat("2", 40)
	initial := completedTaskContentTorrent(hash)
	movedSavePath := initial
	movedSavePath.SavePath = "/moved"
	movedContentPath := initial
	movedContentPath.ContentPath = "/downloads/Other"
	active := initial
	active.Progress = 0.5
	active.State = qbittorrent.TorrentState("downloading")
	changedHash := initial
	changedHash.Hash = strings.Repeat("3", 40)

	tests := []struct {
		name   string
		second []qbittorrent.Torrent
	}{
		{name: "任务消失", second: []qbittorrent.Torrent{}},
		{name: "Hash 变化", second: []qbittorrent.Torrent{changedHash}},
		{name: "保存目录变化", second: []qbittorrent.Torrent{movedSavePath}},
		{name: "内容路径变化", second: []qbittorrent.Torrent{movedContentPath}},
		{name: "完成状态变化", second: []qbittorrent.Torrent{active}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			server, requests := newScriptedTaskContentServer(
				t,
				[][]qbittorrent.Torrent{{initial}, tt.second},
				qbittorrent.TorrentFiles{{Name: "Fixture.mkv", Priority: 1, Progress: 1, Size: 1}},
			)
			_, found, err := newTestQBittorrent(server).InspectTaskContent(t.Context(), "", hash)
			if found {
				t.Fatal("InspectTaskContent() found = true on error")
			}
			if err == nil || err.Error() != "qBittorrent 任务内容查询失败" {
				t.Fatalf("InspectTaskContent() error = %v, want stable sanitized error", err)
			}
			if got := requests.task.Load(); got != 2 {
				t.Fatalf("任务请求数 = %d, want 2", got)
			}
			if got := requests.files.Load(); got != 1 {
				t.Fatalf("文件请求数 = %d, want 1", got)
			}
		})
	}
}

func TestQBittorrentInspectTaskContentRejectsUnsafeFiles(t *testing.T) {
	hash := strings.Repeat("c", 40)

	tests := []struct {
		name  string
		files qbittorrent.TorrentFiles
	}{
		{
			name:  "Unix 绝对路径",
			files: qbittorrent.TorrentFiles{{Name: "/outside/file.mkv", Priority: 1, Progress: 1, Size: 1}},
		},
		{
			name:  "Windows 绝对路径",
			files: qbittorrent.TorrentFiles{{Name: `C:\outside\file.mkv`, Priority: 1, Progress: 1, Size: 1}},
		},
		{
			name:  "父目录穿越",
			files: qbittorrent.TorrentFiles{{Name: "Fixture/../outside.mkv", Priority: 1, Progress: 1, Size: 1}},
		},
		{
			name:  "NUL",
			files: qbittorrent.TorrentFiles{{Name: "Fixture/\x00outside.mkv", Priority: 1, Progress: 1, Size: 1}},
		},
		{
			name:  "路径过长",
			files: qbittorrent.TorrentFiles{{Name: strings.Repeat("a", maxTaskContentRelativePathLength+1), Priority: 1, Progress: 1, Size: 1}},
		},
		{
			name: "规范化后重复",
			files: qbittorrent.TorrentFiles{
				{Name: "Fixture//file.mkv", Priority: 1, Progress: 1, Size: 1},
				{Name: "Fixture/file.mkv", Priority: 1, Progress: 1, Size: 1},
			},
		},
		{
			name:  "负大小",
			files: qbittorrent.TorrentFiles{{Name: "Fixture/file.mkv", Priority: 1, Progress: 1, Size: -1}},
		},
		{
			name: "累计大小溢出",
			files: qbittorrent.TorrentFiles{
				{Name: "Fixture/one.mkv", Priority: 1, Progress: 1, Size: math.MaxInt64},
				{Name: "Fixture/two.mkv", Priority: 1, Progress: 1, Size: 1},
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			server := newTaskContentServer(t, hash, tt.files, http.StatusOK, "")
			_, found, err := newTestQBittorrent(server).InspectTaskContent(t.Context(), "", hash)
			if found {
				t.Fatal("InspectTaskContent() found = true on error")
			}
			if err == nil || err.Error() != "qBittorrent 任务内容查询失败" {
				t.Fatalf("InspectTaskContent() error = %v, want stable sanitized error", err)
			}
		})
	}
}

func TestQBittorrentInspectTaskContentRejectsTooManyFiles(t *testing.T) {
	hash := strings.Repeat("d", 40)
	files := make(qbittorrent.TorrentFiles, maxTaskContentFiles+1)
	for index := range files {
		files[index] = qbittorrent.TorrentFile{
			Name: "Fixture/" + int64String(int64(index)) + ".mkv", Priority: 1, Progress: 1, Size: 1,
		}
	}
	server := newTaskContentServer(t, hash, files, http.StatusOK, "")

	_, found, err := newTestQBittorrent(server).InspectTaskContent(t.Context(), "", hash)
	if found {
		t.Fatal("InspectTaskContent() found = true on error")
	}
	if err == nil || err.Error() != "qBittorrent 任务内容查询失败" {
		t.Fatalf("InspectTaskContent() error = %v, want stable sanitized error", err)
	}
}

func TestQBittorrentInspectTaskContentHonorsCancellation(t *testing.T) {
	hash := strings.Repeat("e", 40)
	tests := []struct {
		name  string
		stage string
	}{
		{name: "文件查询取消", stage: "files"},
		{name: "任务复查取消", stage: "verification"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			started := make(chan struct{})
			var taskRequests atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				switch r.URL.Path {
				case "/api/v2/torrents/info":
					if taskRequests.Add(1) == 2 && tt.stage == "verification" {
						close(started)
						<-r.Context().Done()
						return
					}
					_, _ = io.WriteString(w, taskContentInfoResponse(hash))
				case "/api/v2/torrents/files":
					if tt.stage == "files" {
						close(started)
						<-r.Context().Done()
						return
					}
					_, _ = io.WriteString(w, `[{"name":"Fixture.mkv","priority":1,"progress":1,"size":1}]`)
				default:
					http.NotFound(w, r)
				}
			}))
			t.Cleanup(server.Close)

			ctx, cancel := context.WithCancel(context.Background())
			result := make(chan error, 1)
			go func() {
				_, _, err := newTestQBittorrent(server).InspectTaskContent(ctx, "", hash)
				result <- err
			}()
			<-started
			cancel()

			select {
			case err := <-result:
				if !errors.Is(err, context.Canceled) {
					t.Fatalf("InspectTaskContent() error = %v, want context.Canceled", err)
				}
			case <-time.After(2 * time.Second):
				t.Fatal("InspectTaskContent() 未响应取消")
			}
		})
	}
}

func TestQBittorrentInspectTaskContentRedactsErrors(t *testing.T) {
	hash := strings.Repeat("f", 40)
	const secret = "https://tracker.invalid/announce?passkey=TOPSECRET"

	tests := []struct {
		name   string
		stage  string
		status int
		body   string
	}{
		{name: "文件查询 HTTP 错误", stage: "files", status: http.StatusInternalServerError, body: secret},
		{name: "文件查询无效 JSON", stage: "files", status: http.StatusOK, body: `{"name":"` + secret + `"`},
		{name: "任务复查 HTTP 错误", stage: "verification", status: http.StatusInternalServerError, body: secret},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var taskRequests atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				switch r.URL.Path {
				case "/api/v2/torrents/info":
					if taskRequests.Add(1) == 2 && tt.stage == "verification" {
						w.WriteHeader(tt.status)
						_, _ = io.WriteString(w, tt.body)
						return
					}
					_, _ = io.WriteString(w, taskContentInfoResponse(hash))
				case "/api/v2/torrents/files":
					if tt.stage == "files" {
						w.WriteHeader(tt.status)
						_, _ = io.WriteString(w, tt.body)
						return
					}
					_, _ = io.WriteString(w, `[{"name":"Fixture.mkv","priority":1,"progress":1,"size":1}]`)
				default:
					http.NotFound(w, r)
				}
			}))
			t.Cleanup(server.Close)
			_, found, err := newTestQBittorrent(server).InspectTaskContent(t.Context(), "", hash)
			if found {
				t.Fatal("InspectTaskContent() found = true on error")
			}
			if err == nil || err.Error() != "qBittorrent 任务内容查询失败" {
				t.Fatalf("InspectTaskContent() error = %v, want stable sanitized error", err)
			}
			for _, forbidden := range []string{secret, server.URL, "fixture-api-key"} {
				if strings.Contains(err.Error(), forbidden) {
					t.Fatalf("InspectTaskContent() error 泄露 %q: %q", forbidden, err)
				}
			}
		})
	}
}

func FuzzSafeTaskContentRelativePath(f *testing.F) {
	for _, seed := range []string{
		"Fixture/Video.mkv",
		`Fixture\Subs\subtitle.srt`,
		"../outside.mkv",
		"/absolute/file.mkv",
		`C:\outside\file.mkv`,
		"0:/outside/file.mkv",
		"Fixture/./Video.mkv",
	} {
		f.Add(seed)
	}
	f.Fuzz(func(t *testing.T, input string) {
		relative, ok := safeTaskContentRelativePath(input)
		if !ok {
			return
		}
		if relative == "" || filepath.IsAbs(filepath.FromSlash(relative)) ||
			strings.ContainsRune(relative, 0) || hasWindowsDrivePrefix(relative) {
			t.Fatalf("unsafe normalized path %q from %q", relative, input)
		}
		root := filepath.Join(string(filepath.Separator), "moviepilot-safe-root")
		candidate := filepath.Join(root, filepath.FromSlash(relative))
		within, err := filepath.Rel(root, candidate)
		if err != nil || within == ".." || strings.HasPrefix(within, ".."+string(filepath.Separator)) {
			t.Fatalf("normalized path escaped root: %q", relative)
		}
	})
}

type taskContentIdentityRequest struct {
	path   string
	tag    string
	hash   string
	hashes string
	filter string
	limit  string
}

type taskContentIdentityScript struct {
	labelResponses [][]qbittorrent.Torrent
	hashResponses  [][]qbittorrent.Torrent
	files          qbittorrent.TorrentFiles
}

type taskContentIdentityRequests struct {
	mu            sync.Mutex
	requests      []taskContentIdentityRequest
	labelResponse int
	hashResponse  int
}

func (r *taskContentIdentityRequests) snapshot() []taskContentIdentityRequest {
	r.mu.Lock()
	defer r.mu.Unlock()

	return append([]taskContentIdentityRequest(nil), r.requests...)
}

func newIdentityTaskContentServer(
	t *testing.T,
	script taskContentIdentityScript,
) (*httptest.Server, *taskContentIdentityRequests) {
	t.Helper()
	requests := &taskContentIdentityRequests{}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if request.URL.Path == "/api/v2/app/webapiVersion" {
			_, _ = io.WriteString(w, "2.11.0")
			return
		}
		query := request.URL.Query()
		captured := taskContentIdentityRequest{
			path:   request.URL.Path,
			tag:    query.Get("tag"),
			hash:   query.Get("hash"),
			hashes: query.Get("hashes"),
			filter: query.Get("filter"),
			limit:  query.Get("limit"),
		}

		requests.mu.Lock()
		requests.requests = append(requests.requests, captured)
		switch request.URL.Path {
		case "/api/v2/torrents/info":
			var response []qbittorrent.Torrent
			switch {
			case captured.tag != "":
				if requests.labelResponse < len(script.labelResponses) {
					response = script.labelResponses[requests.labelResponse]
				}
				requests.labelResponse++
			case captured.hashes != "":
				if requests.hashResponse < len(script.hashResponses) {
					response = script.hashResponses[requests.hashResponse]
				}
				requests.hashResponse++
			}
			requests.mu.Unlock()
			if response == nil {
				response = []qbittorrent.Torrent{}
			}
			if err := json.NewEncoder(w).Encode(response); err != nil {
				t.Errorf("编码任务响应: %v", err)
			}
		case "/api/v2/torrents/files":
			requests.mu.Unlock()
			if err := json.NewEncoder(w).Encode(script.files); err != nil {
				t.Errorf("编码文件响应: %v", err)
			}
		default:
			requests.mu.Unlock()
			http.NotFound(w, request)
		}
	}))
	t.Cleanup(server.Close)
	return server, requests
}

func newTaskContentServer(
	t *testing.T,
	hash string,
	files qbittorrent.TorrentFiles,
	filesStatus int,
	filesBody string,
) *httptest.Server {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/api/v2/torrents/info":
			_, _ = io.WriteString(w, taskContentInfoResponse(hash))
		case "/api/v2/torrents/files":
			w.WriteHeader(filesStatus)
			if filesBody != "" {
				_, _ = io.WriteString(w, filesBody)
				return
			}
			if err := json.NewEncoder(w).Encode(files); err != nil {
				t.Errorf("编码文件响应: %v", err)
			}
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(server.Close)
	return server
}

type taskContentRequestCounts struct {
	task  atomic.Int32
	files atomic.Int32
}

func newScriptedTaskContentServer(
	t *testing.T,
	taskResponses [][]qbittorrent.Torrent,
	files qbittorrent.TorrentFiles,
) (*httptest.Server, *taskContentRequestCounts) {
	t.Helper()
	requests := &taskContentRequestCounts{}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/api/v2/torrents/info":
			index := int(requests.task.Add(1)) - 1
			if index >= len(taskResponses) {
				index = len(taskResponses) - 1
			}
			if err := json.NewEncoder(w).Encode(taskResponses[index]); err != nil {
				t.Errorf("编码任务响应: %v", err)
			}
		case "/api/v2/torrents/files":
			requests.files.Add(1)
			if err := json.NewEncoder(w).Encode(files); err != nil {
				t.Errorf("编码文件响应: %v", err)
			}
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(server.Close)
	return server, requests
}

func completedTaskContentTorrent(hash string) qbittorrent.Torrent {
	return qbittorrent.Torrent{
		Hash: hash, Name: "Fixture", TotalSize: 1, Progress: 1,
		State: qbittorrent.TorrentState("uploading"), SavePath: "/downloads",
		ContentPath: "/downloads/Fixture",
	}
}

func taskContentInfoResponse(hash string) string {
	return `[{"hash":"` + hash + `","name":"Fixture","total_size":1,"progress":1,"state":"uploading","save_path":"/downloads"}]`
}

func int64String(value int64) string {
	return strconv.FormatInt(value, 10)
}
