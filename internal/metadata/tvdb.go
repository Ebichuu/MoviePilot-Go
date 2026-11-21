package metadata

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"
)

const defaultTVDBBaseURL = "https://api4.thetvdb.com/v4"

// TVDBSeriesDetail 是 TheTVDB v4 剧集详情的精简结构，供刮削和元数据源补全使用。
type TVDBSeriesDetail struct {
	ID            int
	Title         string
	OriginalTitle string
	Year          int
	Overview      string
	Rating        float64
	Premiered     string
	PosterURL     string
	BackdropURL   string
	Genres        []string
	IMDBID        string
	TMDBID        int
}

// TVDBEpisodeDetail 是 TheTVDB v4 单集详情的精简结构。
type TVDBEpisodeDetail struct {
	ID       int
	Season   int
	Episode  int
	Title    string
	Overview string
	AirDate  string
	Runtime  int
	ImageURL string
}

// TVDB 是纯 Go TheTVDB API v4 客户端，支持自动 JWT 登录、401 刷新重登与元数据 Provider 契约。
type TVDB struct {
	http    *HTTPClient
	baseURL string
	apiKey  string
	pin     string

	mu    sync.Mutex
	token string
}

// NewTVDB 创建 TheTVDB v4 客户端。
func NewTVDB(client *HTTPClient, baseURL, apiKey, pin string) *TVDB {
	baseURL = strings.TrimRight(strings.TrimSpace(baseURL), "/")
	if baseURL == "" {
		baseURL = defaultTVDBBaseURL
	}
	return &TVDB{
		http:    client,
		baseURL: baseURL,
		apiKey:  strings.TrimSpace(apiKey),
		pin:     strings.TrimSpace(pin),
	}
}

// Name 返回元数据源标识（实现 metadata.Provider）。
func (t *TVDB) Name() string { return "thetvdb" }

func (t *TVDB) ensureToken(ctx context.Context, forceRefresh bool) (string, error) {
	if t == nil || t.http == nil || t.apiKey == "" {
		return "", fmt.Errorf("thetvdb client not configured")
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	if !forceRefresh && t.token != "" {
		return t.token, nil
	}
	payload := map[string]string{"apikey": t.apiKey}
	if t.pin != "" {
		payload["pin"] = t.pin
	}
	bodyBytes, err := json.Marshal(payload)
	if err != nil {
		return "", err
	}
	var resp struct {
		Status string `json:"status"`
		Data   struct {
			Token string `json:"token"`
		} `json:"data"`
	}
	headers := http.Header{
		"Content-Type": []string{"application/json"},
		"Accept":       []string{"application/json"},
	}
	if err := t.http.DoJSON(ctx, http.MethodPost, t.baseURL+"/login", headers, bytes.NewReader(bodyBytes), &resp); err != nil {
		return "", fmt.Errorf("thetvdb login failed: %w", err)
	}
	tok := strings.TrimSpace(resp.Data.Token)
	if tok == "" {
		return "", fmt.Errorf("thetvdb login returned empty token")
	}
	t.token = tok
	return tok, nil
}

func (t *TVDB) getAuthedJSON(ctx context.Context, endpoint string, out any) error {
	tok, err := t.ensureToken(ctx, false)
	if err != nil {
		return err
	}
	headers := http.Header{
		"Authorization": []string{"Bearer " + tok},
		"Accept":        []string{"application/json"},
	}
	err = t.http.DoJSON(ctx, http.MethodGet, endpoint, headers, nil, out)
	if err != nil && strings.Contains(err.Error(), "401") {
		tok, loginErr := t.ensureToken(ctx, true)
		if loginErr != nil {
			return loginErr
		}
		headers.Set("Authorization", "Bearer "+tok)
		return t.http.DoJSON(ctx, http.MethodGet, endpoint, headers, nil, out)
	}
	return err
}

// Search 实现 metadata.Provider.Search，检索 TheTVDB 剧集或电影。
func (t *TVDB) Search(ctx context.Context, keyword string) ([]Media, error) {
	keyword = strings.TrimSpace(keyword)
	if keyword == "" {
		return nil, nil
	}
	params := url.Values{}
	params.Set("query", keyword)
	params.Set("type", "series")

	var resp struct {
		Data []struct {
			TVDBID       string            `json:"tvdb_id"`
			ObjectID     string            `json:"objectID"`
			Name         string            `json:"name"`
			Type         string            `json:"type"`
			Year         string            `json:"year"`
			Overview     string            `json:"overview"`
			ImageURL     string            `json:"image_url"`
			PrimaryLang  string            `json:"primary_language"`
			Translations map[string]string `json:"translations"`
			Overviews    map[string]string `json:"overviews"`
		} `json:"data"`
	}
	if err := t.getAuthedJSON(ctx, t.baseURL+"/search?"+params.Encode(), &resp); err != nil {
		return nil, err
	}
	items := make([]Media, 0, len(resp.Data))
	for _, entry := range resp.Data {
		id := strings.TrimSpace(entry.TVDBID)
		if id == "" {
			id = strings.TrimSpace(entry.ObjectID)
		}
		if id == "" {
			continue
		}
		title := firstNonEmptyString(
			entry.Translations["zho"],
			entry.Translations["zh"],
			entry.Translations["zht"],
			entry.Name,
			entry.Translations["eng"],
		)
		overview := firstNonEmptyString(
			entry.Overviews["zho"],
			entry.Overviews["zh"],
			entry.Overview,
			entry.Overviews["eng"],
		)
		year, _ := strconv.Atoi(strings.TrimSpace(entry.Year))
		mediaType := "tv"
		if strings.EqualFold(entry.Type, "movie") {
			mediaType = "movie"
		}
		items = append(items, Media{
			Source:           "thetvdb",
			ID:               id,
			Type:             mediaType,
			Title:            title,
			OriginalTitle:    entry.Name,
			OriginalLanguage: entry.PrimaryLang,
			Year:             year,
			Overview:         overview,
			PosterURL:        strings.TrimSpace(entry.ImageURL),
		})
	}
	return items, nil
}

// Detail 实现 metadata.Provider.Detail，按 TVDB ID 查询剧集扩展详情。
func (t *TVDB) Detail(ctx context.Context, id string) (Media, error) {
	cleaned := strings.TrimSpace(id)
	cleaned = strings.TrimPrefix(cleaned, "thetvdb:")
	cleaned = strings.TrimPrefix(cleaned, "tvdb:")
	cleaned = strings.TrimPrefix(cleaned, "tv:")
	tvdbID, err := strconv.Atoi(cleaned)
	if err != nil || tvdbID <= 0 {
		return Media{}, fmt.Errorf("invalid thetvdb id %q", id)
	}
	detail, err := t.GetSeriesExtended(ctx, tvdbID)
	if err != nil {
		return Media{}, err
	}
	tmdbStr := ""
	if detail.TMDBID > 0 {
		tmdbStr = strconv.Itoa(detail.TMDBID)
	}
	return Media{
		Source:        "thetvdb",
		ID:            strconv.Itoa(detail.ID),
		TMDBID:        tmdbStr,
		Type:          "tv",
		Title:         detail.Title,
		OriginalTitle: detail.OriginalTitle,
		Year:          detail.Year,
		Overview:      detail.Overview,
		PosterURL:     detail.PosterURL,
		BackdropURL:   detail.BackdropURL,
		VoteAverage:   detail.Rating,
		Genres:        append([]string(nil), detail.Genres...),
		ReleaseDate:   detail.Premiered,
	}, nil
}

// GetSeriesExtended 获取 TheTVDB v4 剧集完整信息（含中文/英文翻译、海报/背景图及外部 ID）。
func (t *TVDB) GetSeriesExtended(ctx context.Context, tvdbID int) (*TVDBSeriesDetail, error) {
	if tvdbID <= 0 {
		return nil, fmt.Errorf("invalid tvdb id %d", tvdbID)
	}
	endpoint := fmt.Sprintf("%s/series/%d/extended?meta=translations&short=false", t.baseURL, tvdbID)
	var resp struct {
		Data struct {
			ID       int     `json:"id"`
			Name     string  `json:"name"`
			Year     string  `json:"year"`
			FirstAir string  `json:"firstAired"`
			Overview string  `json:"overview"`
			Image    string  `json:"image"`
			Score    float64 `json:"score"`
			Genres   []struct {
				Name string `json:"name"`
			} `json:"genres"`
			Artworks []struct {
				Image    string  `json:"image"`
				Type     int     `json:"type"`
				Language string  `json:"language"`
				Score    float64 `json:"score"`
			} `json:"artworks"`
			RemoteIDs []struct {
				ID         string `json:"id"`
				SourceName string `json:"sourceName"`
			} `json:"remoteIds"`
			Translations struct {
				NameTranslations []struct {
					Name     string `json:"name"`
					Language string `json:"language"`
				} `json:"nameTranslations"`
				OverviewTranslations []struct {
					Overview string `json:"overview"`
					Language string `json:"language"`
				} `json:"overviewTranslations"`
			} `json:"translations"`
		} `json:"data"`
	}
	if err := t.getAuthedJSON(ctx, endpoint, &resp); err != nil {
		return nil, err
	}
	d := resp.Data
	title := d.Name
	for _, lang := range []string{"zho", "zh", "zht"} {
		for _, tr := range d.Translations.NameTranslations {
			if strings.EqualFold(tr.Language, lang) && strings.TrimSpace(tr.Name) != "" {
				title = strings.TrimSpace(tr.Name)
				break
			}
		}
		if title != d.Name {
			break
		}
	}
	overview := d.Overview
	for _, lang := range []string{"zho", "zh", "zht", "eng"} {
		found := false
		for _, tr := range d.Translations.OverviewTranslations {
			if strings.EqualFold(tr.Language, lang) && strings.TrimSpace(tr.Overview) != "" {
				overview = strings.TrimSpace(tr.Overview)
				found = true
				break
			}
		}
		if found {
			break
		}
	}
	posterURL := strings.TrimSpace(d.Image)
	backdropURL := ""
	bestPosterScore := -1.0
	bestBackdropScore := -1.0
	for _, art := range d.Artworks {
		img := strings.TrimSpace(art.Image)
		if img == "" {
			continue
		}
		// TVDB v4 artwork types: 2 = Series Poster, 3 = Series Background
		switch art.Type {
		case 2:
			if posterURL == "" || art.Score > bestPosterScore {
				posterURL = img
				bestPosterScore = art.Score
			}
		case 3:
			if art.Score > bestBackdropScore {
				backdropURL = img
				bestBackdropScore = art.Score
			}
		}
	}
	genres := make([]string, 0, len(d.Genres))
	for _, g := range d.Genres {
		if name := strings.TrimSpace(g.Name); name != "" {
			genres = append(genres, name)
		}
	}
	imdbID := ""
	tmdbID := 0
	for _, remote := range d.RemoteIDs {
		switch strings.ToLower(strings.TrimSpace(remote.SourceName)) {
		case "imdb":
			imdbID = strings.TrimSpace(remote.ID)
		case "themoviedb.com", "tmdb":
			tmdbID, _ = strconv.Atoi(strings.TrimSpace(remote.ID))
		}
	}
	year, _ := strconv.Atoi(strings.TrimSpace(d.Year))
	if year == 0 && len(d.FirstAir) >= 4 {
		year, _ = strconv.Atoi(d.FirstAir[:4])
	}
	return &TVDBSeriesDetail{
		ID:            d.ID,
		Title:         title,
		OriginalTitle: d.Name,
		Year:          year,
		Overview:      overview,
		Rating:        d.Score,
		Premiered:     d.FirstAir,
		PosterURL:     posterURL,
		BackdropURL:   backdropURL,
		Genres:        genres,
		IMDBID:        imdbID,
		TMDBID:        tmdbID,
	}, nil
}

// GetEpisode 查询指定 TVDB 剧集某季某集的元数据（优先中文翻译，缺失时回退默认语言）。
func (t *TVDB) GetEpisode(ctx context.Context, tvdbID, season, episode int) (*TVDBEpisodeDetail, error) {
	if tvdbID <= 0 || season < 0 || episode <= 0 {
		return nil, fmt.Errorf("invalid tvdb episode query")
	}
	endpoints := []string{
		fmt.Sprintf("%s/series/%d/episodes/default/zho?season=%d&episodeNumber=%d", t.baseURL, tvdbID, season, episode),
		fmt.Sprintf("%s/series/%d/episodes/default?season=%d&episodeNumber=%d", t.baseURL, tvdbID, season, episode),
	}
	var resp struct {
		Data struct {
			Episodes []struct {
				ID            int    `json:"id"`
				Name          string `json:"name"`
				Overview      string `json:"overview"`
				Aired         string `json:"aired"`
				Runtime       int    `json:"runtime"`
				Image         string `json:"image"`
				SeasonNumber  int    `json:"seasonNumber"`
				EpisodeNumber int    `json:"number"`
			} `json:"episodes"`
		} `json:"data"`
	}
	for _, epURL := range endpoints {
		resp.Data.Episodes = nil
		if err := t.getAuthedJSON(ctx, epURL, &resp); err != nil {
			continue
		}
		for _, ep := range resp.Data.Episodes {
			if ep.SeasonNumber == season && ep.EpisodeNumber == episode {
				return &TVDBEpisodeDetail{
					ID:       ep.ID,
					Season:   ep.SeasonNumber,
					Episode:  ep.EpisodeNumber,
					Title:    strings.TrimSpace(ep.Name),
					Overview: strings.TrimSpace(ep.Overview),
					AirDate:  strings.TrimSpace(ep.Aired),
					Runtime:  ep.Runtime,
					ImageURL: strings.TrimSpace(ep.Image),
				}, nil
			}
		}
	}
	return nil, fmt.Errorf("tvdb episode S%02dE%02d not found for %d", season, episode, tvdbID)
}

func firstNonEmptyString(values ...string) string {
	for _, v := range values {
		if s := strings.TrimSpace(v); s != "" {
			return s
		}
	}
	return ""
}

func parseDateOrZero(raw string) time.Time {
	t, _ := time.Parse("2006-01-02", strings.TrimSpace(raw))
	return t
}

var _ Provider = (*TVDB)(nil)
