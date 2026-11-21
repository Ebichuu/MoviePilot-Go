package storage

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
	"net/http"
	"path"
	"strings"
	"sync"
	"time"
)

const (
	defaultAlipanName    = "alipan"
	defaultAlipanBaseURL = "https://openapi.alipan.com"
	defaultAlipanAppID   = "ac1bf04dc9fd4d9aaabb65b4a668d403"
	defaultAlipanTimeout = 30 * time.Second
	alipanMaxBody        = 8 << 20
	alipanMaxListItems   = 10_000
)

// AlipanConfig 是阿里云盘 Open API 适配器的配置切片。
type AlipanConfig struct {
	Name            string
	BaseURL         string
	AppID           string
	AccessToken     string
	RefreshToken    string
	DriveID         string
	ResourceDriveID string
	BackupDriveID   string
	DefaultDriveID  string
	ExpiresIn       int64
	RefreshTime     int64
	Timeout         time.Duration
	OnConfigChange  func(context.Context, map[string]any) error
}

type alipanAuthState struct {
	SID          string
	CodeVerifier string
}

// AlipanProvider 基于阿里云盘 Open API 提供扫码登录、文件浏览、目录管理与只读打开能力。
type AlipanProvider struct {
	client *http.Client

	mu        sync.Mutex
	config    AlipanConfig
	authState alipanAuthState
	now       func() time.Time
}

// NewAlipanProvider 创建阿里云盘存储适配器。
func NewAlipanProvider(cfg AlipanConfig) (*AlipanProvider, error) {
	cfg.Name = strings.TrimSpace(cfg.Name)
	if cfg.Name == "" {
		cfg.Name = defaultAlipanName
	}
	cfg.BaseURL = strings.TrimRight(strings.TrimSpace(cfg.BaseURL), "/")
	if cfg.BaseURL == "" {
		cfg.BaseURL = defaultAlipanBaseURL
	}
	cfg.AppID = strings.TrimSpace(cfg.AppID)
	if cfg.AppID == "" {
		cfg.AppID = defaultAlipanAppID
	}
	if cfg.Timeout <= 0 {
		cfg.Timeout = defaultAlipanTimeout
	}
	return &AlipanProvider{
		client: &http.Client{Timeout: cfg.Timeout},
		config: cfg,
		now:    time.Now,
	}, nil
}

// Name 返回该适配器声明的 V2 存储名。
func (p *AlipanProvider) Name() string {
	if p == nil {
		return ""
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.config.Name
}

func (p *AlipanProvider) require(ctx context.Context, storageName string) error {
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

func (p *AlipanProvider) defaultDriveIDLocked() string {
	if id := strings.TrimSpace(p.config.DriveID); id != "" {
		return id
	}
	if id := strings.TrimSpace(p.config.ResourceDriveID); id != "" {
		return id
	}
	if id := strings.TrimSpace(p.config.BackupDriveID); id != "" {
		return id
	}
	return strings.TrimSpace(p.config.DefaultDriveID)
}

func (p *AlipanProvider) ensureAccessToken(ctx context.Context) (string, string, error) {
	p.mu.Lock()
	accessToken := strings.TrimSpace(p.config.AccessToken)
	refreshToken := strings.TrimSpace(p.config.RefreshToken)
	expiresIn := p.config.ExpiresIn
	refreshTime := p.config.RefreshTime
	baseURL := p.config.BaseURL
	appID := p.config.AppID
	driveID := p.defaultDriveIDLocked()
	nowUnix := p.now().Unix()
	p.mu.Unlock()

	if accessToken == "" && refreshToken == "" {
		return "", "", ErrOperationFailed
	}
	needsRefresh := accessToken == "" || (expiresIn > 0 && refreshTime > 0 && refreshTime+expiresIn <= nowUnix)
	if needsRefresh && refreshToken != "" {
		tokens, err := p.exchangeToken(ctx, baseURL, map[string]any{
			"client_id":     appID,
			"grant_type":    "refresh_token",
			"refresh_token": refreshToken,
		})
		if err != nil {
			return "", "", ErrOperationFailed
		}
		if err := p.applyTokens(ctx, tokens); err != nil {
			return "", "", err
		}
		p.mu.Lock()
		accessToken = strings.TrimSpace(p.config.AccessToken)
		driveID = p.defaultDriveIDLocked()
		p.mu.Unlock()
	}
	if accessToken == "" {
		return "", "", ErrOperationFailed
	}
	if driveID == "" {
		if err := p.fetchAndSaveDriveInfo(ctx, accessToken); err == nil {
			p.mu.Lock()
			driveID = p.defaultDriveIDLocked()
			p.mu.Unlock()
		}
	}
	return accessToken, driveID, nil
}

type alipanTokenResp struct {
	AccessToken  string `json:"access_token"`
	RefreshToken string `json:"refresh_token"`
	ExpiresIn    int64  `json:"expires_in"`
	Code         string `json:"code"`
	Message      string `json:"message"`
}

func (p *AlipanProvider) exchangeToken(ctx context.Context, baseURL string, body map[string]any) (alipanTokenResp, error) {
	var resp alipanTokenResp
	if err := p.doJSON(ctx, http.MethodPost, baseURL+"/oauth/access_token", "", body, &resp); err != nil {
		return alipanTokenResp{}, err
	}
	if resp.Code != "" || strings.TrimSpace(resp.AccessToken) == "" {
		return alipanTokenResp{}, fmt.Errorf("alipan oauth error: %s %s", resp.Code, resp.Message)
	}
	return resp, nil
}

func (p *AlipanProvider) applyTokens(ctx context.Context, tokens alipanTokenResp) error {
	p.mu.Lock()
	p.config.AccessToken = strings.TrimSpace(tokens.AccessToken)
	if strings.TrimSpace(tokens.RefreshToken) != "" {
		p.config.RefreshToken = strings.TrimSpace(tokens.RefreshToken)
	}
	if tokens.ExpiresIn > 0 {
		p.config.ExpiresIn = tokens.ExpiresIn
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

func (p *AlipanProvider) fetchAndSaveDriveInfo(ctx context.Context, accessToken string) error {
	p.mu.Lock()
	baseURL := p.config.BaseURL
	p.mu.Unlock()

	var info struct {
		UserID          string `json:"user_id"`
		Name            string `json:"name"`
		DefaultDriveID  string `json:"default_drive_id"`
		ResourceDriveID string `json:"resource_drive_id"`
		BackupDriveID   string `json:"backup_drive_id"`
		Code            string `json:"code"`
		Message         string `json:"message"`
	}
	if err := p.doJSON(ctx, http.MethodPost, baseURL+"/adrive/v1.0/user/getDriveInfo", accessToken, map[string]any{}, &info); err != nil {
		return err
	}
	if info.Code != "" {
		return fmt.Errorf("alipan drive info error: %s %s", info.Code, info.Message)
	}

	p.mu.Lock()
	if info.DefaultDriveID != "" {
		p.config.DefaultDriveID = info.DefaultDriveID
	}
	if info.ResourceDriveID != "" {
		p.config.ResourceDriveID = info.ResourceDriveID
	}
	if info.BackupDriveID != "" {
		p.config.BackupDriveID = info.BackupDriveID
	}
	persisted := p.exportConfigLocked()
	onChange := p.config.OnConfigChange
	p.mu.Unlock()

	if onChange != nil {
		return onChange(ctx, persisted)
	}
	return nil
}

func (p *AlipanProvider) doJSON(
	ctx context.Context,
	method, fullURL, bearer string,
	payload any,
	out any,
) error {
	var bodyReader io.Reader
	if payload != nil {
		encoded, err := json.Marshal(payload)
		if err != nil {
			return ErrInvalidRequest
		}
		bodyReader = bytes.NewReader(encoded)
	}
	req, err := http.NewRequestWithContext(ctx, method, fullURL, bodyReader)
	if err != nil {
		return ErrInvalidRequest
	}
	req.Header.Set("Content-Type", "application/json")
	if bearer != "" {
		req.Header.Set("Authorization", "Bearer "+bearer)
	}
	resp, err := p.client.Do(req)
	if err != nil {
		return ErrOperationFailed
	}
	defer func() { _ = resp.Body.Close() }()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, alipanMaxBody))
	if err != nil {
		return ErrOperationFailed
	}
	if resp.StatusCode >= 500 || (resp.StatusCode >= 400 && len(raw) == 0) {
		return ErrOperationFailed
	}
	if out != nil && len(raw) > 0 {
		if err := json.Unmarshal(raw, out); err != nil {
			return ErrOperationFailed
		}
	}
	return nil
}

type alipanFileEntry struct {
	DriveID       string `json:"drive_id"`
	FileID        string `json:"file_id"`
	ParentFileID  string `json:"parent_file_id"`
	Name          string `json:"name"`
	Type          string `json:"type"`
	Size          int64  `json:"size"`
	FileExtension string `json:"file_extension"`
	UpdatedAt     string `json:"updated_at"`
	Thumbnail     string `json:"thumbnail"`
	Code          string `json:"code"`
	Message       string `json:"message"`
}

func normalizeVirtualPath(requested string) (string, error) {
	raw := strings.ReplaceAll(strings.TrimSpace(requested), `\`, "/")
	for _, seg := range strings.Split(raw, "/") {
		if seg == ".." {
			return "", ErrPathRejected
		}
	}
	cleaned := path.Clean("/" + raw)
	if cleaned == "." {
		return "/", nil
	}
	return cleaned, nil
}

func (p *AlipanProvider) resolveByPath(ctx context.Context, accessToken, driveID, filePath string) (alipanFileEntry, error) {
	if filePath == "/" {
		return alipanFileEntry{
			DriveID: driveID,
			FileID:  "root",
			Name:    "/",
			Type:    "folder",
		}, nil
	}
	p.mu.Lock()
	baseURL := p.config.BaseURL
	p.mu.Unlock()

	var entry alipanFileEntry
	err := p.doJSON(ctx, http.MethodPost, baseURL+"/adrive/v1.0/openFile/get_by_path", accessToken, map[string]any{
		"drive_id":  driveID,
		"file_path": filePath,
	}, &entry)
	if err != nil {
		return alipanFileEntry{}, err
	}
	if entry.Code != "" || entry.FileID == "" {
		return alipanFileEntry{}, ErrOperationFailed
	}
	return entry, nil
}

// List 浏览阿里云盘目录（自动分页并按 V2 语义排序与过滤）。
func (p *AlipanProvider) List(ctx context.Context, storageName, requested, sortKey, keyword string) ([]Item, error) {
	if err := p.require(ctx, storageName); err != nil {
		return nil, err
	}
	accessToken, driveID, err := p.ensureAccessToken(ctx)
	if err != nil || driveID == "" {
		return nil, ErrOperationFailed
	}
	cleanPath, err := normalizeVirtualPath(requested)
	if err != nil {
		return nil, err
	}
	target, err := p.resolveByPath(ctx, accessToken, driveID, cleanPath)
	if err != nil {
		return nil, err
	}
	if target.Type != "folder" {
		return []Item{p.entryToItem(path.Dir(cleanPath), target)}, nil
	}

	p.mu.Lock()
	baseURL := p.config.BaseURL
	p.mu.Unlock()

	items := make([]Item, 0, 100)
	marker := ""
	for {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		payload := map[string]any{
			"drive_id":       driveID,
			"parent_file_id": target.FileID,
			"limit":          100,
		}
		if marker != "" {
			payload["marker"] = marker
		}
		var listResp struct {
			Items      []alipanFileEntry `json:"items"`
			NextMarker string            `json:"next_marker"`
			Code       string            `json:"code"`
			Message    string            `json:"message"`
		}
		if err := p.doJSON(ctx, http.MethodPost, baseURL+"/adrive/v1.0/openFile/list", accessToken, payload, &listResp); err != nil {
			return nil, err
		}
		if listResp.Code != "" {
			return nil, ErrOperationFailed
		}
		for _, entry := range listResp.Items {
			items = append(items, p.entryToItem(cleanPath, entry))
		}
		if listResp.NextMarker == "" || len(listResp.Items) < 100 || len(items) >= alipanMaxListItems {
			break
		}
		marker = listResp.NextMarker
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

func (p *AlipanProvider) entryToItem(parentPath string, entry alipanFileEntry) Item {
	if !strings.HasSuffix(parentPath, "/") {
		parentPath += "/"
	}
	itemPath := parentPath + entry.Name
	item := Item{
		Name:         entry.Name,
		Children:     []Item{},
		FileID:       entry.FileID,
		ParentFileID: entry.ParentFileID,
		Thumbnail:    entry.Thumbnail,
		Storage:      p.Name(),
	}
	if parsed, err := time.Parse(time.RFC3339, entry.UpdatedAt); err == nil {
		ts := float64(parsed.Unix())
		item.ModifyTime = &ts
	}
	if entry.Type == "folder" {
		item.Type = "dir"
		item.Basename = entry.Name
		item.Path = itemPath + "/"
	} else {
		item.Type = "file"
		ext := path.Ext(entry.Name)
		item.Basename = strings.TrimSuffix(entry.Name, ext)
		item.Extension = entry.FileExtension
		if item.Extension == "" {
			item.Extension = strings.TrimPrefix(ext, ".")
		}
		size := entry.Size
		item.Size = &size
		item.Path = itemPath
	}
	return item
}

// Mkdir 在阿里云盘中创建目录。
func (p *AlipanProvider) Mkdir(ctx context.Context, storageName, requested, name string) (bool, error) {
	if err := p.require(ctx, storageName); err != nil {
		return false, err
	}
	validName, err := validateSMBSegment(name)
	if err != nil {
		return false, err
	}
	accessToken, driveID, err := p.ensureAccessToken(ctx)
	if err != nil || driveID == "" {
		return false, ErrOperationFailed
	}
	cleanPath, err := normalizeVirtualPath(requested)
	if err != nil {
		return false, err
	}
	parent, err := p.resolveByPath(ctx, accessToken, driveID, cleanPath)
	if err != nil {
		return false, err
	}
	p.mu.Lock()
	baseURL := p.config.BaseURL
	p.mu.Unlock()

	var resp struct {
		FileID  string `json:"file_id"`
		Code    string `json:"code"`
		Message string `json:"message"`
	}
	if err := p.doJSON(ctx, http.MethodPost, baseURL+"/adrive/v1.0/openFile/create", accessToken, map[string]any{
		"drive_id":       driveID,
		"parent_file_id": parent.FileID,
		"name":           validName,
		"type":           "folder",
	}, &resp); err != nil {
		return false, err
	}
	if resp.Code != "" {
		return false, ErrOperationFailed
	}
	return true, nil
}

// Delete 将阿里云盘文件/目录移入回收站。
func (p *AlipanProvider) Delete(ctx context.Context, storageName, requested string) (bool, error) {
	if err := p.require(ctx, storageName); err != nil {
		return false, err
	}
	accessToken, driveID, err := p.ensureAccessToken(ctx)
	if err != nil || driveID == "" {
		return false, ErrOperationFailed
	}
	cleanPath, err := normalizeVirtualPath(requested)
	if err != nil {
		return false, err
	}
	if cleanPath == "/" {
		return false, ErrInvalidRequest
	}
	target, err := p.resolveByPath(ctx, accessToken, driveID, cleanPath)
	if err != nil {
		return false, err
	}
	p.mu.Lock()
	baseURL := p.config.BaseURL
	p.mu.Unlock()

	var resp struct {
		Code    string `json:"code"`
		Message string `json:"message"`
	}
	if err := p.doJSON(ctx, http.MethodPost, baseURL+"/adrive/v1.0/openFile/recyclebin/trash", accessToken, map[string]any{
		"drive_id": driveID,
		"file_id":  target.FileID,
	}, &resp); err != nil {
		return false, err
	}
	if resp.Code != "" {
		return false, ErrOperationFailed
	}
	return true, nil
}

// Rename 重命名阿里云盘文件/目录。
func (p *AlipanProvider) Rename(ctx context.Context, storageName, requested, newName string, _ bool) (bool, error) {
	if err := p.require(ctx, storageName); err != nil {
		return false, err
	}
	validName, err := validateSMBSegment(newName)
	if err != nil {
		return false, err
	}
	accessToken, driveID, err := p.ensureAccessToken(ctx)
	if err != nil || driveID == "" {
		return false, ErrOperationFailed
	}
	cleanPath, err := normalizeVirtualPath(requested)
	if err != nil {
		return false, err
	}
	if cleanPath == "/" {
		return false, ErrInvalidRequest
	}
	target, err := p.resolveByPath(ctx, accessToken, driveID, cleanPath)
	if err != nil {
		return false, err
	}
	p.mu.Lock()
	baseURL := p.config.BaseURL
	p.mu.Unlock()

	var resp struct {
		Code    string `json:"code"`
		Message string `json:"message"`
	}
	if err := p.doJSON(ctx, http.MethodPost, baseURL+"/adrive/v1.0/openFile/update", accessToken, map[string]any{
		"drive_id": driveID,
		"file_id":  target.FileID,
		"name":     validName,
	}, &resp); err != nil {
		return false, err
	}
	if resp.Code != "" {
		return false, ErrOperationFailed
	}
	return true, nil
}

// Usage 查询阿里云盘空间配额。
func (p *AlipanProvider) Usage(ctx context.Context, storageName string) (Usage, error) {
	if err := p.require(ctx, storageName); err != nil {
		return Usage{}, err
	}
	accessToken, _, err := p.ensureAccessToken(ctx)
	if err != nil {
		return Usage{}, ErrOperationFailed
	}
	p.mu.Lock()
	baseURL := p.config.BaseURL
	p.mu.Unlock()

	var resp struct {
		PersonalSpaceInfo struct {
			TotalSize float64 `json:"total_size"`
			UsedSize  float64 `json:"used_size"`
		} `json:"personal_space_info"`
		Code    string `json:"code"`
		Message string `json:"message"`
	}
	if err := p.doJSON(ctx, http.MethodPost, baseURL+"/adrive/v1.0/user/getSpaceInfo", accessToken, map[string]any{}, &resp); err != nil {
		return Usage{}, err
	}
	if resp.Code != "" {
		return Usage{}, ErrOperationFailed
	}
	avail := resp.PersonalSpaceInfo.TotalSize - resp.PersonalSpaceInfo.UsedSize
	if avail < 0 {
		avail = 0
	}
	return Usage{Total: resp.PersonalSpaceInfo.TotalSize, Available: avail}, nil
}

// TransType 返回 V2 阿里云盘支持的整理方式。
func (p *AlipanProvider) TransType(ctx context.Context, storageName string) (map[string]any, error) {
	if err := p.require(ctx, storageName); err != nil {
		return nil, err
	}
	return map[string]any{
		"move": "移动",
		"copy": "复制",
	}, nil
}

// Open 获取阿里云盘直链并返回只读内容流。
func (p *AlipanProvider) Open(ctx context.Context, storageName, requested string) (io.ReadCloser, string, error) {
	if err := p.require(ctx, storageName); err != nil {
		return nil, "", err
	}
	accessToken, driveID, err := p.ensureAccessToken(ctx)
	if err != nil || driveID == "" {
		return nil, "", ErrOperationFailed
	}
	cleanPath, err := normalizeVirtualPath(requested)
	if err != nil {
		return nil, "", err
	}
	if cleanPath == "/" {
		return nil, "", ErrInvalidRequest
	}
	target, err := p.resolveByPath(ctx, accessToken, driveID, cleanPath)
	if err != nil || target.Type == "folder" {
		return nil, "", ErrOperationFailed
	}
	p.mu.Lock()
	baseURL := p.config.BaseURL
	p.mu.Unlock()

	var dl struct {
		URL     string `json:"url"`
		Code    string `json:"code"`
		Message string `json:"message"`
	}
	if err := p.doJSON(ctx, http.MethodPost, baseURL+"/adrive/v1.0/openFile/getDownloadUrl", accessToken, map[string]any{
		"drive_id": driveID,
		"file_id":  target.FileID,
	}, &dl); err != nil {
		return nil, "", err
	}
	if dl.Code != "" || strings.TrimSpace(dl.URL) == "" {
		return nil, "", ErrOperationFailed
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, dl.URL, nil)
	if err != nil {
		return nil, "", ErrOperationFailed
	}
	req.Header.Set("Referer", "https://www.aliyundrive.com/")
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
		contentType = mime.TypeByExtension(strings.ToLower(path.Ext(target.Name)))
	}
	if contentType == "" {
		contentType = "application/octet-stream"
	}
	return resp.Body, contentType, nil
}

// QRCode 生成阿里云盘 PKCE 扫码授权二维码。
func (p *AlipanProvider) QRCode(ctx context.Context, storageName string) (any, string, error) {
	if err := p.require(ctx, storageName); err != nil {
		return nil, "存储实现未接入", err
	}
	codeVerifier, err := randomPKCEVerifier()
	if err != nil {
		return nil, "生成验证码失败", ErrOperationFailed
	}
	p.mu.Lock()
	baseURL := p.config.BaseURL
	appID := p.config.AppID
	p.mu.Unlock()

	var resp struct {
		SID       string `json:"sid"`
		QRCodeURL string `json:"qrCodeUrl"`
		Code      string `json:"code"`
		Message   string `json:"message"`
	}
	if err := p.doJSON(ctx, http.MethodPost, baseURL+"/oauth/authorize/qrcode", "", map[string]any{
		"client_id":             appID,
		"scopes":                []string{"user:base", "file:all:read", "file:all:write", "file:share:write"},
		"code_challenge":        codeVerifier,
		"code_challenge_method": "plain",
	}, &resp); err != nil {
		return nil, "获取二维码失败", ErrOperationFailed
	}
	if resp.Code != "" {
		return nil, resp.Message, nil
	}
	if strings.TrimSpace(resp.SID) == "" || strings.TrimSpace(resp.QRCodeURL) == "" {
		return nil, "阿里云盘未返回有效二维码", nil
	}
	p.mu.Lock()
	p.authState = alipanAuthState{SID: resp.SID, CodeVerifier: codeVerifier}
	p.mu.Unlock()
	return map[string]any{"codeUrl": resp.QRCodeURL}, "", nil
}

// AuthURL 阿里云盘仅使用扫码登录。
func (p *AlipanProvider) AuthURL(ctx context.Context, storageName string) (any, string, error) {
	if err := p.require(ctx, storageName); err != nil {
		return nil, "存储实现未接入", err
	}
	return nil, "阿里云盘请使用扫码登录", nil
}

// CheckLogin 检查阿里云盘扫码状态，登录成功后换取 token 并保存默认 drive_id。
func (p *AlipanProvider) CheckLogin(ctx context.Context, storageName, _, _ string) (any, string, error) {
	if err := p.require(ctx, storageName); err != nil {
		return nil, "存储实现未接入", err
	}
	p.mu.Lock()
	state := p.authState
	baseURL := p.config.BaseURL
	appID := p.config.AppID
	p.mu.Unlock()

	if state.SID == "" {
		return nil, "请先生成二维码", nil
	}
	var statusResp struct {
		Status   string `json:"status"`
		AuthCode string `json:"authCode"`
		Code     string `json:"code"`
		Message  string `json:"message"`
	}
	if err := p.doJSON(ctx, http.MethodGet, baseURL+"/oauth/qrcode/"+state.SID+"/status", "", nil, &statusResp); err != nil {
		return nil, "查询扫码状态失败", ErrOperationFailed
	}
	if statusResp.Code != "" {
		return nil, statusResp.Message, nil
	}

	statusTips := map[string]string{
		"WaitLogin":     "等待登录",
		"ScanSuccess":   "扫码成功",
		"LoginSuccess":  "登录成功",
		"QRCodeExpired": "二维码过期",
	}
	if statusResp.Status == "LoginSuccess" {
		tokens, err := p.exchangeToken(ctx, baseURL, map[string]any{
			"client_id":     appID,
			"grant_type":    "authorization_code",
			"code":          statusResp.AuthCode,
			"code_verifier": state.CodeVerifier,
		})
		if err != nil {
			return nil, err.Error(), nil
		}
		if err := p.applyTokens(ctx, tokens); err != nil {
			return nil, "保存授权凭证失败", ErrOperationFailed
		}
		_ = p.fetchAndSaveDriveInfo(ctx, tokens.AccessToken)
		p.mu.Lock()
		p.authState = alipanAuthState{}
		p.mu.Unlock()
	}
	tip := statusTips[statusResp.Status]
	if tip == "" {
		tip = "未知状态"
	}
	return map[string]any{"status": statusResp.Status, "tip": tip}, "", nil
}

// SaveConfig 保存阿里云盘凭证或 drive 配置。
func (p *AlipanProvider) SaveConfig(ctx context.Context, storageName string, conf map[string]any) error {
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
	if v, ok := stringConfigValue(conf, "drive_id"); ok {
		p.config.DriveID = v
	}
	if v, ok := stringConfigValue(conf, "resource_drive_id"); ok {
		p.config.ResourceDriveID = v
	}
	if v, ok := stringConfigValue(conf, "backup_drive_id"); ok {
		p.config.BackupDriveID = v
	}
	if v, ok := stringConfigValue(conf, "default_drive_id"); ok {
		p.config.DefaultDriveID = v
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

// ResetConfig 清空阿里云盘登录状态与配置。
func (p *AlipanProvider) ResetConfig(ctx context.Context, storageName string) error {
	if err := p.require(ctx, storageName); err != nil {
		return err
	}
	p.mu.Lock()
	p.config.AccessToken = ""
	p.config.RefreshToken = ""
	p.config.DriveID = ""
	p.config.ResourceDriveID = ""
	p.config.BackupDriveID = ""
	p.config.DefaultDriveID = ""
	p.config.ExpiresIn = 0
	p.config.RefreshTime = 0
	p.authState = alipanAuthState{}
	onChange := p.config.OnConfigChange
	p.mu.Unlock()

	if onChange != nil {
		return onChange(ctx, map[string]any{})
	}
	return nil
}

func (p *AlipanProvider) exportConfigLocked() map[string]any {
	out := map[string]any{}
	if p.config.AccessToken != "" {
		out["access_token"] = p.config.AccessToken
	}
	if p.config.RefreshToken != "" {
		out["refresh_token"] = p.config.RefreshToken
	}
	if p.config.DriveID != "" {
		out["drive_id"] = p.config.DriveID
	}
	if p.config.ResourceDriveID != "" {
		out["resource_drive_id"] = p.config.ResourceDriveID
	}
	if p.config.BackupDriveID != "" {
		out["backup_drive_id"] = p.config.BackupDriveID
	}
	if p.config.DefaultDriveID != "" {
		out["default_drive_id"] = p.config.DefaultDriveID
	}
	if p.config.ExpiresIn > 0 {
		out["expires_in"] = p.config.ExpiresIn
	}
	if p.config.RefreshTime > 0 {
		out["refresh_time"] = p.config.RefreshTime
	}
	return out
}

func randomPKCEVerifier() (string, error) {
	buf := make([]byte, 64)
	if _, err := rand.Read(buf); err != nil {
		return "", errors.New("random failed")
	}
	encoded := base64.RawURLEncoding.EncodeToString(buf)
	if len(encoded) > 128 {
		encoded = encoded[:128]
	}
	return encoded, nil
}

var _ Provider = (*AlipanProvider)(nil)
var _ FileOpener = (*AlipanProvider)(nil)
var _ AuthManager = (*AlipanProvider)(nil)
