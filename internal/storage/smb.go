package storage

import (
	"context"
	"fmt"
	"io"
	"mime"
	"net"
	"os"
	"path"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/hirochachacha/go-smb2"
)

const (
	defaultSMBName    = "smb"
	defaultSMBPort    = 445
	defaultSMBTimeout = 30 * time.Second
)

// SMBConfig 是 SMB/CIFS 远端共享存储的配置切片。
type SMBConfig struct {
	Name           string
	Host           string
	Port           int
	Share          string
	Username       string
	Password       string
	Domain         string
	Root           string
	Timeout        time.Duration
	OnConfigChange func(context.Context, map[string]any) error
}

// smbFS 抽象已挂载的 SMB 共享句柄，便于单测注入内存伪实现。
type smbFS interface {
	ReadDir(name string) ([]os.FileInfo, error)
	Stat(name string) (os.FileInfo, error)
	MkdirAll(name string, perm os.FileMode) error
	RemoveAll(name string) error
	Rename(oldpath, newpath string) error
	Open(name string) (io.ReadCloser, error)
	Statfs(name string) (total uint64, available uint64, err error)
	Close() error
}

type smbDialFunc func(ctx context.Context, cfg SMBConfig) (smbFS, error)

// SMBProvider 基于纯 Go go-smb2 提供 SMB/CIFS 共享的目录浏览、文件操作与配置管理。
type SMBProvider struct {
	mu     sync.RWMutex
	config SMBConfig
	dial   smbDialFunc
}

// NewSMBProvider 创建 SMB 存储适配器；允许初始未配置（后续通过 SaveConfig 写入）。
func NewSMBProvider(cfg SMBConfig) (*SMBProvider, error) {
	normalized := normalizeSMBConfig(cfg)
	return &SMBProvider{
		config: normalized,
		dial:   dialSMBShare,
	}, nil
}

func normalizeSMBConfig(cfg SMBConfig) SMBConfig {
	cfg.Name = strings.TrimSpace(cfg.Name)
	if cfg.Name == "" {
		cfg.Name = defaultSMBName
	}
	cfg.Host = strings.TrimSpace(cfg.Host)
	cfg.Share = strings.Trim(strings.TrimSpace(cfg.Share), `/\`)
	cfg.Username = strings.TrimSpace(cfg.Username)
	cfg.Domain = strings.TrimSpace(cfg.Domain)
	if cfg.Port <= 0 || cfg.Port > 65535 {
		cfg.Port = defaultSMBPort
	}
	if cfg.Timeout <= 0 {
		cfg.Timeout = defaultSMBTimeout
	}
	cfg.Root = cleanSMBRoot(cfg.Root)
	return cfg
}

func cleanSMBRoot(raw string) string {
	cleaned := strings.Trim(path.Clean("/"+strings.ReplaceAll(strings.TrimSpace(raw), `\`, "/")), "/")
	return cleaned
}

// Name 返回该适配器声明的 V2 存储名。
func (p *SMBProvider) Name() string {
	if p == nil {
		return ""
	}
	p.mu.RLock()
	defer p.mu.RUnlock()
	return p.config.Name
}

func (p *SMBProvider) snapshotConfig(ctx context.Context, storageName string) (SMBConfig, error) {
	if p == nil {
		return SMBConfig{}, ErrUnsupportedStorage
	}
	if ctx == nil {
		return SMBConfig{}, ErrInvalidRequest
	}
	if err := ctx.Err(); err != nil {
		return SMBConfig{}, err
	}
	p.mu.RLock()
	cfg := p.config
	p.mu.RUnlock()
	if strings.TrimSpace(storageName) != cfg.Name {
		return SMBConfig{}, ErrUnsupportedStorage
	}
	return cfg, nil
}

func (p *SMBProvider) openShare(ctx context.Context, storageName string) (smbFS, SMBConfig, error) {
	cfg, err := p.snapshotConfig(ctx, storageName)
	if err != nil {
		return nil, SMBConfig{}, err
	}
	if cfg.Host == "" || cfg.Share == "" {
		return nil, cfg, ErrOperationFailed
	}
	dialFn := p.dial
	if dialFn == nil {
		dialFn = dialSMBShare
	}
	share, err := dialFn(ctx, cfg)
	if err != nil {
		return nil, cfg, ErrOperationFailed
	}
	return share, cfg, nil
}

func resolveSMBRelativePath(root, requested string) (string, string, error) {
	raw := strings.ReplaceAll(strings.TrimSpace(requested), `\`, "/")
	for _, seg := range strings.Split(raw, "/") {
		if seg == ".." {
			return "", "", ErrPathRejected
		}
	}
	cleaned := strings.Trim(path.Clean("/"+raw), "/")
	displayPath := "/"
	if cleaned != "" {
		displayPath = "/" + cleaned
	}
	if root == "" {
		if cleaned == "" {
			return ".", displayPath, nil
		}
		return cleaned, displayPath, nil
	}
	if cleaned == "" {
		return root, displayPath, nil
	}
	return root + "/" + cleaned, displayPath, nil
}

func validateSMBSegment(name string) (string, error) {
	name = strings.TrimSpace(name)
	if name == "" || name == "." || name == ".." || strings.ContainsAny(name, `/\`) {
		return "", ErrInvalidRequest
	}
	return name, nil
}

// List 浏览 SMB 目录下的文件和子目录，支持关键词过滤与自然/时间排序。
func (p *SMBProvider) List(ctx context.Context, storageName, requested, sortKey, keyword string) ([]Item, error) {
	share, cfg, err := p.openShare(ctx, storageName)
	if err != nil {
		return nil, err
	}
	defer func() { _ = share.Close() }()

	rel, displayPath, err := resolveSMBRelativePath(cfg.Root, requested)
	if err != nil {
		return nil, err
	}

	// 如果目标是单个文件，对齐 V2 smb.py 语义返回该文件项。
	if rel != "." {
		if info, statErr := share.Stat(rel); statErr == nil && !info.IsDir() {
			return []Item{smbInfoToItem(cfg.Name, displayPath, info)}, nil
		}
	}

	entries, err := share.ReadDir(rel)
	if err != nil {
		return nil, ErrOperationFailed
	}

	keyword = strings.ToLower(strings.TrimSpace(keyword))
	items := make([]Item, 0, len(entries))
	for _, entry := range entries {
		name := entry.Name()
		if name == "." || name == ".." {
			continue
		}
		if keyword != "" && !globMatch(keyword, strings.ToLower(name)) {
			continue
		}
		childPath := path.Join(displayPath, name)
		if entry.IsDir() && !strings.HasSuffix(childPath, "/") {
			childPath += "/"
		}
		items = append(items, smbInfoToItem(cfg.Name, childPath, entry))
	}
	if strings.TrimSpace(sortKey) == "name" {
		sortItemsByName(items)
	} else {
		sortItemsByModifyTime(items)
	}
	return items, nil
}

func smbInfoToItem(storageName, displayPath string, info os.FileInfo) Item {
	name := info.Name()
	mod := float64(info.ModTime().Unix())
	item := Item{
		Name:       name,
		Children:   []Item{},
		ModifyTime: &mod,
		Path:       displayPath,
		Storage:    storageName,
	}
	if info.IsDir() {
		item.Type = "dir"
		item.Basename = name
		if !strings.HasSuffix(item.Path, "/") {
			item.Path += "/"
		}
	} else {
		item.Type = "file"
		ext := path.Ext(name)
		item.Basename = strings.TrimSuffix(name, ext)
		item.Extension = strings.TrimPrefix(ext, ".")
		size := info.Size()
		item.Size = &size
	}
	return item
}

// Mkdir 在 SMB 共享中创建子目录。
func (p *SMBProvider) Mkdir(ctx context.Context, storageName, requested, name string) (bool, error) {
	validName, err := validateSMBSegment(name)
	if err != nil {
		return false, err
	}
	share, cfg, err := p.openShare(ctx, storageName)
	if err != nil {
		return false, err
	}
	defer func() { _ = share.Close() }()

	parentRel, _, err := resolveSMBRelativePath(cfg.Root, requested)
	if err != nil {
		return false, err
	}
	target := validName
	if parentRel != "." && parentRel != "" {
		target = parentRel + "/" + validName
	}
	if err := share.MkdirAll(target, 0o755); err != nil {
		return false, ErrOperationFailed
	}
	return true, nil
}

// Delete 删除 SMB 文件或目录（禁止删除共享根路径）。
func (p *SMBProvider) Delete(ctx context.Context, storageName, requested string) (bool, error) {
	share, cfg, err := p.openShare(ctx, storageName)
	if err != nil {
		return false, err
	}
	defer func() { _ = share.Close() }()

	rel, displayPath, err := resolveSMBRelativePath(cfg.Root, requested)
	if err != nil {
		return false, err
	}
	if displayPath == "/" || rel == "." {
		return false, ErrInvalidRequest
	}
	if err := share.RemoveAll(rel); err != nil {
		return false, ErrOperationFailed
	}
	return true, nil
}

// Rename 重命名 SMB 文件或目录。
func (p *SMBProvider) Rename(ctx context.Context, storageName, requested, newName string, _ bool) (bool, error) {
	validName, err := validateSMBSegment(newName)
	if err != nil {
		return false, err
	}
	share, cfg, err := p.openShare(ctx, storageName)
	if err != nil {
		return false, err
	}
	defer func() { _ = share.Close() }()

	srcRel, displayPath, err := resolveSMBRelativePath(cfg.Root, requested)
	if err != nil {
		return false, err
	}
	if displayPath == "/" || srcRel == "." {
		return false, ErrInvalidRequest
	}
	parent := path.Dir(srcRel)
	dstRel := validName
	if parent != "." && parent != "/" {
		dstRel = parent + "/" + validName
	}
	if err := share.Rename(srcRel, dstRel); err != nil {
		return false, ErrOperationFailed
	}
	return true, nil
}

// Usage 查询 SMB 共享空间配额。
func (p *SMBProvider) Usage(ctx context.Context, storageName string) (Usage, error) {
	share, _, err := p.openShare(ctx, storageName)
	if err != nil {
		return Usage{}, err
	}
	defer func() { _ = share.Close() }()

	total, avail, err := share.Statfs(".")
	if err != nil {
		return Usage{}, ErrOperationFailed
	}
	return Usage{Total: float64(total), Available: float64(avail)}, nil
}

// TransType 返回 V2 SMB 支持的整理方式映射。
func (p *SMBProvider) TransType(ctx context.Context, storageName string) (map[string]any, error) {
	if _, err := p.snapshotConfig(ctx, storageName); err != nil {
		return nil, err
	}
	return map[string]any{
		"move": "移动",
		"copy": "复制",
		"link": "硬链接",
	}, nil
}

// Open 只读打开 SMB 文件流，关闭流时同步释放 SMB 共享与底层连接。
func (p *SMBProvider) Open(ctx context.Context, storageName, requested string) (io.ReadCloser, string, error) {
	share, cfg, err := p.openShare(ctx, storageName)
	if err != nil {
		return nil, "", err
	}
	rel, _, err := resolveSMBRelativePath(cfg.Root, requested)
	if err != nil {
		_ = share.Close()
		return nil, "", err
	}
	if rel == "." {
		_ = share.Close()
		return nil, "", ErrInvalidRequest
	}
	info, err := share.Stat(rel)
	if err != nil || info.IsDir() {
		_ = share.Close()
		return nil, "", ErrOperationFailed
	}
	file, err := share.Open(rel)
	if err != nil {
		_ = share.Close()
		return nil, "", ErrOperationFailed
	}
	contentType := mime.TypeByExtension(strings.ToLower(path.Ext(rel)))
	if contentType == "" {
		contentType = "application/octet-stream"
	}
	return &smbFileCloser{ReadCloser: file, share: share}, contentType, nil
}

type smbFileCloser struct {
	io.ReadCloser
	share smbFS
}

func (c *smbFileCloser) Close() error {
	fileErr := c.ReadCloser.Close()
	shareErr := c.share.Close()
	if fileErr != nil {
		return fileErr
	}
	return shareErr
}

// QRCode SMB 不支持扫码登录。
func (p *SMBProvider) QRCode(ctx context.Context, storageName string) (any, string, error) {
	if _, err := p.snapshotConfig(ctx, storageName); err != nil {
		return nil, "存储实现未接入", err
	}
	return nil, "SMB 存储不支持扫码登录", nil
}

// AuthURL SMB 不支持 OAuth 授权。
func (p *SMBProvider) AuthURL(ctx context.Context, storageName string) (any, string, error) {
	if _, err := p.snapshotConfig(ctx, storageName); err != nil {
		return nil, "存储实现未接入", err
	}
	return nil, "SMB 存储不支持 OAuth 授权", nil
}

// CheckLogin SMB 无需登录确认。
func (p *SMBProvider) CheckLogin(ctx context.Context, storageName, _, _ string) (any, string, error) {
	if _, err := p.snapshotConfig(ctx, storageName); err != nil {
		return nil, "存储实现未接入", err
	}
	return nil, "SMB 存储无需登录确认", nil
}

// SaveConfig 动态保存并应用 SMB 连接配置。
func (p *SMBProvider) SaveConfig(ctx context.Context, storageName string, conf map[string]any) error {
	cfg, err := p.snapshotConfig(ctx, storageName)
	if err != nil {
		return err
	}
	p.mu.Lock()
	if v, ok := stringConfigValue(conf, "host"); ok {
		p.config.Host = v
	}
	if v, ok := stringConfigValue(conf, "share"); ok {
		p.config.Share = strings.Trim(v, `/\`)
	}
	if v, ok := stringConfigValue(conf, "username"); ok {
		p.config.Username = v
	}
	if v, ok := stringConfigValue(conf, "password"); ok {
		p.config.Password = v
	}
	if v, ok := stringConfigValue(conf, "domain"); ok {
		p.config.Domain = v
	}
	if v, ok := stringConfigValue(conf, "root"); ok {
		p.config.Root = cleanSMBRoot(v)
	}
	if port, ok := intConfigValue(conf, "port"); ok && port > 0 && port <= 65535 {
		p.config.Port = port
	}
	persisted := p.exportConfigLocked()
	onChange := cfg.OnConfigChange
	p.mu.Unlock()

	if onChange != nil {
		return onChange(ctx, persisted)
	}
	return nil
}

// ResetConfig 清空 SMB 连接配置。
func (p *SMBProvider) ResetConfig(ctx context.Context, storageName string) error {
	cfg, err := p.snapshotConfig(ctx, storageName)
	if err != nil {
		return err
	}
	p.mu.Lock()
	p.config.Host = ""
	p.config.Port = defaultSMBPort
	p.config.Share = ""
	p.config.Username = ""
	p.config.Password = ""
	p.config.Domain = ""
	p.config.Root = ""
	onChange := cfg.OnConfigChange
	p.mu.Unlock()

	if onChange != nil {
		return onChange(ctx, map[string]any{})
	}
	return nil
}

func (p *SMBProvider) exportConfigLocked() map[string]any {
	return map[string]any{
		"host":     p.config.Host,
		"port":     p.config.Port,
		"share":    p.config.Share,
		"username": p.config.Username,
		"password": p.config.Password,
		"domain":   p.config.Domain,
		"root":     p.config.Root,
	}
}

type realSMBShare struct {
	conn    net.Conn
	session *smb2.Session
	share   *smb2.Share
}

func dialSMBShare(ctx context.Context, cfg SMBConfig) (smbFS, error) {
	timeout := cfg.Timeout
	if timeout <= 0 {
		timeout = defaultSMBTimeout
	}
	dialer := &net.Dialer{Timeout: timeout}
	addr := net.JoinHostPort(cfg.Host, strconv.Itoa(cfg.Port))
	conn, err := dialer.DialContext(ctx, "tcp", addr)
	if err != nil {
		return nil, err
	}
	if deadline, ok := ctx.Deadline(); ok {
		_ = conn.SetDeadline(deadline)
	} else {
		_ = conn.SetDeadline(time.Now().Add(timeout))
	}
	smbDialer := &smb2.Dialer{
		Initiator: &smb2.NTLMInitiator{
			User:     cfg.Username,
			Password: cfg.Password,
			Domain:   cfg.Domain,
		},
	}
	session, err := smbDialer.DialContext(ctx, conn)
	if err != nil {
		_ = conn.Close()
		return nil, err
	}
	share, err := session.Mount(cfg.Share)
	if err != nil {
		_ = session.Logoff()
		_ = conn.Close()
		return nil, err
	}
	return &realSMBShare{
		conn:    conn,
		session: session,
		share:   share.WithContext(ctx),
	}, nil
}

func (s *realSMBShare) ReadDir(name string) ([]os.FileInfo, error) {
	return s.share.ReadDir(name)
}

func (s *realSMBShare) Stat(name string) (os.FileInfo, error) {
	return s.share.Stat(name)
}

func (s *realSMBShare) MkdirAll(name string, perm os.FileMode) error {
	return s.share.MkdirAll(name, perm)
}

func (s *realSMBShare) RemoveAll(name string) error {
	return s.share.RemoveAll(name)
}

func (s *realSMBShare) Rename(oldpath, newpath string) error {
	return s.share.Rename(oldpath, newpath)
}

func (s *realSMBShare) Open(name string) (io.ReadCloser, error) {
	return s.share.Open(name)
}

func (s *realSMBShare) Statfs(name string) (uint64, uint64, error) {
	info, err := s.share.Statfs(name)
	if err != nil {
		return 0, 0, err
	}
	blockSize := info.BlockSize()
	if blockSize == 0 {
		blockSize = info.FragmentSize()
	}
	availBlocks := info.AvailableBlockCount()
	if availBlocks == 0 {
		availBlocks = info.FreeBlockCount()
	}
	return info.TotalBlockCount() * blockSize, availBlocks * blockSize, nil
}

func (s *realSMBShare) Close() error {
	var firstErr error
	if s.share != nil {
		if err := s.share.Umount(); err != nil && firstErr == nil {
			firstErr = err
		}
	}
	if s.session != nil {
		if err := s.session.Logoff(); err != nil && firstErr == nil {
			firstErr = err
		}
	}
	if s.conn != nil {
		if err := s.conn.Close(); err != nil && firstErr == nil {
			firstErr = err
		}
	}
	return firstErr
}

func stringConfigValue(conf map[string]any, key string) (string, bool) {
	if conf == nil {
		return "", false
	}
	raw, ok := conf[key]
	if !ok || raw == nil {
		return "", false
	}
	switch v := raw.(type) {
	case string:
		return strings.TrimSpace(v), true
	default:
		return strings.TrimSpace(fmt.Sprint(v)), true
	}
}

func intConfigValue(conf map[string]any, key string) (int, bool) {
	if conf == nil {
		return 0, false
	}
	raw, ok := conf[key]
	if !ok || raw == nil {
		return 0, false
	}
	switch v := raw.(type) {
	case int:
		return v, true
	case int64:
		return int(v), true
	case float64:
		return int(v), true
	case string:
		n, err := strconv.Atoi(strings.TrimSpace(v))
		if err != nil {
			return 0, false
		}
		return n, true
	default:
		return 0, false
	}
}

func int64ConfigValue(conf map[string]any, key string) (int64, bool) {
	if conf == nil {
		return 0, false
	}
	raw, ok := conf[key]
	if !ok || raw == nil {
		return 0, false
	}
	switch v := raw.(type) {
	case int:
		return int64(v), true
	case int64:
		return v, true
	case float64:
		return int64(v), true
	case string:
		n, err := strconv.ParseInt(strings.TrimSpace(v), 10, 64)
		if err != nil {
			return 0, false
		}
		return n, true
	default:
		return 0, false
	}
}

var _ Provider = (*SMBProvider)(nil)
var _ FileOpener = (*SMBProvider)(nil)
var _ AuthManager = (*SMBProvider)(nil)
