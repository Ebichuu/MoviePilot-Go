package app

import (
	"context"
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"path"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/moviepilot-go/moviepilot-go/internal/downloadconfig"
	"github.com/moviepilot-go/moviepilot-go/internal/httpapi"
	"github.com/moviepilot-go/moviepilot-go/internal/media"
	"github.com/moviepilot-go/moviepilot-go/internal/organization"
	"github.com/moviepilot-go/moviepilot-go/internal/organizer"
)

const (
	defaultDirectoryMonitorInterval  = time.Minute
	defaultDirectoryMonitorStability = 5 * time.Second
	directoryMonitorDirectoriesKey   = "Directories"
)

type directoryMonitorStore interface {
	GetSetting(ctx context.Context, key string, target any) (bool, error)
	RecordManualOrganizationWithMode(ctx context.Context, source, target, mode string) error
	RecordFailedOrganization(ctx context.Context, source, target, mode, errMsg string) error
}

type processedFileStamp struct {
	size    int64
	modTime int64
}

// directoryMonitor 定时巡检 V2 `Directories` 中 `monitor_type == "monitor"` 的本地目录，
// 对新落盘且已写稳的视频文件执行识别、自定义模板命名、不可变计划整理与成败历史落库。
type directoryMonitor struct {
	store       directoryMonitorStore
	templatesFn func() organization.NamingTemplates
	logger      *slog.Logger
	interval    time.Duration
	stability   time.Duration

	mu        sync.Mutex
	startedAt time.Time
	scanning  bool
	processed map[string]processedFileStamp
	triggerCh chan struct{}
}

func newDirectoryMonitor(
	store directoryMonitorStore,
	templatesFn func() organization.NamingTemplates,
	logger *slog.Logger,
) *directoryMonitor {
	if store == nil {
		return nil
	}
	if logger == nil {
		logger = slog.Default()
	}
	return &directoryMonitor{
		store:       store,
		templatesFn: templatesFn,
		logger:      logger,
		interval:    defaultDirectoryMonitorInterval,
		stability:   defaultDirectoryMonitorStability,
		startedAt:   time.Now().UTC(),
		processed:   make(map[string]processedFileStamp),
		triggerCh:   make(chan struct{}, 1),
	}
}

// Start 在后台协程中按周期运行目录监控，并在 ctx 取消时优雅退出。
func (m *directoryMonitor) Start(ctx context.Context) {
	if m == nil {
		return
	}
	go func() {
		ticker := time.NewTicker(m.interval)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				_ = m.ScanOnce(ctx)
			case <-m.triggerCh:
				_ = m.ScanOnce(ctx)
			}
		}
	}()
}

// TriggerScan 立即触发一次目录监控扫描（若当前已有扫描在进行则合并）。
func (m *directoryMonitor) TriggerScan(ctx context.Context) error {
	if m == nil {
		return nil
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	select {
	case m.triggerCh <- struct{}{}:
	default:
	}
	return nil
}

// ScheduleInfo 将目录监控投影为 V2 `/dashboard/schedule` 条目。
func (m *directoryMonitor) ScheduleInfo(now time.Time, loc *time.Location) (httpapi.ScheduleInfo, bool) {
	if m == nil {
		return httpapi.ScheduleInfo{}, false
	}
	m.mu.Lock()
	base := m.startedAt
	interval := m.interval
	scanning := m.scanning
	m.mu.Unlock()

	if base.IsZero() {
		base = now
	}
	if interval <= 0 {
		interval = defaultDirectoryMonitorInterval
	}
	elapsed := now.Sub(base)
	steps := elapsed/interval + 1
	if elapsed < 0 {
		steps = 1
	}
	next := base.Add(steps * interval)
	status := "等待"
	if scanning {
		status = "正在运行"
	}
	return httpapi.ScheduleInfo{
		ID:       "directory_monitor",
		Name:     "目录监控整理",
		Provider: "系统",
		Status:   status,
		NextRun:  formatScheduleNextRun(now, next, loc),
	}, true
}

// ScanOnce 读取当前配置中所有 `monitor_type == "monitor"` 的本地目录并整理新文件。
func (m *directoryMonitor) ScanOnce(ctx context.Context) error {
	if m == nil || m.store == nil {
		return nil
	}
	if err := ctx.Err(); err != nil {
		return err
	}

	m.mu.Lock()
	if m.scanning {
		m.mu.Unlock()
		return nil
	}
	m.scanning = true
	m.mu.Unlock()
	defer func() {
		m.mu.Lock()
		m.scanning = false
		m.mu.Unlock()
	}()

	var directories []downloadconfig.Directory
	found, err := m.store.GetSetting(ctx, directoryMonitorDirectoriesKey, &directories)
	if err != nil {
		m.logger.Warn("目录监控读取目录配置失败", "error", err)
		return err
	}
	if !found || len(directories) == 0 {
		return nil
	}

	var templates organization.NamingTemplates
	if m.templatesFn != nil {
		templates = m.templatesFn()
	}

	for _, dir := range directories {
		if err := ctx.Err(); err != nil {
			return err
		}
		if !isDirectoryMonitorEnabled(dir) {
			continue
		}
		downloadRoot, okDown := dir.LocalDownloadPath()
		libraryRoot, okLib := dir.LocalLibraryPath()
		mode, okMode := parseMonitorTransferMode(dir.TransferType)
		if !okDown || !okLib || !okMode {
			continue
		}
		m.scanDirectory(ctx, dir, downloadRoot, libraryRoot, mode, templates)
	}
	return nil
}

func (m *directoryMonitor) scanDirectory(
	ctx context.Context,
	dir downloadconfig.Directory,
	downloadRoot string,
	libraryRoot string,
	mode organizer.Mode,
	templates organization.NamingTemplates,
) {
	absDownload, err := filepath.Abs(downloadRoot)
	if err != nil {
		return
	}
	absLibrary, err := filepath.Abs(libraryRoot)
	if err != nil || strings.EqualFold(filepath.Clean(absDownload), filepath.Clean(absLibrary)) {
		return
	}
	info, err := os.Stat(absDownload)
	if err != nil || !info.IsDir() {
		return
	}

	now := time.Now()
	_ = filepath.WalkDir(absDownload, func(filePath string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return nil
		}
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if entry.IsDir() {
			if strings.HasPrefix(entry.Name(), ".") && filePath != absDownload {
				return filepath.SkipDir
			}
			return nil
		}
		name := entry.Name()
		if strings.HasPrefix(name, ".") || isTempDownloadFile(name) || !organization.IsVideoFilename(name) {
			return nil
		}
		fileInfo, statErr := entry.Info()
		if statErr != nil || !fileInfo.Mode().IsRegular() {
			return nil
		}
		if m.stability > 0 && now.Sub(fileInfo.ModTime()) < m.stability {
			return nil
		}

		stamp := processedFileStamp{size: fileInfo.Size(), modTime: fileInfo.ModTime().UnixNano()}
		m.mu.Lock()
		prev, seen := m.processed[filePath]
		m.mu.Unlock()
		if seen && prev == stamp {
			return nil
		}

		if err := m.organizeMonitoredFile(ctx, dir, absDownload, absLibrary, filePath, mode, templates); err != nil {
			m.logger.Warn("目录监控整理失败", "source", filePath, "error", err)
			return nil
		}

		m.mu.Lock()
		m.processed[filePath] = stamp
		m.mu.Unlock()
		return nil
	})
}

func (m *directoryMonitor) organizeMonitoredFile(
	ctx context.Context,
	dir downloadconfig.Directory,
	downloadRoot string,
	libraryRoot string,
	sourcePath string,
	mode organizer.Mode,
	templates organization.NamingTemplates,
) error {
	parsed := media.Parse(filepath.Base(sourcePath), media.Options{})
	mediaType := string(parsed.Type)
	if dir.MediaType != nil && strings.TrimSpace(*dir.MediaType) != "" {
		mediaType = strings.TrimSpace(*dir.MediaType)
	}
	category := ""
	if dir.MediaCategory != nil {
		category = strings.TrimSpace(*dir.MediaCategory)
	}

	renaming := dir.Renaming == nil || *dir.Renaming
	var relTarget string
	if renaming {
		rendered, err := organization.RenderRelativeTarget(organization.NamingInput{
			SourcePath: sourcePath,
			MediaType:  mediaType,
			Category:   category,
		}, templates)
		if err != nil {
			_ = m.store.RecordFailedOrganization(ctx, sourcePath, libraryRoot, string(mode), err.Error())
			return err
		}
		relTarget = rendered
	} else {
		rel, err := filepath.Rel(downloadRoot, sourcePath)
		if err != nil {
			_ = m.store.RecordFailedOrganization(ctx, sourcePath, libraryRoot, string(mode), err.Error())
			return err
		}
		relTarget = filepath.ToSlash(rel)
	}

	prefix := make([]string, 0, 2)
	if dir.LibraryTypeFolder != nil && *dir.LibraryTypeFolder && mediaType != "" && mediaType != string(media.TypeUnknown) {
		prefix = append(prefix, mediaType)
	}
	if dir.LibraryCategoryFolder != nil && *dir.LibraryCategoryFolder && category != "" {
		prefix = append(prefix, category)
	}
	if len(prefix) > 0 {
		relTarget = path.Join(append(prefix, relTarget)...)
	}

	targetPath := filepath.Clean(filepath.Join(libraryRoot, filepath.FromSlash(relTarget)))

	expectedSize, expectedSHA256, err := organizer.FingerprintFile(ctx, sourcePath)
	if err != nil {
		_ = m.store.RecordFailedOrganization(ctx, sourcePath, targetPath, string(mode), fmt.Sprintf("计算文件指纹失败: %v", err))
		return err
	}
	plan, err := organizer.BuildPlan(libraryRoot, []organizer.Operation{{
		Source:         sourcePath,
		Target:         targetPath,
		Mode:           mode,
		ExpectedSize:   expectedSize,
		ExpectedSHA256: expectedSHA256,
	}})
	if err != nil {
		_ = m.store.RecordFailedOrganization(ctx, sourcePath, targetPath, string(mode), fmt.Sprintf("构建整理计划失败: %v", err))
		return err
	}
	if err := organizer.ExecuteWithinSourceRoot(ctx, downloadRoot, plan); err != nil {
		_ = m.store.RecordFailedOrganization(ctx, sourcePath, targetPath, string(mode), err.Error())
		return err
	}
	if err := m.store.RecordManualOrganizationWithMode(ctx, sourcePath, targetPath, string(mode)); err != nil {
		m.logger.Warn("目录监控记录整理成功历史失败", "source", sourcePath, "target", targetPath, "error", err)
	}
	m.logger.Info("目录监控整理完成", "source", sourcePath, "target", targetPath, "mode", string(mode))
	return nil
}

func isDirectoryMonitorEnabled(dir downloadconfig.Directory) bool {
	if dir.MonitorType == nil {
		return false
	}
	return strings.EqualFold(strings.TrimSpace(*dir.MonitorType), "monitor")
}

func parseMonitorTransferMode(raw *string) (organizer.Mode, bool) {
	if raw == nil {
		return organizer.Move, true
	}
	switch strings.ToLower(strings.TrimSpace(*raw)) {
	case "", "move":
		return organizer.Move, true
	case "copy":
		return organizer.Copy, true
	case "link", "hardlink":
		return organizer.Hardlink, true
	case "softlink", "symlink":
		return organizer.Symlink, true
	default:
		return "", false
	}
}

func isTempDownloadFile(name string) bool {
	lower := strings.ToLower(name)
	for _, suffix := range []string{".!qb", ".part", ".tmp", ".crdownload", ".downloading"} {
		if strings.HasSuffix(lower, suffix) {
			return true
		}
	}
	return false
}
