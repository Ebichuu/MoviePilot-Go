package app

import (
	"context"
	"errors"
	"log/slog"

	"github.com/moviepilot-go/moviepilot-go/internal/metadata"
	"github.com/moviepilot-go/moviepilot-go/internal/subscriptionflow"
)

type loggingMediaSupplementer struct {
	delegate subscriptionflow.MediaSupplementer
	logger   *slog.Logger
}

func (s *loggingMediaSupplementer) Supplement(
	ctx context.Context,
	input metadata.TMDBSupplementInput,
) (metadata.Media, bool, error) {
	result, found, err := s.delegate.Supplement(ctx, input)
	if err != nil && ctx.Err() == nil &&
		!errors.Is(err, context.Canceled) && !errors.Is(err, context.DeadlineExceeded) &&
		s.logger != nil {
		s.logger.Warn(
			"补充订阅 TMDB 辅助事实失败",
			"source", input.Media.Source,
			"media_id", input.Media.ID,
			"media_type", input.Media.Type,
			"error", "provider_error",
		)
	}
	return result, found, err
}
