package scraper

import (
	"encoding/xml"
	"fmt"
	"strings"
	"time"
)

// NFOType represents the type of NFO file.
type NFOType string

const (
	NFOTypeMovie   NFOType = "movie"
	NFOTypeTVShow  NFOType = "tvshow"
	NFOTypeEpisode NFOType = "episode"
)

// MovieNFO represents the structure of a movie NFO file.
type MovieNFO struct {
	XMLName       xml.Name   `xml:"movie"`
	Title         string     `xml:"title"`
	OriginalTitle string     `xml:"originaltitle,omitempty"`
	SortTitle     string     `xml:"sorttitle,omitempty"`
	Rating        float64    `xml:"rating,omitempty"`
	Year          int        `xml:"year,omitempty"`
	Plot          string     `xml:"plot,omitempty"`
	Outline       string     `xml:"outline,omitempty"`
	Tagline       string     `xml:"tagline,omitempty"`
	Runtime       int        `xml:"runtime,omitempty"` // 分钟
	Thumb         string     `xml:"thumb,omitempty"`
	Fanart        string     `xml:"fanart,omitempty"`
	Mpaa          string     `xml:"mpaa,omitempty"`
	Premiered     string     `xml:"premiered,omitempty"` // YYYY-MM-DD
	Genre         []string   `xml:"genre,omitempty"`
	Director      []string   `xml:"director,omitempty"`
	Credits       []string   `xml:"credits,omitempty"`
	Actor         []Actor    `xml:"actor,omitempty"`
	UniqueID      []UniqueID `xml:"uniqueid,omitempty"`
}

// TVShowNFO represents the structure of a TV show NFO file.
type TVShowNFO struct {
	XMLName       xml.Name   `xml:"tvshow"`
	Title         string     `xml:"title"`
	OriginalTitle string     `xml:"originaltitle,omitempty"`
	SortTitle     string     `xml:"sorttitle,omitempty"`
	Rating        float64    `xml:"rating,omitempty"`
	Year          int        `xml:"year,omitempty"`
	Plot          string     `xml:"plot,omitempty"`
	Outline       string     `xml:"outline,omitempty"`
	Tagline       string     `xml:"tagline,omitempty"`
	Thumb         string     `xml:"thumb,omitempty"`
	Fanart        string     `xml:"fanart,omitempty"`
	Mpaa          string     `xml:"mpaa,omitempty"`
	Premiered     string     `xml:"premiered,omitempty"` // YYYY-MM-DD
	Genre         []string   `xml:"genre,omitempty"`
	Studio        string     `xml:"studio,omitempty"`
	Actor         []Actor    `xml:"actor,omitempty"`
	UniqueID      []UniqueID `xml:"uniqueid,omitempty"`
}

// EpisodeNFO represents the structure of an episode NFO file.
type EpisodeNFO struct {
	XMLName   xml.Name   `xml:"episodedetails"`
	Title     string     `xml:"title"`
	SortTitle string     `xml:"sorttitle,omitempty"`
	Rating    float64    `xml:"rating,omitempty"`
	Season    int        `xml:"season"`
	Episode   int        `xml:"episode"`
	Plot      string     `xml:"plot,omitempty"`
	Outline   string     `xml:"outline,omitempty"`
	Runtime   int        `xml:"runtime,omitempty"` // 分钟
	Thumb     string     `xml:"thumb,omitempty"`
	Mpaa      string     `xml:"mpaa,omitempty"`
	Aired     string     `xml:"aired,omitempty"` // YYYY-MM-DD
	Director  []string   `xml:"director,omitempty"`
	Credits   []string   `xml:"credits,omitempty"`
	Actor     []Actor    `xml:"actor,omitempty"`
	UniqueID  []UniqueID `xml:"uniqueid,omitempty"`
}

// Actor represents a cast member.
type Actor struct {
	Name  string `xml:"name"`
	Role  string `xml:"role,omitempty"`
	Thumb string `xml:"thumb,omitempty"`
	Order int    `xml:"order,omitempty"`
}

// UniqueID represents an external ID (e.g., tmdb, imdb, tvdb).
type UniqueID struct {
	Type    string `xml:"type,attr"`
	Default bool   `xml:"default,attr,omitempty"`
	Value   string `xml:",chardata"`
}

// NFOMetadata holds the metadata needed to generate NFO files.
type NFOMetadata struct {
	Type          NFOType
	Title         string
	OriginalTitle string
	Year          int
	Rating        float64
	Plot          string
	Tagline       string
	Runtime       int
	Premiered     time.Time
	Genres        []string
	Directors     []string
	Writers       []string
	Cast          []CastMember
	PosterURL     string
	BackdropURL   string
	TMDBID        int
	IMDBID        string
	TVDBID        int

	// Episode-specific fields
	Season     int
	Episode    int
	EpisodeAir time.Time
}

// CastMember represents a cast member with name, role, and optional image.
type CastMember struct {
	Name     string
	Role     string
	ImageURL string
	Order    int
}

// GenerateNFO generates NFO XML content from metadata.
func GenerateNFO(meta NFOMetadata) ([]byte, error) {
	switch meta.Type {
	case NFOTypeMovie:
		return generateMovieNFO(meta)
	case NFOTypeTVShow:
		return generateTVShowNFO(meta)
	case NFOTypeEpisode:
		return generateEpisodeNFO(meta)
	default:
		return nil, fmt.Errorf("unsupported NFO type: %s", meta.Type)
	}
}

func generateMovieNFO(meta NFOMetadata) ([]byte, error) {
	nfo := MovieNFO{
		Title:         meta.Title,
		OriginalTitle: meta.OriginalTitle,
		Rating:        meta.Rating,
		Year:          meta.Year,
		Plot:          meta.Plot,
		Tagline:       meta.Tagline,
		Runtime:       meta.Runtime,
		Thumb:         meta.PosterURL,
		Fanart:        meta.BackdropURL,
		Genre:         meta.Genres,
		Director:      meta.Directors,
		Credits:       meta.Writers,
	}

	if !meta.Premiered.IsZero() {
		nfo.Premiered = meta.Premiered.Format("2006-01-02")
	}

	for _, cast := range meta.Cast {
		nfo.Actor = append(nfo.Actor, Actor{
			Name:  cast.Name,
			Role:  cast.Role,
			Thumb: cast.ImageURL,
			Order: cast.Order,
		})
	}

	if meta.TMDBID > 0 {
		nfo.UniqueID = append(nfo.UniqueID, UniqueID{
			Type:    "tmdb",
			Default: true,
			Value:   fmt.Sprintf("%d", meta.TMDBID),
		})
	}
	if meta.IMDBID != "" {
		nfo.UniqueID = append(nfo.UniqueID, UniqueID{
			Type:  "imdb",
			Value: meta.IMDBID,
		})
	}

	return marshalNFO(nfo)
}

func generateTVShowNFO(meta NFOMetadata) ([]byte, error) {
	nfo := TVShowNFO{
		Title:         meta.Title,
		OriginalTitle: meta.OriginalTitle,
		Rating:        meta.Rating,
		Year:          meta.Year,
		Plot:          meta.Plot,
		Tagline:       meta.Tagline,
		Thumb:         meta.PosterURL,
		Fanart:        meta.BackdropURL,
		Genre:         meta.Genres,
	}

	if !meta.Premiered.IsZero() {
		nfo.Premiered = meta.Premiered.Format("2006-01-02")
	}

	for _, cast := range meta.Cast {
		nfo.Actor = append(nfo.Actor, Actor{
			Name:  cast.Name,
			Role:  cast.Role,
			Thumb: cast.ImageURL,
			Order: cast.Order,
		})
	}

	if meta.TMDBID > 0 {
		nfo.UniqueID = append(nfo.UniqueID, UniqueID{
			Type:    "tmdb",
			Default: true,
			Value:   fmt.Sprintf("%d", meta.TMDBID),
		})
	}
	if meta.TVDBID > 0 {
		nfo.UniqueID = append(nfo.UniqueID, UniqueID{
			Type:  "tvdb",
			Value: fmt.Sprintf("%d", meta.TVDBID),
		})
	}
	if meta.IMDBID != "" {
		nfo.UniqueID = append(nfo.UniqueID, UniqueID{
			Type:  "imdb",
			Value: meta.IMDBID,
		})
	}

	return marshalNFO(nfo)
}

func generateEpisodeNFO(meta NFOMetadata) ([]byte, error) {
	nfo := EpisodeNFO{
		Title:    meta.Title,
		Rating:   meta.Rating,
		Season:   meta.Season,
		Episode:  meta.Episode,
		Plot:     meta.Plot,
		Runtime:  meta.Runtime,
		Thumb:    meta.PosterURL,
		Director: meta.Directors,
		Credits:  meta.Writers,
	}

	if !meta.EpisodeAir.IsZero() {
		nfo.Aired = meta.EpisodeAir.Format("2006-01-02")
	}

	for _, cast := range meta.Cast {
		nfo.Actor = append(nfo.Actor, Actor{
			Name:  cast.Name,
			Role:  cast.Role,
			Thumb: cast.ImageURL,
			Order: cast.Order,
		})
	}

	if meta.TMDBID > 0 {
		nfo.UniqueID = append(nfo.UniqueID, UniqueID{
			Type:    "tmdb",
			Default: true,
			Value:   fmt.Sprintf("%d", meta.TMDBID),
		})
	}
	if meta.TVDBID > 0 {
		nfo.UniqueID = append(nfo.UniqueID, UniqueID{
			Type:  "tvdb",
			Value: fmt.Sprintf("%d", meta.TVDBID),
		})
	}

	return marshalNFO(nfo)
}

func marshalNFO(v interface{}) ([]byte, error) {
	data, err := xml.MarshalIndent(v, "", "  ")
	if err != nil {
		return nil, fmt.Errorf("marshal NFO: %w", err)
	}

	var builder strings.Builder
	builder.WriteString(xml.Header)
	builder.Write(data)
	builder.WriteString("\n")

	return []byte(builder.String()), nil
}
