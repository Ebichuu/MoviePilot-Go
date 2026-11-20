package notification

import (
	"context"
	"fmt"
	"log/slog"
	"sync"
)

// sender dispatches notifications to multiple channels concurrently.
type sender struct {
	logger   *slog.Logger
	channels []channelSender
}

// ChannelSender represents one notification channel (WebPush, Telegram, etc.).
type ChannelSender interface {
	// Name returns the channel identifier.
	Name() Channel
	// Send delivers the message; returns error only on unrecoverable failures.
	Send(ctx context.Context, msg Message) error
}

type channelSender = ChannelSender

// NewSender creates a Sender that dispatches to all provided channels.
func NewSender(logger *slog.Logger, channels ...channelSender) Sender {
	if logger == nil {
		logger = slog.Default()
	}
	return &sender{
		logger:   logger,
		channels: channels,
	}
}

func (s *sender) Send(ctx context.Context, msg Message) error {
	if ctx == nil {
		return fmt.Errorf("notification context is nil")
	}
	if s == nil {
		return fmt.Errorf("notification sender is nil")
	}
	if len(s.channels) == 0 {
		s.logger.WarnContext(ctx, "no notification channels configured, message dropped",
			slog.String("type", string(msg.Type)),
			slog.String("title", msg.Title))
		return nil
	}

	// Filter channels: if msg.Channel is set, send only to that channel.
	targets := s.channels
	if msg.Channel != "" {
		targets = nil
		for _, ch := range s.channels {
			if ch.Name() == msg.Channel {
				targets = append(targets, ch)
			}
		}
		if len(targets) == 0 {
			return fmt.Errorf("notification channel %q not found or disabled", msg.Channel)
		}
	}

	// Send to all target channels concurrently.
	var wg sync.WaitGroup
	errCh := make(chan error, len(targets))

	for _, ch := range targets {
		wg.Add(1)
		go func(channel channelSender) {
			defer wg.Done()
			if err := channel.Send(ctx, msg); err != nil {
				s.logger.ErrorContext(ctx, "notification channel send failed",
					slog.String("channel", string(channel.Name())),
					slog.String("type", string(msg.Type)),
					slog.String("title", msg.Title),
					slog.String("error", err.Error()))
				errCh <- fmt.Errorf("%s: %w", channel.Name(), err)
			}
		}(ch)
	}

	wg.Wait()
	close(errCh)

	// Return the first error encountered.
	for err := range errCh {
		return err
	}
	return nil
}
