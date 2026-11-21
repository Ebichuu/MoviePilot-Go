package subtitle

import (
	"archive/zip"
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func buildTestZip(t *testing.T, files map[string]string) []byte {
	t.Helper()
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	for name, content := range files {
		w, err := zw.Create(name)
		if err != nil {
			t.Fatalf("zip create %s: %v", name, err)
		}
		if _, err := w.Write([]byte(content)); err != nil {
			t.Fatalf("zip write %s: %v", name, err)
		}
	}
	if err := zw.Close(); err != nil {
		t.Fatalf("zip close: %v", err)
	}
	return buf.Bytes()
}

func TestCoordinatorSearchAndDownloadProviders(t *testing.T) {
	zipBytes := buildTestZip(t, map[string]string{
		"sub/Dune.2021.zh.srt": "1\n00:00:01,000 --> 00:00:02,000\n你好\n",
		"README.txt":           "ignore me",
	})
	badZipBytes := buildTestZip(t, map[string]string{
		"../../evil.srt": "bad",
	})

	var server *httptest.Server
	server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/os/subtitles":
			if r.Header.Get("Api-Key") != "os-key" {
				http.Error(w, "unauthorized", http.StatusUnauthorized)
				return
			}
			_ = json.NewEncoder(w).Encode(map[string]any{
				"data": []map[string]any{
					{
						"id": "101",
						"attributes": map[string]any{
							"subtitle_id":    "101",
							"language":       "zh-cn",
							"download_count": 500,
							"release":        "Dune.2021.1080p.BluRay",
							"upload_date":    "2026-01-01",
							"url":            "https://opensubtitles.com/sub/101",
							"uploader":       map[string]any{"name": "alice"},
							"files":          []map[string]any{{"file_id": 777, "file_name": "Dune.2021.zh.srt"}},
						},
					},
				},
			})
		case "/os/download":
			_ = json.NewEncoder(w).Encode(map[string]any{
				"link":      server.URL + "/files/Dune.2021.zh.srt",
				"file_name": "Dune.2021.zh.srt",
			})
		case "/files/Dune.2021.zh.srt":
			_, _ = w.Write([]byte("1\n00:00:01,000 --> 00:00:03,000\nOpenSubtitles\n"))
		case "/assrt/sub/search":
			_ = json.NewEncoder(w).Encode(map[string]any{
				"status": 0,
				"sub": map[string]any{
					"subs": []map[string]any{
						{
							"id":           888,
							"native_name":  "沙丘 中英双语字幕",
							"videoname":    "Dune.2021.WEB-DL",
							"upload_time":  "2026-01-02",
							"vote_score":   900,
							"release_site": "SubHD",
							"lang":         map[string]any{"desc": "简英"},
						},
					},
				},
			})
		case "/assrt/sub/detail":
			_ = json.NewEncoder(w).Encode(map[string]any{
				"status": 0,
				"sub": map[string]any{
					"subs": []map[string]any{
						{"url": server.URL + "/files/dune.zip", "filename": "dune.zip"},
					},
				},
			})
		case "/files/dune.zip":
			_, _ = w.Write(zipBytes)
		case "/files/bad.zip":
			_, _ = w.Write(badZipBytes)
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	osProvider, err := NewOpenSubtitlesProvider(OpenSubtitlesConfig{
		BaseURL: server.URL + "/os",
		APIKey:  "os-key",
		Client:  server.Client(),
	})
	if err != nil {
		t.Fatalf("NewOpenSubtitlesProvider: %v", err)
	}
	assrtProvider, err := NewAssrtProvider(AssrtConfig{
		BaseURL: server.URL + "/assrt",
		Token:   "assrt-tok",
		Client:  server.Client(),
	})
	if err != nil {
		t.Fatalf("NewAssrtProvider: %v", err)
	}

	coord := NewCoordinator(server.Client(), []string{"zh-cn", "en"}, osProvider, assrtProvider)
	var events []Event
	results, err := coord.Search(context.Background(), Query{Keyword: "Dune", Year: "2021"}, func(e Event) {
		events = append(events, e)
	})
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if len(events) != 2 || len(results) != 2 {
		t.Fatalf("events=%d results=%+v", len(events), results)
	}
	// Higher grabs first (Assrt 900 > OpenSubtitles 500)
	if results[0].SiteName != "Assrt" || results[1].SiteName != "OpenSubtitles" {
		t.Fatalf("unexpected ranking: %+v", results)
	}

	// Download plain subtitle from OpenSubtitles
	outDir := t.TempDir()
	osFiles, err := coord.Download(context.Background(), results[1], outDir)
	if err != nil || len(osFiles) != 1 {
		t.Fatalf("OpenSubtitles download: files=%v err=%v", osFiles, err)
	}
	content, err := os.ReadFile(osFiles[0])
	if err != nil || !strings.Contains(string(content), "OpenSubtitles") {
		t.Fatalf("unexpected OpenSubtitles content: %q err=%v", string(content), err)
	}

	// Download ZIP archive from Assrt -> extracts only .srt, ignores README.txt
	zipDir := t.TempDir()
	assrtFiles, err := coord.Download(context.Background(), results[0], zipDir)
	if err != nil || len(assrtFiles) != 1 || filepath.Base(assrtFiles[0]) != "Dune.2021.zh.srt" {
		t.Fatalf("Assrt download: files=%v err=%v", assrtFiles, err)
	}

	// Direct ZIP with path traversal must be rejected
	_, err = coord.Download(context.Background(), Subtitle{
		Enclosure: server.URL + "/files/bad.zip",
		FileName:  "bad.zip",
	}, t.TempDir())
	if err == nil || !strings.Contains(err.Error(), "非法路径") {
		t.Fatalf("expected zip-slip error, got %v", err)
	}
}
