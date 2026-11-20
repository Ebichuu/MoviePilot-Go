package downloader

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"time"
)

// RTorrentConfig 保存 rTorrent XML-RPC（HTTP）连接信息。
type RTorrentConfig struct {
	URL             string
	Username        string
	Password        string
	Timeout         time.Duration
	MaxConnsPerHost int
}

// RTorrent 使用 rTorrent XML-RPC 实现 X01 下载器协议的最小闭环。
type RTorrent struct {
	client      *http.Client
	endpoint    string
	username    string
	password    string
	queryPermit chan struct{}
}

// NewRTorrent 创建带连接和超时上限的 rTorrent 适配器。
func NewRTorrent(cfg RTorrentConfig) (*RTorrent, error) {
	endpoint := strings.TrimSpace(cfg.URL)
	if endpoint == "" {
		return nil, errors.New("rTorrent URL 不能为空")
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
	client := &http.Client{Transport: transport, Timeout: cfg.Timeout}
	return &RTorrent{
		client:      client,
		endpoint:    endpoint,
		username:    cfg.Username,
		password:    cfg.Password,
		queryPermit: make(chan struct{}, 1),
	}, nil
}

// CloseIdleConnections 释放空闲连接。
func (r *RTorrent) CloseIdleConnections() {
	if r == nil || r.client == nil {
		return
	}
	if transport, ok := r.client.Transport.(*http.Transport); ok {
		transport.CloseIdleConnections()
	}
}

func (r *RTorrent) call(ctx context.Context, method string, params []string) (xmlrpcValue, error) {
	if r == nil || r.client == nil || r.queryPermit == nil {
		return xmlrpcValue{}, errors.New("rTorrent 客户端未初始化")
	}
	select {
	case r.queryPermit <- struct{}{}:
		defer func() { <-r.queryPermit }()
	case <-ctx.Done():
		return xmlrpcValue{}, ctx.Err()
	}
	return xmlrpcCall(ctx, r.client, r.endpoint, method, params, r.username, r.password)
}

// rTorrentTorrentRow 是 `d.multicall2` 返回的一行（顺序与请求字段一致）。
type rTorrentTorrentRow struct {
	Hash      string
	Label     string
	Name      string
	State     int64
	Complete  int64
	BytesDone int64
	SizeBytes int64
	DownRate  int64
	UpRate    int64
	Directory string
}

func rTorrentMulticallParams() []string {
	return []string{
		"", "main",
		"d.hash=", "d.custom1=", "d.name=", "d.state=", "d.complete=",
		"d.bytes_done=", "d.size_bytes=", "d.down.rate=", "d.up.rate=", "d.directory=",
	}
}

func (r *RTorrent) multicall(ctx context.Context) ([]rTorrentTorrentRow, error) {
	value, err := r.call(ctx, "d.multicall2", rTorrentMulticallParams())
	if err != nil {
		return nil, err
	}
	rows := make([]rTorrentTorrentRow, 0, len(value.Array))
	for _, entry := range value.Array {
		if len(entry.Array) < 10 {
			continue
		}
		row := rTorrentTorrentRow{
			Hash:      xmlrpcText(entry.Array[0]),
			Label:     xmlrpcText(entry.Array[1]),
			Name:      xmlrpcText(entry.Array[2]),
			State:     xmlrpcInt(entry.Array[3]),
			Complete:  xmlrpcInt(entry.Array[4]),
			BytesDone: xmlrpcInt(entry.Array[5]),
			SizeBytes: xmlrpcInt(entry.Array[6]),
			DownRate:  xmlrpcInt(entry.Array[7]),
			UpRate:    xmlrpcInt(entry.Array[8]),
			Directory: xmlrpcText(entry.Array[9]),
		}
		rows = append(rows, row)
	}
	return rows, nil
}

func xmlrpcText(value xmlrpcValue) string {
	if value.String != nil {
		return *value.String
	}
	return ""
}

func xmlrpcInt(value xmlrpcValue) int64 {
	if value.Int != nil {
		return *value.Int
	}
	if value.Double != nil {
		return int64(*value.Double)
	}
	return 0
}
