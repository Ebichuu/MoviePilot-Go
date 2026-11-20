package indexer

import (
	"context"
	"errors"
	"net/http"
	"testing"
)

// TestSunnyPTResolveDownloadTransportFailures 覆盖下载凭证请求失败与后续
// 种子下载失败两个分支（均为真实可达的传输错误）。
func TestSunnyPTResolveDownloadTransportFailures(t *testing.T) {
	t.Run("凭证请求失败", func(t *testing.T) {
		client := newIndexerHTTPClient(t, 1)
		client.client.Transport = roundTripFunc(func(*http.Request) (*http.Response, error) {
			return nil, errors.New("连接中断")
		})
		adapter, err := NewSunnyPTAdapter(client, 11, "SunnyPT", "https://sunny.example", map[string]any{
			"api_url": "https://sunny.example", "apikey": "k",
		})
		if err != nil {
			t.Fatalf("构造适配器: %v", err)
		}
		if _, err := adapter.ResolveDownload(context.Background(), "42"); err == nil {
			t.Fatal("凭证请求失败应报错")
		}
	})

	t.Run("种子下载失败", func(t *testing.T) {
		requests := 0
		client := newIndexerHTTPClient(t, 1)
		client.client.Transport = roundTripFunc(func(*http.Request) (*http.Response, error) {
			requests++
			if requests == 1 {
				return mtorrentJSONResponse(`{"code":0,"data":{"download_url":"/download/42"}}`), nil
			}
			return nil, errors.New("连接中断")
		})
		adapter, err := NewSunnyPTAdapter(client, 11, "SunnyPT", "https://sunny.example", map[string]any{
			"api_url": "https://sunny.example", "apikey": "k",
		})
		if err != nil {
			t.Fatalf("构造适配器: %v", err)
		}
		if _, err := adapter.ResolveDownload(context.Background(), "42"); err == nil {
			t.Fatalf("种子下载失败应报错，请求次数 = %d", requests)
		}
		if requests < 2 {
			t.Fatalf("未走到下载阶段，请求次数 = %d", requests)
		}
	})
}

// TestRousiUserDataTruncatesUnparsableRegistration 覆盖注册时间无法解析时
// 直接取原始字符串并被截断到 19 个字符的分支。
func TestRousiUserDataTruncatesUnparsableRegistration(t *testing.T) {
	endpoint := probeServer(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/v1/profile" {
			_, _ = w.Write([]byte(`{"code":0,"data":{"id":77,"username":"rousi-user",
				"role_text":"Uploader","registered_at":"2024/01/01 00:00:00.123456789",
				"uploaded":3000,"downloaded":1000,"ratio":2.345}}`))
			return
		}
		_, _ = w.Write([]byte(`{"code":0,"data":{"messages":[],"total_pages":1}}`))
	})
	adapter, err := NewRousiAdapter(newIndexerHTTPClient(t, 1), 5, "Rousi", endpoint, map[string]any{
		"passkey": "k",
	})
	if err != nil {
		t.Fatalf("构造适配器: %v", err)
	}
	data, err := adapter.FetchSiteUserData(context.Background())
	if err != nil {
		t.Fatalf("用户数据: %v", err)
	}
	if data.JoinAt == nil || len(*data.JoinAt) != 19 {
		t.Fatalf("注册时间未截断: %v", data.JoinAt)
	}
}
