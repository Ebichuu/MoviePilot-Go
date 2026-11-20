package httpapi

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"reflect"
	"strconv"
	"strings"
	"testing"

	"github.com/golang-jwt/jwt/v5"

	"github.com/moviepilot-go/moviepilot-go/internal/auth"
	"github.com/moviepilot-go/moviepilot-go/internal/database"
	"github.com/moviepilot-go/moviepilot-go/internal/downloader"
)

const (
	downloadListAliceLabel     = "mp-aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	downloadListBobLabel       = "mp-bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
	downloadListUnknownLabel   = "mp-cccccccccccccccccccccccccccccccc"
	downloadListSecondaryLabel = "mp-dddddddddddddddddddddddddddddddd"
)

func TestListDownloadsScopesKnownOwnersAndForwardsName(t *testing.T) {
	tests := []struct {
		name       string
		path       string
		user       auth.User
		wantHashes []string
	}{
		{
			name:       "普通用户通过无尾斜杠入口只见自己的任务",
			path:       "/download?name=" + url.QueryEscape(" qB Primary "),
			user:       auth.User{ID: 11, Username: "alice"},
			wantHashes: []string{"alice-hash"},
		},
		{
			name:       "管理员通过尾斜杠入口看见全部已知 owner",
			path:       "/download/?name=" + url.QueryEscape(" qB Primary "),
			user:       auth.User{ID: 1, Username: "admin", SuperUser: true},
			wantHashes: []string{"alice-hash", "bob-hash"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			source := &fakeDownloadTaskSource{tasks: []NamedDownloadTask{
				{Downloader: "qB Primary", Task: downloader.Task{Hash: "alice-hash", Title: "Alice Movie 2026", Label: downloadListAliceLabel}},
				{Downloader: "qB Primary", Task: downloader.Task{Hash: "bob-hash", Title: "Bob Movie 2026", Label: downloadListBobLabel}},
				{Downloader: "qB Primary", Task: downloader.Task{Hash: "unknown-hash", Title: "Unknown Movie 2026", Label: downloadListUnknownLabel}},
				{Downloader: "qB Secondary", Task: downloader.Task{Hash: "secondary-hash", Title: "Secondary Movie 2026", Label: downloadListSecondaryLabel}},
			}}
			owners := &fakeDownloadIntentOwnerStore{owners: map[string]database.DownloadIntentOwner{
				downloadListAliceLabel:     {UserID: 11, Username: "alice"},
				downloadListBobLabel:       {UserID: 12, Username: "bob"},
				downloadListSecondaryLabel: {UserID: 13, Username: "carol"},
			}}
			deps := Dependencies{
				Users:          fakeUserLookup{user: tt.user},
				DownloadTasks:  source,
				DownloadOwners: owners,
			}

			response := httptest.NewRecorder()
			deps.listDownloads(response, downloadListRequest(tt.path, tt.user))

			if response.Code != http.StatusOK {
				t.Fatalf("status = %d, want %d; body=%s", response.Code, http.StatusOK, response.Body.String())
			}
			if source.name != "qB Primary" {
				t.Fatalf("ListDownloading() name = %q, want %q", source.name, "qB Primary")
			}
			if !reflect.DeepEqual(owners.labels, []string{
				downloadListAliceLabel,
				downloadListBobLabel,
				downloadListUnknownLabel,
			}) {
				t.Fatalf("owner labels = %#v", owners.labels)
			}

			var items []map[string]any
			if err := json.Unmarshal(response.Body.Bytes(), &items); err != nil {
				t.Fatalf("响应不是 V2 裸数组: %v; body=%s", err, response.Body.String())
			}
			gotHashes := make([]string, 0, len(items))
			for _, item := range items {
				gotHashes = append(gotHashes, item["hash"].(string))
				if item["downloader"] != "qB Primary" {
					t.Fatalf("downloader = %#v", item["downloader"])
				}
			}
			if !reflect.DeepEqual(gotHashes, tt.wantHashes) {
				t.Fatalf("hashes = %#v, want %#v", gotHashes, tt.wantHashes)
			}
		})
	}
}

func TestListDownloadsProjectsV2FieldsAndFormatsValues(t *testing.T) {
	source := &fakeDownloadTaskSource{tasks: []NamedDownloadTask{{
		Downloader: "qB",
		Task: downloader.Task{
			Hash:               "safe-hash",
			Title:              "示例剧 S02E03 2026",
			SizeBytes:          3 * 1024 * 1024,
			ProgressPercent:    37.5,
			State:              downloader.TaskStateDownloading,
			DownSpeedBytes:     1536,
			UpSpeedBytes:       1024 * 1024,
			ETASeconds:         3661,
			Label:              downloadListAliceLabel,
			Tags:               "影视, " + downloadListAliceLabel + ", " + downloadListUnknownLabel,
			SavePath:           "/downloads",
			ContentPath:        "/downloads/example",
			Category:           "movie",
			DownloadLimitBytes: 2048,
			UploadLimitBytes:   4096,
			RatioLimit:         2.5,
			SeedingTimeLimit:   3600,
		},
	}}}
	owners := &fakeDownloadIntentOwnerStore{owners: map[string]database.DownloadIntentOwner{
		downloadListAliceLabel: {UserID: 11, Username: "alice"},
	}}
	user := auth.User{ID: 11, Username: "alice"}
	deps := Dependencies{Users: fakeUserLookup{user: user}, DownloadTasks: source, DownloadOwners: owners}
	response := httptest.NewRecorder()

	deps.listDownloads(response, downloadListRequest("/download/", user))

	if response.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d; body=%s", response.Code, http.StatusOK, response.Body.String())
	}
	var items []map[string]any
	if err := json.Unmarshal(response.Body.Bytes(), &items); err != nil || len(items) != 1 {
		t.Fatalf("items = %#v, err=%v; body=%s", items, err, response.Body.String())
	}
	item := items[0]
	wantKeys := []string{
		"downloader", "hash", "title", "site_name", "name", "year", "season_episode", "path",
		"size", "progress", "state", "upspeed", "dlspeed", "tags", "save_path", "content_path",
		"category", "download_limit", "upload_limit", "ratio_limit", "seeding_time_limit", "trackers",
		"media", "userid", "username", "left_time",
	}
	for _, key := range wantKeys {
		if _, ok := item[key]; !ok {
			t.Errorf("缺少 V2 字段 %q: %#v", key, item)
		}
	}
	if item["name"] != "示例剧" || item["year"] != "2026" || item["season_episode"] != "S02E03" {
		t.Errorf("媒体解析字段 = name:%#v year:%#v season_episode:%#v", item["name"], item["year"], item["season_episode"])
	}
	if item["dlspeed"] != "1.5K" || item["upspeed"] != "1.0M" || item["left_time"] != "1时1分1秒" {
		t.Errorf("格式化字段 = dlspeed:%#v upspeed:%#v left_time:%#v", item["dlspeed"], item["upspeed"], item["left_time"])
	}
	if item["userid"] != "11" || item["username"] != "alice" {
		t.Errorf("owner 字段 = userid:%#v username:%#v", item["userid"], item["username"])
	}
	if item["download_limit"] != float64(2) || item["upload_limit"] != float64(4) {
		t.Errorf("限速字段 = download:%#v upload:%#v", item["download_limit"], item["upload_limit"])
	}
	if item["tags"] != "影视" {
		t.Errorf("tags = %#v, want 影视", item["tags"])
	}
	if trackers, ok := item["trackers"].([]any); !ok || len(trackers) != 0 {
		t.Errorf("trackers = %#v, want empty array", item["trackers"])
	}
	for _, forbidden := range []string{"label", "passkey", "tracker.invalid"} {
		if strings.Contains(strings.ToLower(response.Body.String()), forbidden) {
			t.Fatalf("响应暴露 %q: %s", forbidden, response.Body.String())
		}
	}
}

func TestListDownloadsRedactsDependencyErrors(t *testing.T) {
	tests := []struct {
		name   string
		source *fakeDownloadTaskSource
		owners *fakeDownloadIntentOwnerStore
	}{
		{
			name:   "下载器错误",
			source: &fakeDownloadTaskSource{err: errors.New("tracker.invalid announce passkey=source-secret")},
			owners: &fakeDownloadIntentOwnerStore{},
		},
		{
			name: "owner 查询错误",
			source: &fakeDownloadTaskSource{tasks: []NamedDownloadTask{{
				Downloader: "qB",
				Task:       downloader.Task{Hash: "safe-hash", Label: downloadListAliceLabel},
			}}},
			owners: &fakeDownloadIntentOwnerStore{err: errors.New("database passkey=owner-secret")},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			user := auth.User{ID: 11, Username: "alice"}
			deps := Dependencies{
				Users:          fakeUserLookup{user: user},
				DownloadTasks:  tt.source,
				DownloadOwners: tt.owners,
			}
			response := httptest.NewRecorder()
			deps.listDownloads(response, downloadListRequest("/download/", user))

			if response.Code != http.StatusInternalServerError {
				t.Fatalf("status = %d, want %d; body=%s", response.Code, http.StatusInternalServerError, response.Body.String())
			}
			body := strings.ToLower(response.Body.String())
			for _, forbidden := range []string{"passkey", "secret", "tracker.invalid", "database"} {
				if strings.Contains(body, forbidden) {
					t.Fatalf("错误响应泄漏 %q: %s", forbidden, response.Body.String())
				}
			}
		})
	}
}

func FuzzPublicDownloadTagsRemovesInternalTags(f *testing.F) {
	f.Add("影视, "+downloadListAliceLabel+", keep", downloadListAliceLabel)
	f.Add(downloadListAliceLabel+","+downloadListUnknownLabel, downloadListAliceLabel)
	f.Add("mp-AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA, keep", downloadListAliceLabel)
	f.Fuzz(func(t *testing.T, tags, selectedLabel string) {
		public := publicDownloadTags(tags, selectedLabel)
		for _, raw := range strings.Split(public, ",") {
			if isInternalDownloadTag(strings.TrimSpace(raw)) {
				t.Fatalf("公开 tags 仍包含内部任务标签: %q", public)
			}
		}
	})
}

type fakeDownloadTaskSource struct {
	tasks []NamedDownloadTask
	name  string
	err   error
}

func (s *fakeDownloadTaskSource) ListDownloading(_ context.Context, name string) ([]NamedDownloadTask, error) {
	s.name = name
	if s.err != nil {
		return nil, s.err
	}
	if name == "" {
		return append([]NamedDownloadTask(nil), s.tasks...), nil
	}
	items := make([]NamedDownloadTask, 0, len(s.tasks))
	for _, task := range s.tasks {
		if strings.EqualFold(strings.TrimSpace(task.Downloader), name) {
			items = append(items, task)
		}
	}
	return items, nil
}

type fakeDownloadIntentOwnerStore struct {
	owners map[string]database.DownloadIntentOwner
	labels []string
	err    error
}

func (s *fakeDownloadIntentOwnerStore) ListDownloadIntentOwners(_ context.Context, labels []string) (map[string]database.DownloadIntentOwner, error) {
	s.labels = append(s.labels, labels...)
	if s.err != nil {
		return nil, s.err
	}
	result := make(map[string]database.DownloadIntentOwner, len(labels))
	for _, label := range labels {
		if owner, ok := s.owners[label]; ok {
			result[label] = owner
		}
	}
	return result, nil
}

func downloadListRequest(path string, user auth.User) *http.Request {
	request := httptest.NewRequest(http.MethodGet, path, nil)
	claims := auth.Claims{
		Username:  user.Username,
		SuperUser: user.SuperUser,
		RegisteredClaims: jwt.RegisteredClaims{
			Subject: "11",
		},
	}
	if user.ID > 0 {
		claims.Subject = strconv.FormatInt(user.ID, 10)
	}
	return request.WithContext(context.WithValue(request.Context(), claimsContextKey, claims))
}
