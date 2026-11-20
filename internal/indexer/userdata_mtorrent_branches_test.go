package indexer

import (
	"context"
	"errors"
	"net/http"
	"testing"
)

type mtorrentUserDataServer struct {
	profile  string
	seeding  []string
	tracker  string
	messages string
	served   int
}

func (s *mtorrentUserDataServer) handler(t *testing.T) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/member/profile":
			_, _ = w.Write([]byte(s.profile))
		case "/api/msg/search":
			body := s.messages
			if body == "" {
				body = `{"code":0,"data":{"data":[]}}`
			}
			_, _ = w.Write([]byte(body))
		case "/api/msg/markRead":
			_, _ = w.Write([]byte(`{"code":0}`))
		case "/api/member/getUserTorrentList":
			index := s.served
			s.served++
			if index < len(s.seeding) {
				_, _ = w.Write([]byte(s.seeding[index]))
				return
			}
			_, _ = w.Write([]byte(`{"code":0,"data":{"data":[]}}`))
		case "/api/tracker/myPeerStatus":
			body := s.tracker
			if body == "" {
				body = `{"code":0,"data":{"seeder":1}}`
			}
			_, _ = w.Write([]byte(body))
		default:
			t.Fatalf("未预期的路径: %s", r.URL.Path)
		}
	}
}

func (s *mtorrentUserDataServer) adapter(t *testing.T) (*MTorrentAdapter, string) {
	t.Helper()
	endpoint := probeServer(t, s.handler(t))
	adapter, err := NewMTorrentAdapter(newIndexerHTTPClient(t, 1), 12, "M-Team", endpoint, map[string]any{
		"api_url": endpoint, "apikey": "fixture-key",
	})
	if err != nil {
		t.Fatalf("构造适配器: %v", err)
	}
	return adapter, endpoint
}

// TestMTorrentFetchSiteUserDataProfileBranches 覆盖用户资料与做种分页的早退分支。
func TestMTorrentFetchSiteUserDataProfileBranches(t *testing.T) {
	cases := []struct {
		name   string
		server mtorrentUserDataServer
		check  func(t *testing.T, data SiteUserData, err error)
	}{
		{
			name:   "业务码非零",
			server: mtorrentUserDataServer{profile: `{"code":1,"data":{"id":1}}`},
			check: func(t *testing.T, data SiteUserData, err error) {
				if err != nil || data.Username != nil {
					t.Fatalf("业务码非零 = %+v（%v）", data, err)
				}
			},
		},
		{
			name:   "数据不是对象",
			server: mtorrentUserDataServer{profile: `{"code":0,"data":[]}`},
			check: func(t *testing.T, data SiteUserData, err error) {
				if err != nil || data.Username != nil {
					t.Fatalf("数据非对象 = %+v（%v）", data, err)
				}
			},
		},
		{
			name:   "资料格式无效",
			server: mtorrentUserDataServer{profile: `not json`},
			check: func(t *testing.T, data SiteUserData, err error) {
				if err == nil {
					t.Fatal("非法资料应报错")
				}
			},
		},
		{
			name: "完整资料与一页做种",
			server: mtorrentUserDataServer{
				profile: `{"code":0,"data":{"id":9,"username":"alice","role":"3",
					"memberStatus":{"createdDate":"2024-01-02 03:04:05"},
					"memberCount":{"uploaded":1000,"downloaded":500,"shareRate":2,"bonus":7.5}}}`,
				seeding: []string{`{"code":0,"data":{"data":[
					"skip",
					{"torrent":{"size":100,"source":4}},
					{"torrent":{"size":200}}]}}`},
				tracker:  `{"code":0,"data":{"seeder":2}}`,
				messages: `{"code":0,"data":{"data":[{"id":1,"unread":true,"title":"标题","createdDate":"2026-01-01","context":"正文"},{"id":2,"unread":false,"title":"已读"},{"id":3,"unread":true,"title":"缺正文"}]}}`,
			},
			check: func(t *testing.T, data SiteUserData, err error) {
				if err != nil {
					t.Fatalf("解析失败: %v", err)
				}
				if data.Username == nil || *data.Username != "alice" || data.UserLevel == nil ||
					*data.UserLevel != "Elite User" {
					t.Fatalf("用户字段 = %+v", data)
				}
				if data.JoinAt == nil || data.Upload != 1000 || data.Download != 500 ||
					data.Ratio != 2 || data.Bonus != 7.5 {
					t.Fatalf("统计字段 = %+v", data)
				}
				if data.Seeding != 2 || data.SeedingSize != 300 || len(data.SeedingInfo) != 2 {
					t.Fatalf("做种统计 = %+v", data)
				}
				if len(data.MessageUnreadContents) != 1 {
					t.Fatalf("未读消息 = %+v", data.MessageUnreadContents)
				}
			},
		},
		{
			name: "做种页面业务码非零",
			server: mtorrentUserDataServer{
				profile: `{"code":0,"data":{"id":9,"username":"bob"}}`,
				seeding: []string{`{"code":2,"data":{"data":[]}}`},
			},
			check: func(t *testing.T, data SiteUserData, err error) {
				if err != nil || data.Seeding != 0 {
					t.Fatalf("做种业务码非零 = %+v（%v）", data, err)
				}
			},
		},
		{
			name: "做种数据不是对象",
			server: mtorrentUserDataServer{
				profile: `{"code":0,"data":{"id":9,"username":"bob"}}`,
				seeding: []string{`{"code":0,"data":[]}`},
			},
			check: func(t *testing.T, data SiteUserData, err error) {
				if err != nil || data.Seeding != 0 {
					t.Fatalf("做种数据非对象 = %+v（%v）", data, err)
				}
			},
		},
		{
			name: "做种条目不是数组",
			server: mtorrentUserDataServer{
				profile: `{"code":0,"data":{"id":9,"username":"bob"}}`,
				seeding: []string{`{"code":0,"data":{"data":{}}}`},
			},
			check: func(t *testing.T, data SiteUserData, err error) {
				if err != nil || data.Seeding != 0 {
					t.Fatalf("做种条目非数组 = %+v（%v）", data, err)
				}
			},
		},
		{
			name: "Tracker 请求失败被吞掉",
			server: mtorrentUserDataServer{
				profile: `{"code":0,"data":{"id":9,"username":"bob"}}`,
				seeding: []string{`{"code":0,"data":{"data":[{"torrent":{"size":10,"source":1}}]}}`},
				tracker: `{"code":0}`,
			},
			check: func(t *testing.T, data SiteUserData, err error) {
				if err != nil || data.Seeding != 1 {
					t.Fatalf("Tracker 失败 = %+v（%v）", data, err)
				}
			},
		},
		{
			name: "Tracker 响应无效被吞掉",
			server: mtorrentUserDataServer{
				profile: `{"code":0,"data":{"id":9,"username":"bob"}}`,
				seeding: []string{`{"code":0,"data":{"data":[{"torrent":{"size":10,"source":1}}]}}`},
				tracker: `not json`,
			},
			check: func(t *testing.T, data SiteUserData, err error) {
				if err != nil || data.Seeding != 1 {
					t.Fatalf("Tracker 无效 = %+v（%v）", data, err)
				}
			},
		},
		{
			name: "消息搜索失败被吞掉",
			server: mtorrentUserDataServer{
				profile:  `{"code":0,"data":{"id":9,"username":"bob"}}`,
				seeding:  []string{`{"code":0,"data":{"data":[]}}`},
				tracker:  `{"code":0,"data":{"seeder":0}}`,
				messages: `{"code":9,"data":{}}`,
			},
			check: func(t *testing.T, data SiteUserData, err error) {
				if err != nil || len(data.MessageUnreadContents) != 0 {
					t.Fatalf("消息业务码非零 = %+v（%v）", data, err)
				}
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			server := tc.server
			adapter, _ := server.adapter(t)
			data, err := adapter.FetchSiteUserData(context.Background())
			tc.check(t, data, err)
		})
	}
}

// TestMTorrentFetchSiteUserDataCancelled 覆盖分页过程中的上下文取消。
func TestMTorrentFetchSiteUserDataCancelled(t *testing.T) {
	server := mtorrentUserDataServer{
		profile: `{"code":0,"data":{"id":9,"username":"bob"}}`,
		// 空做种列表 + 大于已整理数的 seeder 总数会让循环继续，直到上下文取消。
		seeding: []string{`{"code":0,"data":{"data":[]}}`},
		tracker: `{"code":0,"data":{"seeder":100}}`,
	}
	adapter, _ := server.adapter(t)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := adapter.FetchSiteUserData(ctx); !errors.Is(err, context.Canceled) {
		t.Fatalf("取消上下文 = %v", err)
	}
}

// TestMTorrentUserDataLevels 覆盖角色到等级名的映射。
func TestMTorrentUserDataLevels(t *testing.T) {
	if level := mTorrentUserDataLevel(""); level == nil || *level != "User" {
		t.Fatalf("空角色 = %v", level)
	}
	if level := mTorrentUserDataLevel(5); level == nil || *level != "Insane User" {
		t.Fatalf("数字角色 = %v", level)
	}
	if level := mTorrentUserDataLevel(" 2 "); level == nil || *level != "Power User" {
		t.Fatalf("字符串角色 = %v", level)
	}
	if level := mTorrentUserDataLevel("99"); level != nil {
		t.Fatalf("未知角色 = %v", level)
	}
}
