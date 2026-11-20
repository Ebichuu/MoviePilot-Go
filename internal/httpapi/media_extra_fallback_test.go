package httpapi

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"

	"github.com/moviepilot-go/moviepilot-go/internal/metadata"
)

func mediaExtraRouteRequest(target, mediaID string) *http.Request {
	request := httptest.NewRequest(http.MethodGet, target, nil)
	rctx := chi.NewRouteContext()
	if mediaID != "" {
		rctx.URLParams.Add("mediaid", mediaID)
	}
	return request.WithContext(context.WithValue(request.Context(), chi.RouteCtxKey, rctx))
}

// TestMediaDetailTitleFallbacks 覆盖「直查找不到 → 按标题识别 → 电视剧结果回退」
// 这三段逐级降级：识别命中直接用，识别不中时在搜索结果里挑电视剧候选再取详情，
// 详情也取不到则回到空 MediaInfo。
func TestMediaDetailTitleFallbacks(t *testing.T) {
	// 识别命中：标题与年份都对得上。
	identified := mediaExtraDeps(t, &mediaExtraFakeProvider{
		name: "douban",
		matches: []metadata.Media{
			{Source: "douban", ID: "9", Type: "tv", Title: "另一部剧", Year: 2021},
		},
	})
	recorder := httptest.NewRecorder()
	identified.mediaDetail(recorder, mediaExtraRouteRequest("/media/?title=%E5%8F%A6%E4%B8%80%E9%83%A8%E5%89%A7&year=2021", ""))
	if !strings.Contains(recorder.Body.String(), "另一部剧") {
		t.Fatalf("标题识别 = %s", recorder.Body.String())
	}

	// 识别不中（标题完全无关）时，回退到搜索结果里的电视剧候选详情。
	fallback := mediaExtraDeps(t, &mediaExtraFakeProvider{
		name: "douban",
		matches: []metadata.Media{
			{Source: "douban", ID: "9", Type: "tv", Title: "完全不同"},
		},
		details: map[string]metadata.Media{
			"tv:9": {Source: "douban", ID: "9", Type: "tv", Title: "回退命中"},
		},
	})
	recorder = httptest.NewRecorder()
	fallback.mediaDetail(recorder, mediaExtraRouteRequest("/media/?title=%E6%9F%90%E5%89%A7", ""))
	if !strings.Contains(recorder.Body.String(), "回退命中") {
		t.Fatalf("电视剧回退 = %s", recorder.Body.String())
	}

	// 候选详情也取不到：仍返回空 MediaInfo。
	brokenFallback := mediaExtraDeps(t, &mediaExtraFakeProvider{
		name: "douban",
		matches: []metadata.Media{
			{Source: "douban", ID: "9", Type: "tv", Title: "完全不同"},
		},
	})
	recorder = httptest.NewRecorder()
	brokenFallback.mediaDetail(recorder, mediaExtraRouteRequest("/media/?title=%E6%9F%90%E5%89%A7", ""))
	if !strings.Contains(recorder.Body.String(), `"title":null`) {
		t.Fatalf("回退详情失败 = %s", recorder.Body.String())
	}

	// 电视剧类型参数：type_name=电视剧 会把识别类型收窄成电视剧。
	tvTyped := mediaExtraDeps(t, &mediaExtraFakeProvider{
		name: "douban",
		details: map[string]metadata.Media{
			"tv:21": {Source: "douban", ID: "21", Type: "tv", Title: "某部剧"},
		},
	})
	recorder = httptest.NewRecorder()
	tvTyped.mediaDetail(recorder,
		mediaExtraRouteRequest("/media/?mediaid=douban%3A21&type_name=%E7%94%B5%E8%A7%86%E5%89%A7", "douban:21"))
	if !strings.Contains(recorder.Body.String(), "某部剧") || !strings.Contains(recorder.Body.String(), `"type":"电视剧"`) {
		t.Fatalf("电视剧类型 = %s", recorder.Body.String())
	}

	// 回退候选里电影排在最前：电影被跳过，电视剧候选命中。
	skipMovie := mediaExtraDeps(t, &mediaExtraFakeProvider{
		name: "douban",
		matches: []metadata.Media{
			{Source: "douban", ID: "31", Type: "movie", Title: "完全不同"},
			{Source: "douban", ID: "32", Type: "tv", Title: "也不同"},
		},
		details: map[string]metadata.Media{
			"tv:32": {Source: "douban", ID: "32", Type: "tv", Title: "跳过电影后命中"},
		},
	})
	recorder = httptest.NewRecorder()
	skipMovie.mediaDetail(recorder, mediaExtraRouteRequest("/media/?title=%E6%9F%90%E5%89%A7", ""))
	if !strings.Contains(recorder.Body.String(), "跳过电影后命中") {
		t.Fatalf("跳过电影候选 = %s", recorder.Body.String())
	}

	// 电影类型参数：type_name=电影 会把识别类型收窄成电影。
	movieOnly := mediaExtraDeps(t, &mediaExtraFakeProvider{
		name: "douban",
		matches: []metadata.Media{
			{Source: "douban", ID: "11", Type: "movie", Title: "某个电影", Year: 2019},
		},
	})
	recorder = httptest.NewRecorder()
	movieOnly.mediaDetail(recorder,
		mediaExtraRouteRequest("/media/?title=%E6%9F%90%E4%B8%AA%E7%94%B5%E5%BD%B1&type_name=%E7%94%B5%E5%BD%B1", ""))
	if !strings.Contains(recorder.Body.String(), "某个电影") || !strings.Contains(recorder.Body.String(), `"type":"电影"`) {
		t.Fatalf("电影类型 = %s", recorder.Body.String())
	}
}

// TestMediaSeasonsTitleFallback 覆盖季列表在「没有媒体 ID、只有标题」时的两条补全路径：
// 先用 TMDB 搜索结果里带季信息的候选，识别命中后还要再取一次详情补齐季表。
func TestMediaSeasonsTitleFallback(t *testing.T) {
	// 搜索候选带季信息：直接采用。
	withSeasons := mediaExtraDeps(t, &mediaExtraFakeProvider{
		name: "themoviedb",
		matches: []metadata.Media{
			{Source: "themoviedb", ID: "5", Type: "tv", Title: "某剧"},
		},
		details: map[string]metadata.Media{
			"tv:5": {
				Source: "themoviedb", ID: "5", Type: "tv", Title: "某剧",
				SeasonInfo: []metadata.SeasonInfo{{SeasonNumber: 3, EpisodeCount: 12}},
			},
		},
	})
	recorder := httptest.NewRecorder()
	withSeasons.mediaSeasons(recorder, mediaExtraRouteRequest("/media/seasons?title=%E6%9F%90%E5%89%A7", ""))
	if !strings.Contains(recorder.Body.String(), "第 3 季") {
		t.Fatalf("搜索补季表 = %s", recorder.Body.String())
	}

	// 识别命中但没有季信息：命中后再按 ID 取一次详情（仍然没有季表则返回空列表）。
	noSeasons := mediaExtraDeps(t, &mediaExtraFakeProvider{
		name: "themoviedb",
		matches: []metadata.Media{
			{Source: "themoviedb", ID: "6", Type: "tv", Title: "没有季的剧"},
		},
		details: map[string]metadata.Media{
			"tv:6": {Source: "themoviedb", ID: "6", Type: "tv", Title: "没有季的剧"},
		},
	})
	recorder = httptest.NewRecorder()
	noSeasons.mediaSeasons(recorder, mediaExtraRouteRequest("/media/seasons?title=%E6%B2%A1%E6%9C%89%E5%AD%A3%E7%9A%84%E5%89%A7", ""))
	if strings.TrimSpace(recorder.Body.String()) != "[]" {
		t.Fatalf("无季信息 = %s", recorder.Body.String())
	}

	// 只有标题与年份、候选都没有季信息时：先用 TMDB 搜索结果并跳过无季候选，
	// 识别不中后按电视剧结果回退取详情，最后再补一次详情。
	yearly := mediaExtraDeps(t, &mediaExtraFakeProvider{
		name: "themoviedb",
		matches: []metadata.Media{
			{Source: "themoviedb", ID: "41", Type: "movie", Title: "完全不同"},
			{Source: "themoviedb", ID: "42", Type: "tv", Title: "也不同"},
		},
		details: map[string]metadata.Media{
			"tv:42": {Source: "themoviedb", ID: "42", Type: "tv", Title: "也不同"},
		},
	})
	recorder = httptest.NewRecorder()
	yearly.mediaSeasons(recorder,
		mediaExtraRouteRequest("/media/seasons?title=%E6%9F%90%E5%89%A7&year=2021", ""))
	if strings.TrimSpace(recorder.Body.String()) != "[]" {
		t.Fatalf("年份补全无季 = %s", recorder.Body.String())
	}

	// 搜索与识别都失败：空列表。
	empty := mediaExtraDeps(t, &mediaExtraFakeProvider{name: "themoviedb"})
	recorder = httptest.NewRecorder()
	empty.mediaSeasons(recorder, mediaExtraRouteRequest("/media/seasons?title=%E6%9F%A5%E4%B8%8D%E5%88%B0", ""))
	if strings.TrimSpace(recorder.Body.String()) != "[]" {
		t.Fatalf("完全查不到 = %s", recorder.Body.String())
	}
}

// TestMediaRecognizeFileWithMetadata 覆盖「有元数据源时识别成功」的出口。
func TestMediaRecognizeFileWithMetadata(t *testing.T) {
	deps := mediaExtraDeps(t, &mediaExtraFakeProvider{
		name: "douban",
		matches: []metadata.Media{
			{Source: "douban", ID: "3", Type: "tv", Title: "某剧", Year: 2024},
		},
	})
	recorder := httptest.NewRecorder()
	deps.mediaRecognizeFile(recorder, mediaExtraRouteRequest(
		"/media/recognize_file?path=%2Fdownloads%2F%E6%9F%90%E5%89%A7.S01E02.2024.1080p.WEB-DL.mkv", ""))
	body := recorder.Body.String()
	if !strings.Contains(body, `"media_info":{`) || !strings.Contains(body, "某剧") {
		t.Fatalf("识别成功 = %s", body)
	}
}
