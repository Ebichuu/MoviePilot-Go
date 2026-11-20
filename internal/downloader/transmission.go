package downloader

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"time"
)

const transmissionSessionHeader = "X-Transmission-Session-Id"

// TransmissionConfig 保存 Transmission RPC 连接信息；密码不得写入日志。
type TransmissionConfig struct {
	URL             string
	Username        string
	Password        string
	Timeout         time.Duration
	MaxConnsPerHost int
}

// Transmission 使用 Transmission RPC 实现 X01 下载器协议的最小闭环。
type Transmission struct {
	client      *http.Client
	baseURL     string
	username    string
	password    string
	queryPermit chan struct{}

	mu        sync.Mutex
	sessionID string
}

// NewTransmission 创建带连接和超时上限的 Transmission 适配器。
func NewTransmission(cfg TransmissionConfig) (*Transmission, error) {
	baseURL := strings.TrimRight(strings.TrimSpace(cfg.URL), "/")
	if baseURL == "" {
		return nil, errors.New("Transmission URL 不能为空")
	}
	if cfg.Timeout <= 0 {
		cfg.Timeout = 30 * time.Second
	}
	if cfg.MaxConnsPerHost < 1 || cfg.MaxConnsPerHost > 32 {
		cfg.MaxConnsPerHost = 4
	}
	transport := &http.Transport{
		Proxy: http.ProxyFromEnvironment, MaxIdleConns: cfg.MaxConnsPerHost * 2,
		MaxIdleConnsPerHost: cfg.MaxConnsPerHost, MaxConnsPerHost: cfg.MaxConnsPerHost,
		IdleConnTimeout: 60 * time.Second, TLSHandshakeTimeout: 10 * time.Second,
		ResponseHeaderTimeout: cfg.Timeout,
	}
	return &Transmission{
		client:      &http.Client{Transport: transport, Timeout: cfg.Timeout},
		baseURL:     baseURL,
		username:    cfg.Username,
		password:    cfg.Password,
		queryPermit: make(chan struct{}, 1),
	}, nil
}

// CloseIdleConnections 释放空闲连接。
func (t *Transmission) CloseIdleConnections() {
	if t == nil || t.client == nil {
		return
	}
	if transport, ok := t.client.Transport.(*http.Transport); ok {
		transport.CloseIdleConnections()
	}
}

type transmissionRequest struct {
	Method    string `json:"method"`
	Arguments any    `json:"arguments,omitempty"`
}

type transmissionResponse struct {
	Result    string          `json:"result"`
	Arguments json.RawMessage `json:"arguments"`
}

func (t *Transmission) call(ctx context.Context, method string, arguments any) (json.RawMessage, error) {
	if ctx == nil {
		return nil, errors.New("Transmission 请求上下文无效")
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if t == nil || t.client == nil || t.queryPermit == nil {
		return nil, errors.New("Transmission 客户端未初始化")
	}
	select {
	case t.queryPermit <- struct{}{}:
		defer func() { <-t.queryPermit }()
	case <-ctx.Done():
		return nil, ctx.Err()
	}
	payload, err := json.Marshal(transmissionRequest{Method: method, Arguments: arguments})
	if err != nil {
		return nil, errors.New("Transmission 请求编码失败")
	}
	for attempt := 0; attempt < 2; attempt++ {
		response, err := t.do(ctx, payload)
		if err != nil {
			return nil, err
		}
		if response.StatusCode == http.StatusConflict {
			t.storeSessionID(response.Header.Get(transmissionSessionHeader))
			_ = response.Body.Close()
			if attempt == 0 {
				continue
			}
			return nil, errors.New("Transmission 会话协商失败")
		}
		if response.StatusCode < 200 || response.StatusCode >= 300 {
			_ = response.Body.Close()
			return nil, fmt.Errorf("Transmission 返回状态 %d", response.StatusCode)
		}
		var decoded transmissionResponse
		decodeErr := json.NewDecoder(response.Body).Decode(&decoded)
		_ = response.Body.Close()
		if decodeErr != nil {
			return nil, errors.New("Transmission 响应解析失败")
		}
		if decoded.Result != "success" {
			return nil, errors.New("Transmission 操作失败")
		}
		return decoded.Arguments, nil
	}
	return nil, errors.New("Transmission 会话协商失败")
}

func (t *Transmission) do(ctx context.Context, payload []byte) (*http.Response, error) {
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, t.baseURL, bytes.NewReader(payload))
	if err != nil {
		return nil, errors.New("Transmission 请求无效")
	}
	request.Header.Set("Content-Type", "application/json")
	if sessionID := t.loadSessionID(); sessionID != "" {
		request.Header.Set(transmissionSessionHeader, sessionID)
	}
	if t.username != "" || t.password != "" {
		request.SetBasicAuth(t.username, t.password)
	}
	return t.client.Do(request)
}

func (t *Transmission) loadSessionID() string {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.sessionID
}

func (t *Transmission) storeSessionID(value string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if value != "" {
		t.sessionID = value
	}
}
