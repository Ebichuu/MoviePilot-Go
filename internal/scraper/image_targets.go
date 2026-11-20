package scraper

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

var mediaImageAliases = map[string][]string{
	"backdrop":  {"fanart"},
	"thumb":     {"landscape"},
	"landscape": {"thumb"},
}

// WriteEpisodeImage 按 V2 约定将单集截图写为视频同目录的同名图片。
func (w *ImageWriter) WriteEpisodeImage(ctx context.Context, mediaPath, sourceURL string) (string, error) {
	if strings.TrimSpace(sourceURL) == "" {
		return "", nil
	}
	target := strings.TrimSuffix(mediaPath, filepath.Ext(mediaPath)) + "." + imageExtension(sourceURL)
	if err := w.writeTargets(ctx, mediaPath, sourceURL, []string{target}); err != nil {
		return "", err
	}
	return target, nil
}

// WriteSeasonImage 只根据已选中视频的目录写季海报，不扫描或创建额外季目录。
// 季目录同时写 poster 和剧集根目录 seasonXX-poster；特别篇使用 season-specials-poster。
func (w *ImageWriter) WriteSeasonImage(ctx context.Context, mediaPath string, season int, sourceURL string) ([]string, error) {
	if season < 0 || season > 999 {
		return nil, errors.New("season is invalid")
	}
	if strings.TrimSpace(sourceURL) == "" {
		return nil, nil
	}
	directory := filepath.Dir(mediaPath)
	root := directory
	extension := "." + imageExtension(sourceURL)
	var targets []string
	if number, ok := seasonDirectoryNumber(filepath.Base(directory)); ok {
		if number != season {
			return nil, errors.New("season does not match selected media directory")
		}
		root = filepath.Dir(directory)
		targets = append(targets, filepath.Join(directory, "poster"+extension))
	}
	name := fmt.Sprintf("season%02d-poster", season)
	if season == 0 {
		name = "season-specials-poster"
	}
	targets = append([]string{filepath.Join(root, name+extension)}, targets...)
	if err := w.writeTargets(ctx, mediaPath, sourceURL, targets); err != nil {
		return nil, err
	}
	return targets, nil
}

func (w *ImageWriter) writeTargets(ctx context.Context, mediaPath, sourceURL string, targets []string) error {
	if ctx == nil {
		return errors.New("context is nil")
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if w == nil || w.client == nil {
		return errors.New("image writer is not configured")
	}
	if strings.TrimSpace(mediaPath) == "" || len(targets) == 0 {
		return errors.New("image target is empty")
	}
	paths := append([]string{mediaPath}, targets...)
	if w.validatePath != nil {
		for _, path := range paths {
			if err := w.validatePath(path); err != nil {
				return fmt.Errorf("invalid image path: %w", err)
			}
		}
	}
	pending := make([]string, 0, len(targets))
	for _, target := range targets {
		info, err := os.Stat(target)
		if err != nil && !errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("stat image target: %w", err)
		}
		if err == nil {
			if info.IsDir() {
				return errors.New("image target is a directory")
			}
			if !w.Overwrite {
				continue
			}
		}
		pending = append(pending, target)
	}
	if len(pending) == 0 {
		return nil
	}
	payload, err := w.download(ctx, strings.TrimSpace(sourceURL))
	if err != nil {
		return err
	}
	var failures []error
	for _, target := range pending {
		if err := ctx.Err(); err != nil {
			return err
		}
		if err := os.MkdirAll(filepath.Dir(target), 0755); err != nil {
			failures = append(failures, fmt.Errorf("create image directory: %w", err))
			continue
		}
		var writeErr error
		if w.Overwrite {
			writeErr = writeImageAtomic(target, payload)
		} else {
			writeErr = writeImageAtomicNoReplace(target, payload)
		}
		if writeErr != nil {
			failures = append(failures, fmt.Errorf("write image %s: %w", target, writeErr))
		}
	}
	return errors.Join(failures...)
}

func writeImageAtomic(target string, payload []byte) error {
	// 不同单集任务会共同补齐季海报，唯一临时文件避免共享 .tmp 路径相互覆盖。
	file, err := os.CreateTemp(filepath.Dir(target), ".moviepilot-image-*")
	if err != nil {
		return err
	}
	temporary := file.Name()
	defer func() { _ = os.Remove(temporary) }()
	if _, err := file.Write(payload); err != nil {
		_ = file.Close()
		return err
	}
	if err := file.Chmod(0644); err != nil {
		_ = file.Close()
		return err
	}
	if err := file.Close(); err != nil {
		return err
	}
	return os.Rename(temporary, target)
}

func writeImageAtomicNoReplace(target string, payload []byte) error {
	file, err := os.CreateTemp(filepath.Dir(target), ".moviepilot-image-*")
	if err != nil {
		return err
	}
	temporary := file.Name()
	defer func() { _ = os.Remove(temporary) }()
	if _, err := file.Write(payload); err != nil {
		_ = file.Close()
		return err
	}
	if err := file.Chmod(0644); err != nil {
		_ = file.Close()
		return err
	}
	if err := file.Close(); err != nil {
		return err
	}
	if err := os.Link(temporary, target); err != nil {
		if errors.Is(err, os.ErrExist) {
			info, statErr := os.Stat(target)
			if statErr != nil {
				return statErr
			}
			if info.IsDir() {
				return errors.New("image target is a directory")
			}
			return nil
		}
		return err
	}
	return nil
}

func seasonDirectoryNumber(name string) (int, bool) {
	value := strings.ToLower(strings.TrimSpace(name))
	if value == "special" || value == "specials" {
		return 0, true
	}
	var digits string
	if strings.HasPrefix(value, "第") && strings.HasSuffix(value, "季") {
		digits = strings.TrimSuffix(strings.TrimPrefix(value, "第"), "季")
	} else {
		for _, prefix := range []string{"season", "s"} {
			if strings.HasPrefix(value, prefix) {
				digits = strings.TrimLeft(strings.TrimPrefix(value, prefix), " ._-")
				break
			}
		}
	}
	if digits == "" {
		return 0, false
	}
	for _, character := range digits {
		if character < '0' || character > '9' {
			return 0, false
		}
	}
	number, err := strconv.Atoi(digits)
	return number, err == nil && number >= 0 && number <= 999
}
