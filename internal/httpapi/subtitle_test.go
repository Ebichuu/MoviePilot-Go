package httpapi

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"

	"github.com/moviepilot-go/moviepilot-go/internal/config"
	"github.com/moviepilot-go/moviepilot-go/internal/subtitle"
)

type stubSubtitleProvider struct {
	name    string
	siteID  int
	results []subtitle.Subtitle
	dlURL   string
}

func (s *stubSubtitleProvider) Name() string { return s.name }
func (s *stubSubtitleProvider) SiteID() int  { return s.siteID }
func (s *stubSubtitleProvider) Search(_ context.Context, _ subtitle.Query) ([]subtitle.Subtitle, error) {
	return s.results, nil
}
func (s *stubSubtitleProvider) ResolveDownload(_ context.Context, item subtitle.Subtitle) (string, string, map[string]string, error) {
	return s.dlURL, item.FileName, nil, nil
}

func TestSubtitleSearchAndDownloadHandlers(t *testing.T) {
	fileServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte("1\n00:00:01,000 --> 00:00:03,000\n测试字幕\n"))
	}))
	defer fileServer.Close()

	provider := &stubSubtitleProvider{
		name:   "OpenSubtitles",
		siteID: 9001,
		results: []subtitle.Subtitle{
			{
				Site:       9001,
				SiteName:   "OpenSubtitles",
				Title:      "Inception.2010.zh",
				Enclosure:  "opensubtitles://42",
				Language:   "zh-cn",
				FileName:   "Inception.2010.zh.srt",
				SubtitleID: "42",
				Grabs:      120,
			},
		},
		dlURL: fileServer.URL + "/sub.srt",
	}

	rootDir := t.TempDir()
	deps := Dependencies{
		Config:   config.Config{StorageLocalPath: rootDir},
		Subtitle: subtitle.NewCoordinator(fileServer.Client(), []string{"zh-cn"}, provider),
	}

	// 1. Title search JSON
	rec := httptest.NewRecorder()
	deps.searchSubtitleTitle(rec, httptest.NewRequest(http.MethodGet, "/search/subtitle/title?keyword=Inception", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}
	var titleResp struct {
		Success bool                `json:"success"`
		Data    []subtitle.Subtitle `json:"data"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &titleResp); err != nil {
		t.Fatalf("unmarshal titleResp: %v", err)
	}
	if !titleResp.Success || len(titleResp.Data) != 1 || titleResp.Data[0].FileName != "Inception.2010.zh.srt" {
		t.Fatalf("unexpected titleResp: %+v", titleResp)
	}

	// 2. Media search SSE stream
	req := httptest.NewRequest(http.MethodGet, "/search/subtitle/media/tmdb:27205/stream?title=Inception", nil)
	rctx := chi.NewRouteContext()
	rctx.URLParams.Add("mediaid", "tmdb:27205")
	req = req.WithContext(context.WithValue(req.Context(), chi.RouteCtxKey, rctx))
	sseRec := httptest.NewRecorder()
	deps.searchSubtitleMediaStream(sseRec, req)
	body := sseRec.Body.String()
	if !strings.Contains(body, `"type":"progress"`) || !strings.Contains(body, `"type":"append"`) || !strings.Contains(body, `"type":"done"`) {
		t.Fatalf("unexpected SSE body: %s", body)
	}

	// 3. Download subtitle into allowed storage path
	saveDir := filepath.Join(rootDir, "Movies", "Inception")
	dlBody, _ := json.Marshal(map[string]any{
		"subtitle_in": titleResp.Data[0],
		"save_path":   saveDir,
	})
	dlRec := httptest.NewRecorder()
	deps.downloadSubtitle(dlRec, httptest.NewRequest(http.MethodPost, "/download/subtitle", strings.NewReader(string(dlBody))))
	var dlResp struct {
		Success bool `json:"success"`
		Data    struct {
			Files []string `json:"files"`
		} `json:"data"`
	}
	if err := json.Unmarshal(dlRec.Body.Bytes(), &dlResp); err != nil {
		t.Fatalf("unmarshal dlResp: %v", err)
	}
	if !dlResp.Success || len(dlResp.Data.Files) != 1 {
		t.Fatalf("unexpected dlResp: %+v body=%s", dlResp, dlRec.Body.String())
	}
	savedBytes, err := os.ReadFile(dlResp.Data.Files[0])
	if err != nil || !strings.Contains(string(savedBytes), "测试字幕") {
		t.Fatalf("saved subtitle mismatch: %q err=%v", string(savedBytes), err)
	}

	// 4. Reject save_path outside StorageLocalPath
	escapeBody, _ := json.Marshal(map[string]any{
		"subtitle_in": titleResp.Data[0],
		"save_path":   filepath.Dir(rootDir),
	})
	escapeRec := httptest.NewRecorder()
	deps.downloadSubtitle(escapeRec, httptest.NewRequest(http.MethodPost, "/download/subtitle", strings.NewReader(string(escapeBody))))
	var escapeResp Response
	_ = json.Unmarshal(escapeRec.Body.Bytes(), &escapeResp)
	if escapeResp.Success {
		t.Fatalf("expected escape save_path to be rejected")
	}
}
