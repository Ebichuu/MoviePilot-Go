package metadata

import (
	"context"
	"crypto/hmac"
	"crypto/sha1"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"
)

const doubanUserAgent = "api-client/1 com.douban.frodo/7.22.0 Android/23 platform/AndroidPad"

// Douban 是豆瓣 Frodo API 的强类型客户端，凭据由部署配置注入。
type Douban struct {
	http      *HTTPClient
	baseURL   string
	basePath  string
	apiKey    string
	secretKey string
	now       func() time.Time
}

// NewDouban 创建豆瓣客户端。baseURL 通常为 https://frodo.douban.com/api/v2。
func NewDouban(client *HTTPClient, baseURL, apiKey, secretKey string) *Douban {
	base := strings.TrimRight(baseURL, "/")
	basePath := ""
	if parsed, err := url.Parse(base); err == nil {
		basePath = strings.TrimRight(parsed.Path, "/")
	}
	return &Douban{
		http: client, baseURL: base, basePath: basePath, apiKey: apiKey,
		secretKey: secretKey, now: time.Now,
	}
}

func (c *Douban) Name() string { return "douban" }

func (c *Douban) Search(ctx context.Context, keyword string) ([]Media, error) {
	if strings.TrimSpace(keyword) == "" {
		return nil, errors.New("豆瓣搜索关键词不能为空")
	}
	var response struct {
		Items []struct {
			TypeName string        `json:"type_name"`
			Target   doubanSubject `json:"target"`
		} `json:"items"`
	}
	query := url.Values{"q": {keyword}, "start": {"0"}, "count": {"20"}}
	if err := c.get(ctx, "/search/weixin", query, &response); err != nil {
		return nil, err
	}
	result := make([]Media, 0, len(response.Items))
	for _, item := range response.Items {
		mediaType := doubanMediaType(item.TypeName, item.Target.Type)
		if mediaType == "" || item.Target.ID == "" || item.Target.Title == "" {
			continue
		}
		result = append(result, item.Target.media(mediaType))
	}
	return result, nil
}

// Detail 查询豆瓣详情，ID 使用 movie:123 或 tv:123。
func (c *Douban) Detail(ctx context.Context, id string) (Media, error) {
	mediaType, subjectID, err := typedMediaID(id)
	if err != nil {
		return Media{}, err
	}
	var response doubanSubject
	if err := c.get(ctx, "/"+mediaType+"/"+url.PathEscape(subjectID), nil, &response); err != nil {
		return Media{}, err
	}
	if response.ID == "" {
		response.ID = flexibleString(subjectID)
	}
	return response.media(mediaType), nil
}

func (c *Douban) get(ctx context.Context, path string, query url.Values, target any) error {
	if c.http == nil || c.baseURL == "" {
		return errors.New("豆瓣客户端未配置")
	}
	if query == nil {
		query = make(url.Values)
	}
	path = "/" + strings.TrimPrefix(path, "/")
	timestamp := c.now().Format("20060102")
	query.Set("apiKey", c.apiKey)
	query.Set("os_rom", "android")
	query.Set("_ts", timestamp)
	if c.secretKey != "" {
		query.Set("_sig", c.signature(c.basePath+path, timestamp))
	}
	headers := make(http.Header)
	headers.Set("User-Agent", doubanUserAgent)
	if err := c.http.DoJSON(ctx, http.MethodGet, c.baseURL+path+"?"+query.Encode(), headers, nil, target); err != nil {
		return fmt.Errorf("豆瓣请求 %s: %w", path, err)
	}
	return nil
}

func (c *Douban) signature(path, timestamp string) string {
	mac := hmac.New(sha1.New, []byte(c.secretKey))
	_, _ = mac.Write([]byte("GET&" + url.QueryEscape(path) + "&" + timestamp))
	return base64.StdEncoding.EncodeToString(mac.Sum(nil))
}

type flexibleString string

func (s *flexibleString) UnmarshalJSON(value []byte) error {
	var text string
	if len(value) > 0 && value[0] == '"' {
		if err := json.Unmarshal(value, &text); err != nil {
			return err
		}
		*s = flexibleString(text)
		return nil
	}
	*s = flexibleString(strings.TrimSpace(string(value)))
	return nil
}

type doubanSubject struct {
	ID            flexibleString `json:"id"`
	Type          string         `json:"type"`
	Title         string         `json:"title"`
	OriginalTitle string         `json:"original_title"`
	Year          flexibleString `json:"year"`
	Intro         string         `json:"intro"`
	CoverURL      string         `json:"cover_url"`
	AKA           []string       `json:"aka"`
	Pic           struct {
		Large  string `json:"large"`
		Normal string `json:"normal"`
	} `json:"pic"`
	Cover struct {
		URL string `json:"url"`
	} `json:"cover"`
	Rating struct {
		Value float64 `json:"value"`
	} `json:"rating"`
	EpisodesCount int `json:"episodes_count"`
}

func (s doubanSubject) media(mediaType string) Media {
	year := yearFromDate(string(s.Year))
	aliases := append([]string(nil), s.AKA...)
	aliases = append(aliases, s.OriginalTitle)
	return Media{
		Source: "douban", ID: string(s.ID), Type: mediaType, Title: s.Title,
		OriginalTitle: s.OriginalTitle, Year: year, Overview: s.Intro,
		PosterURL:    firstNonEmpty(s.CoverURL, s.Pic.Large, s.Pic.Normal, s.Cover.URL),
		VoteAverage:  s.Rating.Value,
		Aliases:      uniqueAliases(aliases...),
		EpisodeCount: s.EpisodesCount,
	}
}

func doubanMediaType(typeName, subjectType string) string {
	switch strings.ToLower(strings.TrimSpace(subjectType)) {
	case "movie":
		return "movie"
	case "tv":
		return "tv"
	}
	switch strings.TrimSpace(typeName) {
	case "电影":
		return "movie"
	case "电视剧", "电视", "剧集":
		return "tv"
	default:
		return ""
	}
}

func typedMediaID(id string) (string, string, error) {
	parts := strings.SplitN(strings.TrimSpace(id), ":", 2)
	if len(parts) != 2 || (parts[0] != "movie" && parts[0] != "tv") || strings.TrimSpace(parts[1]) == "" {
		return "", "", errors.New("媒体 ID 必须使用 movie:123 或 tv:123")
	}
	return parts[0], parts[1], nil
}
