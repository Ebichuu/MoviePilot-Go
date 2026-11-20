package storage

import (
	"context"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"
	"sync"
)

// LocalProvider 在受限根目录内提供本地存储浏览与管理。
type LocalProvider struct {
	mu    sync.RWMutex
	roots map[string]string
}

// NewLocalProvider 创建本地存储适配器；roots 为存储名到绝对根目录的映射。
func NewLocalProvider(roots map[string]string) *LocalProvider {
	provider := &LocalProvider{roots: make(map[string]string, len(roots))}
	for name, root := range roots {
		provider.Register(name, root)
	}
	return provider
}

// Register 注册或替换一个存储名对应的根目录。
func (p *LocalProvider) Register(name, root string) {
	name = strings.TrimSpace(name)
	root = strings.TrimSpace(root)
	if p == nil || name == "" || root == "" {
		return
	}
	absolute, err := filepath.Abs(root)
	if err != nil {
		return
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	p.roots[name] = filepath.Clean(absolute)
}

func (p *LocalProvider) root(name string) (string, error) {
	if p == nil {
		return "", ErrUnsupportedStorage
	}
	p.mu.RLock()
	defer p.mu.RUnlock()
	root, ok := p.roots[strings.TrimSpace(name)]
	if !ok {
		return "", ErrUnsupportedStorage
	}
	return root, nil
}

func resolvePath(root, requested string) (string, error) {
	requested = strings.TrimSpace(requested)
	if requested == "" {
		return root, nil
	}
	cleaned := filepath.Clean(filepath.FromSlash(requested))
	if filepath.IsAbs(cleaned) {
		return "", ErrPathRejected
	}
	for _, segment := range strings.Split(filepath.ToSlash(cleaned), "/") {
		if segment == ".." {
			return "", ErrPathRejected
		}
	}
	target := filepath.Join(root, cleaned)
	relative, err := filepath.Rel(root, target)
	if err != nil || relative == ".." || strings.HasPrefix(relative, ".."+string(filepath.Separator)) {
		return "", ErrPathRejected
	}
	return target, nil
}

// List 复刻 V2 `StorageChain().list_files` 的目录投影与排序/过滤。
func (p *LocalProvider) List(
	ctx context.Context,
	storageName, requested, sortKey, keyword string,
) ([]Item, error) {
	root, err := p.root(storageName)
	if err != nil {
		return nil, err
	}
	if ctx == nil {
		return nil, ErrInvalidRequest
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	target, err := resolvePath(root, requested)
	if err != nil {
		return nil, err
	}
	entries, err := os.ReadDir(target)
	if err != nil {
		return nil, ErrOperationFailed
	}
	items := make([]Item, 0, len(entries))
	for _, entry := range entries {
		info, infoErr := entry.Info()
		if infoErr != nil {
			continue
		}
		itemType := "file"
		if entry.IsDir() {
			itemType = "dir"
		}
		item := Item{
			Type: itemType, Name: entry.Name(),
			Basename: strings.TrimSuffix(entry.Name(), filepath.Ext(entry.Name())),
			Children: []Item{}, FileID: entry.Name(),
			Path:    path.Join(strings.Trim(strings.ReplaceAll(requested, "\\", "/"), "/"), entry.Name()),
			Storage: strings.TrimSpace(storageName),
		}
		if itemType == "file" {
			if extension := strings.TrimPrefix(filepath.Ext(entry.Name()), "."); extension != "" {
				item.Extension = extension
			}
			size := info.Size()
			item.Size = &size
		}
		modified := float64(info.ModTime().Unix())
		item.ModifyTime = &modified
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
		sort.SliceStable(items, func(i, j int) bool { return naturalLess(items[i].Name, items[j].Name) })
	} else {
		sort.SliceStable(items, func(i, j int) bool { return itemModifyTime(items[i]) > itemModifyTime(items[j]) })
	}
	return items, nil
}

// Mkdir 复刻 V2 `create_folder`。
func (p *LocalProvider) Mkdir(ctx context.Context, storageName, requested, name string) (bool, error) {
	root, err := p.root(storageName)
	if err != nil {
		return false, err
	}
	name = strings.TrimSpace(name)
	if name == "" || strings.ContainsAny(name, `/\`) {
		return false, ErrInvalidRequest
	}
	target, err := resolvePath(root, requested)
	if err != nil {
		return false, err
	}
	if err := os.MkdirAll(filepath.Join(target, name), 0755); err != nil {
		return false, ErrOperationFailed
	}
	return true, nil
}

// Delete 复刻 V2 `delete_file`。
func (p *LocalProvider) Delete(ctx context.Context, storageName, requested string) (bool, error) {
	root, err := p.root(storageName)
	if err != nil {
		return false, err
	}
	if strings.TrimSpace(requested) == "" {
		return false, ErrInvalidRequest
	}
	target, err := resolvePath(root, requested)
	if err != nil {
		return false, err
	}
	if target == root {
		return false, ErrPathRejected
	}
	if err := os.RemoveAll(target); err != nil {
		return false, ErrOperationFailed
	}
	return true, nil
}

// Rename 复刻 V2 `rename_file`（recursive 仅影响远端存储）。
func (p *LocalProvider) Rename(
	ctx context.Context,
	storageName, requested, newName string,
	recursive bool,
) (bool, error) {
	root, err := p.root(storageName)
	if err != nil {
		return false, err
	}
	newName = strings.TrimSpace(newName)
	if newName == "" || strings.ContainsAny(newName, `/\`) {
		return false, ErrInvalidRequest
	}
	target, err := resolvePath(root, requested)
	if err != nil {
		return false, err
	}
	if target == root {
		return false, ErrPathRejected
	}
	if err := os.Rename(target, filepath.Join(filepath.Dir(target), newName)); err != nil {
		return false, ErrOperationFailed
	}
	return true, nil
}

// Usage 返回本地存储所在卷的空间（V2 本地存储 `usage()` 的同口径字节值）。
func (p *LocalProvider) Usage(ctx context.Context, storageName string) (Usage, error) {
	root, err := p.root(storageName)
	if err != nil {
		return Usage{}, err
	}
	total, available, ok := filesystemSpace(root)
	if !ok {
		return Usage{}, nil
	}
	return Usage{Total: float64(total), Available: float64(available)}, nil
}

// TransType 返回支持的整理方式；本地实现暂未声明，返回空对象。
func (p *LocalProvider) TransType(ctx context.Context, storageName string) (map[string]any, error) {
	if _, err := p.root(storageName); err != nil {
		return nil, err
	}
	return map[string]any{}, nil
}

func itemModifyTime(item Item) float64 {
	if item.ModifyTime == nil {
		return 0
	}
	return *item.ModifyTime
}
