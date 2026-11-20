package indexer

import (
	"context"
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"
)

// TestYemaSearchPageNormalizationAndDecodeError 覆盖负页码归一与非 JSON 响应解码失败。
func TestYemaSearchPageNormalizationAndDecodeError(t *testing.T) {
	var payload string
	endpoint := probeServer(t, func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		payload = string(body)
		_, _ = w.Write([]byte(`{"success":true,"data":[]}`))
	})
	adapter, err := NewYemaAdapter(newIndexerHTTPClient(t, 1), 21, "YemaPT", endpoint, map[string]any{"apikey": "k"})
	if err != nil {
		t.Fatalf("构造适配器: %v", err)
	}
	if _, err := adapter.Search(context.Background(), Query{Keyword: "某剧", Page: -2, Limit: 10}); err != nil {
		t.Fatalf("负页码搜索: %v", err)
	}
	if !strings.Contains(payload, `"current":1`) {
		t.Fatalf("负页码未归一: %s", payload)
	}

	brokenEndpoint := probeServer(t, func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte("<html>不是 JSON</html>"))
	})
	broken, err := NewYemaAdapter(newIndexerHTTPClient(t, 1), 21, "YemaPT", brokenEndpoint, map[string]any{"apikey": "k"})
	if err != nil {
		t.Fatalf("构造适配器: %v", err)
	}
	if _, err := broken.Search(context.Background(), Query{Keyword: "某剧"}); err == nil {
		t.Fatal("非 JSON 响应应报错")
	}
}

// TestYemaSearchCancelDuringIteration 覆盖条目遍历中的上下文取消检查。
func TestYemaSearchCancelDuringIteration(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	client := newIndexerHTTPClient(t, 1)
	client.client.Transport = roundTripFunc(func(*http.Request) (*http.Response, error) {
		response := mtorrentJSONResponse(`{"success":true,"data":[{"id":"1","showName":"甲"},{"id":"2","showName":"乙"}]}`)
		cancel()
		return response, nil
	})
	adapter, err := NewYemaAdapter(client, 21, "YemaPT", "https://yema.example", map[string]any{"apikey": "k"})
	if err != nil {
		t.Fatalf("构造适配器: %v", err)
	}
	if _, err := adapter.Search(ctx, Query{Keyword: "某剧"}); !errors.Is(err, context.Canceled) {
		t.Fatalf("遍历中取消 = %v", err)
	}
}

// TestYemaResolveDownloadCredentialRequestFailure 覆盖下载凭证请求的传输失败。
func TestYemaResolveDownloadCredentialRequestFailure(t *testing.T) {
	client := newIndexerHTTPClient(t, 1)
	client.client.Transport = roundTripFunc(func(*http.Request) (*http.Response, error) {
		return nil, errors.New("连接中断")
	})
	adapter, err := NewYemaAdapter(client, 21, "YemaPT", "https://yema.example", map[string]any{"apikey": "k"})
	if err != nil {
		t.Fatalf("构造适配器: %v", err)
	}
	if _, err := adapter.ResolveDownload(context.Background(), "7"); err == nil {
		t.Fatal("凭证请求失败应报错")
	}
}

// TestYemaUserDataTransportAndTrailingJSON 覆盖用户数据页的非上下文传输失败
// （按“无响应”处理）与 JSON 之后的多余内容。
func TestYemaUserDataTransportAndTrailingJSON(t *testing.T) {
	failing := newIndexerHTTPClient(t, 1)
	failing.client.Transport = roundTripFunc(func(*http.Request) (*http.Response, error) {
		return nil, errors.New("连接中断")
	})
	failingAdapter, err := NewYemaAdapter(failing, 21, "YemaPT", "https://yema.example", map[string]any{"apikey": "k"})
	if err != nil {
		t.Fatalf("构造适配器: %v", err)
	}
	data, err := failingAdapter.FetchSiteUserData(context.Background())
	if err != nil {
		t.Fatalf("传输失败不应返回错误: %v", err)
	}
	if data.ErrMsg == nil || !strings.Contains(*data.ErrMsg, "未收到开放 API 响应") {
		t.Fatalf("提示 = %v", data.ErrMsg)
	}

	trailing := newIndexerHTTPClient(t, 1)
	trailing.client.Transport = roundTripFunc(func(*http.Request) (*http.Response, error) {
		return mtorrentJSONResponse(`{"success":true,"data":{}} {"多余":1}`), nil
	})
	trailingAdapter, err := NewYemaAdapter(trailing, 21, "YemaPT", "https://yema.example", map[string]any{"apikey": "k"})
	if err != nil {
		t.Fatalf("构造适配器: %v", err)
	}
	data, err = trailingAdapter.FetchSiteUserData(context.Background())
	if err != nil {
		t.Fatalf("多余内容不应返回错误: %v", err)
	}
	if data.ErrMsg == nil || !strings.Contains(*data.ErrMsg, "有效 JSON") {
		t.Fatalf("提示 = %v", data.ErrMsg)
	}
}
