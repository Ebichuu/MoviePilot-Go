package storage

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"path"
	"strings"
	"time"
)

const (
	rcloneMaxBody     = 4 << 20
	defaultRcloneName = "rclone"
)

// RcloneConfig 是 rclone RC（HTTP）接口的最小配置切片。
type RcloneConfig struct {
	// Name 是 V2 存储名（前端 FileItem.storage 字段）。
	Name string
	// URL 是 rclone RC 地址，例如 http://127.0.0.1:5572。
	URL string
	// FileSystem 是 rclone 远端前缀，例如 `gdrive:media`。
	FileSystem string
	Username   string
	Password   string
	Timeout    time.Duration
}

// RcloneProvider 通过 rclone RC API 提供远端存储浏览与管理。
type RcloneProvider struct {
	client *http.Client
	config RcloneConfig
}

// NewRcloneProvider 创建 rclone 存储适配器。
func NewRcloneProvider(config RcloneConfig) (*RcloneProvider, error) {
	name := strings.TrimSpace(config.Name)
	if name == "" {
		name = defaultRcloneName
	}
	rcURL := strings.TrimRight(strings.TrimSpace(config.URL), "/")
	fileSystem := strings.TrimSpace(config.FileSystem)
	if rcURL == "" || fileSystem == "" {
		return nil, ErrInvalidRequest
	}
	timeout := config.Timeout
	if timeout <= 0 {
		timeout = 30 * time.Second
	}
	config.Name, config.URL, config.FileSystem = name, rcURL, fileSystem
	return &RcloneProvider{client: &http.Client{Timeout: timeout}, config: config}, nil
}

// Name 返回该适配器声明的 V2 存储名。
func (p *RcloneProvider) Name() string {
	if p == nil {
		return ""
	}
	return p.config.Name
}

func (p *RcloneProvider) require(ctx context.Context, storageName string) error {
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

func (p *RcloneProvider) call(ctx context.Context, method string, payload map[string]any, output any) error {
	body, err := json.Marshal(payload)
	if err != nil {
		return ErrInvalidRequest
	}
	request, err := http.NewRequestWithContext(
		ctx, http.MethodPost, p.config.URL+method, bytes.NewReader(body),
	)
	if err != nil {
		return ErrInvalidRequest
	}
	request.Header.Set("Content-Type", "application/json")
	if p.config.Username != "" || p.config.Password != "" {
		request.SetBasicAuth(p.config.Username, p.config.Password)
	}
	response, err := p.client.Do(request)
	if err != nil {
		return ErrOperationFailed
	}
	defer func() { _ = response.Body.Close() }()
	payloadBytes, err := io.ReadAll(io.LimitReader(response.Body, rcloneMaxBody))
	if err != nil {
		return ErrOperationFailed
	}
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return ErrOperationFailed
	}
	if output == nil {
		return nil
	}
	if err := json.Unmarshal(payloadBytes, output); err != nil {
		return ErrOperationFailed
	}
	return nil
}

type rcloneEntry struct {
	Path     string `json:"Path"`
	Name     string `json:"Name"`
	Size     int64  `json:"Size"`
	MimeType string `json:"MimeType"`
	ModTime  string `json:"ModTime"`
	IsDir    bool   `json:"IsDir"`
}

type rcloneListResponse struct {
	List []rcloneEntry `json:"list"`
}

func rcloneJoin(base, name string) string {
	name = strings.Trim(strings.TrimSpace(name), "/")
	if name == "" {
		return strings.Trim(base, "/")
	}
	if strings.TrimSpace(base) == "" {
		return name
	}
	return path.Join(base, name)
}

// List 通过 `/operations/list` 读取远端目录。
func (p *RcloneProvider) List(
	ctx context.Context,
	storageName, requested, sortKey, keyword string,
) ([]Item, error) {
	if err := p.require(ctx, storageName); err != nil {
		return nil, err
	}
	var response rcloneListResponse
	remote := strings.Trim(strings.TrimSpace(requested), "/")
	if err := p.call(ctx, "/operations/list", map[string]any{"fs": p.config.FileSystem, "remote": remote}, &response); err != nil {
		return nil, err
	}
	items := make([]Item, 0, len(response.List))
	for _, entry := range response.List {
		itemType := "file"
		if entry.IsDir {
			itemType = "dir"
		}
		item := Item{
			Type: itemType, Name: entry.Name,
			Basename: strings.TrimSuffix(entry.Name, path.Ext(entry.Name)),
			Children: []Item{}, FileID: entry.Name,
			// 路径与请求体里的 remote 用同一个去斜杠形式，保持与本地/Alist 适配器一致。
			Path:    rcloneJoin(remote, entry.Name),
			Storage: p.config.Name,
		}
		if itemType == "file" {
			if extension := strings.TrimPrefix(path.Ext(entry.Name), "."); extension != "" {
				item.Extension = extension
			}
			size := entry.Size
			item.Size = &size
		}
		if modified, err := time.Parse(time.RFC3339Nano, entry.ModTime); err == nil {
			seconds := float64(modified.Unix())
			item.ModifyTime = &seconds
		}
		items = append(items, item)
	}
	if keyword = strings.TrimSpace(keyword); keyword != "" {
		filtered := items[:0]
		for _, item := range items {
			if globMatch(strings.ToLower(keyword), strings.ToLower(item.Name)) {
				filtered = append(filtered, item)
			}
		}
		items = filtered
	}
	if strings.TrimSpace(sortKey) == "name" {
		sortItemsByName(items)
	} else {
		sortItemsByModifyTime(items)
	}
	return items, nil
}

var _ Provider = (*RcloneProvider)(nil)
