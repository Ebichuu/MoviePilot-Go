package metadata

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
)

const (
	aniListSearchQuery = `query ($search: String!, $count: Int!) {
  Page(page: 1, perPage: $count) {
    media(search: $search, type: ANIME, sort: SEARCH_MATCH) {
      id title { romaji english native } seasonYear startDate { year }
      description(asHtml: false) coverImage { extraLarge large }
		bannerImage averageScore episodes synonyms
    }
  }
}`
	aniListDetailQuery = `query ($id: Int!) {
  Media(id: $id, type: ANIME) {
    id title { romaji english native } seasonYear startDate { year }
    description(asHtml: false) coverImage { extraLarge large }
		bannerImage averageScore episodes synonyms
  }
}`
)

// AniList 是 AniList 官方 GraphQL API 的强类型客户端。
type AniList struct {
	http     *HTTPClient
	endpoint string
}

func NewAniList(client *HTTPClient, endpoint string) *AniList {
	return &AniList{http: client, endpoint: strings.TrimRight(endpoint, "/")}
}

func (c *AniList) Name() string { return "anilist" }

func (c *AniList) Search(ctx context.Context, keyword string) ([]Media, error) {
	if strings.TrimSpace(keyword) == "" {
		return nil, errors.New("AniList 搜索关键词不能为空")
	}
	var response struct {
		Data struct {
			Page struct {
				Media []aniListMedia `json:"media"`
			} `json:"Page"`
		} `json:"data"`
		Errors []aniListError `json:"errors"`
	}
	if err := c.do(ctx, aniListSearchQuery, map[string]any{"search": keyword, "count": 20}, &response); err != nil {
		return nil, err
	}
	if err := aniListResponseError(response.Errors); err != nil {
		return nil, err
	}
	result := make([]Media, 0, len(response.Data.Page.Media))
	for _, item := range response.Data.Page.Media {
		if item.ID > 0 {
			result = append(result, item.media())
		}
	}
	return result, nil
}

func (c *AniList) Detail(ctx context.Context, id string) (Media, error) {
	mediaID, err := strconv.Atoi(strings.TrimSpace(id))
	if err != nil || mediaID <= 0 {
		return Media{}, errors.New("AniList ID 必须是正整数")
	}
	var response struct {
		Data struct {
			Media aniListMedia `json:"Media"`
		} `json:"data"`
		Errors []aniListError `json:"errors"`
	}
	if err := c.do(ctx, aniListDetailQuery, map[string]any{"id": mediaID}, &response); err != nil {
		return Media{}, err
	}
	if err := aniListResponseError(response.Errors); err != nil {
		return Media{}, err
	}
	if response.Data.Media.ID == 0 {
		return Media{}, errors.New("AniList 媒体不存在")
	}
	return response.Data.Media.media(), nil
}

func (c *AniList) do(ctx context.Context, query string, variables map[string]any, target any) error {
	if c.http == nil || c.endpoint == "" {
		return errors.New("AniList 客户端未配置")
	}
	return c.http.DoJSON(ctx, http.MethodPost, c.endpoint, nil, map[string]any{
		"query": query, "variables": variables,
	}, target)
}

type aniListError struct {
	Message string `json:"message"`
}

func aniListResponseError(items []aniListError) error {
	if len(items) == 0 {
		return nil
	}
	return fmt.Errorf("AniList 返回错误: %s", firstNonEmpty(items[0].Message, "unknown error"))
}

type aniListMedia struct {
	ID    int `json:"id"`
	Title struct {
		Romaji  string `json:"romaji"`
		English string `json:"english"`
		Native  string `json:"native"`
	} `json:"title"`
	SeasonYear int `json:"seasonYear"`
	StartDate  struct {
		Year int `json:"year"`
	} `json:"startDate"`
	Description string `json:"description"`
	CoverImage  struct {
		ExtraLarge string `json:"extraLarge"`
		Large      string `json:"large"`
	} `json:"coverImage"`
	BannerImage  string   `json:"bannerImage"`
	AverageScore int      `json:"averageScore"`
	Episodes     int      `json:"episodes"`
	Synonyms     []string `json:"synonyms"`
}

func (m aniListMedia) media() Media {
	title := firstNonEmpty(m.Title.English, m.Title.Romaji, m.Title.Native)
	year := m.SeasonYear
	if year == 0 {
		year = m.StartDate.Year
	}
	aliases := append([]string{m.Title.Native, m.Title.Romaji, m.Title.English}, m.Synonyms...)
	filteredAliases := make([]string, 0, len(aliases))
	for _, alias := range uniqueAliases(aliases...) {
		if !strings.EqualFold(alias, title) {
			filteredAliases = append(filteredAliases, alias)
		}
	}
	return Media{
		Source: "anilist", ID: strconv.Itoa(m.ID), Type: "tv", Title: title,
		OriginalTitle: firstNonEmpty(m.Title.Romaji, m.Title.Native), Year: year,
		Overview: m.Description, PosterURL: firstNonEmpty(m.CoverImage.ExtraLarge, m.CoverImage.Large),
		BackdropURL: m.BannerImage, VoteAverage: float64(m.AverageScore) / 10,
		Aliases: filteredAliases, EpisodeCount: m.Episodes,
	}
}
