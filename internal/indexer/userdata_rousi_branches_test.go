package indexer

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"testing"
)

func rousiUserDataAdapter(t *testing.T, endpoint string, messageHeaders http.Header) *RousiAdapter {
	t.Helper()
	adapter, err := NewRousiAdapter(newIndexerHTTPClient(t, 1), 5, "Rousi", endpoint, map[string]any{
		"passkey": "k",
	})
	if err != nil {
		t.Fatalf("构造适配器: %v", err)
	}
	adapter.messageHeaders = messageHeaders
	return adapter
}

// TestFetchRousiUserDataPageBranches 覆盖状态码白名单与传输错误。
func TestFetchRousiUserDataPageBranches(t *testing.T) {
	for _, status := range []int{http.StatusOK, http.StatusForbidden, http.StatusInternalServerError} {
		endpoint := probeServer(t, func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(status)
			_, _ = w.Write([]byte(`{"code":0}`))
		})
		adapter := rousiUserDataAdapter(t, endpoint, nil)
		body, err := adapter.fetchRousiUserDataPage(context.Background(), endpoint, nil)
		if err != nil || string(body) != `{"code":0}` {
			t.Fatalf("状态码 %d = %q（%v）", status, body, err)
		}
	}

	notFound := probeServer(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	})
	adapter := rousiUserDataAdapter(t, notFound, nil)
	if body, err := adapter.fetchRousiUserDataPage(context.Background(), notFound, nil); err != nil || body != nil {
		t.Fatalf("404 = %q（%v）", body, err)
	}

	// 传输层失败（连接被拒）被吞成空结果。
	if body, err := adapter.fetchRousiUserDataPage(context.Background(), "http://127.0.0.1:1/x", nil); err != nil || body != nil {
		t.Fatalf("连接失败 = %q（%v）", body, err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := adapter.fetchRousiUserDataPage(ctx, notFound, nil); !errors.Is(err, context.Canceled) {
		t.Fatalf("取消上下文 = %v", err)
	}
}

// TestFetchRousiUserDataMessagesBranches 覆盖消息分页请求与结果形状。
func TestFetchRousiUserDataMessagesBranches(t *testing.T) {
	var query string
	endpoint := probeServer(t, func(w http.ResponseWriter, r *http.Request) {
		query = r.URL.RawQuery
		_, _ = w.Write([]byte(`{"code":0,"data":{"items":[{"id":1,"title":"t"}],"total":1}}`))
	})
	adapter := rousiUserDataAdapter(t, endpoint, nil)
	data, err := adapter.fetchRousiUserDataMessages(context.Background(), 3)
	if err != nil {
		t.Fatalf("请求失败: %v", err)
	}
	if !strings.Contains(query, "page=3") || !strings.Contains(query, "page_size=100") ||
		!strings.Contains(query, "unread_only=true") {
		t.Fatalf("查询串 = %q", query)
	}
	if len(rousiUserDataMessages(data["items"])) != 1 {
		t.Fatalf("消息数据 = %+v", data)
	}

	cases := []struct {
		name   string
		status int
		body   string
	}{
		{name: "状态码非 200", status: http.StatusBadGateway, body: `{"code":0,"data":{}}`},
		{name: "业务码非零", status: http.StatusOK, body: `{"code":1,"data":{}}`},
		{name: "数据不是对象", status: http.StatusOK, body: `{"code":0,"data":[]}`},
		{name: "非法 JSON", status: http.StatusOK, body: `not json`},
	}
	for _, tc := range cases {
		broken := probeServer(t, func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(tc.status)
			_, _ = w.Write([]byte(tc.body))
		})
		brokenAdapter := rousiUserDataAdapter(t, broken, nil)
		data, err := brokenAdapter.fetchRousiUserDataMessages(context.Background(), 1)
		if err != nil || len(data) != 0 {
			t.Fatalf("%s = %+v（%v）", tc.name, data, err)
		}
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := adapter.fetchRousiUserDataMessages(ctx, 1); !errors.Is(err, context.Canceled) {
		t.Fatalf("取消上下文 = %v", err)
	}
}

// TestDecodeRousiUserDataJSONBranches 覆盖消息 JSON 的解析守卫。
func TestDecodeRousiUserDataJSONBranches(t *testing.T) {
	if _, ok := decodeRousiUserDataJSON([]byte("   ")); ok {
		t.Fatal("空正文应失败")
	}
	if _, ok := decodeRousiUserDataJSON([]byte("not json")); ok {
		t.Fatal("非法 JSON 应失败")
	}
	if _, ok := decodeRousiUserDataJSON([]byte(`[1]`)); ok {
		t.Fatal("根数组应失败")
	}
	if _, ok := decodeRousiUserDataJSON([]byte(`{"a":1} {"b":2}`)); ok {
		t.Fatal("多余内容应失败")
	}
	document, ok := decodeRousiUserDataJSON([]byte(`{"code":0}`))
	if !ok || document["code"] == nil {
		t.Fatalf("合法对象 = %+v（%v）", document, ok)
	}
}

// TestRousiUserDataCodeZeroBranches 覆盖业务码的弱类型判零。
func TestRousiUserDataCodeZeroBranches(t *testing.T) {
	zero := []any{json.Number("0"), float64(0), 0, int64(0)}
	for _, value := range zero {
		if !rousiUserDataCodeZero(value) {
			t.Fatalf("%v 应判为 0", value)
		}
	}
	nonZero := []any{json.Number("1"), json.Number("bad"), float64(1), 1, "0", nil}
	for _, value := range nonZero {
		if rousiUserDataCodeZero(value) {
			t.Fatalf("%v 不应判为 0", value)
		}
	}
}

// TestRousiUserDataMessagesBranches 覆盖消息数组清洗。
func TestRousiUserDataMessagesBranches(t *testing.T) {
	if messages := rousiUserDataMessages(nil); messages != nil {
		t.Fatalf("nil = %+v", messages)
	}
	if messages := rousiUserDataMessages("nope"); messages != nil {
		t.Fatalf("非数组 = %+v", messages)
	}
	messages := rousiUserDataMessages([]any{"skip", map[string]any{"id": 1}})
	if len(messages) != 1 || messages[0]["id"] == nil {
		t.Fatalf("消息清洗 = %+v", messages)
	}
}
