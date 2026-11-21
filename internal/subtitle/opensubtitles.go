package subtitle

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
)

const (
	defaultOpenSubtitlesBaseURL = "https://api.opensubtitles.com/api/v1"
	openSubtitlesSiteID         = 9001
	openSubtitlesSiteName       = "OpenSubtitles"
)

// OpenSubtitlesConfig 配置 OpenSubtitles REST API v1 字幕源。
type OpenSubtitlesConfig struct {
	BaseURL string
	APIKey  string
	Token   string
	Client  *http.Client
}

// OpenSubtitlesProvider 通过 OpenSubtitles REST API v1 搜索与解析字幕下载链接。
type OpenSubtitlesProvider struct {
	baseURL string
	apiKey  string
	token   string
	client  *http.Client
}

// NewOpenSubtitlesProvider 创建 OpenSubtitles 字幕提供方。
func NewOpenSubtitlesProvider(cfg OpenSubtitlesConfig) (*OpenSubtitlesProvider, error) {
	apiKey := strings.TrimSpace(cfg.APIKey)
	if apiKey == "" {
		return nil, fmt.Errorf("opensubtitles api key is required")
	}
	baseURL := strings.TrimRight(strings.TrimSpace(cfg.BaseURL), "/")
	if baseURL == "" {
		baseURL = defaultOpenSubtitlesBaseURL
	}
	client := cfg.Client
	if client == nil {
		client = &http.Client{Timeout: defaultSubtitleTimeout}
	}
	return &OpenSubtitlesProvider{
		baseURL: baseURL,
		apiKey:  apiKey,
		token:   strings.TrimSpace(cfg.Token),
		client:  client,
	}, nil
}

// Name 返回字幕源名称。
func (p *OpenSubtitlesProvider) Name() string { return openSubtitlesSiteName }

// SiteID 返回稳定站点编号。
func (p *OpenSubtitlesProvider) SiteID() int { return openSubtitlesSiteID }

// Search 调用 OpenSubtitles `GET /subtitles` 检索字幕。
func (p *OpenSubtitlesProvider) Search(ctx context.Context, query Query) ([]Subtitle, error) {
	params := url.Values{}
	if kw := strings.TrimSpace(query.Keyword); kw != "" {
		params.Set("query", kw)
	}
	if imdb := strings.TrimSpace(query.IMDBID); imdb != "" {
		params.Set("imdb_id", strings.TrimPrefix(strings.ToLower(imdb), "tt"))
	}
	if query.TMDBID > 0 {
		params.Set("tmdb_id", strconv.Itoa(query.TMDBID))
	}
	if query.Season != nil {
		params.Set("season_number", strconv.Itoa(*query.Season))
	}
	if query.Episode != nil {
		params.Set("episode_number", strconv.Itoa(*query.Episode))
	}
	if len(query.Languages) > 0 {
		params.Set("languages", strings.Join(query.Languages, ","))
	}
	if query.Page > 0 {
		params.Set("page", strconv.Itoa(query.Page))
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, p.baseURL+"/subtitles?"+params.Encode(), nil)
	if err != nil {
		return nil, err
	}
	p.applyHeaders(req)

	resp, err := p.client.Do(req)
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, fmt.Errorf("opensubtitles status %d", resp.StatusCode)
	}
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if err != nil {
		return nil, err
	}
	var payload struct {
		Data []struct {
			ID         string `json:"id"`
			Attributes struct {
				SubtitleID    string `json:"subtitle_id"`
				Language      string `json:"language"`
				DownloadCount int    `json:"download_count"`
				Release       string `json:"release"`
				UploadDate    string `json:"upload_date"`
				URL           string `json:"url"`
				Uploader      struct {
					Name string `json:"name"`
				} `json:"uploader"`
				Files []struct {
					FileID   int    `json:"file_id"`
					FileName string `json:"file_name"`
				} `json:"files"`
			} `json:"attributes"`
		} `json:"data"`
	}
	if err := json.Unmarshal(raw, &payload); err != nil {
		return nil, err
	}

	se := formatSeasonEpisode(query.Season, query.Episode)
	var episodes []int
	if query.Episode != nil {
		episodes = []int{*query.Episode}
	}

	items := make([]Subtitle, 0, len(payload.Data))
	for _, entry := range payload.Data {
		attr := entry.Attributes
		fileID := 0
		fileName := ""
		if len(attr.Files) > 0 {
			fileID = attr.Files[0].FileID
			fileName = strings.TrimSpace(attr.Files[0].FileName)
		}
		if fileID <= 0 {
			continue
		}
		title := strings.TrimSpace(attr.Release)
		if title == "" {
			title = fileName
		}
		subID := strings.TrimSpace(attr.SubtitleID)
		if subID == "" {
			subID = strconv.Itoa(fileID)
		}
		items = append(items, Subtitle{
			Site:          openSubtitlesSiteID,
			SiteName:      openSubtitlesSiteName,
			Title:         title,
			Description:   fmt.Sprintf("OpenSubtitles (%s)", attr.Language),
			Enclosure:     fmt.Sprintf("opensubtitles://%d", fileID),
			PageURL:       attr.URL,
			Language:      attr.Language,
			PubDate:       attr.UploadDate,
			Grabs:         attr.DownloadCount,
			Uploader:      attr.Uploader.Name,
			SubtitleID:    subID,
			FileName:      fileName,
			SeasonEpisode: se,
			EpisodeList:   episodes,
		})
	}
	return items, nil
}

// ResolveDownload 调用 `POST /download` 换取临时下载直链。
func (p *OpenSubtitlesProvider) ResolveDownload(ctx context.Context, item Subtitle) (string, string, map[string]string, error) {
	idStr := strings.TrimPrefix(strings.ToLower(strings.TrimSpace(item.Enclosure)), "opensubtitles://")
	if idStr == "" || strings.Contains(idStr, "://") {
		idStr = strings.TrimSpace(item.SubtitleID)
	}
	fileID, err := strconv.Atoi(idStr)
	if err != nil || fileID <= 0 {
		return "", "", nil, fmt.Errorf("invalid opensubtitles file_id %q", idStr)
	}
	body, _ := json.Marshal(map[string]any{"file_id": fileID})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, p.baseURL+"/download", bytes.NewReader(body))
	if err != nil {
		return "", "", nil, err
	}
	p.applyHeaders(req)
	req.Header.Set("Content-Type", "application/json")

	resp, err := p.client.Do(req)
	if err != nil {
		return "", "", nil, err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return "", "", nil, fmt.Errorf("opensubtitles download status %d", resp.StatusCode)
	}
	var out struct {
		Link     string `json:"link"`
		FileName string `json:"file_name"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&out); err != nil {
		return "", "", nil, err
	}
	if strings.TrimSpace(out.Link) == "" {
		return "", "", nil, fmt.Errorf("opensubtitles returned empty link")
	}
	name := strings.TrimSpace(out.FileName)
	if name == "" {
		name = strings.TrimSpace(item.FileName)
	}
	return strings.TrimSpace(out.Link), name, nil, nil
}

func (p *OpenSubtitlesProvider) applyHeaders(req *http.Request) {
	req.Header.Set("Api-Key", p.apiKey)
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", "MoviePilot-Go v1.0")
	if p.token != "" {
		req.Header.Set("Authorization", "Bearer "+p.token)
	}
}
