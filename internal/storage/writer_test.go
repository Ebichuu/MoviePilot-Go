package storage

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
)

func TestFileWritersAcrossProviders(t *testing.T) {
	ctx := context.Background()

	// 1. LocalProvider Write
	localRoot := t.TempDir()
	local := NewLocalProvider(map[string]string{"local": localRoot})
	if err := local.Write(ctx, "local", "/Movies/Demo/demo.mkv", strings.NewReader("local-video"), 11); err != nil {
		t.Fatalf("local Write: %v", err)
	}
	got, err := os.ReadFile(filepath.Join(localRoot, "Movies", "Demo", "demo.mkv"))
	if err != nil || string(got) != "local-video" {
		t.Fatalf("local content = %q err=%v", string(got), err)
	}

	// 2. SMBProvider Write
	fakeSMB := newFakeSMBFS()
	smb, err := NewSMBProvider(SMBConfig{Host: "127.0.0.1", Share: "media"})
	if err != nil {
		t.Fatalf("NewSMBProvider: %v", err)
	}
	smb.dial = func(context.Context, SMBConfig) (smbFS, error) { return fakeSMB, nil }
	if err := smb.Write(ctx, "smb", "/Movies/Uploaded.mkv", strings.NewReader("smb-video"), 9); err != nil {
		t.Fatalf("smb Write: %v", err)
	}
	if string(fakeSMB.files["Movies/Uploaded.mkv"]) != "smb-video" {
		t.Fatalf("smb uploaded bytes = %q", string(fakeSMB.files["Movies/Uploaded.mkv"]))
	}

	// 3. AlistProvider Write + Open
	var alistPutBody []byte
	var alistSrv *httptest.Server
	alistSrv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/fs/put":
			alistPutBody, _ = io.ReadAll(r.Body)
			_ = json.NewEncoder(w).Encode(map[string]any{"code": 200, "message": "success"})
		case "/api/fs/get":
			_ = json.NewEncoder(w).Encode(map[string]any{
				"code": 200,
				"data": map[string]any{"name": "movie.mkv", "is_dir": false, "raw_url": alistSrv.URL + "/raw"},
			})
		case "/raw":
			_, _ = w.Write(alistPutBody)
		default:
			http.NotFound(w, r)
		}
	}))
	defer alistSrv.Close()
	alist, err := NewAlistProvider(AlistConfig{URL: alistSrv.URL})
	if err != nil {
		t.Fatalf("NewAlistProvider: %v", err)
	}
	if err := alist.Write(ctx, "alist", "/Movies/movie.mkv", strings.NewReader("alist-bytes"), 11); err != nil {
		t.Fatalf("alist Write: %v", err)
	}
	rc, _, err := alist.Open(ctx, "alist", "/Movies/movie.mkv")
	if err != nil {
		t.Fatalf("alist Open: %v", err)
	}
	readBack, _ := io.ReadAll(rc)
	_ = rc.Close()
	if string(readBack) != "alist-bytes" {
		t.Fatalf("alist readBack = %q", string(readBack))
	}

	// 4. AlipanProvider Write (chunk upload + complete)
	var alipanPart []byte
	alipanCompleted := false
	var alipanSrv *httptest.Server
	alipanSrv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/adrive/v1.0/openFile/get_by_path":
			_ = json.NewEncoder(w).Encode(map[string]any{"drive_id": "drv-1", "file_id": "dir-1", "type": "folder"})
		case "/adrive/v1.0/openFile/create":
			_ = json.NewEncoder(w).Encode(map[string]any{
				"file_id": "f-1", "upload_id": "u-1",
				"part_info_list": []map[string]any{{"part_number": 1, "upload_url": alipanSrv.URL + "/part/1"}},
			})
		case "/part/1":
			alipanPart, _ = io.ReadAll(r.Body)
			w.WriteHeader(http.StatusOK)
		case "/adrive/v1.0/openFile/complete":
			alipanCompleted = true
			_ = json.NewEncoder(w).Encode(map[string]any{"file_id": "f-1"})
		default:
			http.NotFound(w, r)
		}
	}))
	defer alipanSrv.Close()
	alipan, _ := NewAlipanProvider(AlipanConfig{BaseURL: alipanSrv.URL, AccessToken: "tok", DriveID: "drv-1"})
	if err := alipan.Write(ctx, "alipan", "/Movies/demo.mkv", bytes.NewReader([]byte("alipan-payload")), 14); err != nil {
		t.Fatalf("alipan Write: %v", err)
	}
	if !alipanCompleted || string(alipanPart) != "alipan-payload" {
		t.Fatalf("alipan upload incomplete: completed=%v part=%q", alipanCompleted, string(alipanPart))
	}

	// 5. U115Provider Write (rapid upload hit)
	u115Srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/open/folder/get_info":
			_ = json.NewEncoder(w).Encode(map[string]any{"state": true, "code": 0, "data": map[string]any{"file_id": "100"}})
		case "/open/upload/init":
			_ = json.NewEncoder(w).Encode(map[string]any{"state": true, "code": 0, "data": map[string]any{"status": 2}})
		default:
			http.NotFound(w, r)
		}
	}))
	defer u115Srv.Close()
	u115, _ := NewU115Provider(U115Config{BaseURL: u115Srv.URL, AccessToken: "tok"})
	if err := u115.Write(ctx, "u115", "/Movies/fast.mkv", strings.NewReader("rapid-bytes"), 11); err != nil {
		t.Fatalf("u115 Write: %v", err)
	}

	// 6. Registry delegates Write cleanly
	reg := NewProviderRegistry()
	reg.Register("local", local)
	reg.Register("smb", smb)
	if err := reg.Write(ctx, "smb", "/Movies/ViaRegistry.mkv", strings.NewReader("reg"), 3); err != nil {
		t.Fatalf("registry Write: %v", err)
	}
	if string(fakeSMB.files["Movies/ViaRegistry.mkv"]) != "reg" {
		t.Fatalf("registry write mismatch")
	}
}
