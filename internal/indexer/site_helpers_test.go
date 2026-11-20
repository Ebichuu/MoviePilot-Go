package indexer

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestProbeDataContainer(t *testing.T) {
	cases := []struct {
		name  string
		value any
		want  bool
	}{
		{"非空对象", map[string]any{"code": 0}, true},
		{"空对象", map[string]any{}, false},
		{"非空数组", []any{1}, true},
		{"空数组", []any{}, false},
		{"字符串", "data", false},
		{"nil", nil, false},
	}
	for _, testCase := range cases {
		if got := probeDataContainer(testCase.value); got != testCase.want {
			t.Fatalf("%s: probeDataContainer(%#v) = %v", testCase.name, testCase.value, got)
		}
	}
	if !probeDataObject(map[string]any{"a": 1}) || probeDataObject(map[string]any{}) ||
		probeDataObject([]any{1}) || probeDataObject(nil) {
		t.Fatal("probeDataObject 判定错误")
	}
}

func TestSiteCategoryMappers(t *testing.T) {
	for value, want := range map[string]string{
		"movie": "电影", "MOVIE": "电影", " movie ": "电影",
		"tv": "电视剧", "TV": "电视剧",
		"anime": "未知", "": "未知",
	} {
		if got := rousiCategory(value); got != want {
			t.Fatalf("rousiCategory(%q) = %q, want %q", value, got, want)
		}
	}
	for value, want := range map[string]string{
		"movie": "电影", "Movie": "电影",
		"tv": "电视剧", "TV": "电视剧",
		// SunnyPT 不做 TrimSpace，带空白即视为未知分类。
		" movie": "未知", "anime": "未知", "": "未知",
	} {
		if got := sunnyPTCategory(value); got != want {
			t.Fatalf("sunnyPTCategory(%q) = %q, want %q", value, got, want)
		}
	}
}

func TestYemaFactors(t *testing.T) {
	for value, want := range map[string]float64{
		"free": 0, "FREE": 0, "half": 0.5, "Half": 0.5, "normal": 1, "": 1,
	} {
		if got := yemaDownloadFactor(value); got != want {
			t.Fatalf("yemaDownloadFactor(%q) = %v, want %v", value, got, want)
		}
	}
	for value, want := range map[string]float64{
		"one_half": 1.5, "ONE_HALF": 1.5, "double_upload": 2, "Double_Upload": 2,
		"normal": 1, "": 1,
	} {
		if got := yemaUploadFactor(value); got != want {
			t.Fatalf("yemaUploadFactor(%q) = %v, want %v", value, got, want)
		}
	}
}

func TestValueBool(t *testing.T) {
	cases := []struct {
		value any
		want  bool
	}{
		{true, true},
		{false, false},
		{"true", true},
		{"TRUE", true},
		{" 1 ", true},
		{" false ", false},
		{"yes", false},
		{"", false},
		{nil, false},
	}
	for _, testCase := range cases {
		if got := valueBool(testCase.value); got != testCase.want {
			t.Fatalf("valueBool(%#v) = %v, want %v", testCase.value, got, testCase.want)
		}
	}
}

func TestUserDataDateTime(t *testing.T) {
	if got := userDataDateTime(nil); got != nil {
		t.Fatalf("nil = %#v", got)
	}
	if got := userDataDateTime("   "); got == nil || *got != "" {
		t.Fatalf("空白 = %#v", got)
	}

	// 秒级时间戳；毫秒时间戳会被折算成秒，两者结果一致。
	seconds := time.Unix(1700000000, 0).Format("2006-01-02 15:04:05")
	for _, raw := range []any{"1700000000", "1700000000123", int64(1700000000)} {
		got := userDataDateTime(raw)
		if got == nil || *got != seconds {
			t.Fatalf("时间戳 %#v = %#v, want %q", raw, got, seconds)
		}
	}

	for raw, want := range map[string]string{
		"2024-05-06T07:08:09Z": "2024-05-06 07:08:09",
		"2024-05-06 07:08:09":  "2024-05-06 07:08:09",
		"2024-05-06T07:08:09":  "2024-05-06 07:08:09",
		"2024-05-06":           "2024-05-06 00:00:00",
	} {
		got := userDataDateTime(raw)
		if got == nil || *got != want {
			t.Fatalf("时间文本 %q = %#v, want %q", raw, got, want)
		}
	}

	// 无法解析的文本原样保留（裁剪空白后）。
	if got := userDataDateTime(" 从未登录 "); got == nil || *got != "从未登录" {
		t.Fatalf("未知文本 = %#v", got)
	}
	// 0 或负数时间戳不参与格式化。
	if got := userDataDateTime("0"); got == nil || *got != "0" {
		t.Fatalf("零时间戳 = %#v", got)
	}
}

func TestTorrentLeechUserDataPage(t *testing.T) {
	newAdapter := func(t *testing.T, client *HTTPClient) *TorrentLeechAdapter {
		t.Helper()
		adapter, err := NewTorrentLeechAdapter(client, 1, "TL", "https://example.test", map[string]any{"cookie": "c"})
		if err != nil {
			t.Fatalf("NewTorrentLeechAdapter(): %v", err)
		}
		return adapter
	}

	cases := map[int]bool{
		http.StatusOK:                  true,
		http.StatusForbidden:           true,
		http.StatusInternalServerError: true,
		http.StatusNotFound:            false,
		http.StatusBadGateway:          false,
	}
	for status, hasBody := range cases {
		t.Run(http.StatusText(status), func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(status)
				_, _ = io.WriteString(w, "body")
			}))
			defer server.Close()
			client := newIndexerHTTPClient(t, 1)
			body, err := torrentLeechUserDataPage(t.Context(), newAdapter(t, client), server.URL)
			if err != nil {
				t.Fatalf("%d: %v", status, err)
			}
			if hasBody && string(body) != "body" {
				t.Fatalf("%d 响应体 = %q", status, body)
			}
			if !hasBody && body != nil {
				t.Fatalf("%d 不应返回响应体: %q", status, body)
			}
		})
	}

	// 上下文取消原样上抛，其它传输错误吞掉返回 nil。
	canceled, cancel := context.WithCancel(t.Context())
	cancel()
	client := newIndexerHTTPClient(t, 1)
	if _, err := torrentLeechUserDataPage(canceled, newAdapter(t, client), "https://example.test/"); !errors.Is(err, context.Canceled) {
		t.Fatalf("已取消上下文 = %v", err)
	}
	client.SetAllowPrivateHosts(true)
	closed := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	closedURL := closed.URL
	closed.Close()
	if body, err := torrentLeechUserDataPage(t.Context(), newAdapter(t, client), closedURL); err != nil || body != nil {
		t.Fatalf("传输错误 = (%q, %v)", body, err)
	}
}
