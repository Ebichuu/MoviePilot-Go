package app

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/moviepilot-go/moviepilot-go/internal/mediaconfig"
)

func TestMediaServerRuntimeEmbyHomepageLatestPlayingAndLibraries(t *testing.T) {
	t.Parallel()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/Users":
			_ = json.NewEncoder(w).Encode([]map[string]any{
				{"Id": "user-admin", "Name": "admin", "Policy": map[string]any{"IsAdministrator": true}},
			})
		case "/Library/VirtualFolders":
			_ = json.NewEncoder(w).Encode([]map[string]any{
				{"Name": "电影", "CollectionType": "movies", "Locations": []string{"/media/movies"}, "ItemId": "lib-movies"},
				{"Name": "隐藏库", "CollectionType": "tvshows", "Locations": []string{"/media/hidden"}, "ItemId": "lib-hidden"},
			})
		case "/Users/user-admin/Items/Latest":
			_ = json.NewEncoder(w).Encode([]map[string]any{
				{
					"Id": "mov-1", "Name": "沙丘", "Type": "Movie",
					"Path": "/media/movies/Dune (2021)/Dune.mkv", "ServerId": "emby-srv-1",
					"ProductionYear": 2021, "BackdropImageTags": []string{"tag-1"},
				},
				{
					"Id": "hid-1", "Name": "隐藏剧", "Type": "Series",
					"Path": "/media/hidden/Hidden/S01E01.mkv", "ServerId": "emby-srv-1",
					"ProductionYear": 2022,
				},
				{
					"Id": "aud-1", "Name": "非视频", "Type": "Audio",
					"Path": "/media/movies/song.mp3", "ServerId": "emby-srv-1",
				},
			})
		case "/Users/user-admin/Items/Resume":
			_ = json.NewEncoder(w).Encode(map[string]any{
				"Items": []map[string]any{
					{
						"Id": "mov-1", "Name": "沙丘", "Type": "Movie",
						"Path": "/media/movies/Dune (2021)/Dune.mkv", "ServerId": "emby-srv-1",
						"ProductionYear": 2021, "BackdropImageTags": []string{"bd-1"},
						"UserData": map[string]any{"PlayedPercentage": 66.5},
					},
					{
						"Id": "ep-9", "Name": "试播集", "Type": "Episode",
						"Path": "/media/movies/Show/S01E01.mkv", "ServerId": "emby-srv-1",
						"SeriesId": "ser-9", "SeriesName": "绝命毒师", "SeriesPrimaryImageTag": "stag-9",
						"ParentIndexNumber": 1, "IndexNumber": 2,
						"UserData": map[string]any{"PlayedPercentage": 25.0},
					},
					{
						"Id": "hid-ep", "Name": "隐藏集", "Type": "Episode",
						"Path": "/media/hidden/Show/S01E01.mkv", "ServerId": "emby-srv-1",
						"SeriesId": "ser-hid", "SeriesName": "隐藏剧",
						"ParentIndexNumber": 1, "IndexNumber": 1,
					},
				},
			})
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	snapshot, _, err := mediaconfig.Normalize([]mediaconfig.Server{{
		Name:          "HomeEmby",
		Type:          "emby",
		Enabled:       true,
		SyncLibraries: []string{"lib-movies"},
		Config: mediaconfig.Config{
			Host:     server.URL,
			PlayHost: "https://emby.play.test",
			Username: "admin",
			APIKey:   "emby-secret-key",
		},
	}})
	if err != nil {
		t.Fatalf("Normalize: %v", err)
	}
	runtime, err := buildMediaServerRuntime(snapshot, 5*time.Second, nil)
	if err != nil {
		t.Fatalf("buildMediaServerRuntime: %v", err)
	}
	defer runtime.CloseIdleConnections()

	ctx := context.Background()

	// 1. Libraries with hidden=true filters by sync_libraries and populates V2 image/link fields
	libs, err := runtime.ListMediaServerLibraries(ctx, "HomeEmby", true)
	if err != nil || len(libs) != 1 || libs[0].ID != "lib-movies" || !strings.Contains(libs[0].Link, "parentId=lib-movies") || !strings.Contains(libs[0].Image, "/Items/lib-movies/Images/Primary") {
		t.Fatalf("ListMediaServerLibraries(hidden=true) = %+v, err = %v", libs, err)
	}

	// 2. Latest filters out non-Movie/Series and excluded library folders
	latest, err := runtime.ListMediaServerLatest(ctx, "HomeEmby", 20)
	if err != nil || len(latest) != 1 {
		t.Fatalf("ListMediaServerLatest = %+v, err = %v", latest, err)
	}
	if latest[0].ID != "mov-1" || latest[0].Title != "沙丘" || latest[0].Subtitle != "2021" ||
		latest[0].Type != "电影" || latest[0].ServerType != "emby" ||
		!strings.Contains(latest[0].Link, "id=mov-1&context=home&serverId=emby-srv-1") ||
		!strings.Contains(latest[0].Image, "/Items/mov-1/Images/Primary") ||
		len(latest[0].BackdropImageTags) != 1 {
		t.Fatalf("unexpected latest item: %+v", latest[0])
	}

	// 3. Playing returns Movie + Episode resume items with V2 titles, subtitles, images, percent
	playing, err := runtime.ListMediaServerPlaying(ctx, "HomeEmby", 12)
	if err != nil || len(playing) != 2 {
		t.Fatalf("ListMediaServerPlaying = %+v, err = %v", playing, err)
	}
	if playing[0].Title != "沙丘" || playing[0].Percent == nil || *playing[0].Percent != 66.5 || !strings.Contains(playing[0].Image, "Images/Backdrop") {
		t.Fatalf("unexpected movie playing item: %+v", playing[0])
	}
	if playing[1].Title != "绝命毒师" || playing[1].Subtitle != "S1:2 - 试播集" ||
		playing[1].Type != "电视剧" || playing[1].Percent == nil || *playing[1].Percent != 25.0 ||
		!strings.Contains(playing[1].Image, "/Items/ser-9/Images/Backdrop") {
		t.Fatalf("unexpected episode playing item: %+v", playing[1])
	}
}
