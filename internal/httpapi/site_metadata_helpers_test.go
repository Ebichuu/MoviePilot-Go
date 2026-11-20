package httpapi

import "testing"

// TestInferSiteAdapter 覆盖按域名或站点名推断适配器：8 个内置适配器分别命中，
// 域名优先于站点名，未知来源返回空串，非法 URL 不 panic。
func TestInferSiteAdapter(t *testing.T) {
	cases := []struct {
		name     string
		rawURL   string
		siteName string
		want     string
	}{
		{"TNode 域名", "https://tnode.example.com", "", "TNodeSpider"},
		{"TorrentLeech 站点名", "https://unknown.example.com", "TorrentLeech", "TorrentLeech"},
		{"m-team 域名", "https://m-team.io", "", "mTorrent"},
		{"mTorrent 站点名", "https://unknown.example.com", "MTorrent 站", "mTorrent"},
		{"SunnyPT 域名", "https://sunnypt.org", "", "SunnyPT"},
		{"Yema 域名", "https://yema.example.com", "", "Yema"},
		{"叶子站点名", "https://unknown.example.com", "叶子PT", "Yema"},
		{"Haidan 域名", "https://haidan.example.com", "", "Haidan"},
		{"海胆站点名", "https://unknown.example.com", "海胆", "Haidan"},
		{"HDDolby 域名", "https://hddolby.example.com", "", "HDDolby"},
		{"Rousi 域名", "https://rousi.example.com", "", "RousiPro"},
		{"未知来源", "https://unknown.example.com", "未知站", ""},
		{"非法 URL", "://bad url", "", ""},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			if got := inferSiteAdapter(testCase.rawURL, testCase.siteName); got != testCase.want {
				t.Fatalf("inferSiteAdapter(%q, %q) = %q, want %q",
					testCase.rawURL, testCase.siteName, got, testCase.want)
			}
		})
	}

	// 域名优先：域名命中 TNode 时忽略站点名里的其它关键词。
	if got := inferSiteAdapter("https://tnode.example.com", "Rousi"); got != "TNodeSpider" {
		t.Fatalf("域名优先 = %q, want TNodeSpider", got)
	}
}

// TestMetadataMatchSource 覆盖元数据来源到匹配键的映射。
func TestMetadataMatchSource(t *testing.T) {
	cases := map[string]string{
		"themoviedb": "tmdbid",
		"tmdb":       "tmdbid",
		"douban":     "doubanid",
		"bangumi":    "bangumiid",
		"anilist":    "anilistid",
		"":           "unknown",
		"TMDB":       "unknown",
	}
	for source, want := range cases {
		if got := metadataMatchSource(source); got != want {
			t.Fatalf("metadataMatchSource(%q) = %q, want %q", source, got, want)
		}
	}
}
