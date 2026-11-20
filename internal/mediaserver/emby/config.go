package emby

import (
	"context"
	"errors"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// Client is a bounded, strongly typed Emby HTTP client.
type Client struct {
	baseURL       *url.URL
	playHost      *url.URL
	token         string
	authorization string
	timeout       time.Duration
	httpClient    *http.Client
	permits       chan struct{}
}

// NewClient validates config and creates an Emby client with at most four
// concurrent requests and four connections per host.
func NewClient(config Config) (*Client, error) {
	baseURL, err := parseBaseURL(config.BaseURL)
	if err != nil {
		return nil, newError(ErrorCodeInvalidConfig, 0, nil)
	}
	playHost := baseURL
	if strings.TrimSpace(config.PlayHost) != "" {
		playHost, err = parseBaseURL(config.PlayHost)
		if err != nil {
			return nil, newError(ErrorCodeInvalidConfig, 0, nil)
		}
	}

	token := strings.TrimSpace(config.Token)
	if token == "" || len(token) > 4096 || strings.ContainsAny(token, "\r\n") {
		return nil, newError(ErrorCodeInvalidConfig, 0, nil)
	}
	authorization := strings.TrimSpace(config.Authorization)
	if len(authorization) > 4096 || strings.ContainsAny(authorization, "\r\n") {
		return nil, newError(ErrorCodeInvalidConfig, 0, nil)
	}
	timeout := config.Timeout
	if timeout == 0 {
		timeout = defaultTimeout
	}
	if timeout < 0 {
		return nil, newError(ErrorCodeInvalidConfig, 0, nil)
	}

	client := &Client{
		baseURL:       baseURL,
		playHost:      playHost,
		token:         token,
		authorization: authorization,
		timeout:       timeout,
		permits:       make(chan struct{}, maxConcurrentRequests),
	}
	client.httpClient = &http.Client{
		Transport:     newTransport(timeout),
		CheckRedirect: client.checkRedirect,
	}
	return client, nil
}

// CloseIdleConnections closes idle connections owned by the client.
func (c *Client) CloseIdleConnections() {
	if c != nil && c.httpClient != nil {
		c.httpClient.CloseIdleConnections()
	}
}

func newTransport(timeout time.Duration) *http.Transport {
	dialer := &net.Dialer{Timeout: 5 * time.Second, KeepAlive: 30 * time.Second}
	return &http.Transport{
		Proxy:                  http.ProxyFromEnvironment,
		DialContext:            dialer.DialContext,
		ForceAttemptHTTP2:      true,
		MaxIdleConns:           maxConcurrentRequests * 2,
		MaxIdleConnsPerHost:    maxConcurrentRequests,
		MaxConnsPerHost:        maxConcurrentRequests,
		IdleConnTimeout:        60 * time.Second,
		TLSHandshakeTimeout:    5 * time.Second,
		ResponseHeaderTimeout:  timeout,
		ExpectContinueTimeout:  time.Second,
		MaxResponseHeaderBytes: maxHeaderBytes,
	}
}

func parseBaseURL(raw string) (*url.URL, error) {
	parsed, err := url.Parse(strings.TrimSpace(raw))
	if err != nil || parsed == nil || parsed.Opaque != "" || parsed.Host == "" || parsed.Hostname() == "" {
		return nil, errors.New("invalid URL")
	}
	if parsed.Scheme != "http" && parsed.Scheme != "https" {
		return nil, errors.New("invalid URL scheme")
	}
	if parsed.User != nil || parsed.RawQuery != "" || parsed.ForceQuery || parsed.Fragment != "" {
		return nil, errors.New("URL credentials, query, and fragment are forbidden")
	}
	if strings.ContainsAny(parsed.Host+parsed.Path, "\r\n\x00") {
		return nil, errors.New("invalid URL characters")
	}
	for _, segment := range strings.Split(parsed.Path, "/") {
		if segment == "." || segment == ".." {
			return nil, errors.New("URL path traversal is forbidden")
		}
	}
	parsed.Path = strings.TrimRight(parsed.Path, "/")
	parsed.RawPath = strings.TrimRight(parsed.RawPath, "/")
	return parsed, nil
}

func (c *Client) operationContext(ctx context.Context) (context.Context, context.CancelFunc, error) {
	if c == nil || c.httpClient == nil || ctx == nil {
		return nil, nil, newError(ErrorCodeInvalidArgument, 0, nil)
	}
	operationCtx, cancel := context.WithTimeout(ctx, c.timeout)
	return operationCtx, cancel, nil
}

func (c *Client) endpoint(suffix string) string {
	return strings.TrimRight(c.baseURL.String(), "/") + suffix
}

func (c *Client) checkRedirect(request *http.Request, via []*http.Request) error {
	if len(via) > 2 {
		return errTooManyRedirects
	}
	if len(via) == 0 {
		return nil
	}
	original := via[0]
	if original.URL.Scheme == "https" && request.URL.Scheme != "https" {
		return errHTTPSDowngrade
	}
	if request.URL.User != nil || originKey(request.URL) != originKey(original.URL) {
		return errCrossOriginRedirect
	}
	for key := range request.URL.Query() {
		if strings.EqualFold(key, "api_key") {
			return errCrossOriginRedirect
		}
	}
	if request.Method != original.Method {
		return errRedirectMethod
	}
	return nil
}

func originKey(value *url.URL) string {
	port := value.Port()
	if port == "" {
		switch strings.ToLower(value.Scheme) {
		case "http":
			port = "80"
		case "https":
			port = "443"
		}
	}
	return strings.ToLower(value.Scheme) + "|" + strings.ToLower(value.Hostname()) + "|" + port
}

func isRedirectError(err error) bool {
	return errors.Is(err, errCrossOriginRedirect) ||
		errors.Is(err, errHTTPSDowngrade) ||
		errors.Is(err, errTooManyRedirects) ||
		errors.Is(err, errRedirectMethod)
}
