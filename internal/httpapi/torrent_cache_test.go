package httpapi

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/moviepilot-go/moviepilot-go/internal/auth"
	"github.com/moviepilot-go/moviepilot-go/internal/config"
	"github.com/moviepilot-go/moviepilot-go/internal/database"
)

type fakeTorrentCacheStore struct {
	items        []database.TorrentCacheItem
	cleared      bool
	updatedTitle map[int64]string
}

func (f *fakeTorrentCacheStore) ListTorrentCache(_ context.Context, _ int) ([]database.TorrentCacheItem, error) {
	return append([]database.TorrentCacheItem(nil), f.items...), nil
}

func (f *fakeTorrentCacheStore) ClearTorrentCache(_ context.Context) (int64, error) {
	n := int64(len(f.items))
	f.items = nil
	f.cleared = true
	return n, nil
}

func (f *fakeTorrentCacheStore) FindTorrentCacheItem(_ context.Context, domain, hash string) (database.TorrentCacheItem, bool, error) {
	for _, item := range f.items {
		if item.Domain == domain && item.Hash == hash {
			return item, true, nil
		}
	}
	return database.TorrentCacheItem{}, false, nil
}

func (f *fakeTorrentCacheStore) DeleteTorrentCacheItem(_ context.Context, domain, hash string) (bool, error) {
	for i, item := range f.items {
		if item.Domain == domain && item.Hash == hash {
			f.items = append(f.items[:i], f.items[i+1:]...)
			return true, nil
		}
	}
	return false, nil
}

func (f *fakeTorrentCacheStore) UpdateTorrentCacheRecognition(_ context.Context, id int64, recognizedTitle string, _ int) error {
	if f.updatedTitle == nil {
		f.updatedTitle = map[int64]string{}
	}
	f.updatedTitle[id] = recognizedTitle
	for i := range f.items {
		if f.items[i].ID == id {
			f.items[i].RecognizedMediaTitle = recognizedTitle
		}
	}
	return nil
}

func TestTorrentCacheListReidentifyDeleteRefreshClear(t *testing.T) {
	t.Parallel()

	hash := database.TorrentCacheHash("Dune.Part.Two.2024.2160p.WEB-DL", "沙丘2")
	cacheStore := &fakeTorrentCacheStore{
		items: []database.TorrentCacheItem{{
			ID:          11,
			SiteID:      1,
			SiteName:    "DemoPT",
			Domain:      "demo.pt",
			Hash:        hash,
			Title:       "Dune.Part.Two.2024.2160p.WEB-DL",
			Description: "沙丘2",
			SizeBytes:   22_000_000_000,
			PublishedAt: time.Date(2026, 11, 20, 12, 0, 0, 0, time.UTC),
			PageURL:     "https://demo.pt/details.php?id=11",
			Enclosure:   "/api/v1/resource/download/1/abc",
		}},
	}
	sched := &fakeSchedulerRunner{triggered: true}
	authSvc := auth.NewService(nil, "tc-jwt", "tc-res", time.Hour)
	admin := auth.User{ID: 1, Username: "admin", SuperUser: true}
	token, _ := authSvc.AccessToken(admin)

	router := NewRouter(Dependencies{
		Config:       config.Config{MaxRequestBody: 1 << 20, FrontendDir: t.TempDir(), Location: time.UTC},
		Auth:         authSvc,
		Users:        fakeUserLookup{user: admin},
		TorrentCache: cacheStore,
		Scheduler:    sched,
	})

	doReq := func(method, path string) Response {
		t.Helper()
		req := httptest.NewRequest(method, path, nil)
		req.Header.Set("Authorization", "Bearer "+token)
		rec := httptest.NewRecorder()
		router.ServeHTTP(rec, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("%s %s status=%d body=%s", method, path, rec.Code, rec.Body.String())
		}
		var resp Response
		if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
			t.Fatalf("decode %s %s: %v", method, path, err)
		}
		return resp
	}

	// 1. GET /torrent/cache
	resp := doReq(http.MethodGet, "/api/v1/torrent/cache")
	if !resp.Success {
		t.Fatalf("list failed: %+v", resp)
	}
	data := resp.Data.(map[string]any)
	if data["count"].(float64) != 1 || data["sites"].(float64) != 1 {
		t.Fatalf("unexpected counts: %+v", data)
	}

	// 2. POST /torrent/cache/reidentify/{domain}/{hash}
	resp = doReq(http.MethodPost, "/api/v1/torrent/cache/reidentify/demo.pt/"+hash+"?title=沙丘2")
	if !resp.Success || cacheStore.updatedTitle[11] != "沙丘2" {
		t.Fatalf("reidentify failed: %+v updated=%v", resp, cacheStore.updatedTitle)
	}

	// 3. POST /torrent/cache/refresh
	resp = doReq(http.MethodPost, "/api/v1/torrent/cache/refresh")
	if !resp.Success || len(sched.jobs) != 1 || sched.jobs[0] != "subscribe_refresh" {
		t.Fatalf("refresh failed: %+v jobs=%v", resp, sched.jobs)
	}

	// 4. DELETE /torrent/cache/{domain}/{hash}
	resp = doReq(http.MethodDelete, "/api/v1/torrent/cache/demo.pt/"+hash)
	if !resp.Success || len(cacheStore.items) != 0 {
		t.Fatalf("delete single failed: %+v items=%d", resp, len(cacheStore.items))
	}

	// 5. DELETE /torrent/cache
	resp = doReq(http.MethodDelete, "/api/v1/torrent/cache")
	if !resp.Success || !cacheStore.cleared {
		t.Fatalf("clear failed: %+v cleared=%v", resp, cacheStore.cleared)
	}
}
