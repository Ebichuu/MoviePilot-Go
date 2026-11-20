package storage

import (
	"context"
	"io"
	"mime"
	"os"
	"path/filepath"
	"strings"
)

// FileOpener 是可选能力：按存储路径打开只读文件流。
type FileOpener interface {
	Open(ctx context.Context, storageName, path string) (io.ReadCloser, string, error)
}

// Open 打开受限根目录内的只读文件，返回内容流与内容类型。
func (p *LocalProvider) Open(
	ctx context.Context,
	storageName, requested string,
) (io.ReadCloser, string, error) {
	root, err := p.root(storageName)
	if err != nil {
		return nil, "", err
	}
	if ctx == nil {
		return nil, "", ErrInvalidRequest
	}
	if err := ctx.Err(); err != nil {
		return nil, "", err
	}
	target, err := resolvePath(root, requested)
	if err != nil {
		return nil, "", err
	}
	info, err := os.Stat(target)
	if err != nil || info.IsDir() {
		return nil, "", ErrOperationFailed
	}
	file, err := os.Open(target)
	if err != nil {
		return nil, "", ErrOperationFailed
	}
	contentType := mime.TypeByExtension(strings.ToLower(filepath.Ext(target)))
	if contentType == "" {
		contentType = "application/octet-stream"
	}
	return file, contentType, nil
}

// QRCode 返回本地存储不支持扫码登录的稳定结果（V2 本地实现同样返回 falsy）。
func (p *LocalProvider) QRCode(ctx context.Context, storageName string) (any, string, error) {
	if _, err := p.root(storageName); err != nil {
		return nil, "存储实现未接入", err
	}
	return nil, "该存储不支持扫码登录", nil
}

// AuthURL 返回本地存储不支持 OAuth 授权的稳定结果。
func (p *LocalProvider) AuthURL(ctx context.Context, storageName string) (any, string, error) {
	if _, err := p.root(storageName); err != nil {
		return nil, "存储实现未接入", err
	}
	return nil, "该存储不支持 OAuth 授权", nil
}

// CheckLogin 返回本地存储不支持登录确认的稳定结果。
func (p *LocalProvider) CheckLogin(ctx context.Context, storageName, ck, token string) (any, string, error) {
	if _, err := p.root(storageName); err != nil {
		return nil, "存储实现未接入", err
	}
	return nil, "该存储无需登录确认", nil
}

// SaveConfig 本地存储没有远端配置，保存为无操作成功。
func (p *LocalProvider) SaveConfig(ctx context.Context, storageName string, conf map[string]any) error {
	_, err := p.root(storageName)
	return err
}

// ResetConfig 本地存储没有远端配置，重置为无操作成功。
func (p *LocalProvider) ResetConfig(ctx context.Context, storageName string) error {
	_, err := p.root(storageName)
	return err
}

var _ FileOpener = (*LocalProvider)(nil)
var _ AuthManager = (*LocalProvider)(nil)

// AuthManager 是本地实现提供的远端存储登录/配置能力（不支持项返回稳定结果）。
type AuthManager interface {
	QRCode(ctx context.Context, storageName string) (any, string, error)
	AuthURL(ctx context.Context, storageName string) (any, string, error)
	CheckLogin(ctx context.Context, storageName, ck, token string) (any, string, error)
	SaveConfig(ctx context.Context, storageName string, conf map[string]any) error
	ResetConfig(ctx context.Context, storageName string) error
}
