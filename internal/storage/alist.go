package storage

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"
)

const (
	alistMaxBody     = 4 << 20
	defaultAlistName = "alist"
)

// AlistConfig 是 Alist 开放 API 的最小配置切片。
type AlistConfig struct {
	Name     string
	URL      string
	Username string
	Password string
	Timeout  time.Duration
	// Root 是映射到该存储名的远端根路径。
	Root string
}

// AlistProvider 通过 Alist 开放 API 提供远端存储浏览与管理。
type AlistProvider struct {
	client *http.Client
	config AlistConfig

	mu    sync.Mutex
	token string
}

// NewAlistProvider 创建 Alist 存储适配器。
func NewAlistProvider(config AlistConfig) (*AlistProvider, error) {
	name := strings.TrimSpace(config.Name)
	if name == "" {
		name = defaultAlistName
	}
	baseURL := strings.TrimRight(strings.TrimSpace(config.URL), "/")
	if baseURL == "" {
		return nil, ErrInvalidRequest
	}
	root := strings.TrimSpace(config.Root)
	if root == "" {
		root = "/"
	}
	if !strings.HasPrefix(root, "/") {
		root = "/" + root
	}
	timeout := config.Timeout
	if timeout <= 0 {
		timeout = 30 * time.Second
	}
	config.Name, config.URL = name, baseURL
	config.Root = strings.TrimRight(root, "/")
	if config.Root == "" {
		config.Root = "/"
	}
	return &AlistProvider{client: &http.Client{Timeout: timeout}, config: config}, nil
}

// Name 返回该适配器声明的 V2 存储名。
func (p *AlistProvider) Name() string {
	if p == nil {
		return ""
	}
	return p.config.Name
}

func (p *AlistProvider) require(ctx context.Context, storageName string) error {
	if p == nil || p.client == nil {
		return ErrUnsupportedStorage
	}
	if strings.TrimSpace(storageName) != p.config.Name {
		return ErrUnsupportedStorage
	}
	if ctx == nil {
		return ErrInvalidRequest
	}
	return ctx.Err()
}

type alistResponse struct {
	Code    int             `json:"code"`
	Message string          `json:"message"`
	Data    json.RawMessage `json:"data"`
}

func (p *AlistProvider) request(
	ctx context.Context,
	endpoint string,
	payload map[string]any,
	output any,
) error {
	body, err := json.Marshal(payload)
	if err != nil {
		return ErrInvalidRequest
	}
	request, err := http.NewRequestWithContext(
		ctx, http.MethodPost, p.config.URL+endpoint, bytes.NewReader(body),
	)
	if err != nil {
		return ErrInvalidRequest
	}
	request.Header.Set("Content-Type", "application/json")
	if token := p.loadToken(); token != "" {
		request.Header.Set("Authorization", token)
	}
	response, err := p.client.Do(request)
	if err != nil {
		return ErrOperationFailed
	}
	defer func() { _ = response.Body.Close() }()
	payloadBytes, err := io.ReadAll(io.LimitReader(response.Body, alistMaxBody))
	if err != nil {
		return ErrOperationFailed
	}
	if response.StatusCode == http.StatusUnauthorized {
		p.storeToken("")
		return ErrOperationFailed
	}
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return ErrOperationFailed
	}
	var decoded alistResponse
	if err := json.Unmarshal(payloadBytes, &decoded); err != nil {
		return ErrOperationFailed
	}
	if decoded.Code != 200 {
		return ErrOperationFailed
	}
	if output == nil || len(decoded.Data) == 0 {
		return nil
	}
	if err := json.Unmarshal(decoded.Data, output); err != nil {
		return ErrOperationFailed
	}
	return nil
}

func (p *AlistProvider) loadToken() string {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.token
}

func (p *AlistProvider) storeToken(token string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.token = token
}

func (p *AlistProvider) ensureToken(ctx context.Context) error {
	if p.loadToken() != "" {
		return nil
	}
	if strings.TrimSpace(p.config.Username) == "" && strings.TrimSpace(p.config.Password) == "" {
		return nil
	}
	var data struct {
		Token string `json:"token"`
	}
	if err := p.request(ctx, "/api/auth/login", map[string]any{
		"username": p.config.Username, "password": p.config.Password,
	}, &data); err != nil {
		return err
	}
	if strings.TrimSpace(data.Token) == "" {
		return ErrOperationFailed
	}
	p.storeToken(data.Token)
	return nil
}

var _ Provider = (*AlistProvider)(nil)
