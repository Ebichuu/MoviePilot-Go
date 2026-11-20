package indexer

import (
	"testing"
	"time"
)

// TestAdapterResolverMetadata 固定各站点适配器的解析器标识：
// 下载解析按 ResolverKind + ResolverVersion 路由，标识漂移会让已落库的
// intent 在升级后无法再被解析，所以这里逐站点写死期望值。
func TestAdapterResolverMetadata(t *testing.T) {
	client, err := NewHTTPClient(1, 5*time.Second, 1<<20)
	if err != nil {
		t.Fatalf("NewHTTPClient(): %v", err)
	}
	t.Cleanup(client.CloseIdleConnections)

	type metadata interface {
		Name() string
		SiteID() int
		ResolverKind() string
		ResolverVersion() int16
	}
	cases := []struct {
		kind  string
		build func() (metadata, error)
	}{
		{"haidan", func() (metadata, error) {
			return NewHaidanAdapter(client, 11, "海胆", "https://example.test", map[string]any{"cookie": "c", "apikey": "k"})
		}},
		{"hddolby", func() (metadata, error) {
			return NewHDDolbyAdapter(client, 12, "HDDolby", "https://example.test", map[string]any{"cookie": "c", "apikey": "k"})
		}},
		{"mtorrent", func() (metadata, error) {
			return NewMTorrentAdapter(client, 13, "M-Team", "https://example.test", map[string]any{"cookie": "c", "apikey": "k"})
		}},
		{"rousi", func() (metadata, error) {
			return NewRousiAdapter(client, 14, "肉丝", "https://example.test", map[string]any{"cookie": "c", "apikey": "k"})
		}},
		{"sunnypt", func() (metadata, error) {
			return NewSunnyPTAdapter(client, 15, "SunnyPT", "https://example.test", map[string]any{"cookie": "c", "apikey": "k"})
		}},
		{"tnode", func() (metadata, error) {
			return NewTNodeAdapter(client, 16, "TNode", "https://example.test", map[string]any{"cookie": "c", "apikey": "k"})
		}},
		{"torrentleech", func() (metadata, error) {
			return NewTorrentLeechAdapter(client, 17, "TorrentLeech", "https://example.test", map[string]any{"cookie": "c", "apikey": "k"})
		}},
		{"yema", func() (metadata, error) {
			return NewYemaAdapter(client, 18, "野马", "https://example.test", map[string]any{"cookie": "c", "apikey": "k"})
		}},
	}
	for _, testCase := range cases {
		t.Run(testCase.kind, func(t *testing.T) {
			adapter, err := testCase.build()
			if err != nil {
				t.Fatalf("构造适配器: %v", err)
			}
			if adapter.Name() == "" {
				t.Fatal("Name() 为空")
			}
			if adapter.SiteID() == 0 {
				t.Fatal("SiteID() 为 0")
			}
			if adapter.ResolverKind() != testCase.kind {
				t.Fatalf("ResolverKind() = %q, want %q", adapter.ResolverKind(), testCase.kind)
			}
			if adapter.ResolverVersion() != downloadResolverVersionV1 {
				t.Fatalf("ResolverVersion() = %d", adapter.ResolverVersion())
			}
		})
	}

	// M-Team 的 RSS 需要 no-cache 键；这里只要求实现可判定且不 panic。
	if adapter, err := NewMTorrentAdapter(client, 19, "M-Team", "https://example.test", map[string]any{"cookie": "c", "apikey": "k"}); err == nil {
		_ = adapter.RSSNoCache()
	}
}

func TestCoordinatorLastSearchSnapshot(t *testing.T) {
	var nilCoordinator *Coordinator
	if nilCoordinator.LastSearchResults() != nil {
		t.Fatal("nil 协调器应返回 nil 结果")
	}
	if query := nilCoordinator.LastSearchQuery(); query.Keyword != "" || query.Page != 0 {
		t.Fatalf("nil 协调器查询 = %#v", query)
	}

	coordinator := &Coordinator{}
	if results := coordinator.LastSearchResults(); results != nil {
		t.Fatalf("初始结果 = %#v", results)
	}
	if query := coordinator.LastSearchQuery(); query.Keyword != "" {
		t.Fatalf("初始查询 = %#v", query)
	}

	coordinator.lastResults = []Resource{{Title: "示例", Seeders: 3}}
	coordinator.lastQuery = Query{Keyword: "沙丘"}
	results := coordinator.LastSearchResults()
	if len(results) != 1 || results[0].Title != "示例" {
		t.Fatalf("最近结果 = %#v", results)
	}
	// 返回副本：调用方改写不影响内部快照。
	results[0].Title = "被改写"
	if again := coordinator.LastSearchResults(); again[0].Title != "示例" {
		t.Fatalf("结果未被拷贝: %#v", again)
	}
	if query := coordinator.LastSearchQuery(); query.Keyword != "沙丘" {
		t.Fatalf("最近查询 = %#v", query)
	}
}
