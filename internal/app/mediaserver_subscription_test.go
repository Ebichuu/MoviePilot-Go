package app

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/moviepilot-go/moviepilot-go/internal/mediaconfig"
	"github.com/moviepilot-go/moviepilot-go/internal/subscription"
	"github.com/moviepilot-go/moviepilot-go/internal/subscriptionflow"
)

func TestMediaServerRuntimeFindExistingMovieUsesV2MovieFlow(t *testing.T) {
	t.Parallel()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		query := r.URL.Query()
		want := map[string]string{
			"IncludeItemTypes": "Movie",
			"Fields": "ProviderIds,OriginalTitle,ProductionYear,Path," +
				"UserDataPlayCount,UserDataLastPlayedDate,ParentId",
			"StartIndex": "0", "Recursive": "true", "SearchTerm": "Example Movie",
			"Limit": "10", "IncludeSearchTypes": "false",
		}
		for key, value := range want {
			if query.Get(key) != value {
				t.Errorf("movie query %s = %q, want %q", key, query.Get(key), value)
			}
		}
		_, _ = io.WriteString(w, `{"Items":[`+
			`{"Name":"Example Movie","ProductionYear":2025,"ProviderIds":{"Tmdb":"123"}},`+
			`{"Name":"Example Movie","ProductionYear":2026,"ProviderIds":{"Tmdb":"999"}},`+
			`{"Name":"Example Movie","ProductionYear":2026,"ProviderIds":{"Tmdb":"123"}}`+
			`],"TotalRecordCount":3,"StartIndex":0}`)
	}))
	t.Cleanup(server.Close)
	setting := mediaServerRuntimeTestServer(nil)
	setting.Config.Host = server.URL
	runtime := newMediaServerRuntimeCompletionPlanTest(t, []mediaconfig.Server{setting})

	found, err := runtime.FindExistingMovie(t.Context(), subscriptionflow.ExistingMovieQuery{
		Title: "Example Movie", Year: "2026", MediaSource: "themoviedb", MediaID: "123",
	})
	if err != nil || !found {
		t.Fatalf("FindExistingMovie() = (%v, %v), want true", found, err)
	}
}

func TestMediaServerRuntimeFindExistingMovieBoundaries(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name      string
		response  string
		query     subscriptionflow.ExistingMovieQuery
		wantFound bool
	}{
		{
			name: "TMDB provider is required when subscription has TMDB identity",
			response: `{"Items":[{"Name":"Movie","ProductionYear":2026}],` +
				`"TotalRecordCount":1,"StartIndex":0}`,
			query: subscriptionflow.ExistingMovieQuery{
				Title: "Movie", Year: "2026", MediaSource: "tmdb", MediaID: "123",
			},
		},
		{
			name: "non TMDB source uses exact title and year",
			response: `{"Items":[{"Name":"Movie","ProductionYear":2026}],` +
				`"TotalRecordCount":1,"StartIndex":0}`,
			query:     subscriptionflow.ExistingMovieQuery{Title: "Movie", Year: "2026", MediaSource: "douban", MediaID: "db1"},
			wantFound: true,
		},
		{
			name: "year mismatch is rejected",
			response: `{"Items":[{"Name":"Movie","ProductionYear":2025}],` +
				`"TotalRecordCount":1,"StartIndex":0}`,
			query: subscriptionflow.ExistingMovieQuery{Title: "Movie", Year: "2026"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				_, _ = io.WriteString(w, tt.response)
			}))
			t.Cleanup(server.Close)
			setting := mediaServerRuntimeTestServer(nil)
			setting.Config.Host = server.URL
			runtime := newMediaServerRuntimeCompletionPlanTest(t, []mediaconfig.Server{setting})
			found, err := runtime.FindExistingMovie(t.Context(), tt.query)
			if err != nil || found != tt.wantFound {
				t.Fatalf("FindExistingMovie() = (%v, %v), want %v", found, err, tt.wantFound)
			}
		})
	}
}

func TestMediaServerRuntimeFindExistingEpisodesUsesV2SeriesFlow(t *testing.T) {
	t.Parallel()
	requests := make([]string, 0, 3)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests = append(requests, r.URL.RequestURI())
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/Items":
			query := r.URL.Query()
			want := map[string]string{
				"IncludeItemTypes": "Series", "Fields": "ProductionYear,ProviderIds",
				"StartIndex": "0", "Recursive": "true", "SearchTerm": "Example Show",
				"Limit": "10", "IncludeSearchTypes": "false",
			}
			for key, value := range want {
				if query.Get(key) != value {
					t.Errorf("series query %s = %q, want %q", key, query.Get(key), value)
				}
			}
			_, _ = io.WriteString(w, `{"Items":[`+
				`{"Id":"wrong","Name":"Other Show","Type":"Series","ProductionYear":2026},`+
				`{"Id":"series-1","Name":"Example Show","Type":"Series","ProductionYear":2026,"ProviderIds":{"Tmdb":"123"}}`+
				`],"TotalRecordCount":2,"StartIndex":0}`)
		case "/Shows/series-1/Episodes":
			query := r.URL.Query()
			if query.Get("Limit") != "200" || query.Get("IsMissing") != "false" {
				t.Errorf("episode query = %v", query)
			}
			switch query.Get("StartIndex") {
			case "":
				_, _ = io.WriteString(w, `{"Items":[`+
					`{"ParentIndexNumber":2,"IndexNumber":3},`+
					`{"ParentIndexNumber":1,"IndexNumber":2}`+
					`],"TotalRecordCount":4,"StartIndex":0}`)
			case "2":
				_, _ = io.WriteString(w, `{"Items":[`+
					`{"ParentIndexNumber":1,"IndexNumber":1},`+
					`{"ParentIndexNumber":1,"IndexNumber":2}`+
					`],"TotalRecordCount":4,"StartIndex":2}`)
			default:
				http.Error(w, "unexpected page", http.StatusBadRequest)
			}
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(server.Close)
	setting := mediaServerRuntimeTestServer(nil)
	setting.Config.Host = server.URL
	runtime := newMediaServerRuntimeCompletionPlanTest(t, []mediaconfig.Server{setting})

	got, err := runtime.FindExistingEpisodes(t.Context(), subscriptionflow.ExistingEpisodesQuery{
		Title: "Example Show", Year: "2026", MediaSource: "themoviedb", MediaID: "123",
	})
	if err != nil {
		t.Fatal(err)
	}
	want := []subscription.Episode{
		{Season: 1, Episode: 1}, {Season: 1, Episode: 2}, {Season: 2, Episode: 3},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("FindExistingEpisodes() = %#v, want %#v", got, want)
	}
	if len(requests) != 3 {
		t.Fatalf("requests = %#v, want series plus two episode pages", requests)
	}
}

func TestMediaServerRuntimeFindExistingEpisodesRejectsTMDBMismatch(t *testing.T) {
	t.Parallel()
	episodeRequests := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.Contains(r.URL.Path, "/Episodes") {
			episodeRequests++
		}
		_, _ = io.WriteString(w, `{"Items":[{"Id":"series-1","Name":"Example Show","Type":"Series",`+
			`"ProductionYear":2026,"ProviderIds":{"Tmdb":"999"}}],"TotalRecordCount":1,"StartIndex":0}`)
	}))
	t.Cleanup(server.Close)
	setting := mediaServerRuntimeTestServer(nil)
	setting.Config.Host = server.URL
	runtime := newMediaServerRuntimeCompletionPlanTest(t, []mediaconfig.Server{setting})

	got, err := runtime.FindExistingEpisodes(t.Context(), subscriptionflow.ExistingEpisodesQuery{
		Title: "Example Show", Year: "2026", MediaSource: "tmdb", MediaID: "123",
	})
	if err != nil || len(got) != 0 {
		t.Fatalf("FindExistingEpisodes() = (%#v, %v), want empty", got, err)
	}
	if episodeRequests != 0 {
		t.Fatalf("TMDB mismatch caused %d episode requests", episodeRequests)
	}
}

func TestMediaServerRuntimeFindExistingEpisodesFailsOpen(t *testing.T) {
	t.Parallel()
	const sensitive = "secret-response-marker"
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, sensitive, http.StatusInternalServerError)
	}))
	t.Cleanup(server.Close)
	setting := mediaServerRuntimeTestServer(nil)
	setting.Config.Host = server.URL
	runtime := newMediaServerRuntimeCompletionPlanTest(t, []mediaconfig.Server{setting})

	got, err := runtime.FindExistingEpisodes(t.Context(), subscriptionflow.ExistingEpisodesQuery{Title: "Example Show"})
	if err != nil || len(got) != 0 {
		t.Fatalf("FindExistingEpisodes() = (%#v, %v), want empty fail-open result", got, err)
	}
}

func TestMediaServerRuntimeFindExistingEpisodesServerOrder(t *testing.T) {
	t.Parallel()
	newServer := func(handler http.HandlerFunc) *httptest.Server {
		server := httptest.NewServer(handler)
		t.Cleanup(server.Close)
		return server
	}
	mediaServerSetting := func(id, name, host string) mediaconfig.Server {
		setting := mediaServerRuntimeTestServer(nil)
		setting.ID = id
		setting.Name = name
		setting.Config.Host = host
		return setting
	}

	t.Run("failure continues to next server and preserves season zero", func(t *testing.T) {
		first := newServer(func(w http.ResponseWriter, _ *http.Request) {
			http.Error(w, "unavailable", http.StatusInternalServerError)
		})
		second := newServer(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			switch r.URL.Path {
			case "/Items":
				_, _ = io.WriteString(w, `{"Items":[{"Id":"series-2","Name":"Show","ProductionYear":2026}],`+
					`"TotalRecordCount":1,"StartIndex":0}`)
			case "/Shows/series-2/Episodes":
				_, _ = io.WriteString(w, `{"Items":[{"ParentIndexNumber":0,"IndexNumber":1},`+
					`{"IndexNumber":2},`+
					`{"ParentIndexNumber":1,"IndexNumber":1}],"TotalRecordCount":3,"StartIndex":0}`)
			default:
				http.NotFound(w, r)
			}
		})
		runtime := newMediaServerRuntimeCompletionPlanTest(t, []mediaconfig.Server{
			mediaServerSetting("03158472-b848-4211-b72a-b3ca83d7b8ae", "First", first.URL),
			mediaServerSetting("13158472-b848-4211-b72a-b3ca83d7b8ae", "Second", second.URL),
		})
		got, err := runtime.FindExistingEpisodes(t.Context(), subscriptionflow.ExistingEpisodesQuery{
			Title: "Show", Year: "2026",
		})
		want := []subscription.Episode{{Season: 0, Episode: 1}, {Season: 1, Episode: 1}}
		if err != nil || !reflect.DeepEqual(got, want) {
			t.Fatalf("FindExistingEpisodes() = (%#v, %v), want %#v", got, err, want)
		}
	})

	t.Run("first nonempty server stops lookup", func(t *testing.T) {
		first := newServer(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			if r.URL.Path == "/Items" {
				_, _ = io.WriteString(w, `{"Items":[{"Id":"series-1","Name":"Show"}],"TotalRecordCount":1,"StartIndex":0}`)
				return
			}
			_, _ = io.WriteString(w, `{"Items":[{"ParentIndexNumber":1,"IndexNumber":1}],"TotalRecordCount":1,"StartIndex":0}`)
		})
		var secondCalls atomic.Int32
		second := newServer(func(w http.ResponseWriter, _ *http.Request) {
			secondCalls.Add(1)
			_, _ = io.WriteString(w, `{"Items":[],"TotalRecordCount":0,"StartIndex":0}`)
		})
		runtime := newMediaServerRuntimeCompletionPlanTest(t, []mediaconfig.Server{
			mediaServerSetting("23158472-b848-4211-b72a-b3ca83d7b8ae", "First Hit", first.URL),
			mediaServerSetting("33158472-b848-4211-b72a-b3ca83d7b8ae", "Must Not Query", second.URL),
		})
		got, err := runtime.FindExistingEpisodes(t.Context(), subscriptionflow.ExistingEpisodesQuery{Title: "Show"})
		if err != nil || !reflect.DeepEqual(got, []subscription.Episode{{Season: 1, Episode: 1}}) {
			t.Fatalf("FindExistingEpisodes() = (%#v, %v)", got, err)
		}
		if secondCalls.Load() != 0 {
			t.Fatalf("second server calls = %d, want 0", secondCalls.Load())
		}
	})
}

func TestMediaServerRuntimeFindExistingEpisodesPropagatesCancellation(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	runtime := &mediaServerRuntime{}
	_, err := runtime.FindExistingEpisodes(ctx, subscriptionflow.ExistingEpisodesQuery{Title: "Show"})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("FindExistingEpisodes() error = %v, want context.Canceled", err)
	}
}
