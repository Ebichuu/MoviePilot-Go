package metadata

import (
	"context"
	"net/http"
	"net/http/httptest"
	"reflect"
	"testing"
)

func TestTMDBMapsOrganizeSuccessFields(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/3/search/multi":
			_, _ = w.Write([]byte(`{"results":[{"id":1,"media_type":"movie","title":"搜索电影","original_language":"zh","vote_average":8.1,"backdrop_path":"/search-backdrop.jpg"}]}`))
		case "/3/movie/1":
			_, _ = w.Write([]byte(`{"id":1,"title":"详情电影","original_language":"en","release_date":"2026-08-16","vote_average":9.2,"backdrop_path":"/detail-backdrop.jpg","genres":[{"id":16},{"id":18}],"production_countries":[{"iso_3166_1":"US"}]}`))
		case "/3/tv/2":
			_, _ = w.Write([]byte(`{"id":2,"name":"无评分剧集","seasons":[{"season_number":0,"episode_count":1},{"season_number":1,"episode_count":12},{"season_number":2,"episode_count":3}]}`))
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(server.Close)

	client := NewTMDB(newMetadataTestHTTPClient(t, 1<<20), server.URL, "test-token")
	tests := []struct {
		name          string
		load          func() (Media, error)
		wantVote      float64
		wantBackdrop  string
		wantSeasons   []SeasonInfo
		wantLanguage  string
		wantGenres    []int
		wantCountries []string
	}{
		{
			name: "search",
			load: func() (Media, error) {
				items, err := client.Search(context.Background(), "搜索电影")
				if err != nil || len(items) != 1 {
					return Media{}, err
				}
				return items[0], nil
			},
			wantVote:     8.1,
			wantBackdrop: "https://image.tmdb.org/t/p/original/search-backdrop.jpg",
			wantLanguage: "zh",
		},
		{
			name: "detail",
			load: func() (Media, error) {
				return client.Detail(context.Background(), "movie:1")
			},
			wantVote:      9.2,
			wantBackdrop:  "https://image.tmdb.org/t/p/original/detail-backdrop.jpg",
			wantLanguage:  "en",
			wantGenres:    []int{16, 18},
			wantCountries: []string{"US"},
		},
		{
			name: "tv detail seasons",
			load: func() (Media, error) {
				return client.Detail(context.Background(), "tv:2")
			},
			wantSeasons: []SeasonInfo{
				{SeasonNumber: 0, EpisodeCount: 1},
				{SeasonNumber: 1, EpisodeCount: 12},
				{SeasonNumber: 2, EpisodeCount: 3},
			},
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			item, err := test.load()
			if err != nil {
				t.Fatal(err)
			}
			if item.VoteAverage != test.wantVote || item.BackdropURL != test.wantBackdrop ||
				!reflect.DeepEqual(item.SeasonInfo, test.wantSeasons) || item.OriginalLanguage != test.wantLanguage ||
				!reflect.DeepEqual(item.GenreIDs, test.wantGenres) ||
				!reflect.DeepEqual(item.ProductionCountries, test.wantCountries) {
				t.Fatalf("detail fields = vote %v backdrop %q seasons %#v language %q genres %#v countries %#v, want %v %q %#v %q %#v %#v",
					item.VoteAverage, item.BackdropURL, item.SeasonInfo, item.OriginalLanguage,
					item.GenreIDs, item.ProductionCountries,
					test.wantVote, test.wantBackdrop, test.wantSeasons, test.wantLanguage,
					test.wantGenres, test.wantCountries)
			}
		})
	}
}
