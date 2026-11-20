package downloadflow

import (
	"context"
	"errors"
	"strings"
	"sync"

	"github.com/google/uuid"

	"github.com/moviepilot-go/moviepilot-go/internal/downloader"
)

var (
	// ErrDownloaderUnavailable 表示执行计划引用的下载器当前没有可用客户端。
	ErrDownloaderUnavailable = errors.New("下载器当前不可用")
	// ErrInvalidDownloaderRegistry 表示下载器注册表包含空实例或无效稳定 ID。
	ErrInvalidDownloaderRegistry = errors.New("下载器注册表无效")
)

// TorrentSubmitter 是 Handler 消费的最小种子字节对账与单次提交接口。
type TorrentSubmitter interface {
	downloader.TaskObserver
	Reconcile(ctx context.Context, intent downloader.Intent) (downloader.Intent, bool, error)
	AddOnce(ctx context.Context, intent downloader.Intent, candidate downloader.Candidate, metainfo []byte) (downloader.Intent, error)
}

// EpisodeFileSelectionSubmitter 是下载器逐集筛选的可选能力。保持它独立
// 于 TorrentSubmitter，旧下载器和测试替身可以继续实现最小提交协议。
type EpisodeFileSelectionSubmitter interface {
	SelectEpisodeFiles(ctx context.Context, intent downloader.Intent, episodes []downloader.Episode) error
}

var _ TorrentSubmitter = (*downloader.TorrentBytesSubmitter)(nil)

// SubmitterProvider 按稳定下载器 ID 解析当前运行实例。
type SubmitterProvider interface {
	ResolveSubmitter(downloaderID string) (TorrentSubmitter, error)
}

// SubmitterRegistry 支持设置热更新时原子替换下载器客户端集合。
type SubmitterRegistry struct {
	mu         sync.RWMutex
	submitters map[string]TorrentSubmitter
}

// NewSubmitterRegistry 创建并校验下载器注册表。
func NewSubmitterRegistry(submitters map[string]TorrentSubmitter) (*SubmitterRegistry, error) {
	registry := &SubmitterRegistry{}
	if err := registry.Replace(submitters); err != nil {
		return nil, err
	}
	return registry, nil
}

// Replace 原子替换全部下载器实例；失败时保留旧快照。
func (r *SubmitterRegistry) Replace(submitters map[string]TorrentSubmitter) error {
	if r == nil {
		return ErrInvalidDownloaderRegistry
	}
	normalized := make(map[string]TorrentSubmitter, len(submitters))
	for rawID, submitter := range submitters {
		id, err := normalizeDownloaderID(rawID)
		if err != nil || submitter == nil {
			return ErrInvalidDownloaderRegistry
		}
		if _, exists := normalized[id]; exists {
			return ErrInvalidDownloaderRegistry
		}
		normalized[id] = submitter
	}
	r.mu.Lock()
	r.submitters = normalized
	r.mu.Unlock()
	return nil
}

// ResolveSubmitter 从当前不可变快照取得下载器实例。
func (r *SubmitterRegistry) ResolveSubmitter(downloaderID string) (TorrentSubmitter, error) {
	id, err := normalizeDownloaderID(downloaderID)
	if err != nil || r == nil {
		return nil, ErrDownloaderUnavailable
	}
	r.mu.RLock()
	submitter := r.submitters[id]
	r.mu.RUnlock()
	if submitter == nil {
		return nil, ErrDownloaderUnavailable
	}
	return submitter, nil
}

func normalizeDownloaderID(value string) (string, error) {
	parsed, err := uuid.Parse(strings.TrimSpace(value))
	if err != nil || parsed == uuid.Nil {
		return "", ErrInvalidDownloaderRegistry
	}
	return parsed.String(), nil
}
