package metadata

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync/atomic"
	"time"
)

// SeasonInfo 是元数据源明确返回的单季集数事实。
type SeasonInfo struct {
	SeasonNumber int    `json:"season_number"`
	EpisodeCount int    `json:"episode_count"`
	AirDate      string `json:"air_date,omitempty"`
}

// Media 是四个元数据源共用的最小媒体模型。
type Media struct {
	Source              string       `json:"source"`
	ID                  string       `json:"id"`
	TMDBID              string       `json:"tmdb_id,omitempty"`
	Type                string       `json:"type"`
	Title               string       `json:"title"`
	OriginalTitle       string       `json:"original_title,omitempty"`
	OriginalLanguage    string       `json:"original_language,omitempty"`
	Year                int          `json:"year,omitempty"`
	Overview            string       `json:"overview,omitempty"`
	PosterURL           string       `json:"poster_url,omitempty"`
	BackdropURL         string       `json:"backdrop_url,omitempty"`
	VoteAverage         float64      `json:"vote_average,omitempty"`
	Aliases             []string     `json:"aliases,omitempty"`
	SeasonInfo          []SeasonInfo `json:"season_info,omitempty"`
	EpisodeCount        int          `json:"episode_count,omitempty"`
	GenreIDs            []int        `json:"genre_ids,omitempty"`
	OriginCountries     []string     `json:"origin_country,omitempty"`
	ProductionCountries []string     `json:"production_countries,omitempty"`
	// 以下是详情页要用的富字段，V2 的 MediaInfo 里同样有；数据源没给就留零值，
	// 前端据此决定显不显示「片长 / 类型 / 演员」这些区块。
	ReleaseDate    string        `json:"release_date,omitempty"`
	Runtime        int           `json:"runtime,omitempty"`
	EpisodeRunTime []int         `json:"episode_run_time,omitempty"`
	VoteCount      int           `json:"vote_count,omitempty"`
	Popularity     float64       `json:"popularity,omitempty"`
	Genres         []string      `json:"genres,omitempty"`
	Actors         []MediaPerson `json:"actors,omitempty"`
	Directors      []MediaPerson `json:"directors,omitempty"`
	// 剧集的季/集数：详情页「共 N 季 M 集」用它，V2 同样取自 TMDB 的
	// number_of_seasons / number_of_episodes。
	NumberOfSeasons  int `json:"number_of_seasons,omitempty"`
	NumberOfEpisodes int `json:"number_of_episodes,omitempty"`
}

// MediaPerson 是演职员条目，字段对齐 V2 `schemas.MediaPerson`：
// 前端 PersonCard 读 source / name / character / profile_path（TMDB）/ avatar（豆瓣）。
type MediaPerson struct {
	Source      string `json:"source,omitempty"`
	ID          string `json:"id,omitempty"`
	Type        int    `json:"type,omitempty"`
	Name        string `json:"name"`
	Character   string `json:"character,omitempty"`
	ProfilePath string `json:"profile_path,omitempty"`
}

// Provider 是媒体识别层依赖的强类型元数据接口。
type Provider interface {
	Name() string
	Search(ctx context.Context, keyword string) ([]Media, error)
	Detail(ctx context.Context, id string) (Media, error)
}

// HTTPClient 对所有元数据网络请求施加并发、超时和响应体限制。
type HTTPClient struct {
	client      *http.Client
	semaphore   chan struct{}
	maxBodySize int64
	userAgent   string
	transport   *switchingTransport
	workers     int
	timeout     time.Duration
}

// switchingTransport 允许运行期切换出口代理（V2 的 PROXY_HOST 属 CONFIG_WATCH）：
// 换代理时重建连接池并用原子指针切换，避免并发改写 http.Client.Transport 造成数据竞争。
type switchingTransport struct {
	current atomic.Pointer[http.Transport]
}

func (t *switchingTransport) RoundTrip(request *http.Request) (*http.Response, error) {
	transport := t.current.Load()
	if transport == nil {
		return nil, errors.New("元数据 HTTP 传输层未初始化")
	}
	return transport.RoundTrip(request)
}

// NewHTTPClient 创建共享的受控元数据客户端。
func NewHTTPClient(workers int, timeout time.Duration, maxBodySize int64) (*HTTPClient, error) {
	if workers < 1 || workers > 128 || timeout <= 0 || maxBodySize < 1 {
		return nil, errors.New("元数据 HTTP 客户端参数无效")
	}
	client := &HTTPClient{
		semaphore: make(chan struct{}, workers), maxBodySize: maxBodySize,
		userAgent: "MoviePilot-Go/0.1", transport: &switchingTransport{},
		workers: workers, timeout: timeout,
	}
	// 没配代理时沿用环境变量（HTTP_PROXY / HTTPS_PROXY），与 Go 默认行为一致。
	proxy, err := proxyFunc("")
	if err != nil {
		return nil, err
	}
	client.transport.current.Store(client.newTransport(proxy))
	client.client = &http.Client{Transport: client.transport, Timeout: timeout}
	return client, nil
}

// newTransport 按当前并发/超时参数构造一个传输层实例。
func (c *HTTPClient) newTransport(proxy func(*http.Request) (*url.URL, error)) *http.Transport {
	return &http.Transport{
		Proxy: proxy, MaxIdleConns: c.workers * 2,
		MaxIdleConnsPerHost: c.workers, MaxConnsPerHost: c.workers,
		IdleConnTimeout: 60 * time.Second, TLSHandshakeTimeout: 10 * time.Second,
		ResponseHeaderTimeout: c.timeout,
	}
}

// SetProxy 切换出口代理（空字符串表示直连，或回落到 HTTP(S)_PROXY 环境变量）。
//
// V2 把 PROXY_HOST 列进 CONFIG_WATCH：改完立即生效、不用重启。这里同样是
// 「换连接池 + 原子切换」，旧连接池的空闲连接会被主动关掉。
func (c *HTTPClient) SetProxy(proxyURL string) error {
	if c == nil || c.transport == nil {
		return errors.New("元数据 HTTP 客户端未初始化")
	}
	proxy, err := proxyFunc(proxyURL)
	if err != nil {
		return err
	}
	previous := c.transport.current.Swap(c.newTransport(proxy))
	if previous != nil {
		previous.CloseIdleConnections()
	}
	return nil
}

// proxyFunc 把配置里的代理地址转成 http.Transport 需要的 Proxy 函数。
func proxyFunc(proxyURL string) (func(*http.Request) (*url.URL, error), error) {
	trimmed := strings.TrimSpace(proxyURL)
	if trimmed == "" {
		return http.ProxyFromEnvironment, nil
	}
	parsed, err := url.Parse(trimmed)
	if err != nil {
		return nil, fmt.Errorf("代理地址无效: %w", err)
	}
	switch strings.ToLower(parsed.Scheme) {
	case "http", "https", "socks5", "socks5h":
	default:
		return nil, fmt.Errorf("代理协议不受支持: %s", parsed.Scheme)
	}
	if parsed.Host == "" {
		return nil, errors.New("代理地址缺少主机与端口")
	}
	return http.ProxyURL(parsed), nil
}

// CloseIdleConnections 主动释放共享传输层中的空闲连接。
func (c *HTTPClient) CloseIdleConnections() {
	if c != nil && c.client != nil {
		c.client.CloseIdleConnections()
	}
}

// DoJSON 执行单个受控请求，并将有限响应体解码为 JSON。
func (c *HTTPClient) DoJSON(ctx context.Context, method, endpoint string, headers http.Header, body any, target any) error {
	select {
	case c.semaphore <- struct{}{}:
		defer func() { <-c.semaphore }()
	case <-ctx.Done():
		return ctx.Err()
	}
	var reader io.Reader
	if body != nil {
		encoded, err := json.Marshal(body)
		if err != nil {
			return fmt.Errorf("编码元数据请求: %w", err)
		}
		reader = bytes.NewReader(encoded)
	}
	request, err := http.NewRequestWithContext(ctx, method, endpoint, reader)
	if err != nil {
		return err
	}
	request.Header.Set("Accept", "application/json")
	request.Header.Set("User-Agent", c.userAgent)
	if body != nil {
		request.Header.Set("Content-Type", "application/json")
	}
	for key, values := range headers {
		request.Header.Del(key)
		for _, value := range values {
			request.Header.Add(key, value)
		}
	}
	response, err := c.client.Do(request)
	if err != nil {
		if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
			return err
		}
		// 不返回 net/url.Error 的文本，避免把带凭据的查询参数写入上层日志。
		return errors.New("元数据 HTTP 请求失败")
	}
	defer response.Body.Close()
	limited := io.LimitReader(response.Body, c.maxBodySize+1)
	encoded, err := io.ReadAll(limited)
	if err != nil {
		return err
	}
	if int64(len(encoded)) > c.maxBodySize {
		return fmt.Errorf("元数据响应超过 %d 字节限制", c.maxBodySize)
	}
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return fmt.Errorf("元数据服务返回 HTTP %d", response.StatusCode)
	}
	if err := json.Unmarshal(encoded, target); err != nil {
		return fmt.Errorf("解析元数据响应: %w", err)
	}
	return nil
}

// TMDB 是 TMDB v3 强类型客户端。
type TMDB struct {
	http    *HTTPClient
	baseURL string
	token   string
}

// NewTMDB 创建 TMDB 客户端。
func NewTMDB(client *HTTPClient, baseURL, token string) *TMDB {
	return &TMDB{http: client, baseURL: strings.TrimRight(baseURL, "/"), token: token}
}

// Name 返回元数据源名称。
func (c *TMDB) Name() string { return "themoviedb" }

// Search 搜索 TMDB 电影和剧集。
func (c *TMDB) Search(ctx context.Context, keyword string) ([]Media, error) {
	if strings.TrimSpace(keyword) == "" {
		return nil, errors.New("TMDB 搜索关键词不能为空")
	}
	if c.http == nil || c.baseURL == "" {
		return nil, errors.New("TMDB 客户端未配置")
	}
	var response struct {
		Results []struct {
			ID               int      `json:"id"`
			MediaType        string   `json:"media_type"`
			Title            string   `json:"title"`
			Name             string   `json:"name"`
			OriginalTitle    string   `json:"original_title"`
			OriginalName     string   `json:"original_name"`
			OriginalLanguage string   `json:"original_language"`
			ReleaseDate      string   `json:"release_date"`
			FirstAirDate     string   `json:"first_air_date"`
			Overview         string   `json:"overview"`
			PosterPath       string   `json:"poster_path"`
			BackdropPath     string   `json:"backdrop_path"`
			VoteAverage      float64  `json:"vote_average"`
			GenreIDs         []int    `json:"genre_ids"`
			OriginCountries  []string `json:"origin_country"`
		} `json:"results"`
	}
	query := url.Values{"query": {keyword}, "language": {"zh-CN"}}
	if err := c.http.DoJSON(ctx, http.MethodGet, c.signed(c.baseURL+"/3/search/multi?"+query.Encode()), c.headers(), nil, &response); err != nil {
		return nil, err
	}
	result := make([]Media, 0, len(response.Results))
	for _, item := range response.Results {
		if item.MediaType != "movie" && item.MediaType != "tv" {
			continue
		}
		title, original, date := item.Title, item.OriginalTitle, item.ReleaseDate
		if item.MediaType == "tv" {
			title, original, date = item.Name, item.OriginalName, item.FirstAirDate
		}
		result = append(result, Media{
			Source: "themoviedb", ID: fmt.Sprint(item.ID), Type: item.MediaType,
			Title: title, OriginalTitle: original, OriginalLanguage: item.OriginalLanguage,
			Year: yearFromDate(date), Overview: item.Overview,
			PosterURL: tmdbPoster(item.PosterPath), BackdropURL: tmdbBackdrop(item.BackdropPath),
			VoteAverage: item.VoteAverage,
			GenreIDs:    append([]int(nil), item.GenreIDs...), OriginCountries: append([]string(nil), item.OriginCountries...),
		})
	}
	return result, nil
}

// Detail 查询单个 TMDB 媒体详情，ID 使用 movie:123 或 tv:123。
func (c *TMDB) Detail(ctx context.Context, id string) (Media, error) {
	if c.http == nil || c.baseURL == "" {
		return Media{}, errors.New("TMDB 客户端未配置")
	}
	parts := strings.SplitN(id, ":", 2)
	if len(parts) != 2 || (parts[0] != "movie" && parts[0] != "tv") {
		return Media{}, errors.New("TMDB ID 必须使用 movie:123 或 tv:123")
	}
	var response struct {
		ID               int          `json:"id"`
		Title            string       `json:"title"`
		Name             string       `json:"name"`
		OriginalTitle    string       `json:"original_title"`
		OriginalName     string       `json:"original_name"`
		OriginalLanguage string       `json:"original_language"`
		ReleaseDate      string       `json:"release_date"`
		FirstAirDate     string       `json:"first_air_date"`
		Overview         string       `json:"overview"`
		PosterPath       string       `json:"poster_path"`
		BackdropPath     string       `json:"backdrop_path"`
		VoteAverage      float64      `json:"vote_average"`
		VoteCount        int          `json:"vote_count"`
		Popularity       float64      `json:"popularity"`
		Runtime          int          `json:"runtime"`
		EpisodeRunTime   []int        `json:"episode_run_time"`
		NumberOfSeasons  int          `json:"number_of_seasons"`
		NumberOfEpisodes int          `json:"number_of_episodes"`
		Seasons          []SeasonInfo `json:"seasons"`
		Genres           []struct {
			ID   int    `json:"id"`
			Name string `json:"name"`
		} `json:"genres"`
		OriginCountries     []string `json:"origin_country"`
		ProductionCountries []struct {
			ISO31661 string `json:"iso_3166_1"`
		} `json:"production_countries"`
		// append_to_response=credits 一起取演职员：详情页的演员/导演卡片靠它，
		// 少发一次请求也少一次失败机会。
		Credits struct {
			Cast []struct {
				ID          int    `json:"id"`
				Name        string `json:"name"`
				Character   string `json:"character"`
				ProfilePath string `json:"profile_path"`
			} `json:"cast"`
			Crew []struct {
				ID          int    `json:"id"`
				Name        string `json:"name"`
				Job         string `json:"job"`
				ProfilePath string `json:"profile_path"`
			} `json:"crew"`
		} `json:"credits"`
	}
	endpoint := c.signed(c.baseURL + "/3/" + parts[0] + "/" + url.PathEscape(parts[1]) + "?language=zh-CN&append_to_response=credits")
	if err := c.http.DoJSON(ctx, http.MethodGet, endpoint, c.headers(), nil, &response); err != nil {
		return Media{}, err
	}
	title, original, date := response.Title, response.OriginalTitle, response.ReleaseDate
	if parts[0] == "tv" {
		title, original, date = response.Name, response.OriginalName, response.FirstAirDate
	}
	var genreIDs []int
	if len(response.Genres) > 0 {
		genreIDs = make([]int, 0, len(response.Genres))
	}
	genres := make([]string, 0, len(response.Genres))
	for _, genre := range response.Genres {
		genreIDs = append(genreIDs, genre.ID)
		if name := strings.TrimSpace(genre.Name); name != "" {
			genres = append(genres, name)
		}
	}
	actors := make([]MediaPerson, 0, min(len(response.Credits.Cast), maxMetadataPeople))
	for _, member := range response.Credits.Cast {
		if len(actors) >= maxMetadataPeople {
			break
		}
		if strings.TrimSpace(member.Name) == "" {
			continue
		}
		actors = append(actors, tmdbPerson(member.ID, member.Name, member.Character, member.ProfilePath))
	}
	var directors []MediaPerson
	for _, member := range response.Credits.Crew {
		if len(directors) >= maxMetadataPeople {
			break
		}
		// 只认导演：V2 的 directors 就是 crew 里 job == "Director" 的那些人。
		if !strings.EqualFold(strings.TrimSpace(member.Job), "Director") {
			continue
		}
		if strings.TrimSpace(member.Name) == "" {
			continue
		}
		directors = append(directors, tmdbPerson(member.ID, member.Name, "", member.ProfilePath))
	}
	var productionCountries []string
	if len(response.ProductionCountries) > 0 {
		productionCountries = make([]string, 0, len(response.ProductionCountries))
	}
	for _, country := range response.ProductionCountries {
		productionCountries = append(productionCountries, country.ISO31661)
	}
	return Media{
		Source: "themoviedb", ID: fmt.Sprint(response.ID), Type: parts[0],
		TMDBID: fmt.Sprint(response.ID),
		Title:  title, OriginalTitle: original, OriginalLanguage: response.OriginalLanguage,
		Year: yearFromDate(date), Overview: response.Overview,
		PosterURL: tmdbPoster(response.PosterPath), BackdropURL: tmdbBackdrop(response.BackdropPath),
		VoteAverage: response.VoteAverage, SeasonInfo: append([]SeasonInfo(nil), response.Seasons...),
		GenreIDs: genreIDs, OriginCountries: append([]string(nil), response.OriginCountries...),
		ProductionCountries: productionCountries,
		// 富字段：详情页的「片长 / 类型 / 演员 / 导演」区块都读它们。
		ReleaseDate: date, Runtime: response.Runtime,
		EpisodeRunTime: append([]int(nil), response.EpisodeRunTime...),
		VoteCount:      response.VoteCount, Popularity: response.Popularity,
		Genres: genres, Actors: actors, Directors: directors,
		NumberOfSeasons: response.NumberOfSeasons, NumberOfEpisodes: response.NumberOfEpisodes,
	}, nil
}

// Credits 对应 V2 的 `tmdb/credits/{tmdbid}/{type_name}`：返回演员表（cast），每页 24 条。
//
// 与 V2 完全一致的三个细节：只取 cast（不含 crew）；补 source="themoviedb"；
// profile_path 保持 TMDB 返回的相对路径——前端 PersonCard 自己拼
// `https://<TMDB_IMAGE_DOMAIN>/t/p/w600_and_h900_bestv2<profile_path>`。
func (c *TMDB) Credits(ctx context.Context, mediaType string, tmdbID, page, count int) ([]MediaPerson, error) {
	if c.http == nil || c.baseURL == "" {
		return nil, errors.New("TMDB 客户端未配置")
	}
	kind := strings.ToLower(strings.TrimSpace(mediaType))
	if kind != "movie" && kind != "tv" {
		return nil, fmt.Errorf("TMDB 媒体类型无效: %s", mediaType)
	}
	if tmdbID <= 0 {
		return nil, errors.New("TMDB ID 必须为正整数")
	}
	if page < 1 {
		page = 1
	}
	if count < 1 || count > 100 {
		count = defaultCreditsPageSize
	}
	var response struct {
		Cast []struct {
			ID          int    `json:"id"`
			Name        string `json:"name"`
			Character   string `json:"character"`
			ProfilePath string `json:"profile_path"`
		} `json:"cast"`
	}
	endpoint := c.signed(c.baseURL + "/3/" + kind + "/" + strconv.Itoa(tmdbID) + "/credits?language=zh-CN")
	if err := c.http.DoJSON(ctx, http.MethodGet, endpoint, c.headers(), nil, &response); err != nil {
		return nil, err
	}
	start := (page - 1) * count
	if start >= len(response.Cast) {
		return []MediaPerson{}, nil
	}
	end := min(start+count, len(response.Cast))
	persons := make([]MediaPerson, 0, end-start)
	for _, member := range response.Cast[start:end] {
		if strings.TrimSpace(member.Name) == "" {
			continue
		}
		persons = append(persons, MediaPerson{
			Source: "themoviedb", ID: strconv.Itoa(member.ID), Type: 1,
			Name:        strings.TrimSpace(member.Name),
			Character:   strings.TrimSpace(member.Character),
			ProfilePath: strings.TrimSpace(member.ProfilePath),
		})
	}
	return persons, nil
}

// defaultCreditsPageSize 是演职员分页大小，跟 V2 的 count=24 一致。
const defaultCreditsPageSize = 24

// maxMetadataPeople 是演职员列表的上限：详情页只展示前若干个，
// 再多对页面没有增益，反而让响应体和图片请求成倍增长。
const maxMetadataPeople = 20

// tmdbPerson 把 TMDB 的演职员转成 V2 MediaPerson 口径。profile_path 保持相对路径
// （V2 同样透传），由前端按 TMDB_IMAGE_DOMAIN 拼完整 URL。
func tmdbPerson(id int, name, character, profilePath string) MediaPerson {
	return MediaPerson{
		Source: "themoviedb", ID: strconv.Itoa(id), Type: 1,
		Name: strings.TrimSpace(name), Character: strings.TrimSpace(character),
		ProfilePath: strings.TrimSpace(profilePath),
	}
}

// isTMDBV3Key 判断是否为 TMDB v3 API Key（32 位十六进制），这类 Key 只能用 api_key 查询参数。
func isTMDBV3Key(token string) bool {
	if len(token) != 32 {
		return false
	}
	for _, r := range token {
		if !((r >= '0' && r <= '9') || (r >= 'a' && r <= 'f')) {
			return false
		}
	}
	return true
}

// signed 为 v3 API Key 补 api_key 查询参数；v4 Read Access Token 由 Authorization 头携带。
func (c *TMDB) signed(endpoint string) string {
	if c.token == "" || !isTMDBV3Key(c.token) {
		return endpoint
	}
	separator := "?"
	if strings.Contains(endpoint, "?") {
		separator = "&"
	}
	return endpoint + separator + "api_key=" + url.QueryEscape(c.token)
}

func (c *TMDB) headers() http.Header {
	headers := make(http.Header)
	// v4 Read Access Token（含点号的 JWT）走 Authorization 头；
	// v3 API Key（32 位十六进制）走 api_key 查询参数，见 signed。
	if c.token != "" && !isTMDBV3Key(c.token) {
		headers.Set("Authorization", "Bearer "+c.token)
	}
	return headers
}

func yearFromDate(value string) int {
	if len(value) < 4 {
		return 0
	}
	year, _ := strconv.Atoi(value[:4])
	return year
}

func tmdbPoster(path string) string {
	if path == "" {
		return ""
	}
	return "https://image.tmdb.org/t/p/w500" + path
}

func tmdbBackdrop(path string) string {
	if path == "" {
		return ""
	}
	return "https://image.tmdb.org/t/p/original" + path
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if strings.TrimSpace(value) != "" {
			return strings.TrimSpace(value)
		}
	}
	return ""
}

func uniqueAliases(values ...string) []string {
	seen := make(map[string]struct{}, len(values))
	result := make([]string, 0, len(values))
	for _, value := range values {
		value = strings.TrimSpace(value)
		if value == "" {
			continue
		}
		key := strings.ToLower(value)
		if _, exists := seen[key]; exists {
			continue
		}
		seen[key] = struct{}{}
		result = append(result, value)
	}
	return result
}
