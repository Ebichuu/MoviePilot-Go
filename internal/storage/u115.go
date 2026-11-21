package storage

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"mime"
	"net/http"
	"net/url"
	"path"
	"strconv"
	"strings"
	"sync"
	"time"
)

const (
	defaultU115Name            = "u115"
	defaultU115BaseURL         = "https://proapi.115.com"
	defaultU115PassportURL     = "https://passportapi.115.com"
	defaultU115QRCodeStatusURL = "https://qrcodeapi.115.com"
	defaultU115AuthServerURL   = "https://movie-pilot.org"
	defaultU115AppID           = "100197847"
	defaultU115UserAgent       = "W115Storage/2.0"
	defaultU115Timeout         = 30 * time.Second
	u115MaxBody                = 8 << 20
	u115MaxListItems           = 10_000
)

// U115Config 是 115 网盘 Open API 适配器的配置切片。
type U115Config struct {
	Name            string
	BaseURL         string
	PassportURL     string
	QRCodeStatusURL string
	AuthServerURL   string
	AppID           string
	AccessToken     string
	RefreshToken    string
	ExpiresIn       int64
	RefreshTime     int64
	Timeout         time.Duration
	OnConfigChange  func(context.Context, map[string]any) error
}

type u115AuthState struct {
	OAuthState   string
	CodeVerifier string
	UID          string
	Time         int64
	Sign         string
}

// U115Provider 基于 115 网盘 Open API 提供扫码/OAuth 登录、文件浏览、目录管理与只读下载流。
type U115Provider struct {
	client *http.Client

	mu        sync.Mutex
	config    U115Config
	authState u115AuthState
	now       func() time.Time
}

// NewU115Provider 创建 115 网盘存储适配器。
func NewU115Provider(cfg U115Config) (*U115Provider, error) {
	cfg.Name = strings.TrimSpace(cfg.Name)
	if cfg.Name == "" {
		cfg.Name = defaultU115Name
	}
	cfg.BaseURL = strings.TrimRight(strings.TrimSpace(cfg.BaseURL), "/")
	if cfg.BaseURL == "" {
		cfg.BaseURL = defaultU115BaseURL
	}
	cfg.PassportURL = strings.TrimRight(strings.TrimSpace(cfg.PassportURL), "/")
	if cfg.PassportURL == "" {
		cfg.PassportURL = defaultU115PassportURL
	}
	cfg.QRCodeStatusURL = strings.TrimRight(strings.TrimSpace(cfg.QRCodeStatusURL), "/")
	if cfg.QRCodeStatusURL == "" {
		cfg.QRCodeStatusURL = defaultU115QRCodeStatusURL
	}
	cfg.AuthServerURL = strings.TrimRight(strings.TrimSpace(cfg.AuthServerURL), "/")
	if cfg.AuthServerURL == "" {
		cfg.AuthServerURL = defaultU115AuthServerURL
	}
	cfg.AppID = strings.TrimSpace(cfg.AppID)
	if cfg.AppID == "" {
		cfg.AppID = defaultU115AppID
	}
	if cfg.Timeout <= 0 {
		cfg.Timeout = defaultU115Timeout
	}
	return &U115Provider{
		client: &http.Client{Timeout: cfg.Timeout},
		config: cfg,
		now:    time.Now,
	}, nil
}

// Name 返回该适配器声明的 V2 存储名。
func (p *U115Provider) Name() string {
	if p == nil {
		return ""
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.config.Name
}

func (p *U115Provider) require(ctx context.Context, storageName string) error {
	if p == nil || p.client == nil {
		return ErrUnsupportedStorage
	}
	if ctx == nil {
		return ErrInvalidRequest
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	p.mu.Lock()
	name := p.config.Name
	p.mu.Unlock()
	if strings.TrimSpace(storageName) != name {
		return ErrUnsupportedStorage
	}
	return nil
}

func (p *U115Provider) ensureAccessToken(ctx context.Context) (string, error) {
	p.mu.Lock()
	accessToken := strings.TrimSpace(p.config.AccessToken)
	refreshToken := strings.TrimSpace(p.config.RefreshToken)
	expiresIn := p.config.ExpiresIn
	refreshTime := p.config.RefreshTime
	passportURL := p.config.PassportURL
	nowUnix := p.now().Unix()
	p.mu.Unlock()

	if accessToken == "" && refreshToken == "" {
		return "", ErrOperationFailed
	}
	needsRefresh := accessToken == "" || (expiresIn > 0 && refreshTime > 0 && refreshTime+expiresIn <= nowUnix)
	if needsRefresh && refreshToken != "" {
		tokens, err := p.refreshAccessToken(ctx, passportURL, refreshToken)
		if err != nil {
			return "", ErrOperationFailed
		}
		if err := p.applyTokens(ctx, tokens); err != nil {
			return "", err
		}
		p.mu.Lock()
		accessToken = strings.TrimSpace(p.config.AccessToken)
		p.mu.Unlock()
	}
	if accessToken == "" {
		return "", ErrOperationFailed
	}
	return accessToken, nil
}

type u115TokenData struct {
	AccessToken  string `json:"access_token"`
	RefreshToken string `json:"refresh_token"`
	ExpiresIn    int64  `json:"expires_in"`
}

func (p *U115Provider) refreshAccessToken(ctx context.Context, passportURL, refreshToken string) (u115TokenData, error) {
	form := url.Values{}
	form.Set("refresh_token", refreshToken)
	var resp struct {
		Code    int           `json:"code"`
		Message string        `json:"message"`
		Data    u115TokenData `json:"data"`
	}
	if err := p.doForm(ctx, http.MethodPost, passportURL+"/open/refreshToken", "", form, &resp); err != nil {
		return u115TokenData{}, err
	}
	if resp.Code != 0 || strings.TrimSpace(resp.Data.AccessToken) == "" {
		return u115TokenData{}, fmt.Errorf("u115 refresh error: %d %s", resp.Code, resp.Message)
	}
	return resp.Data, nil
}

func (p *U115Provider) applyTokens(ctx context.Context, data u115TokenData) error {
	p.mu.Lock()
	p.config.AccessToken = strings.TrimSpace(data.AccessToken)
	if strings.TrimSpace(data.RefreshToken) != "" {
		p.config.RefreshToken = strings.TrimSpace(data.RefreshToken)
	}
	if data.ExpiresIn > 0 {
		p.config.ExpiresIn = data.ExpiresIn
	}
	p.config.RefreshTime = p.now().Unix()
	persisted := p.exportConfigLocked()
	onChange := p.config.OnConfigChange
	p.mu.Unlock()

	if onChange != nil {
		return onChange(ctx, persisted)
	}
	return nil
}

func (p *U115Provider) doForm(
	ctx context.Context,
	method, fullURL, bearer string,
	values url.Values,
	out any,
) error {
	var bodyReader io.Reader
	targetURL := fullURL
	if method == http.MethodGet && len(values) > 0 {
		sep := "?"
		if strings.Contains(targetURL, "?") {
			sep = "&"
		}
		targetURL += sep + values.Encode()
	} else if method != http.MethodGet && values != nil {
		bodyReader = strings.NewReader(values.Encode())
	}
	req, err := http.NewRequestWithContext(ctx, method, targetURL, bodyReader)
	if err != nil {
		return ErrInvalidRequest
	}
	req.Header.Set("User-Agent", defaultU115UserAgent)
	if method != http.MethodGet {
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	}
	if bearer != "" {
		req.Header.Set("Authorization", "Bearer "+bearer)
	}
	resp, err := p.client.Do(req)
	if err != nil {
		return ErrOperationFailed
	}
	defer func() { _ = resp.Body.Close() }()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, u115MaxBody))
	if err != nil {
		return ErrOperationFailed
	}
	if resp.StatusCode >= 400 {
		return ErrOperationFailed
	}
	if out != nil && len(raw) > 0 {
		if err := json.Unmarshal(raw, out); err != nil {
			return ErrOperationFailed
		}
	}
	return nil
}

type u115ItemInfo struct {
	FileID       json.RawMessage `json:"file_id"`
	FileName     string          `json:"file_name"`
	FileCategory string          `json:"file_category"`
	PickCode     string          `json:"pick_code"`
	SizeByte     int64           `json:"size_byte"`
	UTime        json.RawMessage `json:"utime"`
}

func jsonRawToString(raw json.RawMessage) string {
	trimmed := strings.TrimSpace(string(raw))
	if trimmed == "" || trimmed == "null" {
		return ""
	}
	if unquoted, err := strconv.Unquote(trimmed); err == nil {
		return unquoted
	}
	return trimmed
}

func jsonRawToFloat64(raw json.RawMessage) float64 {
	s := jsonRawToString(raw)
	if s == "" {
		return 0
	}
	v, _ := strconv.ParseFloat(s, 64)
	return v
}

func (p *U115Provider) resolveByPath(ctx context.Context, accessToken, filePath string) (u115ItemInfo, error) {
	if filePath == "/" {
		return u115ItemInfo{
			FileID:       json.RawMessage(`"0"`),
			FileName:     "/",
			FileCategory: "0",
		}, nil
	}
	p.mu.Lock()
	baseURL := p.config.BaseURL
	p.mu.Unlock()

	form := url.Values{}
	form.Set("path", filePath)
	var resp struct {
		State   bool         `json:"state"`
		Code    int          `json:"code"`
		Message string       `json:"message"`
		Data    u115ItemInfo `json:"data"`
	}
	if err := p.doForm(ctx, http.MethodPost, baseURL+"/open/folder/get_info", accessToken, form, &resp); err != nil {
		return u115ItemInfo{}, err
	}
	if (resp.Code != 0 && resp.Code != 20004) || jsonRawToString(resp.Data.FileID) == "" {
		return u115ItemInfo{}, ErrOperationFailed
	}
	return resp.Data, nil
}

// List 浏览 115 网盘目录（自动分页、关键词过滤与排序）。
func (p *U115Provider) List(ctx context.Context, storageName, requested, sortKey, keyword string) ([]Item, error) {
	if err := p.require(ctx, storageName); err != nil {
		return nil, err
	}
	accessToken, err := p.ensureAccessToken(ctx)
	if err != nil {
		return nil, ErrOperationFailed
	}
	cleanPath, err := normalizeVirtualPath(requested)
	if err != nil {
		return nil, err
	}
	target, err := p.resolveByPath(ctx, accessToken, cleanPath)
	if err != nil {
		return nil, err
	}
	cid := jsonRawToString(target.FileID)
	if cid == "" {
		cid = "0"
	}
	if target.FileCategory == "1" {
		mod := jsonRawToFloat64(target.UTime)
		size := target.SizeByte
		ext := path.Ext(target.FileName)
		return []Item{{
			Type:       "file",
			Name:       target.FileName,
			Basename:   strings.TrimSuffix(target.FileName, ext),
			Extension:  strings.TrimPrefix(ext, "."),
			Size:       &size,
			ModifyTime: &mod,
			Children:   []Item{},
			FileID:     cid,
			Path:       cleanPath,
			Storage:    p.Name(),
		}}, nil
	}

	p.mu.Lock()
	baseURL := p.config.BaseURL
	p.mu.Unlock()

	items := make([]Item, 0, 100)
	offset := 0
	for {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		params := url.Values{}
		params.Set("cid", cid)
		params.Set("limit", "1000")
		params.Set("offset", strconv.Itoa(offset))
		params.Set("cur", "true")
		params.Set("show_dir", "1")

		var resp struct {
			State   bool   `json:"state"`
			Code    int    `json:"code"`
			Message string `json:"message"`
			Data    []struct {
				FID json.RawMessage `json:"fid"`
				FN  string          `json:"fn"`
				FC  string          `json:"fc"`
				FS  int64           `json:"fs"`
				ICO string          `json:"ico"`
				UPT json.RawMessage `json:"upt"`
				PC  string          `json:"pc"`
			} `json:"data"`
		}
		if err := p.doForm(ctx, http.MethodGet, baseURL+"/open/ufile/files", accessToken, params, &resp); err != nil {
			return nil, err
		}
		if resp.Code != 0 && resp.Code != 20004 {
			return nil, ErrOperationFailed
		}
		parentPrefix := cleanPath
		if !strings.HasSuffix(parentPrefix, "/") {
			parentPrefix += "/"
		}
		for _, entry := range resp.Data {
			mod := jsonRawToFloat64(entry.UPT)
			item := Item{
				Name:         entry.FN,
				Children:     []Item{},
				FileID:       jsonRawToString(entry.FID),
				ParentFileID: cid,
				ModifyTime:   &mod,
				Storage:      p.Name(),
			}
			if entry.FC == "0" {
				item.Type = "dir"
				item.Basename = entry.FN
				item.Path = parentPrefix + entry.FN + "/"
			} else {
				item.Type = "file"
				ext := path.Ext(entry.FN)
				item.Basename = strings.TrimSuffix(entry.FN, ext)
				item.Extension = entry.ICO
				if item.Extension == "" {
					item.Extension = strings.TrimPrefix(ext, ".")
				}
				size := entry.FS
				item.Size = &size
				item.Path = parentPrefix + entry.FN
			}
			items = append(items, item)
		}
		if len(resp.Data) < 1000 || len(items) >= u115MaxListItems {
			break
		}
		offset += len(resp.Data)
	}

	if kw := strings.ToLower(strings.TrimSpace(keyword)); kw != "" {
		filtered := items[:0]
		for _, item := range items {
			if globMatch(kw, strings.ToLower(item.Name)) {
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

// Mkdir 在 115 网盘创建目录（若目录已存在 code=20004 视作幂等成功）。
func (p *U115Provider) Mkdir(ctx context.Context, storageName, requested, name string) (bool, error) {
	if err := p.require(ctx, storageName); err != nil {
		return false, err
	}
	validName, err := validateSMBSegment(name)
	if err != nil {
		return false, err
	}
	accessToken, err := p.ensureAccessToken(ctx)
	if err != nil {
		return false, ErrOperationFailed
	}
	cleanPath, err := normalizeVirtualPath(requested)
	if err != nil {
		return false, err
	}
	parent, err := p.resolveByPath(ctx, accessToken, cleanPath)
	if err != nil {
		return false, err
	}
	pid := jsonRawToString(parent.FileID)
	if pid == "" {
		pid = "0"
	}
	p.mu.Lock()
	baseURL := p.config.BaseURL
	p.mu.Unlock()

	form := url.Values{}
	form.Set("pid", pid)
	form.Set("file_name", validName)
	var resp struct {
		State   bool   `json:"state"`
		Code    int    `json:"code"`
		Message string `json:"message"`
	}
	if err := p.doForm(ctx, http.MethodPost, baseURL+"/open/folder/add", accessToken, form, &resp); err != nil {
		return false, err
	}
	if !resp.State && resp.Code != 20004 {
		return false, ErrOperationFailed
	}
	return true, nil
}

// Delete 删除 115 网盘文件或目录。
func (p *U115Provider) Delete(ctx context.Context, storageName, requested string) (bool, error) {
	if err := p.require(ctx, storageName); err != nil {
		return false, err
	}
	accessToken, err := p.ensureAccessToken(ctx)
	if err != nil {
		return false, ErrOperationFailed
	}
	cleanPath, err := normalizeVirtualPath(requested)
	if err != nil {
		return false, err
	}
	if cleanPath == "/" {
		return false, ErrInvalidRequest
	}
	target, err := p.resolveByPath(ctx, accessToken, cleanPath)
	if err != nil {
		return false, err
	}
	fid := jsonRawToString(target.FileID)
	if fid == "" {
		return false, ErrOperationFailed
	}
	p.mu.Lock()
	baseURL := p.config.BaseURL
	p.mu.Unlock()

	form := url.Values{}
	form.Set("file_ids", fid)
	var resp struct {
		State bool `json:"state"`
		Code  int  `json:"code"`
	}
	if err := p.doForm(ctx, http.MethodPost, baseURL+"/open/ufile/delete", accessToken, form, &resp); err != nil {
		return false, err
	}
	if resp.Code != 0 && !resp.State {
		return false, ErrOperationFailed
	}
	return true, nil
}

// Rename 重命名 115 网盘文件或目录。
func (p *U115Provider) Rename(ctx context.Context, storageName, requested, newName string, _ bool) (bool, error) {
	if err := p.require(ctx, storageName); err != nil {
		return false, err
	}
	validName, err := validateSMBSegment(newName)
	if err != nil {
		return false, err
	}
	accessToken, err := p.ensureAccessToken(ctx)
	if err != nil {
		return false, ErrOperationFailed
	}
	cleanPath, err := normalizeVirtualPath(requested)
	if err != nil {
		return false, err
	}
	if cleanPath == "/" {
		return false, ErrInvalidRequest
	}
	target, err := p.resolveByPath(ctx, accessToken, cleanPath)
	if err != nil {
		return false, err
	}
	fid := jsonRawToString(target.FileID)
	if fid == "" {
		return false, ErrOperationFailed
	}
	p.mu.Lock()
	baseURL := p.config.BaseURL
	p.mu.Unlock()

	form := url.Values{}
	form.Set("file_id", fid)
	form.Set("file_name", validName)
	var resp struct {
		State bool `json:"state"`
		Code  int  `json:"code"`
	}
	if err := p.doForm(ctx, http.MethodPost, baseURL+"/open/ufile/update", accessToken, form, &resp); err != nil {
		return false, err
	}
	if !resp.State {
		return false, ErrOperationFailed
	}
	return true, nil
}

// Usage 查询 115 网盘空间使用情况。
func (p *U115Provider) Usage(ctx context.Context, storageName string) (Usage, error) {
	if err := p.require(ctx, storageName); err != nil {
		return Usage{}, err
	}
	accessToken, err := p.ensureAccessToken(ctx)
	if err != nil {
		return Usage{}, ErrOperationFailed
	}
	p.mu.Lock()
	baseURL := p.config.BaseURL
	p.mu.Unlock()

	var resp struct {
		Code int `json:"code"`
		Data struct {
			SpaceInfo struct {
				AllTotal struct {
					Size float64 `json:"size"`
				} `json:"all_total"`
				AllRemain struct {
					Size float64 `json:"size"`
				} `json:"all_remain"`
			} `json:"rt_space_info"`
		} `json:"data"`
	}
	if err := p.doForm(ctx, http.MethodGet, baseURL+"/open/user/info", accessToken, nil, &resp); err != nil {
		return Usage{}, err
	}
	if resp.Code != 0 {
		return Usage{}, ErrOperationFailed
	}
	return Usage{
		Total:     resp.Data.SpaceInfo.AllTotal.Size,
		Available: resp.Data.SpaceInfo.AllRemain.Size,
	}, nil
}

// TransType 返回 V2 115 网盘支持的整理方式。
func (p *U115Provider) TransType(ctx context.Context, storageName string) (map[string]any, error) {
	if err := p.require(ctx, storageName); err != nil {
		return nil, err
	}
	return map[string]any{
		"move": "移动",
		"copy": "复制",
	}, nil
}

// Open 获取 115 文件下载直链并返回只读内容流。
func (p *U115Provider) Open(ctx context.Context, storageName, requested string) (io.ReadCloser, string, error) {
	if err := p.require(ctx, storageName); err != nil {
		return nil, "", err
	}
	accessToken, err := p.ensureAccessToken(ctx)
	if err != nil {
		return nil, "", ErrOperationFailed
	}
	cleanPath, err := normalizeVirtualPath(requested)
	if err != nil {
		return nil, "", err
	}
	if cleanPath == "/" {
		return nil, "", ErrInvalidRequest
	}
	target, err := p.resolveByPath(ctx, accessToken, cleanPath)
	if err != nil || target.FileCategory != "1" || strings.TrimSpace(target.PickCode) == "" {
		return nil, "", ErrOperationFailed
	}
	p.mu.Lock()
	baseURL := p.config.BaseURL
	p.mu.Unlock()

	form := url.Values{}
	form.Set("pick_code", target.PickCode)
	var dlResp struct {
		Code int `json:"code"`
		Data map[string]struct {
			URL struct {
				URL string `json:"url"`
			} `json:"url"`
		} `json:"data"`
	}
	if err := p.doForm(ctx, http.MethodPost, baseURL+"/open/ufile/downurl", accessToken, form, &dlResp); err != nil {
		return nil, "", err
	}
	if dlResp.Code != 0 {
		return nil, "", ErrOperationFailed
	}
	var downloadURL string
	for _, entry := range dlResp.Data {
		if strings.TrimSpace(entry.URL.URL) != "" {
			downloadURL = strings.TrimSpace(entry.URL.URL)
			break
		}
	}
	if downloadURL == "" {
		return nil, "", ErrOperationFailed
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, downloadURL, nil)
	if err != nil {
		return nil, "", ErrOperationFailed
	}
	req.Header.Set("User-Agent", defaultU115UserAgent)
	resp, err := p.client.Do(req)
	if err != nil {
		return nil, "", ErrOperationFailed
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		_ = resp.Body.Close()
		return nil, "", ErrOperationFailed
	}
	contentType := resp.Header.Get("Content-Type")
	if contentType == "" {
		contentType = mime.TypeByExtension(strings.ToLower(path.Ext(target.FileName)))
	}
	if contentType == "" {
		contentType = "application/octet-stream"
	}
	return resp.Body, contentType, nil
}

// AuthURL 生成 115 OAuth2 授权 URL。
func (p *U115Provider) AuthURL(ctx context.Context, storageName string) (any, string, error) {
	if err := p.require(ctx, storageName); err != nil {
		return nil, "存储实现未接入", err
	}
	p.mu.Lock()
	authServerURL := p.config.AuthServerURL
	p.mu.Unlock()

	var resp struct {
		Success bool   `json:"success"`
		Message string `json:"message"`
		Data    struct {
			AuthURL string `json:"auth_url"`
			State   string `json:"state"`
		} `json:"data"`
	}
	if err := p.doForm(ctx, http.MethodGet, authServerURL+"/u115/auth_url", "", nil, &resp); err != nil {
		return nil, "无法连接到授权服务器", ErrOperationFailed
	}
	if !resp.Success {
		if resp.Message == "" {
			resp.Message = "获取授权 URL 失败"
		}
		return nil, resp.Message, nil
	}
	if resp.Data.AuthURL == "" || resp.Data.State == "" {
		return nil, "授权服务器返回数据不完整", nil
	}
	p.mu.Lock()
	p.authState = u115AuthState{OAuthState: resp.Data.State}
	p.mu.Unlock()
	return map[string]any{"authUrl": resp.Data.AuthURL, "state": resp.Data.State}, "", nil
}

// QRCode 生成 115 PKCE 扫码授权二维码。
func (p *U115Provider) QRCode(ctx context.Context, storageName string) (any, string, error) {
	if err := p.require(ctx, storageName); err != nil {
		return nil, "存储实现未接入", err
	}
	codeVerifier, err := randomPKCEVerifier()
	if err != nil {
		return nil, "生成验证码失败", ErrOperationFailed
	}
	digest := sha256.Sum256([]byte(codeVerifier))
	codeChallenge := base64.StdEncoding.EncodeToString(digest[:])

	p.mu.Lock()
	passportURL := p.config.PassportURL
	appID := p.config.AppID
	p.mu.Unlock()

	form := url.Values{}
	form.Set("client_id", appID)
	form.Set("code_challenge", codeChallenge)
	form.Set("code_challenge_method", "sha256")

	var resp struct {
		Code    int    `json:"code"`
		Message string `json:"message"`
		Data    struct {
			UID    string `json:"uid"`
			Time   int64  `json:"time"`
			Sign   string `json:"sign"`
			QRCode string `json:"qrcode"`
		} `json:"data"`
	}
	if err := p.doForm(ctx, http.MethodPost, passportURL+"/open/authDeviceCode", "", form, &resp); err != nil {
		return nil, "获取 115 二维码失败", ErrOperationFailed
	}
	if resp.Code != 0 {
		return nil, resp.Message, nil
	}
	p.mu.Lock()
	p.authState = u115AuthState{
		CodeVerifier: codeVerifier,
		UID:          resp.Data.UID,
		Time:         resp.Data.Time,
		Sign:         resp.Data.Sign,
	}
	p.mu.Unlock()
	return map[string]any{"codeContent": resp.Data.QRCode}, "", nil
}

// CheckLogin 检查 115 扫码或 OAuth2 授权状态，并在成功后保存 token。
func (p *U115Provider) CheckLogin(ctx context.Context, storageName, _, _ string) (any, string, error) {
	if err := p.require(ctx, storageName); err != nil {
		return nil, "存储实现未接入", err
	}
	p.mu.Lock()
	state := p.authState
	authServerURL := p.config.AuthServerURL
	qrStatusURL := p.config.QRCodeStatusURL
	passportURL := p.config.PassportURL
	p.mu.Unlock()

	if state.OAuthState != "" {
		return p.checkOAuthLogin(ctx, authServerURL, state.OAuthState)
	}
	if state.UID == "" {
		return nil, "请先生成二维码", nil
	}

	params := url.Values{}
	params.Set("uid", state.UID)
	params.Set("time", strconv.FormatInt(state.Time, 10))
	params.Set("sign", state.Sign)

	var resp struct {
		Code    int    `json:"code"`
		Message string `json:"message"`
		Data    struct {
			Status int    `json:"status"`
			Msg    string `json:"msg"`
		} `json:"data"`
	}
	if err := p.doForm(ctx, http.MethodGet, qrStatusURL+"/get/status/", "", params, &resp); err != nil {
		return nil, "查询扫码状态失败", ErrOperationFailed
	}
	if resp.Code != 0 {
		return nil, resp.Message, nil
	}
	if resp.Data.Status == 2 {
		form := url.Values{}
		form.Set("uid", state.UID)
		form.Set("code_verifier", state.CodeVerifier)
		var tokenResp struct {
			Code    int           `json:"code"`
			Message string        `json:"message"`
			Data    u115TokenData `json:"data"`
		}
		if err := p.doForm(ctx, http.MethodPost, passportURL+"/open/deviceCodeToToken", "", form, &tokenResp); err != nil {
			return nil, "换取 access_token 失败", ErrOperationFailed
		}
		if tokenResp.Code != 0 {
			return nil, tokenResp.Message, nil
		}
		if err := p.applyTokens(ctx, tokenResp.Data); err != nil {
			return nil, "保存授权凭证失败", ErrOperationFailed
		}
		p.mu.Lock()
		p.authState = u115AuthState{}
		p.mu.Unlock()
	}
	return map[string]any{"status": resp.Data.Status, "tip": resp.Data.Msg}, "", nil
}

func (p *U115Provider) checkOAuthLogin(ctx context.Context, authServerURL, oauthState string) (any, string, error) {
	params := url.Values{}
	params.Set("state", oauthState)
	var resp struct {
		Status  string        `json:"status"`
		Message string        `json:"message"`
		Data    u115TokenData `json:"data"`
	}
	if err := p.doForm(ctx, http.MethodGet, authServerURL+"/u115/token", "", params, &resp); err != nil {
		return nil, "无法连接到授权服务器", ErrOperationFailed
	}
	switch resp.Status {
	case "completed":
		if strings.TrimSpace(resp.Data.AccessToken) == "" {
			return nil, "授权服务器返回数据不完整", nil
		}
		if err := p.applyTokens(ctx, resp.Data); err != nil {
			return nil, "保存授权凭证失败", ErrOperationFailed
		}
		p.mu.Lock()
		p.authState = u115AuthState{}
		p.mu.Unlock()
		return map[string]any{"status": 2, "tip": "授权成功"}, "", nil
	case "expired":
		p.mu.Lock()
		p.authState = u115AuthState{}
		p.mu.Unlock()
		tip := resp.Message
		if tip == "" {
			tip = "授权已过期"
		}
		return map[string]any{"status": -1, "tip": tip}, "", nil
	default:
		return map[string]any{"status": 0, "tip": "等待用户授权"}, "", nil
	}
}

// SaveConfig 保存 115 授权凭证配置。
func (p *U115Provider) SaveConfig(ctx context.Context, storageName string, conf map[string]any) error {
	if err := p.require(ctx, storageName); err != nil {
		return err
	}
	p.mu.Lock()
	if v, ok := stringConfigValue(conf, "access_token"); ok {
		p.config.AccessToken = v
	}
	if v, ok := stringConfigValue(conf, "refresh_token"); ok {
		p.config.RefreshToken = v
	}
	if v, ok := int64ConfigValue(conf, "expires_in"); ok {
		p.config.ExpiresIn = v
	}
	if v, ok := int64ConfigValue(conf, "refresh_time"); ok {
		p.config.RefreshTime = v
	}
	persisted := p.exportConfigLocked()
	onChange := p.config.OnConfigChange
	p.mu.Unlock()

	if onChange != nil {
		return onChange(ctx, persisted)
	}
	return nil
}

// ResetConfig 清空 115 登录凭证与状态。
func (p *U115Provider) ResetConfig(ctx context.Context, storageName string) error {
	if err := p.require(ctx, storageName); err != nil {
		return err
	}
	p.mu.Lock()
	p.config.AccessToken = ""
	p.config.RefreshToken = ""
	p.config.ExpiresIn = 0
	p.config.RefreshTime = 0
	p.authState = u115AuthState{}
	onChange := p.config.OnConfigChange
	p.mu.Unlock()

	if onChange != nil {
		return onChange(ctx, map[string]any{})
	}
	return nil
}

func (p *U115Provider) exportConfigLocked() map[string]any {
	out := map[string]any{}
	if p.config.AccessToken != "" {
		out["access_token"] = p.config.AccessToken
	}
	if p.config.RefreshToken != "" {
		out["refresh_token"] = p.config.RefreshToken
	}
	if p.config.ExpiresIn > 0 {
		out["expires_in"] = p.config.ExpiresIn
	}
	if p.config.RefreshTime > 0 {
		out["refresh_time"] = p.config.RefreshTime
	}
	return out
}

var _ Provider = (*U115Provider)(nil)
var _ FileOpener = (*U115Provider)(nil)
var _ AuthManager = (*U115Provider)(nil)
