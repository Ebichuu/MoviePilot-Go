package httpapi

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"

	"github.com/moviepilot-go/moviepilot-go/internal/storage"
)

type memoryRemoteStorage struct {
	name  string
	files map[string][]byte
}

func (m *memoryRemoteStorage) List(context.Context, string, string, string, string) ([]storage.Item, error) {
	return nil, nil
}
func (m *memoryRemoteStorage) Mkdir(context.Context, string, string, string) (bool, error) {
	return true, nil
}
func (m *memoryRemoteStorage) Delete(_ context.Context, _, p string) (bool, error) {
	delete(m.files, p)
	return true, nil
}
func (m *memoryRemoteStorage) Rename(context.Context, string, string, string, bool) (bool, error) {
	return true, nil
}
func (m *memoryRemoteStorage) Usage(context.Context, string) (storage.Usage, error) {
	return storage.Usage{}, nil
}
func (m *memoryRemoteStorage) TransType(context.Context, string) (map[string]any, error) {
	return map[string]any{"copy": "复制", "move": "移动"}, nil
}
func (m *memoryRemoteStorage) Open(_ context.Context, _, p string) (io.ReadCloser, string, error) {
	data, ok := m.files[p]
	if !ok {
		return nil, "", storage.ErrOperationFailed
	}
	return io.NopCloser(bytes.NewReader(data)), "application/octet-stream", nil
}
func (m *memoryRemoteStorage) Write(_ context.Context, _, p string, r io.Reader, _ int64) error {
	if m.files == nil {
		m.files = map[string][]byte{}
	}
	data, err := io.ReadAll(r)
	if err != nil {
		return err
	}
	m.files[p] = data
	return nil
}

func TestCrossStorageManualTransferAndTracker(t *testing.T) {
	localRoot := t.TempDir()
	srcFile := filepath.Join(localRoot, "Dune.2021.mkv")
	if err := os.WriteFile(srcFile, []byte("cross-storage-movie-data"), 0o644); err != nil {
		t.Fatalf("write local file: %v", err)
	}

	remote := &memoryRemoteStorage{name: "smb", files: map[string][]byte{}}
	reg := storage.NewProviderRegistry()
	reg.Register("local", storage.NewLocalProvider(map[string]string{"local": localRoot}))
	reg.Register("smb", remote)

	tracker := NewTransferTracker()
	deps := Dependencies{
		Storage:         reg,
		TransferTracker: tracker,
	}

	// 1. Copy local -> smb
	payload, _ := json.Marshal(map[string]any{
		"source_path":    "/Dune.2021.mkv",
		"target_path":    "/Movies/Dune (2021)/Dune.mkv",
		"storage":        "local",
		"target_storage": "smb",
		"transfer_type":  "copy",
	})
	rec := httptest.NewRecorder()
	deps.manualTransfer(rec, httptest.NewRequest(http.MethodPost, "/transfer/manual", bytes.NewReader(payload)))
	var resp struct {
		Success bool           `json:"success"`
		Message string         `json:"message"`
		Data    map[string]any `json:"data"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("unmarshal resp: %v body=%s", err, rec.Body.String())
	}
	if !resp.Success {
		t.Fatalf("expected cross-storage transfer success, got %+v", resp)
	}
	if string(remote.files["/Movies/Dune (2021)/Dune.mkv"]) != "cross-storage-movie-data" {
		t.Fatalf("remote file mismatch: %q", string(remote.files["/Movies/Dune (2021)/Dune.mkv"]))
	}

	// 2. Move smb -> local (deletes from smb after writing to local)
	movePayload, _ := json.Marshal(map[string]any{
		"source_path":    "/Movies/Dune (2021)/Dune.mkv",
		"target_path":    "/Downloaded/Dune.mkv",
		"storage":        "smb",
		"target_storage": "local",
		"transfer_type":  "move",
	})
	moveRec := httptest.NewRecorder()
	deps.manualTransfer(moveRec, httptest.NewRequest(http.MethodPost, "/transfer/manual", bytes.NewReader(movePayload)))
	var moveResp Response
	_ = json.Unmarshal(moveRec.Body.Bytes(), &moveResp)
	if !moveResp.Success {
		t.Fatalf("expected smb->local move success, got %+v", moveResp)
	}
	if _, exists := remote.files["/Movies/Dune (2021)/Dune.mkv"]; exists {
		t.Fatalf("expected source file deleted from smb on move")
	}
	backBytes, err := os.ReadFile(filepath.Join(localRoot, "Downloaded", "Dune.mkv"))
	if err != nil || string(backBytes) != "cross-storage-movie-data" {
		t.Fatalf("downloaded local file mismatch: %q err=%v", string(backBytes), err)
	}

	// 3. Tracker live progress & cancel endpoints
	canceled := false
	taskID := tracker.Start("/src.mkv", "/dst.mkv", "local", "alipan", "copy", 1000, func() { canceled = true })
	tracker.UpdateBytes(taskID, 500)

	qRec := httptest.NewRecorder()
	deps.transferQueueList(qRec, httptest.NewRequest(http.MethodGet, "/transfer/queue", nil))
	if !strings.Contains(qRec.Body.String(), `"percent":50`) {
		t.Fatalf("unexpected queue body: %s", qRec.Body.String())
	}

	pReq := httptest.NewRequest(http.MethodGet, "/system/progress/filetransfer", nil)
	rctx := chi.NewRouteContext()
	rctx.URLParams.Add("process_type", "filetransfer")
	pReq = pReq.WithContext(context.WithValue(pReq.Context(), chi.RouteCtxKey, rctx))
	pRec := httptest.NewRecorder()
	deps.systemProgress(pRec, pReq)
	if !strings.Contains(pRec.Body.String(), `"success":true`) || !strings.Contains(pRec.Body.String(), `"value":50`) {
		t.Fatalf("unexpected progress body: %s", pRec.Body.String())
	}

	rmRec := httptest.NewRecorder()
	deps.removeTransferQueueItem(rmRec, httptest.NewRequest(http.MethodDelete, "/transfer/queue?id="+taskID, nil))
	if !canceled || len(tracker.List(false)) != 0 {
		t.Fatalf("expected task canceled and removed: canceled=%v list=%v", canceled, tracker.List(false))
	}
}

func TestRecommendEpisodeFormat(t *testing.T) {
	deps := Dependencies{}
	body := `{"files":["[SubGroup] Anime - 01 [1080p].mkv","[SubGroup] Anime - 02 [1080p].mkv","[SubGroup] Anime - 03 [1080p].mkv"]}`
	rec := httptest.NewRecorder()
	deps.recommendEpisodeFormat(rec, httptest.NewRequest(http.MethodPost, "/transfer/episode-format/recommend", strings.NewReader(body)))
	var resp struct {
		Success bool `json:"success"`
		Data    struct {
			EpisodeFormat string `json:"episode_format"`
			MatchedCount  int    `json:"matched_count"`
			Episodes      []int  `json:"episodes"`
		} `json:"data"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if !resp.Success || resp.Data.EpisodeFormat != " - {ep}" || resp.Data.MatchedCount != 3 || len(resp.Data.Episodes) != 3 {
		t.Fatalf("unexpected recommendation: %+v", resp)
	}
}
