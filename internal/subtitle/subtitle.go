// Package subtitle 提供纯 Go 的字幕搜索聚合与安全下载/解压能力。
package subtitle

import (
	"archive/zip"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"mime"
	"net/http"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

const (
	maxSubtitleDownloadBytes = 32 << 20 // 32 MiB
	maxExtractedFiles        = 32
	maxExtractedFileBytes    = 16 << 20 // 16 MiB per file
	defaultSubtitleTimeout   = 25 * time.Second
)

var (
	// ErrNoProvider 表示未配置任何字幕源。
	ErrNoProvider = errors.New("未配置字幕源")
	// ErrInvalidQuery 表示字幕搜索参数无效。
	ErrInvalidQuery = errors.New("字幕搜索参数无效")
	// ErrDownloadFailed 表示字幕下载或解压失败。
	ErrDownloadFailed = errors.New("字幕下载失败")
)

var allowedSubtitleExtensions = map[string]struct{}{
	".srt": {},
	".ass": {},
	".ssa": {},
	".sub": {},
	".vtt": {},
	".sup": {},
	".idx": {},
}

// Subtitle 对齐 V2 `schemas.SubtitleInfo` 的核心字段。
type Subtitle struct {
	Site          int    `json:"site,omitempty"`
	SiteName      string `json:"site_name,omitempty"`
	Title         string `json:"title,omitempty"`
	Description   string `json:"description,omitempty"`
	Enclosure     string `json:"enclosure,omitempty"`
	PageURL       string `json:"page_url,omitempty"`
	Language      string `json:"language,omitempty"`
	LanguageIcon  string `json:"language_icon,omitempty"`
	Size          int64  `json:"size,omitempty"`
	PubDate       string `json:"pubdate,omitempty"`
	Grabs         int    `json:"grabs,omitempty"`
	Uploader      string `json:"uploader,omitempty"`
	SubtitleID    string `json:"subtitle_id,omitempty"`
	FileName      string `json:"file_name,omitempty"`
	SeasonEpisode string `json:"season_episode,omitempty"`
	EpisodeList   []int  `json:"episode_list"`
}

// Query 描述字幕搜索条件。
type Query struct {
	Keyword   string
	MediaID   string
	IMDBID    string
	TMDBID    int
	MediaType string
	Year      string
	Season    *int
	Episode   *int
	Languages []string
	Page      int
	SiteIDs   []int
}

// Event 描述渐进式字幕搜索中单个字幕源的完成事件。
type Event struct {
	Site      string
	Subtitles []Subtitle
	Error     string
}

// Provider 定义单个字幕源的搜索与下载链接解析契约。
type Provider interface {
	Name() string
	SiteID() int
	Search(ctx context.Context, query Query) ([]Subtitle, error)
	ResolveDownload(ctx context.Context, item Subtitle) (downloadURL string, fileName string, headers map[string]string, err error)
}

// Coordinator 负责并发扇出多个字幕源搜索、去重排序以及安全下载/解压字幕文件。
type Coordinator struct {
	mu          sync.RWMutex
	providers   []Provider
	client      *http.Client
	defaultLang []string
}

// NewCoordinator 创建字幕协调器。
func NewCoordinator(client *http.Client, defaultLanguages []string, providers ...Provider) *Coordinator {
	if client == nil {
		client = &http.Client{Timeout: defaultSubtitleTimeout}
	}
	valid := make([]Provider, 0, len(providers))
	for _, p := range providers {
		if p != nil {
			valid = append(valid, p)
		}
	}
	return &Coordinator{
		providers:   valid,
		client:      client,
		defaultLang: normalizeLanguages(defaultLanguages),
	}
}

// Enabled 返回是否至少接入了一个字幕源。
func (c *Coordinator) Enabled() bool {
	if c == nil {
		return false
	}
	c.mu.RLock()
	defer c.mu.RUnlock()
	return len(c.providers) > 0
}

// ProviderCount 返回当前启用的字幕源数量。
func (c *Coordinator) ProviderCount() int {
	if c == nil {
		return 0
	}
	c.mu.RLock()
	defer c.mu.RUnlock()
	return len(c.providers)
}

func (c *Coordinator) selectProviders(siteIDs []int) []Provider {
	c.mu.RLock()
	all := append([]Provider(nil), c.providers...)
	c.mu.RUnlock()
	if len(siteIDs) == 0 {
		return all
	}
	allowed := make(map[int]struct{}, len(siteIDs))
	for _, id := range siteIDs {
		allowed[id] = struct{}{}
	}
	filtered := make([]Provider, 0, len(all))
	for _, p := range all {
		if _, ok := allowed[p.SiteID()]; ok {
			filtered = append(filtered, p)
		}
	}
	return filtered
}

// Search 并发检索已配置字幕源，按源通过 onEvent 推送结果，最终返回去重排序后的总列表。
func (c *Coordinator) Search(ctx context.Context, query Query, onEvent func(Event)) ([]Subtitle, error) {
	if c == nil || !c.Enabled() {
		return nil, ErrNoProvider
	}
	if ctx == nil {
		ctx = context.Background()
	}
	if strings.TrimSpace(query.Keyword) == "" && strings.TrimSpace(query.IMDBID) == "" && query.TMDBID <= 0 {
		return nil, ErrInvalidQuery
	}
	if len(query.Languages) == 0 {
		c.mu.RLock()
		query.Languages = append([]string(nil), c.defaultLang...)
		c.mu.RUnlock()
	}
	providers := c.selectProviders(query.SiteIDs)
	if len(providers) == 0 {
		return nil, ErrNoProvider
	}

	var (
		wg       sync.WaitGroup
		eventMu  sync.Mutex
		mergedMu sync.Mutex
		merged   []Subtitle
	)
	for _, provider := range providers {
		p := provider
		wg.Add(1)
		go func() {
			defer wg.Done()
			items, err := p.Search(ctx, query)
			for i := range items {
				if items[i].EpisodeList == nil {
					items[i].EpisodeList = []int{}
				}
			}
			evt := Event{Site: p.Name(), Subtitles: items}
			if err != nil {
				evt.Error = err.Error()
			}
			if onEvent != nil {
				eventMu.Lock()
				onEvent(evt)
				eventMu.Unlock()
			}
			if len(items) > 0 {
				mergedMu.Lock()
				merged = append(merged, items...)
				mergedMu.Unlock()
			}
		}()
	}
	wg.Wait()

	merged = dedupeAndSort(merged)
	return merged, nil
}

// Download 解析字幕下载链接、下载内容并写入 saveDir；若为 ZIP 归档则安全解压出字幕文件。
func (c *Coordinator) Download(ctx context.Context, item Subtitle, saveDir string) ([]string, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	saveDir = strings.TrimSpace(saveDir)
	if saveDir == "" {
		return nil, fmt.Errorf("%w: 保存目录为空", ErrDownloadFailed)
	}
	downloadURL, fileName, headers, err := c.resolveDownload(ctx, item)
	if err != nil {
		return nil, err
	}
	parsedURL, err := url.Parse(downloadURL)
	if err != nil || (parsedURL.Scheme != "http" && parsedURL.Scheme != "https") {
		return nil, fmt.Errorf("%w: 不支持的下载链接协议", ErrDownloadFailed)
	}

	client := &http.Client{Timeout: defaultSubtitleTimeout}
	if c != nil && c.client != nil {
		client = c.client
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, downloadURL, nil)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrDownloadFailed, err)
	}
	req.Header.Set("User-Agent", "MoviePilot-Go/1.0")
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrDownloadFailed, err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, fmt.Errorf("%w: 上游返回状态码 %d", ErrDownloadFailed, resp.StatusCode)
	}

	payload, err := io.ReadAll(io.LimitReader(resp.Body, maxSubtitleDownloadBytes+1))
	if err != nil {
		return nil, fmt.Errorf("%w: 读取响应失败", ErrDownloadFailed)
	}
	if int64(len(payload)) > maxSubtitleDownloadBytes {
		return nil, fmt.Errorf("%w: 字幕文件超过大小上限", ErrDownloadFailed)
	}
	if len(payload) == 0 {
		return nil, fmt.Errorf("%w: 字幕内容为空", ErrDownloadFailed)
	}

	if err := os.MkdirAll(saveDir, 0o755); err != nil {
		return nil, fmt.Errorf("%w: 创建保存目录失败: %v", ErrDownloadFailed, err)
	}

	if fileName == "" {
		fileName = extractHeaderFilename(resp.Header.Get("Content-Disposition"))
	}
	if fileName == "" {
		fileName = strings.TrimSpace(item.FileName)
	}
	if fileName == "" {
		fileName = path.Base(parsedURL.Path)
	}

	if isZipPayload(payload, fileName) {
		return extractSubtitleZip(payload, saveDir)
	}

	safeName := sanitizeSubtitleFilename(fileName, item.Title)
	target := filepath.Join(saveDir, safeName)
	if err := os.WriteFile(target, payload, 0o644); err != nil {
		return nil, fmt.Errorf("%w: 写入字幕文件失败: %v", ErrDownloadFailed, err)
	}
	return []string{target}, nil
}

func (c *Coordinator) resolveDownload(ctx context.Context, item Subtitle) (string, string, map[string]string, error) {
	enclosure := strings.TrimSpace(item.Enclosure)
	if c != nil {
		c.mu.RLock()
		providers := append([]Provider(nil), c.providers...)
		c.mu.RUnlock()
		for _, p := range providers {
			if (item.Site > 0 && p.SiteID() == item.Site) ||
				(item.SiteName != "" && strings.EqualFold(p.Name(), item.SiteName)) ||
				strings.HasPrefix(strings.ToLower(enclosure), strings.ToLower(p.Name())+"://") {
				u, name, hdrs, err := p.ResolveDownload(ctx, item)
				if err == nil && strings.TrimSpace(u) != "" {
					return strings.TrimSpace(u), strings.TrimSpace(name), hdrs, nil
				}
				if err != nil {
					return "", "", nil, fmt.Errorf("%w: %v", ErrDownloadFailed, err)
				}
			}
		}
	}
	if strings.HasPrefix(enclosure, "http://") || strings.HasPrefix(enclosure, "https://") {
		return enclosure, strings.TrimSpace(item.FileName), nil, nil
	}
	return "", "", nil, fmt.Errorf("%w: 无法解析字幕下载链接", ErrDownloadFailed)
}

func isZipPayload(payload []byte, fileName string) bool {
	if len(payload) >= 4 && payload[0] == 'P' && payload[1] == 'K' && payload[2] == 0x03 && payload[3] == 0x04 {
		return true
	}
	return strings.EqualFold(filepath.Ext(fileName), ".zip")
}

func extractSubtitleZip(payload []byte, saveDir string) ([]string, error) {
	reader, err := zip.NewReader(bytes.NewReader(payload), int64(len(payload)))
	if err != nil {
		return nil, fmt.Errorf("%w: ZIP 归档损坏: %v", ErrDownloadFailed, err)
	}
	cleanDir := filepath.Clean(saveDir)
	saved := make([]string, 0, len(reader.File))
	for _, entry := range reader.File {
		if len(saved) >= maxExtractedFiles {
			break
		}
		if entry.FileInfo().IsDir() {
			continue
		}
		if hasTraversalSegment(entry.Name) {
			return nil, fmt.Errorf("%w: 归档包含非法路径 %q", ErrDownloadFailed, entry.Name)
		}
		ext := strings.ToLower(filepath.Ext(entry.Name))
		if _, ok := allowedSubtitleExtensions[ext]; !ok {
			continue
		}
		baseName := sanitizeSubtitleFilename(filepath.Base(entry.Name), "")
		target := filepath.Join(cleanDir, baseName)
		if !isWithinDir(cleanDir, target) {
			return nil, fmt.Errorf("%w: 归档目标越界", ErrDownloadFailed)
		}
		rc, err := entry.Open()
		if err != nil {
			return nil, fmt.Errorf("%w: 打开归档项失败: %v", ErrDownloadFailed, err)
		}
		data, readErr := io.ReadAll(io.LimitReader(rc, maxExtractedFileBytes+1))
		_ = rc.Close()
		if readErr != nil || int64(len(data)) > maxExtractedFileBytes {
			return nil, fmt.Errorf("%w: 解压字幕文件失败或超限", ErrDownloadFailed)
		}
		if err := os.WriteFile(target, data, 0o644); err != nil {
			return nil, fmt.Errorf("%w: 写入解压字幕失败: %v", ErrDownloadFailed, err)
		}
		saved = append(saved, target)
	}
	if len(saved) == 0 {
		return nil, fmt.Errorf("%w: ZIP 归档中未找到有效字幕文件", ErrDownloadFailed)
	}
	return saved, nil
}

func hasTraversalSegment(name string) bool {
	normalized := strings.ReplaceAll(strings.TrimSpace(name), `\`, "/")
	if strings.HasPrefix(normalized, "/") || strings.Contains(normalized, ":") {
		return true
	}
	for _, seg := range strings.Split(normalized, "/") {
		if seg == ".." {
			return true
		}
	}
	return false
}

func isWithinDir(dir, target string) bool {
	rel, err := filepath.Rel(dir, target)
	if err != nil {
		return false
	}
	return rel != ".." && !strings.HasPrefix(rel, ".."+string(os.PathSeparator))
}

func extractHeaderFilename(contentDisposition string) string {
	if strings.TrimSpace(contentDisposition) == "" {
		return ""
	}
	_, params, err := mime.ParseMediaType(contentDisposition)
	if err != nil {
		return ""
	}
	return strings.TrimSpace(params["filename"])
}

func sanitizeSubtitleFilename(fileName, fallbackTitle string) string {
	name := strings.TrimSpace(filepath.Base(strings.ReplaceAll(fileName, `\`, "/")))
	name = strings.Map(func(r rune) rune {
		switch r {
		case '/', '\\', ':', '*', '?', '"', '<', '>', '|', '\r', '\n', 0:
			return '_'
		default:
			return r
		}
	}, name)
	if name == "" || name == "." || name == ".." {
		base := strings.TrimSpace(fallbackTitle)
		if base == "" {
			base = "subtitle"
		}
		name = sanitizeSubtitleFilename(base+".srt", "subtitle")
	}
	if filepath.Ext(name) == "" {
		name += ".srt"
	}
	return name
}

func normalizeLanguages(langs []string) []string {
	out := make([]string, 0, len(langs))
	seen := make(map[string]struct{}, len(langs))
	for _, lang := range langs {
		trimmed := strings.ToLower(strings.TrimSpace(lang))
		if trimmed == "" {
			continue
		}
		if _, ok := seen[trimmed]; ok {
			continue
		}
		seen[trimmed] = struct{}{}
		out = append(out, trimmed)
	}
	return out
}

func dedupeAndSort(items []Subtitle) []Subtitle {
	seen := make(map[string]struct{}, len(items))
	out := make([]Subtitle, 0, len(items))
	for _, item := range items {
		key := strings.ToLower(item.SiteName) + "|" + item.SubtitleID + "|" + item.Enclosure
		if _, ok := seen[key]; ok {
			continue
		}
		seen[key] = struct{}{}
		if item.EpisodeList == nil {
			item.EpisodeList = []int{}
		}
		out = append(out, item)
	}
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].Grabs != out[j].Grabs {
			return out[i].Grabs > out[j].Grabs
		}
		return out[i].Title < out[j].Title
	})
	return out
}

func formatSeasonEpisode(season, episode *int) string {
	if season == nil && episode == nil {
		return ""
	}
	if season != nil && episode != nil {
		return fmt.Sprintf("S%02dE%02d", *season, *episode)
	}
	if season != nil {
		return fmt.Sprintf("S%02d", *season)
	}
	return "E" + strconv.Itoa(*episode)
}
