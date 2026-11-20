package scraper

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestNFOWriter_WriteNFOForMedia(t *testing.T) {
	tempDir := t.TempDir()
	mediaPath := filepath.Join(tempDir, "Movie (2010)", "Movie (2010).mkv")
	nfoPath := filepath.Join(tempDir, "Movie (2010)", "Movie (2010).nfo")

	// Create parent directory.
	if err := os.MkdirAll(filepath.Dir(mediaPath), 0755); err != nil {
		t.Fatalf("Failed to create test directory: %v", err)
	}

	// Create dummy media file.
	if err := os.WriteFile(mediaPath, []byte("dummy"), 0644); err != nil {
		t.Fatalf("Failed to create dummy media file: %v", err)
	}

	writer := NewNFOWriter()
	meta := NFOMetadata{
		Type:      NFOTypeMovie,
		Title:     "Movie",
		Year:      2010,
		Rating:    7.5,
		Plot:      "Test plot",
		Premiered: time.Date(2010, 1, 1, 0, 0, 0, 0, time.UTC),
		TMDBID:    12345,
	}

	ctx := context.Background()
	if err := writer.WriteNFOForMedia(ctx, mediaPath, meta); err != nil {
		t.Fatalf("WriteNFOForMedia failed: %v", err)
	}

	// Verify NFO file exists.
	if _, err := os.Stat(nfoPath); os.IsNotExist(err) {
		t.Fatalf("NFO file was not created at %s", nfoPath)
	}

	// Verify NFO content.
	nfoData, err := os.ReadFile(nfoPath)
	if err != nil {
		t.Fatalf("Failed to read NFO file: %v", err)
	}

	content := string(nfoData)
	if !strings.Contains(content, "<title>Movie</title>") {
		t.Error("NFO content missing title")
	}
	if !strings.Contains(content, "<year>2010</year>") {
		t.Error("NFO content missing year")
	}
	if !strings.Contains(content, `<uniqueid type="tmdb" default="true">12345</uniqueid>`) {
		t.Error("NFO content missing TMDB ID")
	}
}

func TestNFOWriter_WriteNFOForMedia_EpisodeNFO(t *testing.T) {
	tempDir := t.TempDir()
	mediaPath := filepath.Join(tempDir, "Show", "Season 01", "Show S01E01.mp4")
	nfoPath := filepath.Join(tempDir, "Show", "Season 01", "Show S01E01.nfo")

	// Create parent directory.
	if err := os.MkdirAll(filepath.Dir(mediaPath), 0755); err != nil {
		t.Fatalf("Failed to create test directory: %v", err)
	}

	writer := NewNFOWriter()
	meta := NFOMetadata{
		Type:       NFOTypeEpisode,
		Title:      "Pilot",
		Season:     1,
		Episode:    1,
		Plot:       "First episode",
		EpisodeAir: time.Date(2020, 1, 1, 0, 0, 0, 0, time.UTC),
		TMDBID:     67890,
	}

	ctx := context.Background()
	if err := writer.WriteNFOForMedia(ctx, mediaPath, meta); err != nil {
		t.Fatalf("WriteNFOForMedia failed: %v", err)
	}

	// Verify NFO file exists.
	if _, err := os.Stat(nfoPath); os.IsNotExist(err) {
		t.Fatalf("NFO file was not created at %s", nfoPath)
	}

	// Verify NFO content.
	nfoData, err := os.ReadFile(nfoPath)
	if err != nil {
		t.Fatalf("Failed to read NFO file: %v", err)
	}

	content := string(nfoData)
	if !strings.Contains(content, "<title>Pilot</title>") {
		t.Error("NFO content missing title")
	}
	if !strings.Contains(content, "<season>1</season>") {
		t.Error("NFO content missing season")
	}
	if !strings.Contains(content, "<episode>1</episode>") {
		t.Error("NFO content missing episode")
	}
}

func TestNFOWriter_WriteNFOForMedia_ValidationErrors(t *testing.T) {
	writer := NewNFOWriter()
	meta := NFOMetadata{
		Type:  NFOTypeMovie,
		Title: "Test",
	}

	tests := []struct {
		name      string
		ctx       context.Context
		mediaPath string
		meta      NFOMetadata
		wantErr   string
	}{
		{
			name:      "nil context",
			ctx:       nil,
			mediaPath: "/valid/path.mkv",
			meta:      meta,
			wantErr:   "context is nil",
		},
		{
			name:      "canceled context",
			ctx:       canceledContext(),
			mediaPath: "/valid/path.mkv",
			meta:      meta,
			wantErr:   "context canceled",
		},
		{
			name:      "empty media path",
			ctx:       context.Background(),
			mediaPath: "",
			meta:      meta,
			wantErr:   "media path is empty",
		},
		{
			name:      "relative path",
			ctx:       context.Background(),
			mediaPath: "relative/path.mkv",
			meta:      meta,
			wantErr:   "path must be absolute",
		},
		{
			name:      "path traversal",
			ctx:       context.Background(),
			mediaPath: filepath.VolumeName(os.TempDir()) + string(filepath.Separator) + strings.Join([]string{"valid", "..", "traversal", "path.mkv"}, string(filepath.Separator)),
			meta:      meta,
			wantErr:   "path contains navigation elements",
		},
		{
			name:      "empty title",
			ctx:       context.Background(),
			mediaPath: "/valid/path.mkv",
			meta:      NFOMetadata{Type: NFOTypeMovie, Title: ""},
			wantErr:   "title is empty",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := writer.WriteNFOForMedia(tt.ctx, tt.mediaPath, tt.meta)
			if err == nil {
				t.Error("Expected error, got nil")
				return
			}
			if !strings.Contains(err.Error(), tt.wantErr) {
				t.Errorf("Error = %q, want to contain %q", err.Error(), tt.wantErr)
			}
		})
	}
}

func TestDeriveNFOPath(t *testing.T) {
	tests := []struct {
		mediaPath string
		want      string
	}{
		{
			mediaPath: "/path/to/Movie (2010)/Movie (2010).mkv",
			want:      "/path/to/Movie (2010)/Movie (2010).nfo",
		},
		{
			mediaPath: "/path/to/Show/S01/Show S01E01.mp4",
			want:      "/path/to/Show/S01/Show S01E01.nfo",
		},
		{
			mediaPath: "/path/to/file.avi",
			want:      "/path/to/file.nfo",
		},
		{
			mediaPath: "/path/to/file.with.dots.mkv",
			want:      "/path/to/file.with.dots.nfo",
		},
		{
			mediaPath: "/path/to/noextension",
			want:      "/path/to/noextension.nfo",
		},
	}

	for _, tt := range tests {
		t.Run(tt.mediaPath, func(t *testing.T) {
			got := deriveNFOPath(tt.mediaPath)
			if got != tt.want {
				t.Errorf("deriveNFOPath(%q) = %q, want %q", tt.mediaPath, got, tt.want)
			}
		})
	}
}

func TestWriteFileAtomic(t *testing.T) {
	t.Run("replaces existing file", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "movie.nfo")
		if err := os.WriteFile(path, []byte("old"), 0644); err != nil {
			t.Fatalf("write old file: %v", err)
		}

		if err := writeFileAtomic(path, []byte("new")); err != nil {
			t.Fatalf("writeFileAtomic() error = %v", err)
		}
		content, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("read final file: %v", err)
		}
		if string(content) != "new" {
			t.Fatalf("final content = %q, want new", content)
		}
		if _, err := os.Stat(path + ".tmp"); !os.IsNotExist(err) {
			t.Fatalf("temporary file remains after success: %v", err)
		}
	})

	t.Run("cleans temporary file when rename fails", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "destination")
		if err := os.Mkdir(path, 0755); err != nil {
			t.Fatalf("create destination directory: %v", err)
		}

		if err := writeFileAtomic(path, []byte("new")); err == nil {
			t.Fatal("writeFileAtomic() error = nil, want rename failure")
		}
		if _, err := os.Stat(path + ".tmp"); !os.IsNotExist(err) {
			t.Fatalf("temporary file remains after failure: %v", err)
		}
	})
}

func canceledContext() context.Context {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	return ctx
}
