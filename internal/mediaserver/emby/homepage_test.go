package emby

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestClientHomepageLatestAndResume(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/Users/u-1/Items/Latest":
			if r.URL.Query().Get("MediaTypes") != "Video" {
				t.Errorf("unexpected MediaTypes=%q", r.URL.Query().Get("MediaTypes"))
			}
			_ = json.NewEncoder(w).Encode([]map[string]any{
				{
					"Id":                "m-1",
					"Name":              "Dune",
					"Type":              "Movie",
					"Path":              "/media/movies/Dune.mkv",
					"ServerId":          "srv-1",
					"ProductionYear":    2021,
					"BackdropImageTags": []string{"bd-tag-1"},
				},
			})
		case "/Users/u-1/Items/Resume":
			_ = json.NewEncoder(w).Encode(map[string]any{
				"Items": []map[string]any{
					{
						"Id":                    "ep-1",
						"Name":                  "Pilot",
						"Type":                  "Episode",
						"Path":                  "/media/tv/Show/S01E01.mkv",
						"ServerId":              "srv-1",
						"SeriesId":              "ser-1",
						"SeriesName":            "Breaking Bad",
						"SeriesPrimaryImageTag": "ser-tag-1",
						"ParentIndexNumber":     1,
						"IndexNumber":           1,
						"UserData":              map[string]any{"PlayedPercentage": 42.5},
					},
				},
			})
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	client, err := NewClient(Config{
		BaseURL:  server.URL,
		PlayHost: "https://emby.play.test",
		Token:    "emby-tok",
	})
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	defer client.CloseIdleConnections()

	latest, err := client.ListLatestItems(context.Background(), "u-1", 20)
	if err != nil || len(latest) != 1 || latest[0].ID != "m-1" || latest[0].ProductionYear != 2021 || len(latest[0].BackdropImageTags) != 1 {
		t.Fatalf("ListLatestItems = %+v, err = %v", latest, err)
	}

	resume, err := client.ListResumeItems(context.Background(), "u-1", 12)
	if err != nil || len(resume) != 1 || resume[0].SeriesName != "Breaking Bad" || resume[0].UserData.PlayedPercentage == nil || *resume[0].UserData.PlayedPercentage != 42.5 {
		t.Fatalf("ListResumeItems = %+v, err = %v", resume, err)
	}

	playURL, err := client.PlayURLWithServer("m-1", "srv-1")
	if err != nil || playURL != "https://emby.play.test/web/index.html#!/item?id=m-1&context=home&serverId=srv-1" {
		t.Fatalf("PlayURLWithServer = %q err=%v", playURL, err)
	}
	if img := client.PrimaryImageURL("m-1"); !strings.HasSuffix(img, "/Items/m-1/Images/Primary") {
		t.Fatalf("PrimaryImageURL = %q", img)
	}
	if bg := client.BackdropImageURL("m-1", "bd-tag-1"); !strings.Contains(bg, "/Items/m-1/Images/Backdrop?") || !strings.Contains(bg, "tag=bd-tag-1") {
		t.Fatalf("BackdropImageURL = %q", bg)
	}
	if libURL := client.LibraryURL("lib-1", "srv-1"); libURL != "https://emby.play.test/web/index.html#!/videos?serverId=srv-1&parentId=lib-1" {
		t.Fatalf("LibraryURL = %q", libURL)
	}
}
