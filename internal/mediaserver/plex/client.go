package plex

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"
)

const (
	defaultTimeout   = 15 * time.Second
	maxResponseBytes = 16 << 20
	maxSectionLimit  = 500
)

// Client 是线程安全的 Plex HTTP JSON API 客户端。
type Client struct {
	baseURL   *url.URL
	playHost  *url.URL
	token     string
	timeout   time.Duration
	client    *http.Client
	machineMu sync.RWMutex
	machineID string
}

// NewClient 校验配置并创建 Plex 客户端。
func NewClient(cfg Config) (*Client, error) {
	baseURL, err := parsePlexURL(cfg.BaseURL)
	if err != nil {
		return nil, &Error{Code: ErrorCodeInvalidConfig}
	}
	playHost := baseURL
	if strings.TrimSpace(cfg.PlayHost) != "" {
		playHost, err = parsePlexURL(cfg.PlayHost)
		if err != nil {
			return nil, &Error{Code: ErrorCodeInvalidConfig}
		}
	}
	token := strings.TrimSpace(cfg.Token)
	if token == "" {
		return nil, &Error{Code: ErrorCodeInvalidConfig}
	}
	timeout := cfg.Timeout
	if timeout <= 0 {
		timeout = defaultTimeout
	}
	transport := http.DefaultTransport.(*http.Transport).Clone()
	return &Client{
		baseURL:  baseURL,
		playHost: playHost,
		token:    token,
		timeout:  timeout,
		client:   &http.Client{Transport: transport, Timeout: timeout},
	}, nil
}

func parsePlexURL(raw string) (*url.URL, error) {
	trimmed := strings.TrimSpace(raw)
	if trimmed == "" {
		return nil, &Error{Code: ErrorCodeInvalidConfig}
	}
	parsed, err := url.Parse(trimmed)
	if err != nil || parsed.Host == "" || (parsed.Scheme != "http" && parsed.Scheme != "https") {
		return nil, &Error{Code: ErrorCodeInvalidConfig}
	}
	parsed.Path = strings.TrimRight(parsed.Path, "/")
	parsed.RawQuery = ""
	parsed.Fragment = ""
	return parsed, nil
}

// CloseIdleConnections 释放底层 HTTP 连接池空闲连接。
func (c *Client) CloseIdleConnections() {
	if c == nil || c.client == nil {
		return
	}
	c.client.CloseIdleConnections()
}

func (c *Client) endpoint(path string) string {
	return strings.TrimRight(c.baseURL.String(), "/") + path
}

func (c *Client) getJSON(ctx context.Context, fullURL string, target any) error {
	if ctx == nil {
		return &Error{Code: ErrorCodeInvalidArgument}
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, fullURL, nil)
	if err != nil {
		return &Error{Code: ErrorCodeInvalidArgument, cause: err}
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("X-Plex-Token", c.token)
	req.Header.Set("X-Plex-Product", "MoviePilot-Go")
	req.Header.Set("X-Plex-Client-Identifier", "moviepilot-go")

	resp, err := c.client.Do(req)
	if err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		return &Error{Code: ErrorCodeTransport, cause: err}
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode == http.StatusTooManyRequests {
		return &Error{Code: ErrorCodeRateLimited, StatusCode: resp.StatusCode}
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return &Error{Code: ErrorCodeHTTPStatus, StatusCode: resp.StatusCode}
	}
	raw, err := io.ReadAll(io.LimitReader(resp.Body, maxResponseBytes))
	if err != nil {
		return &Error{Code: ErrorCodeTransport, cause: err}
	}
	if target == nil || len(raw) == 0 {
		return nil
	}
	if err := json.Unmarshal(raw, target); err != nil {
		return &Error{Code: ErrorCodeInvalidResponse, cause: err}
	}
	return nil
}

// MachineIdentifier 查询并缓存 Plex 服务器 machineIdentifier。
func (c *Client) MachineIdentifier(ctx context.Context) (string, error) {
	c.machineMu.RLock()
	cached := c.machineID
	c.machineMu.RUnlock()
	if cached != "" {
		return cached, nil
	}
	var payload struct {
		MediaContainer struct {
			MachineIdentifier string `json:"machineIdentifier"`
		} `json:"MediaContainer"`
	}
	if err := c.getJSON(ctx, c.endpoint("/identity"), &payload); err != nil {
		return "", err
	}
	id := strings.TrimSpace(payload.MediaContainer.MachineIdentifier)
	if id != "" {
		c.machineMu.Lock()
		c.machineID = id
		c.machineMu.Unlock()
	}
	return id, nil
}

// ListLibraries 返回 Plex 所有媒体库分区（movie / show）。
func (c *Client) ListLibraries(ctx context.Context) ([]Library, error) {
	var payload struct {
		MediaContainer struct {
			MachineIdentifier string `json:"machineIdentifier"`
			Directory         []struct {
				Key       string `json:"key"`
				Title     string `json:"title"`
				Type      string `json:"type"`
				Composite string `json:"composite"`
				Location  []struct {
					Path string `json:"path"`
				} `json:"Location"`
			} `json:"Directory"`
		} `json:"MediaContainer"`
	}
	if err := c.getJSON(ctx, c.endpoint("/library/sections"), &payload); err != nil {
		return nil, err
	}
	if id := strings.TrimSpace(payload.MediaContainer.MachineIdentifier); id != "" {
		c.machineMu.Lock()
		c.machineID = id
		c.machineMu.Unlock()
	}
	libraries := make([]Library, 0, len(payload.MediaContainer.Directory))
	for _, dir := range payload.MediaContainer.Directory {
		locs := make([]string, 0, len(dir.Location))
		for _, loc := range dir.Location {
			if p := strings.TrimSpace(loc.Path); p != "" {
				locs = append(locs, p)
			}
		}
		libraries = append(libraries, Library{
			Key:       strings.TrimSpace(dir.Key),
			Title:     strings.TrimSpace(dir.Title),
			Type:      strings.ToLower(strings.TrimSpace(dir.Type)),
			Locations: locs,
			Composite: strings.TrimSpace(dir.Composite),
		})
	}
	return libraries, nil
}

type plexMetadataEntry struct {
	RatingKey        string `json:"ratingKey"`
	Type             string `json:"type"`
	Title            string `json:"title"`
	OriginalTitle    string `json:"originalTitle"`
	Year             int    `json:"year"`
	LibrarySectionID any    `json:"librarySectionID"`
	ParentIndex      *int   `json:"parentIndex"`
	Index            *int   `json:"index"`
	Guid             string `json:"guid"`
	Guids            []struct {
		ID string `json:"id"`
	} `json:"Guid"`
	Media []struct {
		Part []struct {
			File string `json:"file"`
		} `json:"Part"`
	} `json:"Media"`
}

func (e plexMetadataEntry) toItem(fallbackLibraryKey string) Item {
	libKey := fallbackLibraryKey
	switch v := e.LibrarySectionID.(type) {
	case string:
		if strings.TrimSpace(v) != "" {
			libKey = strings.TrimSpace(v)
		}
	case float64:
		if v > 0 {
			libKey = strconv.Itoa(int(v))
		}
	}
	filePath := ""
	for _, media := range e.Media {
		for _, part := range media.Part {
			if p := strings.TrimSpace(part.File); p != "" {
				filePath = p
				break
			}
		}
		if filePath != "" {
			break
		}
	}
	providerIDs := parsePlexGuids(e.Guid, e.Guids)
	return Item{
		RatingKey:     strings.TrimSpace(e.RatingKey),
		Type:          strings.ToLower(strings.TrimSpace(e.Type)),
		Title:         strings.TrimSpace(e.Title),
		OriginalTitle: strings.TrimSpace(e.OriginalTitle),
		Year:          e.Year,
		LibraryKey:    libKey,
		ParentIndex:   e.ParentIndex,
		Index:         e.Index,
		Path:          filePath,
		ProviderIDs:   providerIDs,
	}
}

func parsePlexGuids(primary string, list []struct {
	ID string `json:"id"`
}) map[string]string {
	out := make(map[string]string)
	apply := func(raw string) {
		raw = strings.TrimSpace(raw)
		if raw == "" {
			return
		}
		lower := strings.ToLower(raw)
		switch {
		case strings.HasPrefix(lower, "tmdb://"):
			out["Tmdb"] = strings.TrimSpace(strings.Split(raw[7:], "?")[0])
		case strings.HasPrefix(lower, "imdb://"):
			out["Imdb"] = strings.TrimSpace(strings.Split(raw[7:], "?")[0])
		case strings.HasPrefix(lower, "tvdb://"):
			out["Tvdb"] = strings.TrimSpace(strings.Split(raw[7:], "?")[0])
		case strings.Contains(lower, "themoviedb://"):
			idx := strings.Index(lower, "themoviedb://")
			out["Tmdb"] = strings.TrimSpace(strings.Split(raw[idx+13:], "?")[0])
		case strings.Contains(lower, "thetvdb://"):
			idx := strings.Index(lower, "thetvdb://")
			rest := strings.Split(raw[idx+10:], "?")[0]
			out["Tvdb"] = strings.TrimSpace(strings.Split(rest, "/")[0])
		}
	}
	apply(primary)
	for _, g := range list {
		apply(g.ID)
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// ListSectionItems 按分页拉取指定 Plex 媒体库内的电影或剧集条目（含 Guid 与文件路径）。
func (c *Client) ListSectionItems(ctx context.Context, sectionKey string, start, size int) (ItemsPage, error) {
	sectionKey = strings.TrimSpace(sectionKey)
	if sectionKey == "" || start < 0 {
		return ItemsPage{}, &Error{Code: ErrorCodeInvalidArgument}
	}
	if size <= 0 || size > maxSectionLimit {
		size = 200
	}
	params := url.Values{}
	params.Set("includeGuids", "1")
	params.Set("X-Plex-Container-Start", strconv.Itoa(start))
	params.Set("X-Plex-Container-Size", strconv.Itoa(size))

	var payload struct {
		MediaContainer struct {
			TotalSize int                 `json:"totalSize"`
			Size      int                 `json:"size"`
			Offset    int                 `json:"offset"`
			Metadata  []plexMetadataEntry `json:"Metadata"`
		} `json:"MediaContainer"`
	}
	endpoint := c.endpoint("/library/sections/" + url.PathEscape(sectionKey) + "/all?" + params.Encode())
	if err := c.getJSON(ctx, endpoint, &payload); err != nil {
		return ItemsPage{}, err
	}
	items := make([]Item, 0, len(payload.MediaContainer.Metadata))
	for _, entry := range payload.MediaContainer.Metadata {
		items = append(items, entry.toItem(sectionKey))
	}
	total := payload.MediaContainer.TotalSize
	if total == 0 {
		total = start + len(items)
	}
	return ItemsPage{Items: items, StartIndex: start, TotalSize: total}, nil
}

// ListEpisodes 分页列出某部剧集下的全部单集（`/library/metadata/{ratingKey}/allLeaves`）。
func (c *Client) ListEpisodes(ctx context.Context, ratingKey string, start, size int) (ItemsPage, error) {
	ratingKey = strings.TrimSpace(ratingKey)
	if ratingKey == "" || start < 0 {
		return ItemsPage{}, &Error{Code: ErrorCodeInvalidArgument}
	}
	if size <= 0 || size > maxSectionLimit {
		size = 200
	}
	params := url.Values{}
	params.Set("includeGuids", "1")
	params.Set("X-Plex-Container-Start", strconv.Itoa(start))
	params.Set("X-Plex-Container-Size", strconv.Itoa(size))

	var payload struct {
		MediaContainer struct {
			TotalSize int                 `json:"totalSize"`
			Metadata  []plexMetadataEntry `json:"Metadata"`
		} `json:"MediaContainer"`
	}
	endpoint := c.endpoint("/library/metadata/" + url.PathEscape(ratingKey) + "/allLeaves?" + params.Encode())
	if err := c.getJSON(ctx, endpoint, &payload); err != nil {
		return ItemsPage{}, err
	}
	items := make([]Item, 0, len(payload.MediaContainer.Metadata))
	for _, entry := range payload.MediaContainer.Metadata {
		items = append(items, entry.toItem(""))
	}
	total := payload.MediaContainer.TotalSize
	if total == 0 {
		total = start + len(items)
	}
	return ItemsPage{Items: items, StartIndex: start, TotalSize: total}, nil
}

// GetItem 查询单个 Plex 条目详情；404 时返回 found=false, err=nil。
func (c *Client) GetItem(ctx context.Context, ratingKey string) (Item, bool, error) {
	ratingKey = strings.TrimSpace(ratingKey)
	if ratingKey == "" {
		return Item{}, false, &Error{Code: ErrorCodeInvalidArgument}
	}
	var payload struct {
		MediaContainer struct {
			Metadata []plexMetadataEntry `json:"Metadata"`
		} `json:"MediaContainer"`
	}
	endpoint := c.endpoint("/library/metadata/" + url.PathEscape(ratingKey) + "?includeGuids=1")
	if err := c.getJSON(ctx, endpoint, &payload); err != nil {
		var plexErr *Error
		if err != nil && strings.Contains(err.Error(), "status 404") {
			return Item{}, false, nil
		}
		if e, ok := err.(*Error); ok && e.StatusCode == http.StatusNotFound {
			return Item{}, false, nil
		}
		_ = plexErr
		return Item{}, false, err
	}
	if len(payload.MediaContainer.Metadata) == 0 {
		return Item{}, false, nil
	}
	return payload.MediaContainer.Metadata[0].toItem(""), true, nil
}

// SearchItems 在 Plex 全局按标题与类型检索电影或剧集（mediaType: "movie"|"show"|"episode"|""）。
func (c *Client) SearchItems(ctx context.Context, query string, mediaType string, limit int) ([]Item, error) {
	query = strings.TrimSpace(query)
	if query == "" {
		return nil, &Error{Code: ErrorCodeInvalidArgument}
	}
	if limit <= 0 || limit > 200 {
		limit = 50
	}
	params := url.Values{}
	params.Set("query", query)
	params.Set("limit", strconv.Itoa(limit))
	params.Set("includeGuids", "1")

	var payload struct {
		MediaContainer struct {
			Hub []struct {
				Type     string              `json:"type"`
				Metadata []plexMetadataEntry `json:"Metadata"`
			} `json:"Hub"`
		} `json:"MediaContainer"`
	}
	if err := c.getJSON(ctx, c.endpoint("/hubs/search?"+params.Encode()), &payload); err != nil {
		return nil, err
	}
	wantType := strings.ToLower(strings.TrimSpace(mediaType))
	items := make([]Item, 0)
	for _, hub := range payload.MediaContainer.Hub {
		for _, entry := range hub.Metadata {
			item := entry.toItem("")
			if wantType != "" && item.Type != wantType {
				continue
			}
			items = append(items, item)
			if len(items) >= limit {
				return items, nil
			}
		}
	}
	return items, nil
}

// RefreshAll 触发 Plex 刷新所有媒体库分区。
func (c *Client) RefreshAll(ctx context.Context) error {
	if ctx == nil {
		return &Error{Code: ErrorCodeInvalidArgument}
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.endpoint("/library/sections/all/refresh"), nil)
	if err != nil {
		return &Error{Code: ErrorCodeInvalidArgument, cause: err}
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("X-Plex-Token", c.token)
	resp, err := c.client.Do(req)
	if err != nil {
		return &Error{Code: ErrorCodeOutcomeUnknown, cause: err}
	}
	defer func() { _ = resp.Body.Close() }()
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 4096))
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return &Error{Code: ErrorCodeHTTPStatus, StatusCode: resp.StatusCode}
	}
	return nil
}

// PlayURL 生成 Plex Web 客户端的条目详情/播放链接。
func (c *Client) PlayURL(itemID string) (string, error) {
	itemID = strings.TrimSpace(itemID)
	if c == nil || c.playHost == nil || itemID == "" || strings.ContainsAny(itemID, "\r\n") {
		return "", &Error{Code: ErrorCodeInvalidArgument}
	}
	c.machineMu.RLock()
	machineID := c.machineID
	c.machineMu.RUnlock()

	key := itemID
	if !strings.HasPrefix(key, "/") {
		key = "/library/metadata/" + key
	}
	base := strings.TrimRight(c.playHost.String(), "/")
	if machineID != "" {
		return base + "/web/index.html#!/server/" + url.PathEscape(machineID) + "/details?key=" + url.QueryEscape(key), nil
	}
	return base + "/web/index.html#!/details?key=" + url.QueryEscape(key), nil
}
