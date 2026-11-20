package httpapi

import (
	"bufio"
	"context"
	"encoding/json"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestCompatStubHandlersReturnEmptyV2Shapes(t *testing.T) {
	tests := []struct {
		name    string
		handler http.HandlerFunc
	}{
		{name: "排程列表", handler: dashboardSchedule},
		{name: "整理队列", handler: transferQueue},
		{name: "插件侧栏", handler: pluginSidebarNav},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			request := httptest.NewRequest(http.MethodGet, "/stub", nil)
			recorder := httptest.NewRecorder()
			test.handler(recorder, request)

			if recorder.Code != http.StatusOK {
				t.Fatalf("status = %d", recorder.Code)
			}
			// V2 兼容端点必须返回空数组，而不是 null 或 404。
			if strings.TrimSpace(recorder.Body.String()) != "[]" {
				t.Fatalf("body = %s", recorder.Body.String())
			}
		})
	}

	// MCP 服务器列表返回 Response 包装的空集合。
	recorder := httptest.NewRecorder()
	agentMCPServers(recorder, httptest.NewRequest(http.MethodGet, "/message/agent/mcp/servers", nil))
	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d", recorder.Code)
	}
	var body struct {
		Success bool `json:"success"`
		Data    struct {
			Servers      []any `json:"servers"`
			EnabledCount int   `json:"enabled_count"`
			TotalCount   int   `json:"total_count"`
		} `json:"data"`
	}
	if err := json.Unmarshal(recorder.Body.Bytes(), &body); err != nil {
		t.Fatalf("解析响应: %v; body=%s", err, recorder.Body.String())
	}
	if !body.Success || body.Data.Servers == nil || len(body.Data.Servers) != 0 ||
		body.Data.EnabledCount != 0 || body.Data.TotalCount != 0 {
		t.Fatalf("body = %s", recorder.Body.String())
	}
}

func TestRequestIDReadsContextValue(t *testing.T) {
	if got := requestID(context.Background()); got != "" {
		t.Fatalf("空上下文 = %q", got)
	}
	ctx := context.WithValue(context.Background(), requestIDContextKey, "req-123")
	if got := requestID(ctx); got != "req-123" {
		t.Fatalf("requestID() = %q", got)
	}
	// 类型不符时按空串处理，不 panic。
	wrongType := context.WithValue(context.Background(), requestIDContextKey, 42)
	if got := requestID(wrongType); got != "" {
		t.Fatalf("类型不符 = %q", got)
	}
}

func TestErrorsUnsupportedNamesFeature(t *testing.T) {
	err := errorsUnsupported("hijacking")
	if err == nil || !strings.Contains(err.Error(), "hijacking") {
		t.Fatalf("errorsUnsupported() = %v", err)
	}
}

type hijackableResponseWriter struct {
	*httptest.ResponseRecorder
	hijacked bool
}

func (w *hijackableResponseWriter) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	w.hijacked = true
	return nil, nil, nil
}

func TestStatusWriterHijackDelegatesWhenSupported(t *testing.T) {
	inner := &hijackableResponseWriter{ResponseRecorder: httptest.NewRecorder()}
	writer := &statusWriter{ResponseWriter: inner}

	connection, rw, err := writer.Hijack()
	if err != nil || connection != nil || rw != nil {
		t.Fatalf("Hijack() = (%v, %v, %v)", connection, rw, err)
	}
	if !inner.hijacked {
		t.Fatal("未委托给底层 Hijacker")
	}

	// httptest.ResponseRecorder 不支持 hijack，必须给出可读错误。
	unsupported := &statusWriter{ResponseWriter: httptest.NewRecorder()}
	if _, _, err := unsupported.Hijack(); err == nil || !strings.Contains(err.Error(), "hijacking") {
		t.Fatalf("不支持 hijack 时 err = %v", err)
	}
}

func TestStatusWriterFlushDelegates(t *testing.T) {
	writer := &statusWriter{ResponseWriter: httptest.NewRecorder()}
	// ResponseRecorder 实现 http.Flusher，这里只验证不 panic 且成功分支被走到。
	writer.Flush()

	writer = &statusWriter{ResponseWriter: &nonFlusherWriter{}}
	writer.Flush() // 底层不支持时静默跳过
}

type nonFlusherWriter struct{}

func (*nonFlusherWriter) Header() http.Header        { return http.Header{} }
func (*nonFlusherWriter) Write([]byte) (int, error)  { return 0, nil }
func (*nonFlusherWriter) WriteHeader(statusCode int) {}

func TestDashboardEventStatisticsWithoutStoreReturnsEmptyArray(t *testing.T) {
	// 没有事件统计存储时按 V2 返回空数组：非法/越界 days 只影响查询窗口，不应该报错。
	for _, query := range []string{"", "?days=abc", "?days=0", "?days=999", "?days=7"} {
		recorder := httptest.NewRecorder()
		Dependencies{}.dashboardEventStatistics(recorder, httptest.NewRequest(http.MethodGet, "/dashboard/events"+query, nil))
		if recorder.Code != http.StatusOK || strings.TrimSpace(recorder.Body.String()) != "[]" {
			t.Fatalf("query %q = (%d, %s)", query, recorder.Code, recorder.Body.String())
		}
	}
}
