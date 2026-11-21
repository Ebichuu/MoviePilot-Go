package storage

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestU115ProviderAuthAndOperations(t *testing.T) {
	var server *httptest.Server
	server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/open/authDeviceCode":
			_ = json.NewEncoder(w).Encode(map[string]any{
				"code": 0,
				"data": map[string]any{
					"uid":    "uid-115",
					"time":   1700000000,
					"sign":   "sig-115",
					"qrcode": "https://115.com/scan/uid-115",
				},
			})
		case "/get/status/":
			_ = json.NewEncoder(w).Encode(map[string]any{
				"code": 0,
				"data": map[string]any{"status": 2, "msg": "登录成功"},
			})
		case "/open/deviceCodeToToken":
			_ = json.NewEncoder(w).Encode(map[string]any{
				"code": 0,
				"data": map[string]any{
					"access_token":  "u115-access-1",
					"refresh_token": "u115-refresh-1",
					"expires_in":    7200,
				},
			})
		case "/open/folder/get_info":
			_ = r.ParseForm()
			switch r.Form.Get("path") {
			case "/TV":
				_ = json.NewEncoder(w).Encode(map[string]any{
					"state": true, "code": 0,
					"data": map[string]any{"file_id": "1001", "file_name": "TV", "file_category": "0"},
				})
			case "/TV/S01E01.mkv":
				_ = json.NewEncoder(w).Encode(map[string]any{
					"state": true, "code": 0,
					"data": map[string]any{
						"file_id": "2001", "file_name": "S01E01.mkv", "file_category": "1",
						"pick_code": "pk-2001", "size_byte": 8192, "utime": "1700000200",
					},
				})
			default:
				_ = json.NewEncoder(w).Encode(map[string]any{"state": false, "code": 430004, "message": "not found"})
			}
		case "/open/ufile/files":
			_ = json.NewEncoder(w).Encode(map[string]any{
				"state": true, "code": 0,
				"data": []map[string]any{
					{"fid": "2001", "fn": "S01E01.mkv", "fc": "1", "fs": 8192, "ico": "mkv", "upt": 1700000200, "pc": "pk-2001"},
					{"fid": "2002", "fn": "Season 01", "fc": "0", "fs": 0, "upt": 1700000100, "pc": "pk-2002"},
				},
			})
		case "/open/folder/add":
			_ = json.NewEncoder(w).Encode(map[string]any{"state": true, "code": 0, "data": map[string]any{"file_id": "3001"}})
		case "/open/ufile/update":
			_ = json.NewEncoder(w).Encode(map[string]any{"state": true, "code": 0})
		case "/open/ufile/delete":
			_ = json.NewEncoder(w).Encode(map[string]any{"state": true, "code": 0})
		case "/open/user/info":
			_ = json.NewEncoder(w).Encode(map[string]any{
				"code": 0,
				"data": map[string]any{
					"rt_space_info": map[string]any{
						"all_total":  map[string]any{"size": 50000},
						"all_remain": map[string]any{"size": 12000},
					},
				},
			})
		case "/open/ufile/downurl":
			_ = json.NewEncoder(w).Encode(map[string]any{
				"code": 0,
				"data": map[string]any{
					"2001": map[string]any{"url": map[string]any{"url": server.URL + "/download/s01e01"}},
				},
			})
		case "/download/s01e01":
			w.Header().Set("Content-Type", "video/x-matroska")
			_, _ = w.Write([]byte("115-video-bytes"))
		case "/u115/auth_url":
			_ = json.NewEncoder(w).Encode(map[string]any{
				"success": true,
				"data":    map[string]any{"auth_url": "https://passportapi.115.com/oauth", "state": "st-1"},
			})
		case "/u115/token":
			_ = json.NewEncoder(w).Encode(map[string]any{
				"status": "completed",
				"data":   map[string]any{"access_token": "oauth-tok", "refresh_token": "oauth-ref", "expires_in": 3600},
			})
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	var persisted map[string]any
	provider, err := NewU115Provider(U115Config{
		BaseURL:         server.URL,
		PassportURL:     server.URL,
		QRCodeStatusURL: server.URL,
		AuthServerURL:   server.URL,
		OnConfigChange: func(_ context.Context, conf map[string]any) error {
			persisted = conf
			return nil
		},
	})
	if err != nil {
		t.Fatalf("NewU115Provider: %v", err)
	}

	ctx := context.Background()
	qrData, msg, err := provider.QRCode(ctx, "u115")
	if err != nil || msg != "" || qrData.(map[string]any)["codeContent"] != "https://115.com/scan/uid-115" {
		t.Fatalf("QRCode: data=%v msg=%q err=%v", qrData, msg, err)
	}
	checkData, msg, err := provider.CheckLogin(ctx, "u115", "", "")
	if err != nil || msg != "" || checkData.(map[string]any)["status"] != 2 {
		t.Fatalf("CheckLogin: data=%v msg=%q err=%v", checkData, msg, err)
	}
	if persisted["access_token"] != "u115-access-1" {
		t.Fatalf("unexpected persisted tokens: %+v", persisted)
	}

	items, err := provider.List(ctx, "u115", "/TV", "name", "")
	if err != nil || len(items) != 2 || items[0].Name != "S01E01.mkv" {
		t.Fatalf("List: items=%+v err=%v", items, err)
	}
	if ok, err := provider.Mkdir(ctx, "u115", "/TV", "Season 02"); err != nil || !ok {
		t.Fatalf("Mkdir: ok=%v err=%v", ok, err)
	}
	if ok, err := provider.Rename(ctx, "u115", "/TV/S01E01.mkv", "S01E02.mkv", false); err != nil || !ok {
		t.Fatalf("Rename: ok=%v err=%v", ok, err)
	}
	if ok, err := provider.Delete(ctx, "u115", "/TV/S01E01.mkv"); err != nil || !ok {
		t.Fatalf("Delete: ok=%v err=%v", ok, err)
	}
	usage, err := provider.Usage(ctx, "u115")
	if err != nil || usage.Total != 50000 || usage.Available != 12000 {
		t.Fatalf("Usage = %+v, err = %v", usage, err)
	}
	rc, ct, err := provider.Open(ctx, "u115", "/TV/S01E01.mkv")
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	body, _ := io.ReadAll(rc)
	_ = rc.Close()
	if string(body) != "115-video-bytes" || ct != "video/x-matroska" {
		t.Fatalf("Open body=%q ct=%q", string(body), ct)
	}

	// OAuth flow check
	authData, msg, err := provider.AuthURL(ctx, "u115")
	if err != nil || msg != "" || authData.(map[string]any)["state"] != "st-1" {
		t.Fatalf("AuthURL: data=%v msg=%q err=%v", authData, msg, err)
	}
	oauthCheck, msg, err := provider.CheckLogin(ctx, "u115", "", "")
	if err != nil || msg != "" || oauthCheck.(map[string]any)["status"] != 2 {
		t.Fatalf("OAuth CheckLogin: data=%v msg=%q err=%v", oauthCheck, msg, err)
	}
	if persisted["access_token"] != "oauth-tok" {
		t.Fatalf("expected oauth token persisted, got %+v", persisted)
	}
}
