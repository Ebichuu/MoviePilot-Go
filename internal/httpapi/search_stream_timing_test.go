package httpapi

import (
	"bufio"
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/moviepilot-go/moviepilot-go/internal/auth"
	"github.com/moviepilot-go/moviepilot-go/internal/config"
	"github.com/moviepilot-go/moviepilot-go/internal/indexer"
)

// blockingSearchAdapter keeps the HTTP handler inside Search until the test
// has observed the first SSE event at the client boundary.
type blockingSearchAdapter struct {
	entered  chan<- struct{}
	release  <-chan struct{}
	onCancel chan<- struct{}
}

func (a *blockingSearchAdapter) Name() string { return "timing-fixture" }

func (a *blockingSearchAdapter) SiteID() int { return 7 }

func (a *blockingSearchAdapter) Search(ctx context.Context, _ indexer.Query) ([]indexer.Resource, error) {
	select {
	case a.entered <- struct{}{}:
	case <-ctx.Done():
		return nil, ctx.Err()
	}
	select {
	case <-a.release:
		return []indexer.Resource{{Title: "Timing Fixture S01E01"}}, nil
	case <-ctx.Done():
		if a.onCancel != nil {
			a.onCancel <- struct{}{}
		}
		return nil, ctx.Err()
	}
}

type searchStreamHTTPResult struct {
	response *http.Response
	err      error
}

func newSearchStreamRouter(t *testing.T, adapter indexer.Adapter) (http.Handler, func()) {
	t.Helper()
	coordinator, err := indexer.NewCoordinator(1)
	if err != nil {
		t.Fatalf("创建搜索协调器失败: %v", err)
	}
	authService := auth.NewService(nil, "sse-timing-jwt-secret", "sse-timing-resource-secret", time.Hour)
	handler := NewRouter(Dependencies{
		Config: config.Config{MaxRequestBody: 1 << 20, RequestTimeout: 2 * time.Second},
		Auth:   authService,
		Search: coordinator,
		SearchAdapters: []indexer.Adapter{
			adapter,
		},
	})
	return handler, coordinator.Close
}

func startSearchStreamRequest(t *testing.T, serverURL, token string, requestContext context.Context) <-chan searchStreamHTTPResult {
	t.Helper()
	request, err := http.NewRequestWithContext(requestContext, http.MethodGet, serverURL+"/api/v1/search/title/stream?keyword=timing", nil)
	if err != nil {
		t.Fatalf("创建 SSE 请求失败: %v", err)
	}
	request.AddCookie(&http.Cookie{Name: "MoviePilot", Value: token})
	client := &http.Client{Transport: &http.Transport{ForceAttemptHTTP2: false}}
	result := make(chan searchStreamHTTPResult, 1)
	go func() {
		response, requestErr := client.Do(request)
		result <- searchStreamHTTPResult{response: response, err: requestErr}
	}()
	return result
}

func resourceTokenForSSE(t *testing.T) string {
	t.Helper()
	service := auth.NewService(nil, "sse-timing-jwt-secret", "sse-timing-resource-secret", time.Hour)
	token, err := service.ResourceToken(auth.User{ID: 1, Username: "sse-timing"}, time.Minute)
	if err != nil {
		t.Fatalf("创建资源令牌失败: %v", err)
	}
	return token
}

func waitForSignal(t *testing.T, name string, signal <-chan struct{}) {
	t.Helper()
	select {
	case <-signal:
	case <-time.After(2 * time.Second):
		t.Fatalf("等待 %s 超时", name)
	}
}

func awaitSearchStreamResponse(t *testing.T, result <-chan searchStreamHTTPResult) *http.Response {
	t.Helper()
	select {
	case requestResult := <-result:
		if requestResult.err != nil {
			t.Fatalf("SSE 请求未建立响应: %v", requestResult.err)
		}
		return requestResult.response
	case <-time.After(2 * time.Second):
		t.Fatal("SSE 请求未建立响应")
		return nil
	}
}

func TestSearchTitleStreamFlushesFirstEventBeforeHandlerCompletes(t *testing.T) {
	entered := make(chan struct{}, 1)
	release := make(chan struct{})
	adapter := &blockingSearchAdapter{entered: entered, release: release}
	handler, closeCoordinator := newSearchStreamRouter(t, adapter)
	defer closeCoordinator()
	handlerDone := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer close(handlerDone)
		handler.ServeHTTP(w, r)
	}))
	defer server.Close()

	requestContext, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	result := startSearchStreamRequest(t, server.URL, resourceTokenForSSE(t), requestContext)
	waitForSignal(t, "搜索适配器进入", entered)
	response := awaitSearchStreamResponse(t, result)
	reader := bufio.NewReader(response.Body)
	firstLine, err := reader.ReadString('\n')
	if err != nil {
		t.Fatalf("读取首个 SSE 分块失败: %v", err)
	}
	if !strings.HasPrefix(firstLine, "data: ") {
		t.Fatalf("首个 SSE 分块 = %q, want data prefix", firstLine)
	}
	secondLine, err := reader.ReadString('\n')
	if err != nil {
		t.Fatalf("读取首个 SSE 事件结束分隔失败: %v", err)
	}
	if secondLine != "\n" {
		t.Fatalf("首个 SSE 事件分隔 = %q, want blank line", secondLine)
	}
	select {
	case <-handlerDone:
		t.Fatal("handler 在客户端收到首个 SSE 事件前已完成")
	default:
	}

	close(release)
	select {
	case <-handlerDone:
	case <-time.After(2 * time.Second):
		t.Fatal("释放搜索适配器后 handler 未完成")
	}
	if _, err := io.ReadAll(response.Body); err != nil {
		t.Fatalf("读取 SSE 响应失败: %v", err)
	}
	if err := response.Body.Close(); err != nil {
		t.Fatalf("关闭 SSE 响应失败: %v", err)
	}
}

func TestSearchTitleStreamCancelsBlockedSearchOnRequestTimeout(t *testing.T) {
	entered := make(chan struct{}, 1)
	release := make(chan struct{})
	canceled := make(chan struct{}, 1)
	adapter := &blockingSearchAdapter{entered: entered, release: release, onCancel: canceled}
	handler, closeCoordinator := newSearchStreamRouter(t, adapter)
	defer closeCoordinator()
	handlerDone := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer close(handlerDone)
		handler.ServeHTTP(w, r)
	}))
	defer server.Close()

	requestContext, cancel := context.WithTimeout(context.Background(), 250*time.Millisecond)
	defer cancel()
	result := startSearchStreamRequest(t, server.URL, resourceTokenForSSE(t), requestContext)
	waitForSignal(t, "搜索适配器进入", entered)
	response := awaitSearchStreamResponse(t, result)
	reader := bufio.NewReader(response.Body)
	firstLine, err := reader.ReadString('\n')
	if err != nil {
		t.Fatalf("超时用例读取首个 SSE 分块失败: %v", err)
	}
	if !strings.HasPrefix(firstLine, "data: ") {
		t.Fatalf("超时用例首个 SSE 分块 = %q", firstLine)
	}

	waitForSignal(t, "请求取消传播", canceled)
	select {
	case <-handlerDone:
	case <-time.After(2 * time.Second):
		t.Fatal("请求超时后 handler 未退出")
	}
	_ = response.Body.Close()
}

func TestSearchTitleStreamRejectsInvalidSitesBeforeSSE(t *testing.T) {
	handler, closeCoordinator := newSearchStreamRouter(t, &blockingSearchAdapter{
		entered: make(chan struct{}, 1), release: make(chan struct{}),
	})
	defer closeCoordinator()
	server := httptest.NewServer(handler)
	defer server.Close()

	request, err := http.NewRequest(http.MethodGet, server.URL+"/api/v1/search/title/stream?keyword=timing&sites=bad", nil)
	if err != nil {
		t.Fatalf("创建错误路径请求失败: %v", err)
	}
	request.AddCookie(&http.Cookie{Name: "MoviePilot", Value: resourceTokenForSSE(t)})
	response, err := server.Client().Do(request)
	if err != nil {
		t.Fatalf("错误路径请求失败: %v", err)
	}
	defer response.Body.Close()
	body, err := io.ReadAll(response.Body)
	if err != nil {
		t.Fatalf("读取错误路径响应失败: %v", err)
	}
	if response.StatusCode != http.StatusBadRequest {
		t.Fatalf("错误路径 status = %d, want %d; body=%s", response.StatusCode, http.StatusBadRequest, body)
	}
	if !strings.HasPrefix(response.Header.Get("Content-Type"), "application/json") {
		t.Fatalf("错误路径 Content-Type = %q", response.Header.Get("Content-Type"))
	}
	if strings.Contains(string(body), "data: ") {
		t.Fatalf("错误路径不应伪装成 SSE: %s", body)
	}
}
