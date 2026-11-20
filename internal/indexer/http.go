package indexer

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/cookiejar"
	"net/url"
	"strings"
	"time"
)

const maxSiteResponseHeaderBytes = 256 << 10

var errHTTPRequestNotStarted = errors.New("站点 HTTP 请求尚未开始")

type httpRequestNotStartedError struct {
	cause error
}

func (e *httpRequestNotStartedError) Error() string { return e.cause.Error() }

func (e *httpRequestNotStartedError) Unwrap() []error {
	return []error{errHTTPRequestNotStarted, e.cause}
}

type immediateHTTPAdmissionKey struct{}

func withImmediateHTTPAdmission(ctx context.Context) context.Context {
	return context.WithValue(ctx, immediateHTTPAdmissionKey{}, true)
}

func requiresImmediateHTTPAdmission(ctx context.Context) bool {
	value, _ := ctx.Value(immediateHTTPAdmissionKey{}).(bool)
	return value
}

// HTTPClient 为站点适配器共享受控的 HTTP 传输层。
type HTTPClient struct {
	client          *http.Client
	semaphore       chan struct{}
	maxBodyBytes    int64
	maxRequestBytes int64
	allowPrivate    bool
}

type HTTPStatusError struct {
	StatusCode int
}

type httpResponse struct {
	StatusCode   int
	Body         []byte
	ETag         string
	LastModified string
}

func (e *HTTPStatusError) Error() string {
	return fmt.Sprintf("site returned HTTP %d", e.StatusCode)
}

// NewHTTPClient 创建具有并发、超时和响应体上限的站点客户端。
func NewHTTPClient(workers int, timeout time.Duration, maxBodyBytes int64) (*HTTPClient, error) {
	if workers < 1 || workers > 128 || timeout <= 0 || maxBodyBytes < 1 || maxBodyBytes > 16<<20 {
		return nil, errors.New("站点 HTTP 客户端参数无效")
	}
	siteClient := &HTTPClient{
		semaphore: make(chan struct{}, workers), maxBodyBytes: maxBodyBytes,
	}
	transport := &http.Transport{
		DialContext: siteClient.dialContext, MaxIdleConns: workers * 2,
		MaxIdleConnsPerHost: workers, MaxConnsPerHost: workers,
		IdleConnTimeout: 60 * time.Second, TLSHandshakeTimeout: 10 * time.Second,
		ResponseHeaderTimeout: timeout, MaxResponseHeaderBytes: maxSiteResponseHeaderBytes,
	}
	client := &http.Client{Transport: transport, Timeout: timeout}
	client.CheckRedirect = sameHostRedirect
	requestLimit := maxBodyBytes
	if requestLimit > 1<<20 {
		requestLimit = 1 << 20
	}
	siteClient.client, siteClient.maxRequestBytes = client, requestLimit
	return siteClient, nil
}

// SetAllowPrivateHosts 放开站点地址的内网限制；默认关闭，仅本地自建站点需要。
func (c *HTTPClient) SetAllowPrivateHosts(allow bool) {
	if c == nil {
		return
	}
	c.allowPrivate = allow
}

// CloseIdleConnections 释放共享传输层的空闲连接。
func (c *HTTPClient) CloseIdleConnections() {
	if c != nil && c.client != nil {
		c.client.CloseIdleConnections()
	}
}

func (c *HTTPClient) get(ctx context.Context, endpoint string, headers http.Header) ([]byte, error) {
	return c.do(ctx, http.MethodGet, endpoint, headers, nil)
}

func (c *HTTPClient) getLimited(ctx context.Context, endpoint string, headers http.Header, maxBodyBytes int64) ([]byte, error) {
	return c.doLimited(ctx, http.MethodGet, endpoint, headers, nil, maxBodyBytes)
}

func (c *HTTPClient) getResponseLimited(
	ctx context.Context,
	endpoint string,
	headers http.Header,
	maxBodyBytes int64,
) (httpResponse, error) {
	return c.doResponseLimited(ctx, http.MethodGet, endpoint, headers, nil, maxBodyBytes, true)
}

func (c *HTTPClient) postJSON(ctx context.Context, endpoint string, headers http.Header, payload any) ([]byte, error) {
	return c.postJSONLimited(ctx, endpoint, headers, payload, c.maxBodyBytes)
}

func (c *HTTPClient) postJSONLimited(
	ctx context.Context,
	endpoint string,
	headers http.Header,
	payload any,
	maxBodyBytes int64,
) ([]byte, error) {
	body, err := json.Marshal(payload)
	if err != nil {
		return nil, errors.New("编码站点请求体失败")
	}
	if int64(len(body)) > c.maxRequestBytes {
		return nil, fmt.Errorf("站点请求体超过 %d 字节限制", c.maxRequestBytes)
	}
	if headers == nil {
		headers = make(http.Header)
	}
	if headers.Get("Content-Type") == "" {
		headers.Set("Content-Type", "application/json")
	}
	return c.doLimited(ctx, http.MethodPost, endpoint, headers, body, maxBodyBytes)
}

func (c *HTTPClient) do(ctx context.Context, method, endpoint string, headers http.Header, body []byte) ([]byte, error) {
	return c.doLimited(ctx, method, endpoint, headers, body, c.maxBodyBytes)
}

func (c *HTTPClient) doLimited(
	ctx context.Context,
	method string,
	endpoint string,
	headers http.Header,
	body []byte,
	maxBodyBytes int64,
) ([]byte, error) {
	response, err := c.doResponseLimited(ctx, method, endpoint, headers, body, maxBodyBytes, false)
	if err != nil {
		return nil, err
	}
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return nil, &HTTPStatusError{StatusCode: response.StatusCode}
	}
	return response.Body, nil
}

func (c *HTTPClient) doResponseLimited(
	ctx context.Context,
	method string,
	endpoint string,
	headers http.Header,
	body []byte,
	maxBodyBytes int64,
	readErrorBody bool,
) (httpResponse, error) {
	if maxBodyBytes < 1 || maxBodyBytes > c.maxBodyBytes {
		maxBodyBytes = c.maxBodyBytes
	}
	if err := ctx.Err(); err != nil {
		return httpResponse{}, err
	}
	if requiresImmediateHTTPAdmission(ctx) {
		select {
		case c.semaphore <- struct{}{}:
		case <-ctx.Done():
			return httpResponse{}, ctx.Err()
		default:
			return httpResponse{}, errHTTPRequestNotStarted
		}
	} else {
		select {
		case c.semaphore <- struct{}{}:
		case <-ctx.Done():
			return httpResponse{}, &httpRequestNotStartedError{cause: ctx.Err()}
		}
	}
	defer func() { <-c.semaphore }()
	var reader io.Reader
	if body != nil {
		reader = bytes.NewReader(body)
	}
	request, err := http.NewRequestWithContext(ctx, method, endpoint, reader)
	if err != nil {
		return httpResponse{}, err
	}
	for key, values := range headers {
		request.Header.Del(key)
		for _, value := range values {
			request.Header.Add(key, value)
		}
	}
	response, err := c.client.Do(request)
	if err != nil {
		if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
			return httpResponse{}, err
		}
		return httpResponse{}, errors.New("站点 HTTP 请求失败")
	}
	defer response.Body.Close()
	result := httpResponse{
		StatusCode:   response.StatusCode,
		ETag:         boundedResponseHeader(response.Header.Get("ETag")),
		LastModified: boundedResponseHeader(response.Header.Get("Last-Modified")),
	}
	if !readErrorBody && (response.StatusCode < 200 || response.StatusCode >= 300) {
		return result, nil
	}
	responseBody, err := io.ReadAll(io.LimitReader(response.Body, maxBodyBytes+1))
	if err != nil {
		return httpResponse{}, errors.New("读取站点响应失败")
	}
	if int64(len(responseBody)) > maxBodyBytes {
		return httpResponse{}, fmt.Errorf("站点响应超过 %d 字节限制", maxBodyBytes)
	}
	result.Body = responseBody
	return result, nil
}

func boundedResponseHeader(value string) string {
	value = strings.TrimSpace(value)
	if len(value) > 4096 || strings.ContainsAny(value, "\r\n") {
		return ""
	}
	return value
}

func (c *HTTPClient) dialContext(ctx context.Context, network, address string) (net.Conn, error) {
	host, port, err := net.SplitHostPort(address)
	if err != nil {
		return nil, errors.New("站点地址端口无效")
	}
	addresses, err := net.DefaultResolver.LookupIPAddr(ctx, host)
	if err != nil || len(addresses) == 0 {
		return nil, errors.New("解析站点地址失败")
	}
	for _, address := range addresses {
		if !c.allowPrivate && blockedIP(address.IP) {
			return nil, errors.New("站点地址解析到受保护网络")
		}
	}
	dialer := net.Dialer{Timeout: 10 * time.Second, KeepAlive: 30 * time.Second}
	return dialer.DialContext(ctx, network, net.JoinHostPort(addresses[0].IP.String(), port))
}

func validateHeaders(headers map[string]string) error {
	for key, value := range headers {
		key = strings.TrimSpace(key)
		if key == "" || strings.ContainsAny(key, "\r\n") || strings.EqualFold(key, "Host") {
			return errors.New("站点请求头名称无效")
		}
		if strings.ContainsAny(value, "\r\n") {
			return errors.New("站点请求头值无效")
		}
	}
	return nil
}

func sameHostRedirect(request *http.Request, via []*http.Request) error {
	if len(via) >= 3 {
		return errors.New("站点重定向次数超过限制")
	}
	if len(via) == 0 {
		return nil
	}
	previous := via[len(via)-1].URL
	if !strings.EqualFold(previous.Scheme, request.URL.Scheme) || !strings.EqualFold(previous.Host, request.URL.Host) {
		return errors.New("站点重定向到不同主机被拒绝")
	}
	return nil
}

func validateBaseURL(value string, allowPrivate bool) (*url.URL, error) {
	parsed, err := url.Parse(strings.TrimSpace(value))
	if err != nil || parsed.Host == "" || (parsed.Scheme != "http" && parsed.Scheme != "https") {
		return nil, errors.New("站点地址必须是 HTTP 或 HTTPS URL")
	}
	if parsed.User != nil {
		return nil, errors.New("站点地址不允许内嵌用户信息")
	}
	hostname := strings.TrimSuffix(strings.ToLower(parsed.Hostname()), ".")
	if hostname == "localhost" || strings.HasSuffix(hostname, ".localhost") {
		return nil, errors.New("站点地址不允许使用 localhost")
	}
	if !allowPrivate {
		if ip := net.ParseIP(hostname); ip != nil && blockedIP(ip) {
			return nil, errors.New("站点地址不允许使用受保护网络")
		}
	}
	return parsed, nil
}

func blockedIP(ip net.IP) bool {
	if ip.IsPrivate() || ip.IsLoopback() || ip.IsLinkLocalUnicast() ||
		ip.IsLinkLocalMulticast() || ip.IsUnspecified() || ip.IsMulticast() {
		return true
	}
	for _, network := range blockedSpecialNetworks {
		if network.Contains(ip) {
			return true
		}
	}
	return false
}

var blockedSpecialNetworks = func() []*net.IPNet {
	networks := make([]*net.IPNet, 0, 3)
	for _, raw := range []string{"0.0.0.0/8", "100.64.0.0/10", "198.18.0.0/15"} {
		_, network, err := net.ParseCIDR(raw)
		if err != nil {
			panic(err)
		}
		networks = append(networks, network)
	}
	return networks
}()

func resolveSiteURL(base *url.URL, raw string) (string, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return "", nil
	}
	parsed, err := url.Parse(raw)
	if err != nil {
		return "", errors.New("站点链接无效")
	}
	if parsed.IsAbs() && (parsed.Scheme != "http" && parsed.Scheme != "https") {
		return "", errors.New("站点链接协议不受支持")
	}
	resolved := base.ResolveReference(parsed)
	if !strings.EqualFold(base.Host, resolved.Host) || !strings.EqualFold(base.Scheme, resolved.Scheme) {
		return "", errors.New("站点链接跳转到不同主机被拒绝")
	}
	return resolved.String(), nil
}

// signInForm 提交登录表单并返回站点下发的 Cookie。
// NexusPHP 一类站点用「302 + Set-Cookie」表达登录成功，所以这里不跟随跳转，
// 而是单独挂一个 cookie jar 收集本次登录的 Cookie。
func (c *HTTPClient) signInForm(
	ctx context.Context,
	endpoint string,
	headers http.Header,
	form url.Values,
) ([]*http.Cookie, int, error) {
	if c == nil || c.client == nil {
		return nil, 0, errors.New("站点 HTTP 客户端未初始化")
	}
	if err := ctx.Err(); err != nil {
		return nil, 0, err
	}
	parsed, err := url.Parse(endpoint)
	if err != nil {
		return nil, 0, errors.New("站点登录地址无效")
	}
	encoded := form.Encode()
	if int64(len(encoded)) > c.maxRequestBytes {
		return nil, 0, errors.New("站点登录请求超过大小限制")
	}
	jar, err := cookiejar.New(nil)
	if err != nil {
		return nil, 0, errors.New("初始化 Cookie 容器失败")
	}
	clone := *c.client
	clone.Jar = jar
	clone.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }

	select {
	case c.semaphore <- struct{}{}:
	case <-ctx.Done():
		return nil, 0, ctx.Err()
	}
	defer func() { <-c.semaphore }()

	request, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, strings.NewReader(encoded))
	if err != nil {
		return nil, 0, errors.New("站点登录请求无效")
	}
	for key, values := range headers {
		for _, value := range values {
			request.Header.Add(key, value)
		}
	}
	request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	response, err := clone.Do(request)
	if err != nil {
		if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
			return nil, 0, err
		}
		return nil, 0, errors.New("站点登录请求失败")
	}
	defer response.Body.Close()
	_, _ = io.Copy(io.Discard, io.LimitReader(response.Body, c.maxBodyBytes))
	return jar.Cookies(parsed), response.StatusCode, nil
}
