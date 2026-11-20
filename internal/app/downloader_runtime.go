package app

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"

	"github.com/moviepilot-go/moviepilot-go/internal/downloadconfig"
	"github.com/moviepilot-go/moviepilot-go/internal/downloader"
	"github.com/moviepilot-go/moviepilot-go/internal/downloadflow"
	"github.com/moviepilot-go/moviepilot-go/internal/httpapi"
)

type downloaderRuntimeStore interface {
	LoadDownloaders(context.Context) ([]downloadconfig.Downloader, bool, error)
}

type downloadClient interface {
	downloader.DownloadingTaskLister
	CloseIdleConnections()
}

type downloadClientEntry struct {
	name      string
	isDefault bool
	client    downloadClient
}

type downloadRuntime struct {
	mu         sync.Mutex
	submitters *downloadflow.SubmitterRegistry
	entries    map[string]downloadflow.TorrentSubmitter
	clients    []downloadClientEntry
}

func (r *downloadRuntime) CloseIdleConnections() {
	if r == nil {
		return
	}
	r.mu.Lock()
	clients := r.clients
	r.clients = nil
	r.mu.Unlock()
	closeDownloadClients(clients)
}

func (r *downloadRuntime) Reload(ctx context.Context, store downloaderRuntimeStore, requestTimeout time.Duration, logger *slog.Logger) error {
	if r == nil || r.submitters == nil {
		return errors.New("下载器运行时不可用")
	}
	r.mu.Lock()
	next, err := loadDownloadRuntime(ctx, store, requestTimeout, logger)
	if err != nil {
		r.mu.Unlock()
		return err
	}
	if err := r.submitters.Replace(next.entries); err != nil {
		r.mu.Unlock()
		next.CloseIdleConnections()
		return fmt.Errorf("替换下载器注册表: %w", err)
	}
	previousClients := r.clients
	r.entries = next.entries
	r.clients = next.clients
	next.clients = nil
	r.mu.Unlock()
	closeDownloadClients(previousClients)
	return nil
}

func closeDownloadClients(clients []downloadClientEntry) {
	for _, entry := range clients {
		entry.client.CloseIdleConnections()
	}
}

// TransferStats 汇总所有实现 downloader.TransferStatReader 的启用下载器。速率与累计
// 流量按 V2 相加；剩余空间取其中最大值（多下载器常常指向同一块盘，相加会重复计算）。
func (r *downloadRuntime) TransferStats(ctx context.Context) (downloader.TransferStats, error) {
	if r == nil || ctx == nil {
		return downloader.TransferStats{}, errors.New("下载器统计不可用")
	}
	if err := ctx.Err(); err != nil {
		return downloader.TransferStats{}, err
	}
	r.mu.Lock()
	clients := append([]downloadClientEntry(nil), r.clients...)
	r.mu.Unlock()

	var total downloader.TransferStats
	readers := 0
	for _, entry := range clients {
		reader, ok := entry.client.(downloader.TransferStatReader)
		if !ok {
			continue
		}
		stats, err := reader.TransferStats(ctx)
		if err != nil {
			continue
		}
		readers++
		total.DownloadSpeed += stats.DownloadSpeed
		total.UploadSpeed += stats.UploadSpeed
		total.DownloadSize += stats.DownloadSize
		total.UploadSize += stats.UploadSize
		if stats.FreeSpace > total.FreeSpace {
			total.FreeSpace = stats.FreeSpace
		}
		if stats.UsedSpace > total.UsedSpace {
			total.UsedSpace = stats.UsedSpace
		}
	}
	if readers == 0 {
		return downloader.TransferStats{}, downloader.ErrNoTransferStats
	}
	return total, nil
}

func (r *downloadRuntime) ListDownloading(ctx context.Context, name string) ([]httpapi.NamedDownloadTask, error) {
	if r == nil || ctx == nil {
		return nil, errors.New("下载任务查询不可用")
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	name = strings.TrimSpace(name)
	r.mu.Lock()
	entries := make([]downloadClientEntry, 0, len(r.clients))
	for _, entry := range r.clients {
		if name == "" || strings.EqualFold(entry.name, name) {
			entries = append(entries, entry)
		}
	}
	r.mu.Unlock()

	const (
		maxConcurrentQueries = 4
		maxTasks             = 1000
	)
	tasks := make([]httpapi.NamedDownloadTask, 0)
	for start := 0; start < len(entries) && len(tasks) < maxTasks; start += maxConcurrentQueries {
		end := min(start+maxConcurrentQueries, len(entries))
		results := make([]downloadClientResult, end-start)
		var group sync.WaitGroup
		for index := start; index < end; index++ {
			resultIndex := index - start
			group.Add(1)
			go func(entry downloadClientEntry) {
				defer group.Done()
				results[resultIndex].tasks, results[resultIndex].err = entry.client.ListDownloading(ctx)
			}(entries[index])
		}
		group.Wait()

		for index, result := range results {
			if result.err != nil {
				if err := ctx.Err(); err != nil {
					return nil, err
				}
				return nil, errors.New("查询下载任务失败")
			}
			entry := entries[start+index]
			remaining := maxTasks - len(tasks)
			if len(result.tasks) > remaining {
				result.tasks = result.tasks[:remaining]
			}
			for _, task := range result.tasks {
				tasks = append(tasks, httpapi.NamedDownloadTask{Downloader: entry.name, Task: task})
			}
		}
	}
	return tasks, nil
}

// ListDownloadHistory 汇总各下载器已完成的种子，供「下载历史」使用。
// 支持历史查询的下载器（qBittorrent/Transmission/rTorrent）才参与汇总，
// 其余下载器直接跳过，不伪造空记录。
func (r *downloadRuntime) ListDownloadHistory(
	ctx context.Context,
	name string,
) ([]httpapi.NamedDownloadTask, error) {
	if r == nil || ctx == nil {
		return nil, errors.New("下载历史查询不可用")
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	name = strings.TrimSpace(name)
	r.mu.Lock()
	entries := make([]downloadClientEntry, 0, len(r.clients))
	for _, entry := range r.clients {
		if name == "" || strings.EqualFold(entry.name, name) {
			entries = append(entries, entry)
		}
	}
	r.mu.Unlock()

	const maxTasks = 2000
	tasks := make([]httpapi.NamedDownloadTask, 0)
	for _, entry := range entries {
		if len(tasks) >= maxTasks {
			break
		}
		lister, ok := entry.client.(downloader.CompletedTaskLister)
		if !ok {
			continue
		}
		completed, err := lister.ListCompleted(ctx)
		if err != nil {
			if ctxErr := ctx.Err(); ctxErr != nil {
				return nil, ctxErr
			}
			return nil, errors.New("查询下载历史失败")
		}
		for _, task := range completed {
			if len(tasks) >= maxTasks {
				break
			}
			tasks = append(tasks, httpapi.NamedDownloadTask{Downloader: entry.name, Task: task})
		}
	}
	return tasks, nil
}
func (r *downloadRuntime) FindDownloadTask(
	ctx context.Context,
	name string,
	hash string,
) (httpapi.NamedDownloadTask, bool, error) {
	if r == nil || ctx == nil {
		return httpapi.NamedDownloadTask{}, false, errors.New("下载任务查询不可用")
	}
	if err := ctx.Err(); err != nil {
		return httpapi.NamedDownloadTask{}, false, err
	}
	entry, ok := r.controlClient(name)
	if !ok {
		return httpapi.NamedDownloadTask{}, false, nil
	}
	controller, ok := entry.client.(downloader.TaskController)
	if !ok {
		return httpapi.NamedDownloadTask{}, false, nil
	}
	task, found, err := controller.InspectControlTask(ctx, hash)
	if err != nil || !found {
		return httpapi.NamedDownloadTask{}, found, err
	}
	return httpapi.NamedDownloadTask{Downloader: entry.name, Task: task}, true, nil
}

func (r *downloadRuntime) ControlDownloadTask(
	ctx context.Context,
	name string,
	hash string,
	action downloader.TaskControlAction,
) error {
	if r == nil || ctx == nil {
		return errors.New("下载任务控制不可用")
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	entry, ok := r.controlClient(name)
	if !ok {
		return errors.New("下载任务控制不可用")
	}
	controller, ok := entry.client.(downloader.TaskController)
	if !ok {
		return errors.New("下载任务控制不可用")
	}
	return controller.ControlTask(ctx, hash, action)
}

func (r *downloadRuntime) controlClient(name string) (downloadClientEntry, bool) {
	name = strings.TrimSpace(name)
	r.mu.Lock()
	defer r.mu.Unlock()
	if name != "" {
		for _, entry := range r.clients {
			if entry.name == name {
				return entry, true
			}
		}
		return downloadClientEntry{}, false
	}
	var first downloadClientEntry
	hasFirst := false
	for _, entry := range r.clients {
		if !hasFirst {
			first = entry
			hasFirst = true
		}
		if entry.isDefault {
			return entry, true
		}
	}
	return first, hasFirst
}

type downloadClientResult struct {
	tasks []downloader.Task
	err   error
}

func loadDownloadRuntime(ctx context.Context, store downloaderRuntimeStore, requestTimeout time.Duration, logger *slog.Logger) (*downloadRuntime, error) {
	if store == nil {
		return nil, errors.New("下载器设置存储不可用")
	}
	settings, found, err := store.LoadDownloaders(ctx)
	if err != nil {
		return nil, errors.New("加载下载器设置失败")
	}
	if logger == nil {
		logger = slog.Default()
	}
	submitters := make(map[string]downloadflow.TorrentSubmitter, len(settings))
	if !found {
		registry, registryErr := downloadflow.NewSubmitterRegistry(submitters)
		return &downloadRuntime{submitters: registry, entries: submitters}, registryErr
	}
	clients := make([]downloadClientEntry, 0, len(settings))
	keepClients := false
	defer func() {
		if !keepClients {
			closeDownloadClients(clients)
		}
	}()
	for _, setting := range settings {
		if !setting.Enabled {
			continue
		}
		if !strings.EqualFold(setting.Type, "qbittorrent") &&
			!strings.EqualFold(setting.Type, "transmission") &&
			!strings.EqualFold(setting.Type, "rtorrent") {
			logger.Warn("跳过尚未支持的下载器", "downloader", setting)
			continue
		}
		runtime, runtimeErr := setting.Runtime()
		if runtimeErr != nil {
			return nil, errors.New("下载器运行时配置无效")
		}
		authentication := runtime.Authentication()
		if strings.EqualFold(setting.Type, "transmission") {
			client, clientErr := downloader.NewTransmission(downloader.TransmissionConfig{
				URL: runtime.Host, Username: authentication.Username(), Password: authentication.Password(),
				Timeout: requestTimeout, MaxConnsPerHost: 4,
			})
			if clientErr != nil {
				return nil, errors.New("Transmission 运行时配置无效")
			}
			submitters[runtime.ID] = downloader.NewTorrentBytesSubmitter(client)
			clients = append(clients, downloadClientEntry{name: runtime.Name, isDefault: setting.Default, client: client})
			logger.Info("已装载下载器", "downloader", setting)
			continue
		}
		if strings.EqualFold(setting.Type, "rtorrent") {
			client, clientErr := downloader.NewRTorrent(downloader.RTorrentConfig{
				URL: runtime.Host, Username: authentication.Username(), Password: authentication.Password(),
				Timeout: requestTimeout, MaxConnsPerHost: 4,
			})
			if clientErr != nil {
				return nil, errors.New("rTorrent 运行时配置无效")
			}
			submitters[runtime.ID] = downloader.NewTorrentBytesSubmitter(client)
			clients = append(clients, downloadClientEntry{name: runtime.Name, isDefault: setting.Default, client: client})
			logger.Info("已装载下载器", "downloader", setting)
			continue
		}
		client, clientErr := downloader.NewQBittorrent(downloader.QBittorrentConfig{
			URL: runtime.Host, Username: authentication.Username(), Password: authentication.Password(), APIKey: authentication.APIKey(),
			Timeout: requestTimeout, MaxConnsPerHost: 4, ForceResume: runtime.ForceResume(),
		})
		if clientErr != nil {
			return nil, errors.New("qBittorrent 运行时配置无效")
		}
		submitters[runtime.ID] = downloader.NewTorrentBytesSubmitter(client)
		clients = append(clients, downloadClientEntry{name: runtime.Name, isDefault: setting.Default, client: client})
		logger.Info("已装载下载器", "downloader", setting)
	}
	registry, err := downloadflow.NewSubmitterRegistry(submitters)
	if err != nil {
		return nil, fmt.Errorf("初始化下载器注册表: %w", err)
	}
	keepClients = true
	return &downloadRuntime{submitters: registry, entries: submitters, clients: clients}, nil
}
