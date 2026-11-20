package scraper

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// NFOWriter writes NFO files to the filesystem.
type NFOWriter struct {
	// validatePath checks if the path is safe to write to.
	validatePath func(string) error
}

// NewNFOWriter creates a new NFO writer.
func NewNFOWriter() *NFOWriter {
	return &NFOWriter{
		validatePath: defaultPathValidator,
	}
}

// WriteNFOForMedia writes an NFO file for a media file.
// The NFO file is placed in the same directory as the media file.
//
// For movies: /path/to/Movie (2010)/Movie (2010).nfo
// For episodes: /path/to/Show/Season 01/Show S01E01.nfo
func (w *NFOWriter) WriteNFOForMedia(ctx context.Context, mediaPath string, meta NFOMetadata) error {
	if ctx == nil {
		return errors.New("context is nil")
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if mediaPath == "" {
		return errors.New("media path is empty")
	}
	if meta.Title == "" {
		return errors.New("metadata title is empty")
	}

	// Validate the media path.
	if w.validatePath != nil {
		if err := w.validatePath(mediaPath); err != nil {
			return fmt.Errorf("invalid media path: %w", err)
		}
	}

	// Generate NFO XML content.
	nfoData, err := GenerateNFO(meta)
	if err != nil {
		return fmt.Errorf("generate NFO: %w", err)
	}

	// Determine NFO file path.
	nfoPath := deriveNFOPath(mediaPath)

	// Ensure parent directory exists.
	nfoDir := filepath.Dir(nfoPath)
	if err := os.MkdirAll(nfoDir, 0755); err != nil {
		return fmt.Errorf("create NFO directory: %w", err)
	}

	// Write NFO file atomically.
	if err := writeFileAtomic(nfoPath, nfoData); err != nil {
		return fmt.Errorf("write NFO file: %w", err)
	}

	return nil
}

// deriveNFOPath derives the NFO file path from the media file path.
// It replaces the media file extension with .nfo.
//
// Examples:
//
//	/path/to/Movie (2010)/Movie (2010).mkv -> /path/to/Movie (2010)/Movie (2010).nfo
//	/path/to/Show/S01/Show S01E01.mp4 -> /path/to/Show/S01/Show S01E01.nfo
func deriveNFOPath(mediaPath string) string {
	ext := filepath.Ext(mediaPath)
	basePath := strings.TrimSuffix(mediaPath, ext)
	return basePath + ".nfo"
}

// writeFileAtomic writes data to a file atomically using a temp file + rename.
func writeFileAtomic(path string, data []byte) error {
	// Write to a temporary file first.
	tempPath := path + ".tmp"
	if err := os.WriteFile(tempPath, data, 0644); err != nil {
		return err
	}

	// Rename atomically.
	if err := os.Rename(tempPath, path); err != nil {
		_ = os.Remove(tempPath) // Clean up on failure.
		return err
	}

	return nil
}

// defaultPathValidator validates that the path is absolute and not attempting traversal.
func defaultPathValidator(path string) error {
	if !filepath.IsAbs(path) {
		return errors.New("path must be absolute")
	}
	// Check if the original path contains .. which would be traversal.
	if strings.Contains(path, "..") {
		return errors.New("path contains navigation elements")
	}
	return nil
}
