package indexer

import (
	"context"
	"net/http"
	"testing"
)

// mtorrentUserDataErrorServer 让指定的端点以 HTTP 错误或非法 JSON 响应，
// 用于覆盖「请求失败被吞掉」与「请求失败必须上抛」两类分支。
type mtorrentUserDataErrorServer struct {
	failPath string
	status   int
	body     string
}

func (s mtorrentUserDataErrorServer) handler() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == s.failPath {
			if s.body != "" {
				_, _ = w.Write([]byte(s.body))
				return
			}
			status := s.status
			if status == 0 {
				status = http.StatusInternalServerError
			}
			http.Error(w, "boom", status)
			return
		}
		switch r.URL.Path {
		case "/api/member/profile":
			_, _ = w.Write([]byte(`{"code":0,"data":{"id":9,"username":"bob"}}`))
		case "/api/msg/search":
			_, _ = w.Write([]byte(`{"code":0,"data":{"data":[]}}`))
		case "/api/msg/markRead":
			_, _ = w.Write([]byte(`{"code":0}`))
		case "/api/member/getUserTorrentList":
			_, _ = w.Write([]byte(`{"code":0,"data":{"data":[{"torrent":{"size":10,"source":1}}]}}`))
		case "/api/tracker/myPeerStatus":
			_, _ = w.Write([]byte(`{"code":0,"data":{"seeder":1}}`))
		default:
			http.NotFound(w, r)
		}
	}
}

func TestMTorrentFetchSiteUserDataRequestFailures(t *testing.T) {
	tests := []struct {
		name      string
		server    mtorrentUserDataErrorServer
		wantError bool
	}{
		{
			name:      "做种请求 HTTP 失败需上抛",
			server:    mtorrentUserDataErrorServer{failPath: "/api/member/getUserTorrentList"},
			wantError: true,
		},
		{
			name:      "做种响应非法 JSON 需上抛",
			server:    mtorrentUserDataErrorServer{failPath: "/api/member/getUserTorrentList", body: "not json"},
			wantError: true,
		},
		{
			name:      "Tracker 请求失败被吞掉",
			server:    mtorrentUserDataErrorServer{failPath: "/api/tracker/myPeerStatus"},
			wantError: false,
		},
		{
			name:      "Tracker 返回非法 JSON 被吞掉",
			server:    mtorrentUserDataErrorServer{failPath: "/api/tracker/myPeerStatus", body: "not json"},
			wantError: false,
		},
		{
			name:      "消息搜索请求失败被吞掉",
			server:    mtorrentUserDataErrorServer{failPath: "/api/msg/search"},
			wantError: false,
		},
		{
			name:      "消息搜索非法 JSON 需上抛",
			server:    mtorrentUserDataErrorServer{failPath: "/api/msg/search", body: "not json"},
			wantError: true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			endpoint := probeServer(t, tt.server.handler())
			adapter, err := NewMTorrentAdapter(newIndexerHTTPClient(t, 1), 31, "M-Team", endpoint, map[string]any{
				"api_url": endpoint, "apikey": "fixture-key",
			})
			if err != nil {
				t.Fatalf("构造适配器: %v", err)
			}
			data, err := adapter.FetchSiteUserData(context.Background())
			if tt.wantError && err == nil {
				t.Fatalf("应返回错误，得到 %+v", data)
			}
			if !tt.wantError && err != nil {
				t.Fatalf("错误应被吞掉，得到 %v", err)
			}
		})
	}
}
