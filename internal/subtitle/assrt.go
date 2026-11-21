package subtitle

import (
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
	defaultAssrtBaseURL = "https://api.assrt.net/v1"
	assrtSiteID         = 9002
	assrtSiteName       = "Assrt"
)

// AssrtConfig 配置伪射手（Assrt）API v1 字幕源。
type AssrtConfig struct {
	BaseURL string
	Token   string
	Client  *http.Client
}

// AssrtProvider 通过伪射手（Assrt）API v1 搜索中文字幕并解析下载链接。
type AssrtProvider struct {
	baseURL string
	token   string
	client  *http.Client
}

// NewAssrtProvider 创建 Assrt 字幕提供方。
func NewAssrtProvider(cfg AssrtConfig) (*AssrtProvider, error) {
	token := strings.TrimSpace(cfg.Token)
	if token == "" {
		return nil, fmt.Errorf("assrt token is required")
	}
	baseURL := strings.TrimRight(strings.TrimSpace(cfg.BaseURL), "/")
	if baseURL == "" {
		baseURL = defaultAssrtBaseURL
	}
	client := cfg.Client
	if client == nil {
		client = &http.Client{Timeout: defaultSubtitleTimeout}
	}
	return &AssrtProvider{
		baseURL: baseURL,
		token:   token,
		client:  client,
	}, nil
}

// Name 返回字幕源名称。
func (p *AssrtProvider) Name() string { return assrtSiteName }

// SiteID 返回稳定站点编号。
func (p *AssrtProvider) SiteID() int { return assrtSiteID }

// Search 调用 Assrt `GET /sub/search` 检索字幕。
func (p *AssrtProvider) Search(ctx context.Context, query Query) ([]Subtitle, error) {
	searchText := buildAssrtQuery(query)
	if searchText == "" {
		return nil, nil
	}
	params := url.Values{}
	params.Set("token", p.token)
	params.Set("q", searchText)
	params.Set("cnt", "15")
	if query.Page > 1 {
		params.Set("pos", strconv.Itoa((query.Page-1)*15))
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, p.baseURL+"/sub/search?"+params.Encode(), nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", "MoviePilot-Go/1.0")
	resp, err := p.client.Do(req)
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, fmt.Errorf("assrt status %d", resp.StatusCode)
	}
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if err != nil {
		return nil, err
	}
	var payload struct {
		Status int    `json:"status"`
		ErrMsg string `json:"errmsg"`
		Sub    struct {
			Subs []struct {
				ID          int    `json:"id"`
				NativeName  string `json:"native_name"`
				VideoName   string `json:"videoname"`
				Subtype     string `json:"subtype"`
				UploadTime  string `json:"upload_time"`
				VoteScore   int    `json:"vote_score"`
				ReleaseSite string `json:"release_site"`
				Lang        struct {
					Desc string `json:"desc"`
				} `json:"lang"`
			} `json:"subs"`
		} `json:"sub"`
	}
	if err := json.Unmarshal(raw, &payload); err != nil {
		return nil, err
	}
	if payload.Status != 0 {
		return nil, fmt.Errorf("assrt error %d: %s", payload.Status, payload.ErrMsg)
	}

	se := formatSeasonEpisode(query.Season, query.Episode)
	var episodes []int
	if query.Episode != nil {
		episodes = []int{*query.Episode}
	}

	items := make([]Subtitle, 0, len(payload.Sub.Subs))
	for _, entry := range payload.Sub.Subs {
		if entry.ID <= 0 {
			continue
		}
		title := strings.TrimSpace(entry.NativeName)
		if title == "" {
			title = strings.TrimSpace(entry.VideoName)
		}
		lang := strings.TrimSpace(entry.Lang.Desc)
		if lang == "" {
			lang = "zh"
		}
		items = append(items, Subtitle{
			Site:          assrtSiteID,
			SiteName:      assrtSiteName,
			Title:         title,
			Description:   strings.TrimSpace(entry.VideoName),
			Enclosure:     fmt.Sprintf("assrt://%d", entry.ID),
			PageURL:       fmt.Sprintf("https://assrt.net/xml/sub/%d/%d.xml", entry.ID/1000, entry.ID),
			Language:      lang,
			PubDate:       entry.UploadTime,
			Grabs:         entry.VoteScore,
			Uploader:      entry.ReleaseSite,
			SubtitleID:    strconv.Itoa(entry.ID),
			SeasonEpisode: se,
			EpisodeList:   episodes,
		})
	}
	return items, nil
}

// ResolveDownload 调用 Assrt `GET /sub/detail` 获取真实归档/字幕下载链接。
func (p *AssrtProvider) ResolveDownload(ctx context.Context, item Subtitle) (string, string, map[string]string, error) {
	idStr := strings.TrimPrefix(strings.ToLower(strings.TrimSpace(item.Enclosure)), "assrt://")
	if idStr == "" || strings.Contains(idStr, "://") {
		idStr = strings.TrimSpace(item.SubtitleID)
	}
	subID, err := strconv.Atoi(idStr)
	if err != nil || subID <= 0 {
		return "", "", nil, fmt.Errorf("invalid assrt subtitle id %q", idStr)
	}
	params := url.Values{}
	params.Set("token", p.token)
	params.Set("id", strconv.Itoa(subID))

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, p.baseURL+"/sub/detail?"+params.Encode(), nil)
	if err != nil {
		return "", "", nil, err
	}
	req.Header.Set("User-Agent", "MoviePilot-Go/1.0")
	resp, err := p.client.Do(req)
	if err != nil {
		return "", "", nil, err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return "", "", nil, fmt.Errorf("assrt detail status %d", resp.StatusCode)
	}
	var payload struct {
		Status int    `json:"status"`
		ErrMsg string `json:"errmsg"`
		Sub    struct {
			Subs []struct {
				URL      string `json:"url"`
				Filename string `json:"filename"`
				FileList []struct {
					URL string `json:"url"`
					F   string `json:"f"`
				} `json:"filelist"`
			} `json:"subs"`
		} `json:"sub"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 2<<20)).Decode(&payload); err != nil {
		return "", "", nil, err
	}
	if payload.Status != 0 || len(payload.Sub.Subs) == 0 {
		return "", "", nil, fmt.Errorf("assrt detail unavailable: %s", payload.ErrMsg)
	}
	detail := payload.Sub.Subs[0]
	if strings.TrimSpace(detail.URL) != "" {
		return strings.TrimSpace(detail.URL), strings.TrimSpace(detail.Filename), nil, nil
	}
	for _, f := range detail.FileList {
		if strings.TrimSpace(f.URL) != "" {
			return strings.TrimSpace(f.URL), strings.TrimSpace(f.F), nil, nil
		}
	}
	return "", "", nil, fmt.Errorf("assrt detail missing download url")
}

func buildAssrtQuery(q Query) string {
	parts := make([]string, 0, 3)
	if kw := strings.TrimSpace(q.Keyword); kw != "" {
		parts = append(parts, kw)
	} else if imdb := strings.TrimSpace(q.IMDBID); imdb != "" {
		parts = append(parts, imdb)
	}
	if se := formatSeasonEpisode(q.Season, q.Episode); se != "" {
		parts = append(parts, se)
	} else if y := strings.TrimSpace(q.Year); y != "" {
		parts = append(parts, y)
	}
	return strings.TrimSpace(strings.Join(parts, " "))
}
