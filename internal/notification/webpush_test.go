package notification

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

type webPushSourceFake struct {
	subscriptions []WebPushSubscription
	removed       []string
	listErr       error
	listCalls     int
}

func (f *webPushSourceFake) List(context.Context) ([]WebPushSubscription, error) {
	f.listCalls++
	if f.listErr != nil {
		return nil, f.listErr
	}
	return f.subscriptions, nil
}

func (f *webPushSourceFake) Remove(_ context.Context, endpoint string) error {
	f.removed = append(f.removed, endpoint)
	return nil
}

type webPushRecorder struct {
	mu       sync.Mutex
	requests int
	headers  []http.Header
	bodies   [][]byte
	paths    []string
}

func (r *webPushRecorder) record(request *http.Request) {
	body := make([]byte, 0)
	if request.Body != nil {
		buffer := make([]byte, 8192)
		for {
			read, err := request.Body.Read(buffer)
			body = append(body, buffer[:read]...)
			if err != nil {
				break
			}
		}
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.requests++
	r.headers = append(r.headers, request.Header.Clone())
	r.bodies = append(r.bodies, body)
	r.paths = append(r.paths, request.URL.Path)
}

func (r *webPushRecorder) count() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.requests
}

func newTestWebPushSender(t *testing.T, source WebPushSubscriptionSource, config WebPushConfig) *WebPushSender {
	t.Helper()
	privateBytes := make([]byte, 32)
	if _, err := rand.Read(privateBytes); err != nil {
		t.Fatalf("生成 VAPID 私钥失败: %v", err)
	}
	if config.VAPIDPrivateKey == "" {
		config.VAPIDPrivateKey = base64.RawURLEncoding.EncodeToString(privateBytes)
	}
	if config.VAPIDSubject == "" {
		config.VAPIDSubject = "mailto:admin@example.com"
	}
	return NewWebPushSender(nil, config, source)
}

func TestNewWebPushSenderDefaults(t *testing.T) {
	sender := NewWebPushSender(nil, WebPushConfig{}, &webPushSourceFake{})
	if sender.logger == nil {
		t.Fatal("nil logger 应回落到默认 logger")
	}
	if sender.httpClient == nil || sender.httpClient.Timeout != webPushRequestTimeout {
		t.Fatalf("HTTP 客户端 = %+v", sender.httpClient)
	}
	if sender.now == nil {
		t.Fatal("now 必须初始化")
	}
	if sender.Name() != ChannelWebPush {
		t.Fatalf("渠道名 = %q", sender.Name())
	}
}

func TestWebPushPayloadShape(t *testing.T) {
	payload, err := WebPushPayload(Message{Title: " 沙丘 ", Text: "下载完成", Link: "https://app.example/#/x"})
	if err != nil {
		t.Fatalf("构造载荷失败: %v", err)
	}
	var decoded map[string]any
	if err := json.Unmarshal(payload, &decoded); err != nil {
		t.Fatalf("载荷不是 JSON: %v", err)
	}
	if decoded["title"] != "沙丘" || decoded["body"] != "下载完成" || decoded["url"] != "https://app.example/#/x" {
		t.Fatalf("载荷 = %v", decoded)
	}

	// 只有正文没有标题时，正文顶替标题（V2 的 fallback），且不重复展示。
	payload, err = WebPushPayload(Message{Text: "只有正文"})
	if err != nil {
		t.Fatalf("构造载荷失败: %v", err)
	}
	_ = json.Unmarshal(payload, &decoded)
	if decoded["title"] != "只有正文" || decoded["body"] != "" {
		t.Fatalf("正文回退 = %v", decoded)
	}

	// 没有链接时用默认站内地址。
	payload, err = WebPushPayload(Message{Title: "标题"})
	if err != nil {
		t.Fatalf("构造载荷失败: %v", err)
	}
	_ = json.Unmarshal(payload, &decoded)
	if decoded["url"] != webPushDefaultURL {
		t.Fatalf("默认链接 = %v", decoded["url"])
	}

	// 超过载荷上限必须拒绝（推送服务会 413）。
	oversize := Message{Title: "标题", Text: strings.Repeat("长", webPushPayloadMaxBytes)}
	if _, err := WebPushPayload(oversize); !errors.Is(err, errWebPushConfig) {
		t.Fatalf("超大载荷 = %v", err)
	}
}

func TestWebPushTTLAndEndpointHelpers(t *testing.T) {
	wns := "https://wns2-sg2p.notify.windows.com/w/?token=abc"
	if !isWNSEndpoint(wns) {
		t.Fatal("WNS 端点未识别")
	}
	if isWNSEndpoint("https://fcm.googleapis.com/fcm/send/abc") {
		t.Fatal("非 WNS 端点被误判")
	}
	if got := webPushEndpointHost(wns); got != "wns2-sg2p.notify.windows.com" {
		t.Fatalf("端点主机 = %q", got)
	}
	// 无法解析的端点不返回垃圾字符串。
	if got := webPushEndpointHost("://坏端点"); got != "" {
		t.Fatalf("非法端点主机 = %q", got)
	}

	source := &webPushSourceFake{}
	sender := newTestWebPushSender(t, source, WebPushConfig{TTL: 2 * time.Hour})
	// WNS 固定 24 小时，且忽略配置里的 TTL。
	if got := sender.ttlFor(wns); got != webPushWNSDefaultTTL {
		t.Fatalf("WNS TTL = %v", got)
	}
	if got := sender.ttlFor("https://fcm.googleapis.com/fcm/send/abc"); got != 2*time.Hour {
		t.Fatalf("普通端点 TTL = %v", got)
	}
	// 未配置 TTL 时用 0（立即投递、不缓存）。
	if got := newTestWebPushSender(t, source, WebPushConfig{}).ttlFor("https://fcm.googleapis.com/x"); got != 0 {
		t.Fatalf("默认 TTL = %v", got)
	}

	if got := webPushTTLSeconds(2 * time.Hour); got != 7200 {
		t.Fatalf("TTL 秒 = %d", got)
	}
	if got := webPushTTLSeconds(0); got != 0 {
		t.Fatalf("零 TTL 秒 = %d", got)
	}
	if got := webPushTTLSeconds(-time.Minute); got != 0 {
		t.Fatalf("负 TTL 秒 = %d", got)
	}
}

func TestWebPushUsernameAllowed(t *testing.T) {
	open := newTestWebPushSender(t, &webPushSourceFake{}, WebPushConfig{})
	if !open.usernameAllowed("") || !open.usernameAllowed("alice") {
		t.Fatal("未配置用户过滤时应全部放行")
	}
	filtered := newTestWebPushSender(t, &webPushSourceFake{}, WebPushConfig{Usernames: []string{" alice ", "bob"}})
	if !filtered.usernameAllowed("alice") {
		t.Fatal("配置里的用户名（含空白）应放行")
	}
	if filtered.usernameAllowed(" carol") {
		t.Fatal("未配置的用户名不应放行")
	}
	// 配置了白名单时，无用户名的消息一律不发（否则会广播给所有人）。
	if filtered.usernameAllowed("   ") {
		t.Fatal("空用户名不应放行")
	}
}

func TestWebPushSendGuards(t *testing.T) {
	ctx := context.Background()
	var nilSender *WebPushSender
	if err := nilSender.Send(ctx, Message{Title: "x"}); !errors.Is(err, errWebPushUnavailable) {
		t.Fatalf("nil 发送器 = %v", err)
	}
	broken := &WebPushSender{config: WebPushConfig{}, source: &webPushSourceFake{}, now: time.Now}
	if err := broken.Send(ctx, Message{Title: "x"}); !errors.Is(err, errWebPushUnavailable) {
		t.Fatalf("缺 HTTP 客户端 = %v", err)
	}
	noClock := newTestWebPushSender(t, &webPushSourceFake{}, WebPushConfig{})
	noClock.now = nil
	if err := noClock.Send(ctx, Message{Title: "x"}); !errors.Is(err, errWebPushUnavailable) {
		t.Fatalf("缺时钟 = %v", err)
	}

	sender := newTestWebPushSender(t, nil, WebPushConfig{})
	if err := sender.Send(ctx, Message{Title: "x"}); !errors.Is(err, errWebPushUnavailable) {
		t.Fatalf("缺订阅来源 = %v", err)
	}
	if err := sender.Send(nil, Message{Title: "x"}); err == nil {
		t.Fatal("nil 上下文应报错")
	}
	cancelled, cancel := context.WithCancel(ctx)
	cancel()
	if err := sender.Send(cancelled, Message{Title: "x"}); !errors.Is(err, context.Canceled) {
		t.Fatalf("已取消上下文 = %v", err)
	}

	// 标题与正文都为空：不发请求，只记一条 WARN。
	source := &webPushSourceFake{subscriptions: []WebPushSubscription{{Endpoint: "https://push.example/x", P256dh: "a", Auth: "b"}}}
	recorder := &webPushRecorder{}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		recorder.record(r)
		w.WriteHeader(http.StatusCreated)
	}))
	defer server.Close()
	quiet := newTestWebPushSender(t, source, WebPushConfig{})
	quiet.httpClient = server.Client()
	if err := quiet.Send(ctx, Message{}); err != nil {
		t.Fatalf("空消息 = %v", err)
	}
	if recorder.count() != 0 || source.listCalls != 0 {
		t.Fatal("空消息不应发起投递")
	}

	// 用户被过滤时不投递。
	blocked := newTestWebPushSender(t, source, WebPushConfig{Usernames: []string{"alice"}})
	blocked.httpClient = server.Client()
	if err := blocked.Send(ctx, Message{Title: "x", Username: "bob"}); err != nil {
		t.Fatalf("用户过滤 = %v", err)
	}
	if recorder.count() != 0 || source.listCalls != 0 {
		t.Fatal("被过滤的用户不应发起投递")
	}

	// 订阅来源报错时返回不可用。
	failing := &webPushSourceFake{listErr: errors.New("数据库不可用")}
	failingSender := newTestWebPushSender(t, failing, WebPushConfig{})
	failingSender.httpClient = server.Client()
	if err := failingSender.Send(ctx, Message{Title: "x"}); !errors.Is(err, errWebPushUnavailable) {
		t.Fatalf("订阅来源错误 = %v", err)
	}
}

func TestWebPushSendDeliversEncryptedPayload(t *testing.T) {
	receiver := newRWReceiver(t)
	recorder := &webPushRecorder{}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		recorder.record(r)
		w.WriteHeader(http.StatusCreated)
	}))
	defer server.Close()

	endpoint := server.URL + "/push/v2/sub-1"
	source := &webPushSourceFake{subscriptions: []WebPushSubscription{{
		Endpoint: endpoint, P256dh: receiver.publicB64, Auth: receiver.authB64, Username: "alice",
	}}}
	sender := newTestWebPushSender(t, source, WebPushConfig{TTL: time.Hour})
	sender.httpClient = server.Client()

	if err := sender.Send(context.Background(), Message{
		Title: "沙丘", Text: "下载完成", Link: "https://app.example/#/x", Username: "alice",
	}); err != nil {
		t.Fatalf("投递失败: %v", err)
	}
	if recorder.count() != 1 {
		t.Fatalf("请求数 = %d", recorder.count())
	}
	headers := recorder.headers[0]
	if headers.Get("Content-Encoding") != "aes128gcm" {
		t.Fatalf("Content-Encoding = %q", headers.Get("Content-Encoding"))
	}
	if headers.Get("Content-Type") != "application/octet-stream" {
		t.Fatalf("Content-Type = %q", headers.Get("Content-Type"))
	}
	if headers.Get("TTL") != "3600" {
		t.Fatalf("TTL = %q", headers.Get("TTL"))
	}
	if !strings.HasPrefix(headers.Get("Authorization"), "vapid t=") {
		t.Fatalf("Authorization = %q", headers.Get("Authorization"))
	}
	// 非 WNS 端点不应带 X-WNS-Cache-Policy。
	if headers.Get("X-WNS-Cache-Policy") != "" {
		t.Fatalf("非 WNS 端点带了 WNS 头: %q", headers.Get("X-WNS-Cache-Policy"))
	}

	// 密文必须真能被浏览器侧解出原始 JSON 载荷——这才是 RFC 8291 的验收。
	plaintext := receiver.decrypt(t, recorder.bodies[0])
	want, err := WebPushPayload(Message{Title: "沙丘", Text: "下载完成", Link: "https://app.example/#/x"})
	if err != nil {
		t.Fatalf("构造期望载荷失败: %v", err)
	}
	if string(plaintext) != string(want) {
		t.Fatalf("解密结果 = %s，期望 %s", plaintext, want)
	}
	if len(source.removed) != 0 {
		t.Fatalf("成功投递不应移除订阅: %v", source.removed)
	}
}

func TestWebPushSendRemovesGoneSubscription(t *testing.T) {
	receiver := newRWReceiver(t)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusGone)
	}))
	defer server.Close()

	endpoint := server.URL + "/push/v2/gone"
	source := &webPushSourceFake{subscriptions: []WebPushSubscription{{
		Endpoint: endpoint, P256dh: receiver.publicB64, Auth: receiver.authB64,
	}}}
	sender := newTestWebPushSender(t, source, WebPushConfig{})
	sender.httpClient = server.Client()

	// 410 表示订阅已失效：移除但不让整次发送失败（其它订阅还要继续）。
	if err := sender.Send(context.Background(), Message{Title: "x"}); err != nil {
		t.Fatalf("投递失败: %v", err)
	}
	if len(source.removed) != 1 || source.removed[0] != endpoint {
		t.Fatalf("移除记录 = %v", source.removed)
	}
}

func TestWebPushSendKeepsSubscriptionOnServerError(t *testing.T) {
	receiver := newRWReceiver(t)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer server.Close()

	source := &webPushSourceFake{subscriptions: []WebPushSubscription{{
		Endpoint: server.URL + "/push/v2/err", P256dh: receiver.publicB64, Auth: receiver.authB64,
	}}}
	sender := newTestWebPushSender(t, source, WebPushConfig{})
	sender.httpClient = server.Client()

	// 5xx 是临时故障：不能移除订阅（否则用户要重新授权）。
	if err := sender.Send(context.Background(), Message{Title: "x"}); err != nil {
		t.Fatalf("投递失败: %v", err)
	}
	if len(source.removed) != 0 {
		t.Fatalf("临时故障不应移除订阅: %v", source.removed)
	}
}

func TestWebPushSendIsolatesSubscriptionFailures(t *testing.T) {
	receiver := newRWReceiver(t)
	var mu sync.Mutex
	attempts := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		attempts++
		mu.Unlock()
		if strings.HasSuffix(r.URL.Path, "/bad") {
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		w.WriteHeader(http.StatusCreated)
	}))
	defer server.Close()

	source := &webPushSourceFake{subscriptions: []WebPushSubscription{
		{Endpoint: server.URL + "/bad", P256dh: receiver.publicB64, Auth: receiver.authB64},
		{Endpoint: server.URL + "/good", P256dh: receiver.publicB64, Auth: receiver.authB64},
	}}
	sender := newTestWebPushSender(t, source, WebPushConfig{})
	sender.httpClient = server.Client()

	// 第一个订阅失败不应阻止第二个订阅收到通知。
	if err := sender.Send(context.Background(), Message{Title: "x"}); err != nil {
		t.Fatalf("投递失败: %v", err)
	}
	mu.Lock()
	total := attempts
	mu.Unlock()
	if total != 2 {
		t.Fatalf("尝试次数 = %d", total)
	}
}

func TestWebPushSendInvalidSubscriptionKeys(t *testing.T) {
	recorder := &webPushRecorder{}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		recorder.record(r)
		w.WriteHeader(http.StatusCreated)
	}))
	defer server.Close()

	// 密钥非法时在本地失败，不发请求，也不影响其它订阅。
	source := &webPushSourceFake{subscriptions: []WebPushSubscription{{
		Endpoint: server.URL + "/push/v2/bad", P256dh: "not-a-key", Auth: "bad",
	}}}
	sender := newTestWebPushSender(t, source, WebPushConfig{})
	sender.httpClient = server.Client()
	if err := sender.Send(context.Background(), Message{Title: "x"}); err != nil {
		t.Fatalf("投递失败: %v", err)
	}
	if recorder.count() != 0 {
		t.Fatalf("密钥非法时不应发请求，实际 %d 次", recorder.count())
	}
}
