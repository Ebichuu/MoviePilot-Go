package cookiecloud

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
)

const maxResponseBytes = 8 << 20

// ignoreCookies 复刻 V2 `CookieCloudHelper._ignore_cookies`。
var ignoreCookies = map[string]struct{}{
	"CookieAutoDeleteBrowsingDataCleanup": {},
	"CookieAutoDeleteCleaningDiscarded":   {},
}

var (
	// ErrConfig 对应 V2「CookieCloud参数不正确」。
	ErrConfig = errors.New("CookieCloud参数不正确")
	// ErrRequest 对应 V2 的下载/请求失败。
	ErrRequest = errors.New("CookieCloud请求失败，请检查服务器地址、用户KEY及加密密码是否正确")
	// ErrNoCipher 对应 V2「未获取到cookie密文」。
	ErrNoCipher = errors.New("未获取到cookie密文")
	// ErrEmpty 对应 V2「cookie解密为空」。
	ErrEmpty = errors.New("cookie解密为空")
)

// Config 是 V2 CookieCloud 配置切片（COOKIECLOUD_*）。
type Config struct {
	Server      string
	Key         string
	Password    string
	EnableLocal bool
	LocalDir    string
}

// Helper 复刻 V2 `CookieCloudHelper`（app/helper/cookiecloud.py:12–132）。
type Helper struct {
	config     Config
	httpClient *http.Client
	readFile   func(string) ([]byte, error)
}

// NewHelper 创建 CookieCloud 下载器。
func NewHelper(config Config, httpClient *http.Client) *Helper {
	if httpClient == nil {
		httpClient = http.DefaultClient
	}
	return &Helper{config: config, httpClient: httpClient, readFile: os.ReadFile}
}

type cookieEntry struct {
	Domain string `json:"domain"`
	Name   string `json:"name"`
	Value  string `json:"value"`
}

// Download 复刻 V2 `download`：返回「域名 → Cookie 字符串」，按域名最后两级
// 归组，跳过纯 cf_clearance 组与内置忽略项。
func (h *Helper) Download(ctx context.Context) (map[string]string, error) {
	server := strings.TrimRight(strings.TrimSpace(h.config.Server), "/")
	key := strings.TrimSpace(h.config.Key)
	password := strings.TrimSpace(h.config.Password)
	if (server == "" && !h.config.EnableLocal) || key == "" || password == "" {
		return nil, ErrConfig
	}

	payload, err := h.loadEncryptedPayload(ctx, server, key)
	if err != nil {
		return nil, err
	}
	if len(payload) == 0 {
		return nil, ErrRequest
	}
	encrypted, _ := payload["encrypted"].(string)
	if strings.TrimSpace(encrypted) == "" {
		return nil, ErrNoCipher
	}
	decrypted, err := Decrypt(encrypted, CryptKey(key, password))
	if err != nil {
		return nil, err
	}
	var decoded map[string]any
	if err := json.Unmarshal(decrypted, &decoded); err != nil || len(decoded) == 0 {
		return nil, ErrEmpty
	}

	contents, ok := decoded["cookie_data"].(map[string]any)
	if !ok || len(contents) == 0 {
		contents = decoded
	}

	groups := make(map[string][]cookieEntry)
	order := make([]string, 0, len(contents))
	for _, rawCookies := range contents {
		list, ok := rawCookies.([]any)
		if !ok {
			continue
		}
		for _, raw := range list {
			entry, ok := decodeCookieEntry(raw)
			if !ok {
				continue
			}
			domainKey := URLDomain(entry.Domain)
			if domainKey == "" {
				continue
			}
			if _, exists := groups[domainKey]; !exists {
				order = append(order, domainKey)
			}
			groups[domainKey] = append(groups[domainKey], entry)
		}
	}

	result := make(map[string]string, len(groups))
	for _, domain := range order {
		entries := groups[domain]
		if len(entries) == 0 {
			continue
		}
		cloudflareOnly := true
		for _, entry := range entries {
			if entry.Name != "cf_clearance" {
				cloudflareOnly = false
				break
			}
		}
		if cloudflareOnly {
			continue
		}
		parts := make([]string, 0, len(entries))
		for _, entry := range entries {
			if entry.Name == "" {
				continue
			}
			if _, ignored := ignoreCookies[entry.Name]; ignored {
				continue
			}
			parts = append(parts, entry.Name+"="+entry.Value)
		}
		result[domain] = strings.Join(parts, ";")
	}
	return result, nil
}

func (h *Helper) loadEncryptedPayload(ctx context.Context, server, key string) (map[string]any, error) {
	if h.config.EnableLocal {
		data, err := h.readFile(filepath.Join(h.config.LocalDir, key+".json"))
		if err != nil {
			return nil, ErrRequest
		}
		var payload map[string]any
		if err := json.Unmarshal(data, &payload); err != nil {
			return nil, ErrRequest
		}
		return payload, nil
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, server+"/get/"+url.PathEscape(key), nil)
	if err != nil {
		return nil, ErrRequest
	}
	request.Header.Set("Accept", "application/json")
	response, err := h.httpClient.Do(request)
	if err != nil {
		return nil, ErrRequest
	}
	defer func() { _ = response.Body.Close() }()
	if response.StatusCode != http.StatusOK {
		return nil, ErrRequest
	}
	body, err := io.ReadAll(io.LimitReader(response.Body, maxResponseBytes))
	if err != nil {
		return nil, ErrRequest
	}
	var payload map[string]any
	if err := json.Unmarshal(body, &payload); err != nil {
		return nil, ErrRequest
	}
	return payload, nil
}

func decodeCookieEntry(raw any) (cookieEntry, bool) {
	object, ok := raw.(map[string]any)
	if !ok {
		return cookieEntry{}, false
	}
	entry := cookieEntry{}
	if value, ok := object["domain"].(string); ok {
		entry.Domain = value
	}
	if value, ok := object["name"].(string); ok {
		entry.Name = value
	}
	if value, ok := object["value"].(string); ok {
		entry.Value = value
	}
	return entry, true
}
