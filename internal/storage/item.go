// Package storage 迁移 V2 存储浏览/管理接口的本地适配。
package storage

import (
	"context"
	"errors"
)

var (
	// ErrInvalidRequest 表示存储请求参数无效。
	ErrInvalidRequest = errors.New("存储请求参数无效")
	// ErrPathRejected 表示路径超出允许范围或存在越权段。
	ErrPathRejected = errors.New("存储路径被拒绝")
	// ErrUnsupportedStorage 表示请求的存储实现未接入。
	ErrUnsupportedStorage = errors.New("存储实现未接入")
	// ErrOperationFailed 表示底层存储操作失败。
	ErrOperationFailed = errors.New("存储操作失败")
)

// Item 对应 V2 `schemas.FileItem`。
type Item struct {
	Type         string   `json:"type,omitempty"`
	Name         string   `json:"name,omitempty"`
	Basename     string   `json:"basename,omitempty"`
	Extension    string   `json:"extension,omitempty"`
	Size         *int64   `json:"size,omitempty"`
	ModifyTime   *float64 `json:"modify_time,omitempty"`
	Children     []Item   `json:"children"`
	FileID       string   `json:"fileid,omitempty"`
	ParentFileID string   `json:"parent_fileid,omitempty"`
	Thumbnail    string   `json:"thumbnail,omitempty"`
	Path         string   `json:"path,omitempty"`
	Storage      string   `json:"storage,omitempty"`
}

// Usage 对应 V2 `schemas.StorageUsage`。
type Usage struct {
	Total     float64 `json:"total"`
	Available float64 `json:"available"`
}

// Provider 是存储域消费的最小能力集合。
type Provider interface {
	List(ctx context.Context, storageName, path, sort, keyword string) ([]Item, error)
	Mkdir(ctx context.Context, storageName, path, name string) (bool, error)
	Delete(ctx context.Context, storageName, path string) (bool, error)
	Rename(ctx context.Context, storageName, path, newName string, recursive bool) (bool, error)
	Usage(ctx context.Context, storageName string) (Usage, error)
	TransType(ctx context.Context, storageName string) (map[string]any, error)
}
