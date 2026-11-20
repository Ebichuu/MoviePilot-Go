package app

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"net/url"
	"strings"
	"testing"

	"github.com/moviepilot-go/moviepilot-go/internal/metadata"
)

func TestLoggingMediaSupplementerDelegatesAndLogsNonContextFailure(t *testing.T) {
	t.Parallel()
	providerErr := &url.Error{
		Op: "Get", URL: "https://api.example.test/search/multi?query=不应记录",
		Err: errors.New("sensitive upstream marker"),
	}
	delegate := &appMediaSupplementerFake{err: providerErr}
	var output bytes.Buffer
	resolver := &loggingMediaSupplementer{
		delegate: delegate,
		logger:   slog.New(slog.NewTextHandler(&output, nil)),
	}
	input := metadata.TMDBSupplementInput{Media: metadata.Media{Source: "douban", ID: "d1", Type: "tv", Title: "不应记录"}}

	got, found, err := resolver.Supplement(t.Context(), input)
	if !errors.Is(err, providerErr) || found || got.Source != input.Media.Source || delegate.calls != 1 {
		t.Fatalf("got=%#v found=%v err=%v calls=%d", got, found, err, delegate.calls)
	}
	logText := output.String()
	if !strings.Contains(logText, "source=douban") || !strings.Contains(logText, "media_id=d1") ||
		!strings.Contains(logText, "error=provider_error") || strings.Contains(logText, input.Media.Title) ||
		strings.Contains(logText, providerErr.URL) || strings.Contains(logText, "sensitive upstream marker") {
		t.Fatalf("日志字段不符合边界: %q", logText)
	}
}

func TestLoggingMediaSupplementerDoesNotLogCancellation(t *testing.T) {
	t.Parallel()
	var output bytes.Buffer
	resolver := &loggingMediaSupplementer{
		delegate: &appMediaSupplementerFake{err: context.Canceled},
		logger:   slog.New(slog.NewTextHandler(&output, nil)),
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, _, _ = resolver.Supplement(ctx, metadata.TMDBSupplementInput{Media: metadata.Media{Source: "anilist", ID: "9"}})
	if output.Len() != 0 {
		t.Fatalf("取消请求不应记录辅助故障: %q", output.String())
	}
}

func TestLoggingMediaSupplementerDoesNotLogIndependentDeadline(t *testing.T) {
	t.Parallel()
	var output bytes.Buffer
	resolver := &loggingMediaSupplementer{
		delegate: &appMediaSupplementerFake{err: &url.Error{
			Op: "Get", URL: "https://api.example.test/search/multi?query=不应记录",
			Err: context.DeadlineExceeded,
		}},
		logger: slog.New(slog.NewTextHandler(&output, nil)),
	}
	_, _, err := resolver.Supplement(t.Context(), metadata.TMDBSupplementInput{
		Media: metadata.Media{Source: "douban", ID: "7"},
	})
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("error = %v, want context deadline", err)
	}
	if output.Len() != 0 {
		t.Fatalf("独立超时不应记录含 URL 的辅助故障: %q", output.String())
	}
}

type appMediaSupplementerFake struct {
	err   error
	calls int
}

func (s *appMediaSupplementerFake) Supplement(
	_ context.Context,
	input metadata.TMDBSupplementInput,
) (metadata.Media, bool, error) {
	s.calls++
	return input.Media, false, s.err
}
