package metadata

import (
	"context"
	"errors"
	"net/http"
	"net/url"
	"strconv"
	"strings"
)

// Bangumi 是 Bangumi v0 API 的强类型客户端。
type Bangumi struct {
	http    *HTTPClient
	baseURL string
	token   string
}

func NewBangumi(client *HTTPClient, baseURL, token string) *Bangumi {
	return &Bangumi{http: client, baseURL: strings.TrimRight(baseURL, "/"), token: token}
}

func (c *Bangumi) Name() string { return "bangumi" }

func (c *Bangumi) Search(ctx context.Context, keyword string) ([]Media, error) {
	if strings.TrimSpace(keyword) == "" {
		return nil, errors.New("Bangumi 搜索关键词不能为空")
	}
	var response struct {
		Data []bangumiSubject `json:"data"`
	}
	body := map[string]any{
		"keyword": keyword,
		"sort":    "match",
		"filter":  map[string]any{"type": []int{2}},
	}
	query := url.Values{"limit": {"20"}, "offset": {"0"}}
	if err := c.do(ctx, http.MethodPost, "/v0/search/subjects?"+query.Encode(), body, &response); err != nil {
		return nil, err
	}
	result := make([]Media, 0, len(response.Data))
	for _, subject := range response.Data {
		if subject.ID <= 0 || subject.Type != 2 {
			continue
		}
		result = append(result, subject.media())
	}
	return result, nil
}

func (c *Bangumi) Detail(ctx context.Context, id string) (Media, error) {
	subjectID, err := strconv.Atoi(strings.TrimSpace(id))
	if err != nil || subjectID <= 0 {
		return Media{}, errors.New("Bangumi ID 必须是正整数")
	}
	var response bangumiSubject
	if err := c.do(ctx, http.MethodGet, "/v0/subjects/"+strconv.Itoa(subjectID), nil, &response); err != nil {
		return Media{}, err
	}
	return response.media(), nil
}

func (c *Bangumi) do(ctx context.Context, method, path string, body, target any) error {
	if c.http == nil || c.baseURL == "" {
		return errors.New("Bangumi 客户端未配置")
	}
	headers := make(http.Header)
	if c.token != "" {
		headers.Set("Authorization", "Bearer "+c.token)
	}
	return c.http.DoJSON(ctx, method, c.baseURL+path, headers, body, target)
}

type bangumiSubject struct {
	ID      int    `json:"id"`
	Type    int    `json:"type"`
	Name    string `json:"name"`
	NameCN  string `json:"name_cn"`
	Summary string `json:"summary"`
	Date    string `json:"date"`
	Images  struct {
		Large  string `json:"large"`
		Common string `json:"common"`
	} `json:"images"`
	Rating struct {
		Score float64 `json:"score"`
	} `json:"rating"`
	TotalEpisodes int `json:"total_episodes"`
}

func (s bangumiSubject) media() Media {
	return Media{
		Source: "bangumi", ID: strconv.Itoa(s.ID), Type: "tv",
		Title: firstNonEmpty(s.NameCN, s.Name), OriginalTitle: s.Name,
		Year: yearFromDate(s.Date), Overview: s.Summary,
		PosterURL:    firstNonEmpty(s.Images.Large, s.Images.Common),
		VoteAverage:  s.Rating.Score,
		Aliases:      uniqueAliases(s.NameCN, s.Name),
		EpisodeCount: s.TotalEpisodes,
	}
}
