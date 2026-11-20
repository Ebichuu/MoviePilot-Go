package scraper

import (
	"encoding/xml"
	"strings"
	"testing"
	"time"
)

func TestGenerateMovieNFO(t *testing.T) {
	premiered, _ := time.Parse("2006-01-02", "2010-07-16")
	meta := NFOMetadata{
		Type:          NFOTypeMovie,
		Title:         "Inception",
		OriginalTitle: "Inception",
		Year:          2010,
		Rating:        8.8,
		Plot:          "A thief who steals corporate secrets through the use of dream-sharing technology.",
		Tagline:       "Your mind is the scene of the crime.",
		Runtime:       148,
		Premiered:     premiered,
		Genres:        []string{"Action", "Sci-Fi", "Thriller"},
		Directors:     []string{"Christopher Nolan"},
		Writers:       []string{"Christopher Nolan"},
		Cast: []CastMember{
			{Name: "Leonardo DiCaprio", Role: "Cobb", Order: 0},
			{Name: "Joseph Gordon-Levitt", Role: "Arthur", Order: 1},
		},
		PosterURL:   "https://image.tmdb.org/poster.jpg",
		BackdropURL: "https://image.tmdb.org/backdrop.jpg",
		TMDBID:      27205,
		IMDBID:      "tt1375666",
	}

	data, err := GenerateNFO(meta)
	if err != nil {
		t.Fatalf("GenerateNFO failed: %v", err)
	}

	if !strings.Contains(string(data), `<?xml version="1.0" encoding="UTF-8"?>`) {
		t.Error("NFO missing XML header")
	}

	var nfo MovieNFO
	if err := xml.Unmarshal(data, &nfo); err != nil {
		t.Fatalf("Failed to parse generated NFO: %v", err)
	}

	if nfo.Title != "Inception" {
		t.Errorf("Title = %q, want %q", nfo.Title, "Inception")
	}
	if nfo.Year != 2010 {
		t.Errorf("Year = %d, want %d", nfo.Year, 2010)
	}
	if nfo.Rating != 8.8 {
		t.Errorf("Rating = %f, want %f", nfo.Rating, 8.8)
	}
	if nfo.Runtime != 148 {
		t.Errorf("Runtime = %d, want %d", nfo.Runtime, 148)
	}
	if nfo.Premiered != "2010-07-16" {
		t.Errorf("Premiered = %q, want %q", nfo.Premiered, "2010-07-16")
	}
	if len(nfo.Genre) != 3 {
		t.Errorf("Genre count = %d, want 3", len(nfo.Genre))
	}
	if len(nfo.Director) != 1 || nfo.Director[0] != "Christopher Nolan" {
		t.Errorf("Director = %v, want [Christopher Nolan]", nfo.Director)
	}
	if len(nfo.Actor) != 2 {
		t.Errorf("Actor count = %d, want 2", len(nfo.Actor))
	}
	if nfo.Actor[0].Name != "Leonardo DiCaprio" || nfo.Actor[0].Role != "Cobb" {
		t.Errorf("Actor[0] = %+v, want {Name:Leonardo DiCaprio Role:Cobb}", nfo.Actor[0])
	}
	if len(nfo.UniqueID) < 1 {
		t.Fatal("UniqueID missing")
	}

	var hasTMDB, hasIMDB bool
	for _, id := range nfo.UniqueID {
		if id.Type == "tmdb" && id.Value == "27205" && id.Default {
			hasTMDB = true
		}
		if id.Type == "imdb" && id.Value == "tt1375666" {
			hasIMDB = true
		}
	}
	if !hasTMDB {
		t.Error("TMDB ID not found or incorrect")
	}
	if !hasIMDB {
		t.Error("IMDB ID not found or incorrect")
	}
}

func TestGenerateTVShowNFO(t *testing.T) {
	premiered, _ := time.Parse("2006-01-02", "2011-04-17")
	meta := NFOMetadata{
		Type:          NFOTypeTVShow,
		Title:         "Game of Thrones",
		OriginalTitle: "Game of Thrones",
		Year:          2011,
		Rating:        9.3,
		Plot:          "Nine noble families fight for control over the lands of Westeros.",
		Premiered:     premiered,
		Genres:        []string{"Drama", "Fantasy", "Adventure"},
		Cast: []CastMember{
			{Name: "Emilia Clarke", Role: "Daenerys Targaryen", Order: 0},
			{Name: "Kit Harington", Role: "Jon Snow", Order: 1},
		},
		PosterURL:   "https://image.tmdb.org/poster.jpg",
		BackdropURL: "https://image.tmdb.org/backdrop.jpg",
		TMDBID:      1399,
		TVDBID:      121361,
	}

	data, err := GenerateNFO(meta)
	if err != nil {
		t.Fatalf("GenerateNFO failed: %v", err)
	}

	var nfo TVShowNFO
	if err := xml.Unmarshal(data, &nfo); err != nil {
		t.Fatalf("Failed to parse generated NFO: %v", err)
	}

	if nfo.Title != "Game of Thrones" {
		t.Errorf("Title = %q, want %q", nfo.Title, "Game of Thrones")
	}
	if nfo.Year != 2011 {
		t.Errorf("Year = %d, want %d", nfo.Year, 2011)
	}
	if nfo.Premiered != "2011-04-17" {
		t.Errorf("Premiered = %q, want %q", nfo.Premiered, "2011-04-17")
	}
	if len(nfo.Actor) != 2 {
		t.Errorf("Actor count = %d, want 2", len(nfo.Actor))
	}

	var hasTMDB, hasTVDB bool
	for _, id := range nfo.UniqueID {
		if id.Type == "tmdb" && id.Value == "1399" && id.Default {
			hasTMDB = true
		}
		if id.Type == "tvdb" && id.Value == "121361" {
			hasTVDB = true
		}
	}
	if !hasTMDB {
		t.Error("TMDB ID not found or incorrect")
	}
	if !hasTVDB {
		t.Error("TVDB ID not found or incorrect")
	}
}

func TestGenerateEpisodeNFO(t *testing.T) {
	aired, _ := time.Parse("2006-01-02", "2011-04-17")
	meta := NFOMetadata{
		Type:       NFOTypeEpisode,
		Title:      "Winter Is Coming",
		Season:     1,
		Episode:    1,
		Rating:     8.9,
		Plot:       "Eddard Stark is torn between his family and an old friend.",
		Runtime:    62,
		EpisodeAir: aired,
		Directors:  []string{"Tim Van Patten"},
		Writers:    []string{"David Benioff", "D. B. Weiss"},
		Cast: []CastMember{
			{Name: "Sean Bean", Role: "Eddard Stark", Order: 0},
		},
		PosterURL: "https://image.tmdb.org/episode.jpg",
		TMDBID:    63056,
		TVDBID:    3254641,
	}

	data, err := GenerateNFO(meta)
	if err != nil {
		t.Fatalf("GenerateNFO failed: %v", err)
	}

	var nfo EpisodeNFO
	if err := xml.Unmarshal(data, &nfo); err != nil {
		t.Fatalf("Failed to parse generated NFO: %v", err)
	}

	if nfo.Title != "Winter Is Coming" {
		t.Errorf("Title = %q, want %q", nfo.Title, "Winter Is Coming")
	}
	if nfo.Season != 1 {
		t.Errorf("Season = %d, want 1", nfo.Season)
	}
	if nfo.Episode != 1 {
		t.Errorf("Episode = %d, want 1", nfo.Episode)
	}
	if nfo.Aired != "2011-04-17" {
		t.Errorf("Aired = %q, want %q", nfo.Aired, "2011-04-17")
	}
	if nfo.Runtime != 62 {
		t.Errorf("Runtime = %d, want 62", nfo.Runtime)
	}
	if len(nfo.Director) != 1 || nfo.Director[0] != "Tim Van Patten" {
		t.Errorf("Director = %v, want [Tim Van Patten]", nfo.Director)
	}
	if len(nfo.Credits) != 2 {
		t.Errorf("Credits count = %d, want 2", len(nfo.Credits))
	}
	if len(nfo.Actor) != 1 {
		t.Errorf("Actor count = %d, want 1", len(nfo.Actor))
	}
}

func TestGenerateNFO_UnsupportedType(t *testing.T) {
	meta := NFOMetadata{
		Type:  "invalid",
		Title: "Test",
	}

	_, err := GenerateNFO(meta)
	if err == nil {
		t.Error("Expected error for unsupported NFO type, got nil")
	}
	if !strings.Contains(err.Error(), "unsupported NFO type") {
		t.Errorf("Error message = %q, want to contain 'unsupported NFO type'", err.Error())
	}
}

func TestGenerateNFO_MinimalMetadata(t *testing.T) {
	meta := NFOMetadata{
		Type:  NFOTypeMovie,
		Title: "Minimal Movie",
	}

	data, err := GenerateNFO(meta)
	if err != nil {
		t.Fatalf("GenerateNFO failed: %v", err)
	}

	var nfo MovieNFO
	if err := xml.Unmarshal(data, &nfo); err != nil {
		t.Fatalf("Failed to parse generated NFO: %v", err)
	}

	if nfo.Title != "Minimal Movie" {
		t.Errorf("Title = %q, want %q", nfo.Title, "Minimal Movie")
	}
	if nfo.Year != 0 {
		t.Errorf("Year = %d, want 0", nfo.Year)
	}
	if len(nfo.UniqueID) != 0 {
		t.Errorf("UniqueID count = %d, want 0", len(nfo.UniqueID))
	}
}
