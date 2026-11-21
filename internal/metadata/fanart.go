package metadata

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
)

const defaultFanartBaseURL = "https://webservice.fanart.tv/v3"

// FanartImages 汇总从 Fanart.tv 挑选出的最佳高清图片地址（对齐 V2 FanartModule）。
type FanartImages struct {
	PosterURL     string
	BackdropURL   string
	LogoURL       string
	ClearArtURL   string
	BannerURL     string
	ThumbURL      string
	DiscURL       string
	SeasonPosters map[int]string
}

type fanartImageEntry struct {
	ID     string `json:"id"`
	URL    string `json:"url"`
	Lang   string `json:"lang"`
	Likes  string `json:"likes"`
	Season string `json:"season"`
}

// Fanart 是纯 Go Fanart.tv v3 客户端，为电影和剧集补充 Logo、ClearArt、Banner、Thumb、Disc 与季海报。
type Fanart struct {
	http      *HTTPClient
	baseURL   string
	apiKey    string
	languages []string
}

// NewFanart 创建 Fanart.tv 客户端。
func NewFanart(client *HTTPClient, baseURL, apiKey string, languages []string) *Fanart {
	baseURL = strings.TrimRight(strings.TrimSpace(baseURL), "/")
	if baseURL == "" {
		baseURL = defaultFanartBaseURL
	}
	langs := make([]string, 0, len(languages))
	for _, l := range languages {
		if trimmed := strings.ToLower(strings.TrimSpace(l)); trimmed != "" {
			langs = append(langs, trimmed)
		}
	}
	if len(langs) == 0 {
		langs = []string{"zh", "en"}
	}
	return &Fanart{
		http:      client,
		baseURL:   baseURL,
		apiKey:    strings.TrimSpace(apiKey),
		languages: langs,
	}
}

// GetMovieImages 按 TMDB ID 获取电影的 Fanart 高清艺术图集合。
func (f *Fanart) GetMovieImages(ctx context.Context, tmdbID int) (*FanartImages, error) {
	if f == nil || f.http == nil || f.apiKey == "" || tmdbID <= 0 {
		return nil, fmt.Errorf("fanart movie query invalid")
	}
	endpoint := fmt.Sprintf("%s/movies/%d?api_key=%s", f.baseURL, tmdbID, url.QueryEscape(f.apiKey))
	var resp struct {
		Status          string             `json:"status"`
		MoviePoster     []fanartImageEntry `json:"movieposter"`
		MovieBackground []fanartImageEntry `json:"moviebackground"`
		HDMovieLogo     []fanartImageEntry `json:"hdmovielogo"`
		MovieLogo       []fanartImageEntry `json:"movielogo"`
		HDMovieClearArt []fanartImageEntry `json:"hdmovieclearart"`
		MovieArt        []fanartImageEntry `json:"movieart"`
		MovieBanner     []fanartImageEntry `json:"moviebanner"`
		MovieThumb      []fanartImageEntry `json:"moviethumb"`
		MovieDisc       []fanartImageEntry `json:"moviedisc"`
	}
	if err := f.http.DoJSON(ctx, http.MethodGet, endpoint, http.Header{"Accept": []string{"application/json"}}, nil, &resp); err != nil {
		return nil, err
	}
	if strings.EqualFold(resp.Status, "error") {
		return nil, fmt.Errorf("fanart returned error status for movie %d", tmdbID)
	}
	return &FanartImages{
		PosterURL:   f.pickBest(resp.MoviePoster),
		BackdropURL: f.pickBest(resp.MovieBackground),
		LogoURL:     firstNonEmptyString(f.pickBest(resp.HDMovieLogo), f.pickBest(resp.MovieLogo)),
		ClearArtURL: firstNonEmptyString(f.pickBest(resp.HDMovieClearArt), f.pickBest(resp.MovieArt)),
		BannerURL:   f.pickBest(resp.MovieBanner),
		ThumbURL:    f.pickBest(resp.MovieThumb),
		DiscURL:     f.pickBest(resp.MovieDisc),
	}, nil
}

// GetTVImages 按 TVDB ID 获取剧集的 Fanart 高清艺术图与季海报集合。
func (f *Fanart) GetTVImages(ctx context.Context, tvdbID int) (*FanartImages, error) {
	if f == nil || f.http == nil || f.apiKey == "" || tvdbID <= 0 {
		return nil, fmt.Errorf("fanart tv query invalid")
	}
	endpoint := fmt.Sprintf("%s/tv/%d?api_key=%s", f.baseURL, tvdbID, url.QueryEscape(f.apiKey))
	var resp struct {
		Status         string             `json:"status"`
		TVPoster       []fanartImageEntry `json:"tvposter"`
		ShowBackground []fanartImageEntry `json:"showbackground"`
		HDTVLogo       []fanartImageEntry `json:"hdtvlogo"`
		ClearLogo      []fanartImageEntry `json:"clearlogo"`
		HDClearArt     []fanartImageEntry `json:"hdclearart"`
		ClearArt       []fanartImageEntry `json:"clearart"`
		TVBanner       []fanartImageEntry `json:"tvbanner"`
		TVThumb        []fanartImageEntry `json:"tvthumb"`
		SeasonPoster   []fanartImageEntry `json:"seasonposter"`
	}
	if err := f.http.DoJSON(ctx, http.MethodGet, endpoint, http.Header{"Accept": []string{"application/json"}}, nil, &resp); err != nil {
		return nil, err
	}
	if strings.EqualFold(resp.Status, "error") {
		return nil, fmt.Errorf("fanart returned error status for tv %d", tvdbID)
	}

	seasonPosters := make(map[int]string)
	bySeason := make(map[int][]fanartImageEntry)
	for _, entry := range resp.SeasonPoster {
		sNum, err := strconv.Atoi(strings.TrimSpace(entry.Season))
		if err != nil || sNum < 0 {
			continue
		}
		bySeason[sNum] = append(bySeason[sNum], entry)
	}
	for sNum, list := range bySeason {
		if picked := f.pickBest(list); picked != "" {
			seasonPosters[sNum] = picked
		}
	}

	return &FanartImages{
		PosterURL:     f.pickBest(resp.TVPoster),
		BackdropURL:   f.pickBest(resp.ShowBackground),
		LogoURL:       firstNonEmptyString(f.pickBest(resp.HDTVLogo), f.pickBest(resp.ClearLogo)),
		ClearArtURL:   firstNonEmptyString(f.pickBest(resp.HDClearArt), f.pickBest(resp.ClearArt)),
		BannerURL:     f.pickBest(resp.TVBanner),
		ThumbURL:      f.pickBest(resp.TVThumb),
		SeasonPosters: seasonPosters,
	}, nil
}

// pickBest 复刻 V2 FanartModule.__pick_best_image：
// 按配置语言顺序（默认 zh -> en）优先，再无字("00")，同语言内按 likes 降序。
func (f *Fanart) pickBest(entries []fanartImageEntry) string {
	valid := make([]fanartImageEntry, 0, len(entries))
	for _, entry := range entries {
		if strings.TrimSpace(entry.URL) != "" {
			valid = append(valid, entry)
		}
	}
	if len(valid) == 0 {
		return ""
	}
	pickByLang := func(lang string) string {
		matched := make([]fanartImageEntry, 0)
		for _, e := range valid {
			if strings.EqualFold(strings.TrimSpace(e.Lang), lang) {
				matched = append(matched, e)
			}
		}
		if len(matched) == 0 {
			return ""
		}
		sort.SliceStable(matched, func(i, j int) bool {
			return parseFanartLikes(matched[i].Likes) > parseFanartLikes(matched[j].Likes)
		})
		return normalizeFanartURL(matched[0].URL)
	}
	for _, lang := range f.languages {
		if picked := pickByLang(lang); picked != "" {
			return picked
		}
	}
	for _, fallback := range []string{"zh", "en", "00", ""} {
		if picked := pickByLang(fallback); picked != "" {
			return picked
		}
	}
	sort.SliceStable(valid, func(i, j int) bool {
		return parseFanartLikes(valid[i].Likes) > parseFanartLikes(valid[j].Likes)
	})
	return normalizeFanartURL(valid[0].URL)
}

func parseFanartLikes(raw string) int {
	n, _ := strconv.Atoi(strings.TrimSpace(raw))
	return n
}

func normalizeFanartURL(raw string) string {
	trimmed := strings.TrimSpace(raw)
	if strings.HasPrefix(trimmed, "http://assets.fanart.tv/") {
		return "https://" + strings.TrimPrefix(trimmed, "http://")
	}
	return trimmed
}
