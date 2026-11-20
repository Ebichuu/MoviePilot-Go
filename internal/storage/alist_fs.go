package storage

import (
	"context"
	"path"
	"strings"
	"time"
)

func (p *AlistProvider) remotePath(requested string) string {
	cleaned := strings.Trim(path.Clean("/"+strings.TrimSpace(requested)), "/")
	if cleaned == "" {
		return p.config.Root
	}
	if p.config.Root == "/" {
		return "/" + cleaned
	}
	return p.config.Root + "/" + cleaned
}

type alistFile struct {
	Name     string `json:"name"`
	Size     int64  `json:"size"`
	IsDir    bool   `json:"is_dir"`
	Modified string `json:"modified"`
}

type alistListData struct {
	Content []alistFile `json:"content"`
}

// openlistMaxListPageSize 复刻 V2 `OPENLIST_MAX_LIST_PAGE_SIZE`：显式指定最大页大小，
// 避免 AList/OpenList 在 per_page<=0 时退回后端默认 200 导致大目录被截断。
const (
	openlistMaxListPageSize = 500
	alistMaxListItems       = 10_000
)

// List 通过 `/api/fs/list` 分页读取远端目录（V2 `alist.py:198–243`）。
func (p *AlistProvider) List(
	ctx context.Context,
	storageName, requested, sortKey, keyword string,
) ([]Item, error) {
	if err := p.require(ctx, storageName); err != nil {
		return nil, err
	}
	if err := p.ensureToken(ctx); err != nil {
		return nil, err
	}
	items := make([]Item, 0, openlistMaxListPageSize)
	for page := 1; ; page++ {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		var data alistListData
		if err := p.request(ctx, "/api/fs/list", map[string]any{
			"path": p.remotePath(requested), "password": "", "page": page,
			"per_page": openlistMaxListPageSize, "refresh": false,
		}, &data); err != nil {
			return nil, err
		}
		if len(data.Content) == 0 {
			break
		}
		items = append(items, p.projectItems(requested, data.Content)...)
		if len(data.Content) < openlistMaxListPageSize || len(items) >= alistMaxListItems {
			break
		}
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

func (p *AlistProvider) projectItems(requested string, entries []alistFile) []Item {
	items := make([]Item, 0, len(entries))
	for _, entry := range entries {
		itemType := "file"
		if entry.IsDir {
			itemType = "dir"
		}
		item := Item{
			Type: itemType, Name: entry.Name,
			Basename: strings.TrimSuffix(entry.Name, path.Ext(entry.Name)),
			Children: []Item{}, FileID: entry.Name,
			Path:    rcloneJoin(requested, entry.Name),
			Storage: p.config.Name,
		}
		if itemType == "file" {
			if extension := strings.TrimPrefix(path.Ext(entry.Name), "."); extension != "" {
				item.Extension = extension
			}
			size := entry.Size
			item.Size = &size
		}
		if modified, err := time.Parse(time.RFC3339, entry.Modified); err == nil {
			seconds := float64(modified.Unix())
			item.ModifyTime = &seconds
		}
		items = append(items, item)
	}
	return items
}

// Mkdir 通过 `/api/fs/mkdir` 创建远端目录。
func (p *AlistProvider) Mkdir(ctx context.Context, storageName, requested, name string) (bool, error) {
	if err := p.require(ctx, storageName); err != nil {
		return false, err
	}
	name = strings.TrimSpace(name)
	if name == "" || strings.ContainsAny(name, `/\`) {
		return false, ErrInvalidRequest
	}
	if err := p.ensureToken(ctx); err != nil {
		return false, err
	}
	if err := p.request(ctx, "/api/fs/mkdir", map[string]any{
		"path": p.remotePath(rcloneJoin(requested, name)),
	}, nil); err != nil {
		return false, err
	}
	return true, nil
}

// Delete 通过 `/api/fs/remove` 删除文件或目录。
func (p *AlistProvider) Delete(ctx context.Context, storageName, requested string) (bool, error) {
	if err := p.require(ctx, storageName); err != nil {
		return false, err
	}
	remote := strings.Trim(strings.TrimSpace(requested), "/")
	if remote == "" {
		return false, ErrInvalidRequest
	}
	if err := p.ensureToken(ctx); err != nil {
		return false, err
	}
	directory := directoryOf(remote)
	name := remote
	if index := strings.LastIndex(remote, "/"); index >= 0 {
		name = remote[index+1:]
	}
	if err := p.request(ctx, "/api/fs/remove", map[string]any{
		"dir": p.remotePath(directory), "names": []string{name},
	}, nil); err != nil {
		return false, err
	}
	return true, nil
}

// Rename 通过 `/api/fs/rename` 重命名文件或目录。
func (p *AlistProvider) Rename(
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
	remote := strings.Trim(strings.TrimSpace(requested), "/")
	if remote == "" {
		return false, ErrInvalidRequest
	}
	if err := p.ensureToken(ctx); err != nil {
		return false, err
	}
	if err := p.request(ctx, "/api/fs/rename", map[string]any{
		"path": p.remotePath(remote), "name": newName,
	}, nil); err != nil {
		return false, err
	}
	return true, nil
}

// Usage 返回零值空间信息（Alist 未提供统一容量接口）。
func (p *AlistProvider) Usage(ctx context.Context, storageName string) (Usage, error) {
	if err := p.require(ctx, storageName); err != nil {
		return Usage{}, err
	}
	return Usage{}, nil
}

// TransType 返回 Alist 支持的整理方式（移动/复制，软链接不适用）。
func (p *AlistProvider) TransType(ctx context.Context, storageName string) (map[string]any, error) {
	if err := p.require(ctx, storageName); err != nil {
		return nil, err
	}
	return map[string]any{"move": true, "copy": true, "link": false}, nil
}

var _ Provider = (*AlistProvider)(nil)
