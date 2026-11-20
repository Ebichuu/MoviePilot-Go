package storage

import (
	"context"
	"strings"
)

// Mkdir 通过 `/operations/mkdir` 创建远端目录。
func (p *RcloneProvider) Mkdir(ctx context.Context, storageName, requested, name string) (bool, error) {
	if err := p.require(ctx, storageName); err != nil {
		return false, err
	}
	name = strings.TrimSpace(name)
	if name == "" || strings.ContainsAny(name, `/\`) {
		return false, ErrInvalidRequest
	}
	if err := p.call(ctx, "/operations/mkdir", map[string]any{
		"fs": p.config.FileSystem, "remote": rcloneJoin(requested, name),
	}, nil); err != nil {
		return false, err
	}
	return true, nil
}

// Delete 先按文件删除，失败后按目录递归清理（V2 delete_file 的远端语义）。
func (p *RcloneProvider) Delete(ctx context.Context, storageName, requested string) (bool, error) {
	if err := p.require(ctx, storageName); err != nil {
		return false, err
	}
	remote := strings.Trim(strings.TrimSpace(requested), "/")
	if remote == "" {
		return false, ErrInvalidRequest
	}
	fileErr := p.call(ctx, "/operations/deletefile", map[string]any{
		"fs": p.config.FileSystem, "remote": remote,
	}, nil)
	if fileErr == nil {
		return true, nil
	}
	if err := p.call(ctx, "/operations/purge", map[string]any{
		"fs": p.config.FileSystem, "remote": remote,
	}, nil); err != nil {
		return false, err
	}
	return true, nil
}

// Rename 通过 `/operations/movefile`（文件）或 `/operations/move`（目录）重命名。
func (p *RcloneProvider) Rename(
	ctx context.Context,
	storageName, requested, newName string,
	recursive bool,
) (bool, error) {
	if err := p.require(ctx, storageName); err != nil {
		return false, err
	}
	newName = strings.TrimSpace(newName)
	if newName == "" || strings.ContainsAny(newName, `/\`) {
		return false, ErrInvalidRequest
	}
	srcRemote := strings.Trim(strings.TrimSpace(requested), "/")
	if srcRemote == "" {
		return false, ErrInvalidRequest
	}
	dstRemote := rcloneJoin(directoryOf(srcRemote), newName)
	moveFile := func() error {
		return p.call(ctx, "/operations/movefile", map[string]any{
			"srcFs": p.config.FileSystem, "srcRemote": srcRemote,
			"dstFs": p.config.FileSystem, "dstRemote": dstRemote,
		}, nil)
	}
	moveDir := func() error {
		return p.call(ctx, "/operations/move", map[string]any{
			"srcFs": p.config.FileSystem, "srcRemote": srcRemote,
			"dstFs": p.config.FileSystem, "dstRemote": dstRemote,
		}, nil)
	}
	primary, fallback := moveFile, moveDir
	if recursive {
		primary, fallback = moveDir, moveFile
	}
	if err := primary(); err != nil {
		if err := fallback(); err != nil {
			return false, err
		}
	}
	return true, nil
}

// Usage 通过 `/operations/about` 返回远端空间。
func (p *RcloneProvider) Usage(ctx context.Context, storageName string) (Usage, error) {
	if err := p.require(ctx, storageName); err != nil {
		return Usage{}, err
	}
	var response struct {
		Total int64 `json:"total"`
		Free  int64 `json:"free"`
	}
	if err := p.call(ctx, "/operations/about", map[string]any{
		"fs": p.config.FileSystem,
	}, &response); err != nil {
		return Usage{}, err
	}
	return Usage{Total: float64(response.Total), Available: float64(response.Free)}, nil
}

// TransType 返回 rclone 支持的整理方式（移动/复制，软链接不适用）。
func (p *RcloneProvider) TransType(ctx context.Context, storageName string) (map[string]any, error) {
	if err := p.require(ctx, storageName); err != nil {
		return nil, err
	}
	return map[string]any{"move": true, "copy": true, "link": false}, nil
}

func directoryOf(remote string) string {
	index := strings.LastIndex(remote, "/")
	if index < 0 {
		return ""
	}
	return remote[:index]
}

var _ Provider = (*RcloneProvider)(nil)
