package httpapi

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"reflect"
	"testing"
	"time"

	"github.com/moviepilot-go/moviepilot-go/internal/metadata"
)

func TestRecognizeMediaUsesMetadataCoordinator(t *testing.T) {
	coordinator, err := metadata.NewCoordinator([]metadata.Provider{recognizeProvider{}}, metadata.Options{
		RequestTimeout: time.Second, CacheTTL: time.Hour, MaxResults: 20,
	})
	if err != nil {
		t.Fatal(err)
	}
	deps := Dependencies{Metadata: coordinator}
	request := httptest.NewRequest(http.MethodGet, "/media/recognize?title=Example%20Movie%202026&source=tmdb", nil)
	response := httptest.NewRecorder()

	deps.recognizeMedia(response, request)

	if response.Code != http.StatusOK {
		t.Fatalf("识别状态码错误: %d body=%s", response.Code, response.Body.String())
	}
	var payload map[string]any
	if err := json.Unmarshal(response.Body.Bytes(), &payload); err != nil {
		t.Fatal(err)
	}
	mediaInfo, ok := payload["media_info"].(map[string]any)
	if !ok || mediaInfo["source"] != "themoviedb" || mediaInfo["title"] != "Example Movie" {
		t.Fatalf("元数据未映射到 V2 上下文: %#v", payload)
	}
	if payload["match_source"] != "title" || payload["candidate_recognized"] != true {
		t.Fatalf("识别上下文状态错误: %#v", payload)
	}
	if mediaInfo["tmdb_id"] != float64(7) || mediaInfo["type"] != "电影" {
		t.Fatalf("V2 媒体字段映射错误: %#v", mediaInfo)
	}
}

func TestRecognizeMediaReturnsV2SeasonDataFromDetail(t *testing.T) {
	coordinator, err := metadata.NewCoordinator([]metadata.Provider{recognizeTVProvider{}}, metadata.Options{
		RequestTimeout: time.Second, CacheTTL: time.Hour, MaxResults: 20,
	})
	if err != nil {
		t.Fatal(err)
	}
	deps := Dependencies{Metadata: coordinator}
	title := "Example.Show.S02.{[tmdbid=8;type=tv]}"
	request := httptest.NewRequest(http.MethodGet, "/media/recognize?title="+url.QueryEscape(title), nil)
	response := httptest.NewRecorder()

	deps.recognizeMedia(response, request)

	if response.Code != http.StatusOK {
		t.Fatalf("识别状态码错误: %d body=%s", response.Code, response.Body.String())
	}
	var payload map[string]any
	if err := json.Unmarshal(response.Body.Bytes(), &payload); err != nil {
		t.Fatal(err)
	}
	mediaInfo, ok := payload["media_info"].(map[string]any)
	if !ok {
		t.Fatalf("媒体详情缺失: %#v", payload)
	}
	seasons, ok := mediaInfo["seasons"].(map[string]any)
	if !ok || !reflect.DeepEqual(seasons["2"], []any{float64(1), float64(2), float64(3)}) {
		t.Fatalf("V2 seasons 映射错误: %#v", mediaInfo["seasons"])
	}
	wantSeasonInfo := []any{map[string]any{"season_number": float64(2), "episode_count": float64(3)}}
	if !reflect.DeepEqual(mediaInfo["season_info"], wantSeasonInfo) || mediaInfo["season"] != float64(2) {
		t.Fatalf("V2 season_info 映射错误: %#v", mediaInfo)
	}
}

type recognizeProvider struct{}

func (recognizeProvider) Name() string { return "themoviedb" }

func (recognizeProvider) Search(context.Context, string) ([]metadata.Media, error) {
	return []metadata.Media{{
		Source: "themoviedb", ID: "7", Type: "movie", Title: "Example Movie",
		OriginalTitle: "Example Movie", Year: 2026,
	}}, nil
}

func (recognizeProvider) Detail(context.Context, string) (metadata.Media, error) {
	return metadata.Media{}, nil
}

type recognizeTVProvider struct{}

func (recognizeTVProvider) Name() string { return "themoviedb" }

func (recognizeTVProvider) Search(context.Context, string) ([]metadata.Media, error) {
	return nil, nil
}

func (recognizeTVProvider) Detail(_ context.Context, id string) (metadata.Media, error) {
	return metadata.Media{
		Source: "themoviedb", ID: id, Type: "tv", Title: "Example Show",
		SeasonInfo: []metadata.SeasonInfo{{SeasonNumber: 2, EpisodeCount: 3}},
	}, nil
}
