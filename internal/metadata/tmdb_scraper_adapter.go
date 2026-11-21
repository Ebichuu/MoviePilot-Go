package metadata

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"time"

	"github.com/moviepilot-go/moviepilot-go/internal/scraper"
)

// TMDBScraperAdapter 将 TMDB API（及可选的 TheTVDB / Fanart.tv 补全）适配为 scraper.MetadataProvider 接口。
type TMDBScraperAdapter struct {
	tmdb   *TMDB
	tvdb   *TVDB
	fanart *Fanart
}

// NewTMDBScraperAdapter 创建 TMDB 刮削适配器。
func NewTMDBScraperAdapter(tmdb *TMDB) *TMDBScraperAdapter {
	return &TMDBScraperAdapter{tmdb: tmdb}
}

// SetTVDB 挂载可选的 TheTVDB v4 客户端，用于补全剧集与单集元数据。
func (a *TMDBScraperAdapter) SetTVDB(tvdb *TVDB) {
	if a != nil {
		a.tvdb = tvdb
	}
}

// SetFanart 挂载可选的 Fanart.tv v3 客户端，用于补全 Logo / ClearArt / Banner / Thumb / Disc / 季海报。
func (a *TMDBScraperAdapter) SetFanart(fanart *Fanart) {
	if a != nil {
		a.fanart = fanart
	}
}

// GetMovieDetails 获取电影详细信息，包括演员、导演、编剧。
func (a *TMDBScraperAdapter) GetMovieDetails(ctx context.Context, tmdbID int) (*scraper.MovieDetails, error) {
	if a.tmdb == nil || a.tmdb.http == nil {
		return nil, fmt.Errorf("TMDB 客户端未配置")
	}

	// 请求电影详情 + credits（演员、剧组）
	endpoint := a.tmdb.signed(fmt.Sprintf("%s/3/movie/%d?append_to_response=credits&language=zh-CN", a.tmdb.baseURL, tmdbID))

	var response struct {
		ID               int     `json:"id"`
		Title            string  `json:"title"`
		OriginalTitle    string  `json:"original_title"`
		OriginalLanguage string  `json:"original_language"`
		ReleaseDate      string  `json:"release_date"`
		Overview         string  `json:"overview"`
		Tagline          string  `json:"tagline"`
		Runtime          int     `json:"runtime"`
		VoteAverage      float64 `json:"vote_average"`
		PosterPath       string  `json:"poster_path"`
		BackdropPath     string  `json:"backdrop_path"`
		ImdbID           string  `json:"imdb_id"`
		Genres           []struct {
			ID   int    `json:"id"`
			Name string `json:"name"`
		} `json:"genres"`
		Credits struct {
			Cast []struct {
				ID          int    `json:"id"`
				Name        string `json:"name"`
				Character   string `json:"character"`
				ProfilePath string `json:"profile_path"`
				Order       int    `json:"order"`
			} `json:"cast"`
			Crew []struct {
				ID          int    `json:"id"`
				Name        string `json:"name"`
				Job         string `json:"job"`
				Department  string `json:"department"`
				ProfilePath string `json:"profile_path"`
			} `json:"crew"`
		} `json:"credits"`
	}

	if err := a.tmdb.http.DoJSON(ctx, http.MethodGet, endpoint, a.tmdb.headers(), nil, &response); err != nil {
		return nil, fmt.Errorf("TMDB API 请求失败: %w", err)
	}

	// 解析首映日期
	var premiered time.Time
	if response.ReleaseDate != "" {
		premiered, _ = time.Parse("2006-01-02", response.ReleaseDate)
	}

	// 提取类型
	genres := make([]string, 0, len(response.Genres))
	for _, g := range response.Genres {
		genres = append(genres, g.Name)
	}

	// 提取导演
	var directors []string
	for _, crew := range response.Credits.Crew {
		if crew.Job == "Director" {
			directors = append(directors, crew.Name)
		}
	}

	// 提取编剧
	var writers []string
	writerSet := make(map[string]bool) // 去重
	for _, crew := range response.Credits.Crew {
		if crew.Department == "Writing" && !writerSet[crew.Name] {
			writers = append(writers, crew.Name)
			writerSet[crew.Name] = true
		}
	}

	// 提取演员（前 20 位）
	cast := make([]scraper.CastMember, 0, len(response.Credits.Cast))
	maxCast := 20
	if len(response.Credits.Cast) < maxCast {
		maxCast = len(response.Credits.Cast)
	}
	for i := 0; i < maxCast; i++ {
		actor := response.Credits.Cast[i]
		cast = append(cast, scraper.CastMember{
			Name:     actor.Name,
			Role:     actor.Character,
			ImageURL: tmdbProfile(actor.ProfilePath),
			Order:    actor.Order,
		})
	}

	posterPath, backdropPath := response.PosterPath, response.BackdropPath
	if posterPath == "" || backdropPath == "" {
		images, imageErr := a.getMediaImages(ctx, "movie", tmdbID, response.OriginalLanguage)
		if imageErr != nil {
			if ctx.Err() != nil {
				return nil, ctx.Err()
			}
		} else {
			if posterPath == "" {
				posterPath = bestTMDBImagePath(images.Posters)
			}
			if backdropPath == "" {
				backdropPath = bestTMDBImagePath(images.Backdrops)
			}
		}
	}

	details := &scraper.MovieDetails{
		Title:         response.Title,
		OriginalTitle: response.OriginalTitle,
		Year:          yearFromDate(response.ReleaseDate),
		Rating:        response.VoteAverage,
		Plot:          response.Overview,
		Tagline:       response.Tagline,
		Runtime:       response.Runtime,
		Premiered:     premiered,
		Genres:        genres,
		Directors:     directors,
		Writers:       writers,
		Cast:          cast,
		PosterURL:     tmdbOriginalImage(posterPath),
		BackdropURL:   tmdbOriginalImage(backdropPath),
		TMDBID:        response.ID,
		IMDBID:        response.ImdbID,
	}
	if a.fanart != nil {
		if fanartImgs, fanartErr := a.fanart.GetMovieImages(ctx, tmdbID); fanartErr == nil && fanartImgs != nil {
			if details.PosterURL == "" {
				details.PosterURL = fanartImgs.PosterURL
			}
			if details.BackdropURL == "" {
				details.BackdropURL = fanartImgs.BackdropURL
			}
			details.LogoURL = fanartImgs.LogoURL
			details.ClearArtURL = fanartImgs.ClearArtURL
			details.BannerURL = fanartImgs.BannerURL
			details.ThumbURL = fanartImgs.ThumbURL
			details.DiscURL = fanartImgs.DiscURL
		}
	}
	return details, nil
}

// GetTVShowDetails 获取剧集详细信息。
func (a *TMDBScraperAdapter) GetTVShowDetails(ctx context.Context, tmdbID int) (*scraper.TVShowDetails, error) {
	if a.tmdb == nil || a.tmdb.http == nil {
		return nil, fmt.Errorf("TMDB 客户端未配置")
	}

	// 请求剧集详情 + credits + external_ids
	endpoint := a.tmdb.signed(fmt.Sprintf("%s/3/tv/%d?append_to_response=credits,external_ids&language=zh-CN", a.tmdb.baseURL, tmdbID))

	var response struct {
		ID               int     `json:"id"`
		Name             string  `json:"name"`
		OriginalName     string  `json:"original_name"`
		OriginalLanguage string  `json:"original_language"`
		FirstAirDate     string  `json:"first_air_date"`
		Overview         string  `json:"overview"`
		Tagline          string  `json:"tagline"`
		VoteAverage      float64 `json:"vote_average"`
		PosterPath       string  `json:"poster_path"`
		BackdropPath     string  `json:"backdrop_path"`
		Genres           []struct {
			ID   int    `json:"id"`
			Name string `json:"name"`
		} `json:"genres"`
		Credits struct {
			Cast []struct {
				ID          int    `json:"id"`
				Name        string `json:"name"`
				Character   string `json:"character"`
				ProfilePath string `json:"profile_path"`
				Order       int    `json:"order"`
			} `json:"cast"`
		} `json:"credits"`
		ExternalIDs struct {
			ImdbID string `json:"imdb_id"`
			TvdbID int    `json:"tvdb_id"`
		} `json:"external_ids"`
	}

	if err := a.tmdb.http.DoJSON(ctx, http.MethodGet, endpoint, a.tmdb.headers(), nil, &response); err != nil {
		return nil, fmt.Errorf("TMDB API 请求失败: %w", err)
	}

	// 解析首播日期
	var premiered time.Time
	if response.FirstAirDate != "" {
		premiered, _ = time.Parse("2006-01-02", response.FirstAirDate)
	}

	// 提取类型
	genres := make([]string, 0, len(response.Genres))
	for _, g := range response.Genres {
		genres = append(genres, g.Name)
	}

	// 提取演员（前 20 位）
	cast := make([]scraper.CastMember, 0, len(response.Credits.Cast))
	maxCast := 20
	if len(response.Credits.Cast) < maxCast {
		maxCast = len(response.Credits.Cast)
	}
	for i := 0; i < maxCast; i++ {
		actor := response.Credits.Cast[i]
		cast = append(cast, scraper.CastMember{
			Name:     actor.Name,
			Role:     actor.Character,
			ImageURL: tmdbProfile(actor.ProfilePath),
			Order:    actor.Order,
		})
	}

	posterPath, backdropPath := response.PosterPath, response.BackdropPath
	if posterPath == "" || backdropPath == "" {
		images, imageErr := a.getMediaImages(ctx, "tv", tmdbID, response.OriginalLanguage)
		if imageErr != nil {
			if ctx.Err() != nil {
				return nil, ctx.Err()
			}
		} else {
			if posterPath == "" {
				posterPath = bestTMDBImagePath(images.Posters)
			}
			if backdropPath == "" {
				backdropPath = bestTMDBImagePath(images.Backdrops)
			}
		}
	}

	details := &scraper.TVShowDetails{
		Title:         response.Name,
		OriginalTitle: response.OriginalName,
		Year:          yearFromDate(response.FirstAirDate),
		Rating:        response.VoteAverage,
		Plot:          response.Overview,
		Tagline:       response.Tagline,
		Premiered:     premiered,
		Genres:        genres,
		Cast:          cast,
		PosterURL:     tmdbOriginalImage(posterPath),
		BackdropURL:   tmdbOriginalImage(backdropPath),
		TMDBID:        response.ID,
		IMDBID:        response.ExternalIDs.ImdbID,
		TVDBID:        response.ExternalIDs.TvdbID,
	}
	if a.tvdb != nil && details.TVDBID > 0 {
		if tvdbDetail, tvdbErr := a.tvdb.GetSeriesExtended(ctx, details.TVDBID); tvdbErr == nil && tvdbDetail != nil {
			if strings.TrimSpace(details.Plot) == "" {
				details.Plot = tvdbDetail.Overview
			}
			if strings.TrimSpace(details.Title) == "" {
				details.Title = tvdbDetail.Title
			}
			if strings.TrimSpace(details.PosterURL) == "" {
				details.PosterURL = tvdbDetail.PosterURL
			}
			if strings.TrimSpace(details.BackdropURL) == "" {
				details.BackdropURL = tvdbDetail.BackdropURL
			}
			if len(details.Genres) == 0 && len(tvdbDetail.Genres) > 0 {
				details.Genres = append([]string(nil), tvdbDetail.Genres...)
			}
			if details.IMDBID == "" {
				details.IMDBID = tvdbDetail.IMDBID
			}
		}
	}
	if a.fanart != nil && details.TVDBID > 0 {
		if fanartImgs, fanartErr := a.fanart.GetTVImages(ctx, details.TVDBID); fanartErr == nil && fanartImgs != nil {
			if details.PosterURL == "" {
				details.PosterURL = fanartImgs.PosterURL
			}
			if details.BackdropURL == "" {
				details.BackdropURL = fanartImgs.BackdropURL
			}
			details.LogoURL = fanartImgs.LogoURL
			details.ClearArtURL = fanartImgs.ClearArtURL
			details.BannerURL = fanartImgs.BannerURL
			details.ThumbURL = fanartImgs.ThumbURL
		}
	}
	return details, nil
}

// GetEpisodeDetails 获取单集详细信息。
func (a *TMDBScraperAdapter) GetEpisodeDetails(ctx context.Context, tmdbID int, season, episode int) (*scraper.EpisodeDetails, error) {
	if a.tmdb == nil || a.tmdb.http == nil {
		return nil, fmt.Errorf("TMDB 客户端未配置")
	}

	// 请求单集详情 + credits
	endpoint := a.tmdb.signed(fmt.Sprintf("%s/3/tv/%d/season/%d/episode/%d?append_to_response=credits&language=zh-CN",
		a.tmdb.baseURL, tmdbID, season, episode))

	var response struct {
		ID          int     `json:"id"`
		Name        string  `json:"name"`
		Overview    string  `json:"overview"`
		AirDate     string  `json:"air_date"`
		Runtime     int     `json:"runtime"`
		VoteAverage float64 `json:"vote_average"`
		StillPath   string  `json:"still_path"`
		Credits     struct {
			Cast []struct {
				ID          int    `json:"id"`
				Name        string `json:"name"`
				Character   string `json:"character"`
				ProfilePath string `json:"profile_path"`
				Order       int    `json:"order"`
			} `json:"cast"`
			Crew []struct {
				ID         int    `json:"id"`
				Name       string `json:"name"`
				Job        string `json:"job"`
				Department string `json:"department"`
			} `json:"crew"`
		} `json:"credits"`
		ExternalIDs struct {
			TvdbID int `json:"tvdb_id"`
		} `json:"external_ids"`
	}

	if err := a.tmdb.http.DoJSON(ctx, http.MethodGet, endpoint, a.tmdb.headers(), nil, &response); err != nil {
		return nil, fmt.Errorf("TMDB API 请求失败: %w", err)
	}

	// 解析播出日期
	var aired time.Time
	if response.AirDate != "" {
		aired, _ = time.Parse("2006-01-02", response.AirDate)
	}

	// 提取导演
	var directors []string
	for _, crew := range response.Credits.Crew {
		if crew.Job == "Director" {
			directors = append(directors, crew.Name)
		}
	}

	// 提取编剧
	var writers []string
	writerSet := make(map[string]bool)
	for _, crew := range response.Credits.Crew {
		if crew.Department == "Writing" && !writerSet[crew.Name] {
			writers = append(writers, crew.Name)
			writerSet[crew.Name] = true
		}
	}

	// 提取演员（前 20 位）
	cast := make([]scraper.CastMember, 0, len(response.Credits.Cast))
	maxCast := 20
	if len(response.Credits.Cast) < maxCast {
		maxCast = len(response.Credits.Cast)
	}
	for i := 0; i < maxCast; i++ {
		actor := response.Credits.Cast[i]
		cast = append(cast, scraper.CastMember{
			Name:     actor.Name,
			Role:     actor.Character,
			ImageURL: tmdbProfile(actor.ProfilePath),
			Order:    actor.Order,
		})
	}

	epDetails := &scraper.EpisodeDetails{
		Title:      response.Name,
		Season:     season,
		Episode:    episode,
		Rating:     response.VoteAverage,
		Plot:       response.Overview,
		Runtime:    response.Runtime,
		EpisodeAir: aired,
		Directors:  directors,
		Writers:    writers,
		Cast:       cast,
		PosterURL:  tmdbStill(response.StillPath),
		TMDBID:     response.ID,
		TVDBID:     response.ExternalIDs.TvdbID,
	}
	if a.tvdb != nil && (strings.TrimSpace(epDetails.Plot) == "" || strings.TrimSpace(epDetails.PosterURL) == "") {
		if show, showErr := a.GetTVShowDetails(ctx, tmdbID); showErr == nil && show != nil && show.TVDBID > 0 {
			if tvdbEp, tvdbErr := a.tvdb.GetEpisode(ctx, show.TVDBID, season, episode); tvdbErr == nil && tvdbEp != nil {
				if strings.TrimSpace(epDetails.Title) == "" {
					epDetails.Title = tvdbEp.Title
				}
				if strings.TrimSpace(epDetails.Plot) == "" {
					epDetails.Plot = tvdbEp.Overview
				}
				if strings.TrimSpace(epDetails.PosterURL) == "" {
					epDetails.PosterURL = tvdbEp.ImageURL
				}
				if epDetails.Runtime == 0 {
					epDetails.Runtime = tvdbEp.Runtime
				}
				if epDetails.EpisodeAir.IsZero() && tvdbEp.AirDate != "" {
					epDetails.EpisodeAir = parseDateOrZero(tvdbEp.AirDate)
				}
				if epDetails.TVDBID == 0 {
					epDetails.TVDBID = tvdbEp.ID
				}
			}
		}
	}
	return epDetails, nil
}

// GetSeasonPosterURL 获取指定剧季的 TMDB 原图海报 URL，缺失时回退 Fanart.tv 季海报。
func (a *TMDBScraperAdapter) GetSeasonPosterURL(ctx context.Context, tmdbID, season int) (string, error) {
	if a.tmdb == nil || a.tmdb.http == nil {
		return "", fmt.Errorf("TMDB 客户端未配置")
	}
	if season < 0 || season > 999 {
		return "", fmt.Errorf("季号无效")
	}
	endpoint := a.tmdb.signed(fmt.Sprintf("%s/3/tv/%d/season/%d?language=zh-CN", a.tmdb.baseURL, tmdbID, season))
	var response struct {
		PosterPath string `json:"poster_path"`
	}
	if err := a.tmdb.http.DoJSON(ctx, http.MethodGet, endpoint, a.tmdb.headers(), nil, &response); err != nil {
		return "", fmt.Errorf("TMDB 季详情请求失败: %w", err)
	}
	if url := tmdbOriginalImage(response.PosterPath); url != "" {
		return url, nil
	}
	if a.fanart != nil {
		if show, showErr := a.GetTVShowDetails(ctx, tmdbID); showErr == nil && show != nil && show.TVDBID > 0 {
			if imgs, imgErr := a.fanart.GetTVImages(ctx, show.TVDBID); imgErr == nil && imgs != nil {
				return imgs.SeasonPosters[season], nil
			}
		}
	}
	return "", nil
}

// tmdbProfile 构建 TMDB 演员头像原图 URL。
func tmdbProfile(path string) string {
	if path == "" {
		return ""
	}
	return tmdbOriginalImage(path)
}

func tmdbOriginalImage(path string) string {
	if path == "" {
		return ""
	}
	return "https://image.tmdb.org/t/p/original" + path
}

type tmdbImagesResponse struct {
	Posters   []tmdbImage `json:"posters"`
	Backdrops []tmdbImage `json:"backdrops"`
}

type tmdbImage struct {
	FilePath    string  `json:"file_path"`
	VoteAverage float64 `json:"vote_average"`
	VoteCount   int     `json:"vote_count"`
}

func (a *TMDBScraperAdapter) getMediaImages(ctx context.Context, mediaType string, tmdbID int, originalLanguage string) (tmdbImagesResponse, error) {
	query := url.Values{}
	var languages []string
	for _, language := range []string{"zh-CN", "en", "null", originalLanguage} {
		language = strings.TrimSpace(language)
		if language == "" {
			continue
		}
		found := false
		for _, existing := range languages {
			if existing == language {
				found = true
				break
			}
		}
		if !found {
			languages = append(languages, language)
		}
	}
	query.Set("include_image_language", strings.Join(languages, ","))
	endpoint := a.tmdb.signed(fmt.Sprintf("%s/3/%s/%d/images?%s", a.tmdb.baseURL, mediaType, tmdbID, query.Encode()))
	var response tmdbImagesResponse
	if err := a.tmdb.http.DoJSON(ctx, http.MethodGet, endpoint, a.tmdb.headers(), nil, &response); err != nil {
		return tmdbImagesResponse{}, err
	}
	return response, nil
}

func bestTMDBImagePath(images []tmdbImage) string {
	if len(images) == 0 {
		return ""
	}
	sort.SliceStable(images, func(i, j int) bool {
		if images[i].VoteAverage != images[j].VoteAverage {
			return images[i].VoteAverage > images[j].VoteAverage
		}
		return images[i].VoteCount > images[j].VoteCount
	})
	return images[0].FilePath
}

// tmdbStill 构建 TMDB 剧照原图 URL。
func tmdbStill(path string) string {
	return tmdbOriginalImage(path)
}
