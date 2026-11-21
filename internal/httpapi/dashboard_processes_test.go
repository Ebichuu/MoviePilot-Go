package httpapi

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"
)

func TestDashboardProcessesReturnsCurrentProcess(t *testing.T) {
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/api/v1/dashboard/processes", nil)

	Dependencies{}.dashboardProcesses(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusOK)
	}
	var procs []ProcessInfo
	if err := json.Unmarshal(rec.Body.Bytes(), &procs); err != nil {
		t.Fatalf("decode processes: %v", err)
	}
	if len(procs) == 0 {
		t.Fatal("expected at least one process entry")
	}
	if procs[0].PID <= 0 || procs[0].Name == "" || procs[0].Status == "" {
		t.Fatalf("unexpected process info: %+v", procs[0])
	}
}

func TestCollectLinuxProcProcessesParsesSyntheticProcTree(t *testing.T) {
	procRoot := t.TempDir()
	if err := os.WriteFile(filepath.Join(procRoot, "stat"), []byte("cpu 1 2 3 4\nbtime 1700000000\n"), 0o600); err != nil {
		t.Fatalf("write /proc/stat: %v", err)
	}
	pidDir := filepath.Join(procRoot, "4242")
	if err := os.MkdirAll(pidDir, 0o700); err != nil {
		t.Fatalf("mkdir pid dir: %v", err)
	}
	// Fields after (name): 0=state, ..., 19=starttime(ticks), 20=vsize, 21=rss(pages)
	statLine := "4242 (movie pilot (go)) R 1 4242 4242 0 -1 4194304 100 0 0 0 10 5 0 0 20 0 8 0 6000 104857600 4096 18446744073709551615"
	if err := os.WriteFile(filepath.Join(pidDir, "stat"), []byte(statLine), 0o600); err != nil {
		t.Fatalf("write pid stat: %v", err)
	}

	now := time.Unix(1700000200, 0)
	procs := collectLinuxProcProcesses(procRoot, now)
	if len(procs) != 1 {
		t.Fatalf("len(procs) = %d, want 1", len(procs))
	}
	p := procs[0]
	if p.PID != 4242 || p.Name != "movie pilot (go)" || p.Status != "running" {
		t.Fatalf("unexpected process metadata: %+v", p)
	}
	// btime=1700000000, startTicks=6000 (60s) -> create_time=1700000060, run_time=140s
	if p.CreateTime != 1700000060 || p.RunTime != 140 {
		t.Fatalf("unexpected timing: create=%v run=%v", p.CreateTime, p.RunTime)
	}
	if p.Memory <= 0 {
		t.Fatalf("expected positive memory MB, got %v", p.Memory)
	}
}

func TestSystemVersionsHandlerFetchesCachesAndHandlesErrors(t *testing.T) {
	var hits atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode([]map[string]any{
			{"tag_name": "v2.15.6", "name": "v2.15.6", "body": "Release notes"},
		})
	}))
	defer server.Close()

	deps := Dependencies{Versions: NewVersionFetcher(server.URL)}

	for i := 0; i < 2; i++ {
		rec := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodGet, "/api/v1/system/versions", nil)
		deps.systemVersionsHandler(rec, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("call %d status = %d, want 200", i, rec.Code)
		}
		var body struct {
			Success bool             `json:"success"`
			Data    []map[string]any `json:"data"`
		}
		if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
			t.Fatalf("decode body: %v", err)
		}
		if !body.Success || len(body.Data) != 1 || body.Data[0]["tag_name"] != "v2.15.6" {
			t.Fatalf("unexpected versions response: %+v", body)
		}
	}
	if hits.Load() != 1 {
		t.Fatalf("expected cached response (1 upstream hit), got %d", hits.Load())
	}

	// Error path returns V2 failure shape
	failServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "rate limited", http.StatusForbidden)
	}))
	defer failServer.Close()

	failDeps := Dependencies{Versions: NewVersionFetcher(failServer.URL)}
	rec := httptest.NewRecorder()
	failDeps.systemVersionsHandler(rec, httptest.NewRequest(http.MethodGet, "/api/v1/system/versions", nil))
	var failBody Response
	if err := json.Unmarshal(rec.Body.Bytes(), &failBody); err != nil {
		t.Fatalf("decode fail body: %v", err)
	}
	if failBody.Success || failBody.Message != "无法获取版本列表" {
		t.Fatalf("unexpected fail response: %+v", failBody)
	}
}
