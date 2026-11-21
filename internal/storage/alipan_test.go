package storage

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestAlipanProviderAuthAndOperations(t *testing.T) {
	var server *httptest.Server
	server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/oauth/authorize/qrcode":
			_ = json.NewEncoder(w).Encode(map[string]any{
				"sid":       "sid-123",
				"qrCodeUrl": "https://openapi.alipan.com/qr/sid-123",
			})
		case "/oauth/qrcode/sid-123/status":
			_ = json.NewEncoder(w).Encode(map[string]any{
				"status":   "LoginSuccess",
				"authCode": "auth-code-xyz",
			})
		case "/oauth/access_token":
			_ = json.NewEncoder(w).Encode(map[string]any{
				"access_token":  "ali-access-1",
				"refresh_token": "ali-refresh-1",
				"expires_in":    7200,
			})
		case "/adrive/v1.0/user/getDriveInfo":
			_ = json.NewEncoder(w).Encode(map[string]any{
				"default_drive_id":  "drv-default",
				"resource_drive_id": "drv-resource",
			})
		case "/adrive/v1.0/openFile/get_by_path":
			var body map[string]string
			_ = json.NewDecoder(r.Body).Decode(&body)
			switch body["file_path"] {
			case "/Movies":
				_ = json.NewEncoder(w).Encode(map[string]any{
					"drive_id": "drv-resource", "file_id": "dir-movies", "name": "Movies", "type": "folder",
				})
			case "/Movies/Dune.mkv":
				_ = json.NewEncoder(w).Encode(map[string]any{
					"drive_id": "drv-resource", "file_id": "file-dune", "name": "Dune.mkv", "type": "file", "size": 4096,
				})
			default:
				_ = json.NewEncoder(w).Encode(map[string]any{"code": "NotFound.File", "message": "not found"})
			}
		case "/adrive/v1.0/openFile/list":
			_ = json.NewEncoder(w).Encode(map[string]any{
				"items": []map[string]any{
					{
						"drive_id": "drv-resource", "file_id": "file-dune", "parent_file_id": "dir-movies",
						"name": "Dune.mkv", "type": "file", "size": 4096, "file_extension": "mkv",
						"updated_at": "2026-01-02T03:04:05Z",
					},
					{
						"drive_id": "drv-resource", "file_id": "dir-sub", "parent_file_id": "dir-movies",
						"name": "Extras", "type": "folder", "updated_at": "2026-01-01T00:00:00Z",
					},
				},
			})
		case "/adrive/v1.0/openFile/create":
			_ = json.NewEncoder(w).Encode(map[string]any{"file_id": "dir-new"})
		case "/adrive/v1.0/openFile/update":
			_ = json.NewEncoder(w).Encode(map[string]any{"file_id": "file-dune"})
		case "/adrive/v1.0/openFile/recyclebin/trash":
			_ = json.NewEncoder(w).Encode(map[string]any{})
		case "/adrive/v1.0/user/getSpaceInfo":
			_ = json.NewEncoder(w).Encode(map[string]any{
				"personal_space_info": map[string]any{"total_size": 10000, "used_size": 2500},
			})
		case "/adrive/v1.0/openFile/getDownloadUrl":
			_ = json.NewEncoder(w).Encode(map[string]any{"url": server.URL + "/download/dune"})
		case "/download/dune":
			w.Header().Set("Content-Type", "video/x-matroska")
			_, _ = w.Write([]byte("dune-bytes"))
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	var persisted map[string]any
	provider, err := NewAlipanProvider(AlipanConfig{
		BaseURL: server.URL,
		OnConfigChange: func(_ context.Context, conf map[string]any) error {
			persisted = conf
			return nil
		},
	})
	if err != nil {
		t.Fatalf("NewAlipanProvider: %v", err)
	}

	ctx := context.Background()
	qrData, msg, err := provider.QRCode(ctx, "alipan")
	if err != nil || msg != "" {
		t.Fatalf("QRCode: data=%v msg=%q err=%v", qrData, msg, err)
	}
	qrMap := qrData.(map[string]any)
	if !strings.Contains(qrMap["codeUrl"].(string), "sid-123") {
		t.Fatalf("unexpected QR data: %+v", qrMap)
	}

	loginData, msg, err := provider.CheckLogin(ctx, "alipan", "", "")
	if err != nil || msg != "" {
		t.Fatalf("CheckLogin: data=%v msg=%q err=%v", loginData, msg, err)
	}
	if persisted["access_token"] != "ali-access-1" || persisted["resource_drive_id"] != "drv-resource" {
		t.Fatalf("unexpected persisted config: %+v", persisted)
	}

	items, err := provider.List(ctx, "alipan", "/Movies", "name", "")
	if err != nil || len(items) != 2 || items[0].Name != "Dune.mkv" {
		t.Fatalf("List: items=%+v err=%v", items, err)
	}
	if ok, err := provider.Mkdir(ctx, "alipan", "/Movies", "4K"); err != nil || !ok {
		t.Fatalf("Mkdir: ok=%v err=%v", ok, err)
	}
	if ok, err := provider.Rename(ctx, "alipan", "/Movies/Dune.mkv", "Dune2.mkv", false); err != nil || !ok {
		t.Fatalf("Rename: ok=%v err=%v", ok, err)
	}
	if ok, err := provider.Delete(ctx, "alipan", "/Movies/Dune.mkv"); err != nil || !ok {
		t.Fatalf("Delete: ok=%v err=%v", ok, err)
	}
	usage, err := provider.Usage(ctx, "alipan")
	if err != nil || usage.Total != 10000 || usage.Available != 7500 {
		t.Fatalf("Usage = %+v, err = %v", usage, err)
	}
	rc, ct, err := provider.Open(ctx, "alipan", "/Movies/Dune.mkv")
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	body, _ := io.ReadAll(rc)
	_ = rc.Close()
	if string(body) != "dune-bytes" || ct != "video/x-matroska" {
		t.Fatalf("Open body=%q ct=%q", string(body), ct)
	}
}
