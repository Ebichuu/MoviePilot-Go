package httpapi

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"

	"github.com/moviepilot-go/moviepilot-go/internal/metadata"
)

// mediaExtraFakeProvider 是 metadata.Provider 的最小实现：固定搜索结果 + 按 ID 取详情。
type mediaExtraFakeProvider struct {
	name      string
	matches   []metadata.Media
	searchErr error
	details   map[string]metadata.Media
	detailErr error
	searches  []string
	detailIDs []string
}

func (p *mediaExtraFakeProvider) Name() string { return p.name }

func (p *mediaExtraFakeProvider) Search(_ context.Context, keyword string) ([]metadata.Media, error) {
	p.searches = append(p.searches, keyword)
	if p.searchErr != nil {
		return nil, p.searchErr
	}
	return append([]metadata.Media(nil), p.matches...), nil
}

func (p *mediaExtraFakeProvider) Detail(_ context.Context, id string) (metadata.Media, error) {
	p.detailIDs = append(p.detailIDs, id)
	if p.detailErr != nil {
		return metadata.Media{}, p.detailErr
	}
	if item, ok := p.details[id]; ok {
		return item, nil
	}
	return metadata.Media{}, fmt.Errorf("元数据源没有 ID %q", id)
}

func mediaExtraDeps(t *testing.T, provider metadata.Provider) Dependencies {
	t.Helper()
	deps, _ := metadataDeps(t, provider)
	return deps
}

func TestMediaSearchGuardsAndPagination(t *testing.T) {
	provider := &mediaExtraFakeProvider{
		name: "douban",
		matches: []metadata.Media{
			{Source: "douban", ID: "1", Type: "movie", Title: "电影1", Year: 2020},
			{Source: "douban", ID: "2", Type: "movie", Title: "电影2", Year: 2021},
			{Source: "douban", ID: "3", Type: "movie", Title: "电影3", Year: 2022},
		},
	}
	deps := mediaExtraDeps(t, provider)

	guards := []string{
		"/media/search",
		"/media/search?title=%20%20",
		"/media/search?title=%E6%9F%90%E5%89%A7&type=collection",
		"/media/search?title=%E6%9F%90%E5%89%A7&type=person",
	}
	for _, target := range guards {
		recorder := httptest.NewRecorder()
		deps.mediaSearch(recorder, httptest.NewRequest(http.MethodGet, target, nil))
		if recorder.Code != http.StatusOK || strings.TrimSpace(recorder.Body.String()) != "[]" {
			t.Fatalf("%s = (%d, %s)", target, recorder.Code, recorder.Body.String())
		}
	}
	if len(provider.searches) != 0 {
		t.Fatalf("守卫分支不应查数据源: %v", provider.searches)
	}

	// 没有元数据源时同样返回空数组。
	recorder := httptest.NewRecorder()
	Dependencies{}.mediaSearch(recorder, httptest.NewRequest(http.MethodGet, "/media/search?title=x", nil))
	if strings.TrimSpace(recorder.Body.String()) != "[]" {
		t.Fatalf("无数据源 body = %s", recorder.Body.String())
	}

	// 搜索失败按空结果降级，不把上游错误透出。
	failing := mediaExtraDeps(t, &mediaExtraFakeProvider{name: "douban", searchErr: errors.New("上游 500")})
	recorder = httptest.NewRecorder()
	failing.mediaSearch(recorder, httptest.NewRequest(http.MethodGet, "/media/search?title=x", nil))
	if strings.TrimSpace(recorder.Body.String()) != "[]" {
		t.Fatalf("搜索失败 body = %s", recorder.Body.String())
	}

	// 分页：page=2&count=2 只取到第三条。
	recorder = httptest.NewRecorder()
	deps.mediaSearch(recorder, httptest.NewRequest(http.MethodGet, "/media/search?title=xx&page=2&count=2", nil))
	var items []map[string]any
	if err := json.Unmarshal(recorder.Body.Bytes(), &items); err != nil {
		t.Fatalf("解析搜索结果: %v; body=%s", err, recorder.Body.String())
	}
	if len(items) != 1 || items[0]["title"] != "电影3" || items[0]["type"] != "电影" {
		t.Fatalf("分页结果 = %#v", items)
	}
	if len(provider.searches) != 1 || provider.searches[0] != "xx" {
		t.Fatalf("搜索关键词 = %v", provider.searches)
	}
}

func TestMediaDetailDirectHitAndEmptyFallback(t *testing.T) {
	provider := &mediaExtraFakeProvider{
		name: "douban",
		details: map[string]metadata.Media{
			"movie:123": {Source: "douban", ID: "123", Type: "movie", Title: "某电影", Year: 2021},
		},
	}
	deps := mediaExtraDeps(t, provider)
	router := chi.NewRouter()
	router.Get("/media/{mediaid}", deps.mediaDetail)

	recorder := httptest.NewRecorder()
	router.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/media/douban:123?type_name=%E7%94%B5%E5%BD%B1", nil))
	info := decodeMediaInfo(t, recorder.Body.Bytes())
	if info["title"] != "某电影" || info["type"] != "电影" || info["year"] != "2021" {
		t.Fatalf("详情 = %#v", info)
	}
	if len(provider.detailIDs) == 0 || provider.detailIDs[0] != "movie:123" {
		t.Fatalf("详情请求 ID = %v", provider.detailIDs)
	}

	// 查不到且没有标题：返回全空 MediaInfo，而不是报错。
	recorder = httptest.NewRecorder()
	router.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/media/douban:999", nil))
	empty := decodeMediaInfo(t, recorder.Body.Bytes())
	if empty["title"] != nil || empty["type"] != nil || empty["media_id"] != nil {
		t.Fatalf("空详情 = %#v", empty)
	}
}

func TestMediaSeasonsProjectsSeasonInfo(t *testing.T) {
	provider := &mediaExtraFakeProvider{
		name: "douban",
		details: map[string]metadata.Media{
			"tv:123": {
				Source: "douban", ID: "123", Type: "tv", Title: "某剧", PosterURL: "https://img/tv.jpg",
				SeasonInfo: []metadata.SeasonInfo{
					{SeasonNumber: 1, EpisodeCount: 12, AirDate: "2020-01-01"},
					{SeasonNumber: 2, EpisodeCount: 10},
				},
			},
		},
	}
	deps := mediaExtraDeps(t, provider)
	router := chi.NewRouter()
	router.Get("/media/seasons", deps.mediaSeasons)

	recorder := httptest.NewRecorder()
	router.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/media/seasons?mediaid=douban:123", nil))
	var seasons []map[string]any
	if err := json.Unmarshal(recorder.Body.Bytes(), &seasons); err != nil {
		t.Fatalf("解析季列表: %v; body=%s", err, recorder.Body.String())
	}
	if len(seasons) != 2 || seasons[0]["season_number"] != float64(1) ||
		seasons[0]["name"] != "第 1 季" || seasons[0]["air_date"] != "2020-01-01" ||
		seasons[0]["episode_count"] != float64(12) || seasons[0]["poster_path"] != "https://img/tv.jpg" {
		t.Fatalf("季列表 = %#v", seasons)
	}

	// season 参数过滤单季。
	recorder = httptest.NewRecorder()
	router.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/media/seasons?mediaid=douban:123&season=2", nil))
	seasons = nil
	if err := json.Unmarshal(recorder.Body.Bytes(), &seasons); err != nil {
		t.Fatalf("解析单季: %v", err)
	}
	if len(seasons) != 1 || seasons[0]["season_number"] != float64(2) || seasons[0]["episode_count"] != float64(10) {
		t.Fatalf("单季 = %#v", seasons)
	}

	// 未知媒体键返回空数组。
	recorder = httptest.NewRecorder()
	router.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/media/seasons?mediaid=unknown", nil))
	if strings.TrimSpace(recorder.Body.String()) != "[]" {
		t.Fatalf("未知媒体键 = %s", recorder.Body.String())
	}
}

func TestMediaRecognizeFileProjectsParsedName(t *testing.T) {
	recorder := httptest.NewRecorder()
	Dependencies{}.mediaRecognizeFile(recorder, httptest.NewRequest(http.MethodGet,
		"/media/recognize_file?path=%2Fdownloads%2F%E6%9F%90%E5%89%A7.S01E02.2024.1080p.WEB-DL.mkv", nil))
	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, body=%s", recorder.Code, recorder.Body.String())
	}
	var body struct {
		MetaInfo  map[string]any `json:"meta_info"`
		MediaInfo any            `json:"media_info"`
	}
	if err := json.Unmarshal(recorder.Body.Bytes(), &body); err != nil {
		t.Fatalf("解析识别结果: %v; body=%s", err, recorder.Body.String())
	}
	if body.MetaInfo["path"] != "/downloads/某剧.S01E02.2024.1080p.WEB-DL.mkv" {
		t.Fatalf("path = %v", body.MetaInfo["path"])
	}
	if title, _ := body.MetaInfo["title"].(string); !strings.Contains(title, "某剧") {
		t.Fatalf("title = %v", body.MetaInfo["title"])
	}
	if body.MetaInfo["year"] != "2024" {
		t.Fatalf("year = %v", body.MetaInfo["year"])
	}
	if body.MediaInfo != nil {
		t.Fatalf("没有元数据源时 media_info 应为 null: %#v", body.MediaInfo)
	}

	// 空路径直接 400。
	recorder = httptest.NewRecorder()
	Dependencies{}.mediaRecognizeFile(recorder, httptest.NewRequest(http.MethodGet, "/media/recognize_file", nil))
	if recorder.Code != http.StatusBadRequest || !strings.Contains(recorder.Body.String(), "path不能为空") {
		t.Fatalf("空路径 = (%d, %s)", recorder.Code, recorder.Body.String())
	}
}

func TestEmptyMediaInfoShape(t *testing.T) {
	info := emptyMediaInfo()
	wantKeys := []string{
		"source", "type", "title", "en_title", "year", "original_title", "overview",
		"poster_path", "names", "mediaid_prefix", "media_id", "seasons", "season_info", "title_year",
		// 详情页模板会直接 `episode_run_time[0]` / `genres.map`，这几个缺了就整页空白。
		"episode_run_time", "genres", "runtime", "vote_average",
	}
	if len(info) < len(wantKeys) {
		t.Fatalf("字段数 = %d, 至少应有 %d", len(info), len(wantKeys))
	}
	for _, key := range wantKeys {
		if _, ok := info[key]; !ok {
			t.Fatalf("缺少字段 %q", key)
		}
	}
	if names, ok := info["names"].([]string); !ok || len(names) != 0 {
		t.Fatalf("names = %#v", info["names"])
	}
	if seasons, ok := info["seasons"].(map[string]any); !ok || len(seasons) != 0 {
		t.Fatalf("seasons = %#v", info["seasons"])
	}
	if info["title"] != nil || info["year"] != nil || info["type"] != nil {
		t.Fatalf("空值字段 = %#v", info)
	}
}
