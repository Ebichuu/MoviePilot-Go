package httpapi

import (
	"context"
	"errors"
	"testing"

	"github.com/moviepilot-go/moviepilot-go/internal/database"
	"github.com/moviepilot-go/moviepilot-go/internal/downloadconfig"
	"github.com/moviepilot-go/moviepilot-go/internal/indexer"
)

type downloadIntentOwnerFake struct {
	owners map[string]database.DownloadIntentOwner
}

func (f downloadIntentOwnerFake) ListDownloadIntentOwners(context.Context, []string) (map[string]database.DownloadIntentOwner, error) {
	return f.owners, nil
}

// TestDownloadStoreSelectors 覆盖「优先用注入的存储，否则回落到数据库存储」的选择器。
func TestDownloadStoreSelectors(t *testing.T) {
	empty := Dependencies{}
	if empty.downloadRequestStore() != nil || empty.downloadIntentOwnerStore() != nil {
		t.Fatal("没有存储时两个选择器都应返回 nil")
	}

	requests := standardDownloadRequestStore()
	owners := downloadIntentOwnerFake{}
	deps := Dependencies{DownloadRequests: requests, DownloadOwners: owners}
	if deps.downloadRequestStore() != DownloadRequestStore(requests) {
		t.Fatal("注入的下载请求存储未被选中")
	}
	if deps.downloadIntentOwnerStore() == nil {
		t.Fatal("注入的下载归属存储未被选中")
	}
}

// TestLoadDownloadDirectoriesBranches 覆盖下载目录设置的四种读取结果。
func TestLoadDownloadDirectoriesBranches(t *testing.T) {
	if _, err := (Dependencies{}).loadDownloadDirectories(t.Context()); !errors.Is(err, errDownloadRequestStoreUnavailable) {
		t.Fatalf("无存储 error = %v", err)
	}
	if _, err := (Dependencies{DownloadRequests: &fakeDownloadRequestStore{
		settingErr: errors.New("boom"),
	}}).loadDownloadDirectories(t.Context()); !errors.Is(err, errDownloadRequestStoreUnavailable) {
		t.Fatalf("读取失败 error = %v", err)
	}
	directories, err := (Dependencies{DownloadRequests: &fakeDownloadRequestStore{}}).
		loadDownloadDirectories(t.Context())
	if err != nil || directories == nil || len(directories) != 0 {
		t.Fatalf("未配置目录 = %v, %v", directories, err)
	}
	directories, err = (Dependencies{DownloadRequests: standardDownloadRequestStore()}).
		loadDownloadDirectories(t.Context())
	if err != nil || len(directories) != 1 {
		t.Fatalf("已配置目录 = %v, %v", directories, err)
	}
}

// TestSelectDownloadClientBranches 覆盖下载器选择的全部分支。
func TestSelectDownloadClientBranches(t *testing.T) {
	if _, err := (Dependencies{}).selectDownloadClient(t.Context(), "", ""); !errors.Is(err, errDownloadRequestStoreUnavailable) {
		t.Fatalf("无存储 error = %v", err)
	}
	if _, err := (Dependencies{DownloadRequests: &fakeDownloadRequestStore{
		loadErr: errors.New("boom"),
	}}).selectDownloadClient(t.Context(), "", ""); !errors.Is(err, errDownloadRequestStoreUnavailable) {
		t.Fatalf("读取失败 error = %v", err)
	}
	if _, err := (Dependencies{DownloadRequests: &fakeDownloadRequestStore{}}).
		selectDownloadClient(t.Context(), "", ""); !errors.Is(err, errDownloadClientUnavailable) {
		t.Fatalf("未配置下载器 error = %v", err)
	}

	deps := Dependencies{DownloadRequests: standardDownloadRequestStore()}
	selected, err := deps.selectDownloadClient(t.Context(), "QB Primary", "")
	if err != nil || selected.Name != "QB Primary" {
		t.Fatalf("按名称选择 = %+v, %v", selected, err)
	}
	if _, err := deps.selectDownloadClient(t.Context(), "不存在", ""); !errors.Is(err, errDownloadClientUnavailable) {
		t.Fatalf("名称不存在 error = %v", err)
	}
	selected, err = deps.selectDownloadClient(t.Context(), "", "Secondary")
	if err != nil || selected.Name != "Secondary" {
		t.Fatalf("按站点下载器选择 = %+v, %v", selected, err)
	}
	// 都不指定时用默认下载器。
	selected, err = deps.selectDownloadClient(t.Context(), "", "")
	if err != nil || selected.Name != "QB Primary" {
		t.Fatalf("默认选择 = %+v, %v", selected, err)
	}
	// 全部停用时没有任何可用下载器。
	disabled := &fakeDownloadRequestStore{
		downloaders: []downloadconfig.Downloader{
			{ID: "1", Name: "停用", Type: "qbittorrent", Enabled: false},
		},
		downloadersFound: true,
	}
	if _, err := (Dependencies{DownloadRequests: disabled}).selectDownloadClient(t.Context(), "", ""); !errors.Is(err, errDownloadClientUnavailable) {
		t.Fatalf("全部停用 error = %v", err)
	}
}

// TestAllowedDownloadSavePathBranches 覆盖保存路径白名单的四种结论。
func TestAllowedDownloadSavePathBranches(t *testing.T) {
	if path, err := (Dependencies{}).allowedDownloadSavePath(t.Context(), "  "); err != nil || path != "" {
		t.Fatalf("空路径 = %q, %v", path, err)
	}
	if _, err := (Dependencies{}).allowedDownloadSavePath(t.Context(), "/downloads"); !errors.Is(err, errDownloadRequestStoreUnavailable) {
		t.Fatalf("无存储 error = %v", err)
	}
	deps := Dependencies{DownloadRequests: standardDownloadRequestStore()}
	path, err := deps.allowedDownloadSavePath(t.Context(), "/downloads")
	if err != nil || path == "" {
		t.Fatalf("白名单内路径 = %q, %v", path, err)
	}
	if _, err := deps.allowedDownloadSavePath(t.Context(), "/etc/passwd"); !errors.Is(err, errDownloadPathNotAllowed) {
		t.Fatalf("白名单外 error = %v", err)
	}
}

// TestDownloadMediaIdentityBranches 覆盖媒体标识归一：显式来源、来源别名、
// 以及没有显式来源时按 TMDB/豆瓣/Bangumi/AniList 的优先级回退。
func TestDownloadMediaIdentityBranches(t *testing.T) {
	cases := []struct {
		name       string
		input      downloadMediaInput
		wantSource string
		wantID     string
	}{
		{name: "显式来源与 ID", input: downloadMediaInput{Source: "douban", MediaID: "123"},
			wantSource: "douban", wantID: "123"},
		{name: "TMDB 别名", input: downloadMediaInput{Source: "tmdb", MediaID: "456"},
			wantSource: "themoviedb", wantID: "456"},
		{name: "按来源挑 TMDB", input: downloadMediaInput{Source: "themoviedb", TMDBID: 7},
			wantSource: "themoviedb", wantID: "7"},
		{name: "按来源挑豆瓣", input: downloadMediaInput{Source: "douban", DoubanID: "8"},
			wantSource: "douban", wantID: "8"},
		{name: "来源没有对应 ID", input: downloadMediaInput{Source: "anilist", TMDBID: 9},
			wantSource: "", wantID: ""},
		{name: "无来源回退 TMDB", input: downloadMediaInput{TMDBID: 10},
			wantSource: "themoviedb", wantID: "10"},
		{name: "无来源回退豆瓣", input: downloadMediaInput{DoubanID: "11"},
			wantSource: "douban", wantID: "11"},
		{name: "无来源回退 Bangumi", input: downloadMediaInput{BangumiID: 12},
			wantSource: "bangumi", wantID: "12"},
		{name: "无来源回退 AniList", input: downloadMediaInput{AniListID: 13},
			wantSource: "anilist", wantID: "13"},
		{name: "全空", input: downloadMediaInput{}, wantSource: "", wantID: ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			source, id := downloadMediaIdentity(tc.input)
			if source != tc.wantSource || id != tc.wantID {
				t.Fatalf("标识 = (%q,%q)，期望 (%q,%q)", source, id, tc.wantSource, tc.wantID)
			}
		})
	}

	if got := positiveDownloadMediaID(0); got != "" {
		t.Fatalf("非正 ID 应返回空: %q", got)
	}
	if got := positiveDownloadMediaID(-3); got != "" {
		t.Fatalf("负 ID 应返回空: %q", got)
	}
	if got := positiveDownloadMediaID(21); got != "21" {
		t.Fatalf("正 ID = %q", got)
	}
}

// TestDownloadResolverBranches 覆盖解析器查找：未装配注册表、站点 ID 非法、
// 命中但解析器版本未声明、以及完全没有匹配站点。
func TestDownloadResolverBranches(t *testing.T) {
	if _, err := (Dependencies{}).downloadResolver(7); !errors.Is(err, errDownloadResolverUnavailable) {
		t.Fatalf("无注册表 error = %v", err)
	}
	deps := Dependencies{SearchRegistry: downloadHTTPRegistry()}
	if _, err := deps.downloadResolver(0); !errors.Is(err, errDownloadResolverUnavailable) {
		t.Fatalf("站点 ID 非法 error = %v", err)
	}
	if _, err := deps.downloadResolver(99); !errors.Is(err, errDownloadResolverUnavailable) {
		t.Fatalf("站点不存在 error = %v", err)
	}
	resolver, err := deps.downloadResolver(7)
	if err != nil || resolver == nil {
		t.Fatalf("命中解析器 = %v, %v", resolver, err)
	}

	// 声明了站点 ID 但没声明版本的适配器必须被跳过。
	unversioned := Dependencies{SearchRegistry: indexer.NewRegistry([]indexer.Adapter{
		downloadHTTPAdapter{siteID: 8, kind: "test", version: 0},
	})}
	if _, err := unversioned.downloadResolver(8); !errors.Is(err, errDownloadResolverUnavailable) {
		t.Fatalf("未声明版本 error = %v", err)
	}
}
