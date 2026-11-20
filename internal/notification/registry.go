package notification

import (
	"context"
	"errors"
	"log/slog"
	"sync/atomic"

	"github.com/moviepilot-go/moviepilot-go/internal/notificationconfig"
)

var (
	errRegistryUnavailable = errors.New("notification registry unavailable")
	errRegistryBuild       = errors.New("telegram notification runtime build failed")
	errTargetUnavailable   = errors.New("notification target not found or disabled")
	errRegistrySend        = errors.New("telegram notification delivery failed")
)

// Target identifies one configured notification destination.
type Target struct {
	Channel Channel
	Source  string
}

type telegramSenderFactory func(*slog.Logger, notificationconfig.Telegram) (ChannelSender, error)

type registryState struct {
	snapshot notificationconfig.Snapshot
	senders  map[string]ChannelSender
	channels map[Channel]map[string]ChannelSender
}

// Registry atomically publishes Telegram configuration and delivers through
// the sender built for the same immutable snapshot.
type Registry struct {
	logger           *slog.Logger
	factory          telegramSenderFactory
	current          atomic.Pointer[registryState]
	webPushAvailable atomic.Bool
}

var _ Sender = (*Registry)(nil)

// NewRegistry creates an empty Telegram runtime registry.
func NewRegistry(logger *slog.Logger) *Registry {
	return newRegistryWithFactory(logger, buildTelegramSender)
}

func newRegistryWithFactory(logger *slog.Logger, factory telegramSenderFactory) *Registry {
	if logger == nil {
		logger = slog.Default()
	}
	if factory == nil {
		factory = buildTelegramSender
	}
	registry := &Registry{logger: logger, factory: factory}
	registry.current.Store(&registryState{
		senders:  map[string]ChannelSender{},
		channels: map[Channel]map[string]ChannelSender{},
	})
	return registry
}

func buildTelegramSender(logger *slog.Logger, config notificationconfig.Telegram) (ChannelSender, error) {
	return NewTelegramSender(logger, TelegramConfig{
		BotToken: config.BotToken,
		ChatID:   config.ChatID,
		APIUrl:   config.APIURL,
	})
}

// Targets returns the Telegram destinations selected by the current snapshot.
func (r *Registry) Targets(msg Message) []Target {
	state := r.load()
	targets := targetsForState(state, msg)
	if r == nil || state == nil || !r.webPushAvailable.Load() {
		return targets
	}
	for _, config := range state.snapshot.Configs {
		if Channel(config.Type) != ChannelWebPush || !config.Enabled || config.Name == "" ||
			!registryTargetSelected(msg, ChannelWebPush, config.Name, switchValues(config.Switchs)) {
			continue
		}
		targets = append(targets, Target{Channel: ChannelWebPush, Source: config.Name})
	}
	return targets
}

// SetWebPushAvailable controls whether configured WebPush destinations are
// projected as delivery targets. Sending remains delegated to the app's VAPID
// dispatcher, which shares the Registry's channel selection.
func (r *Registry) SetWebPushAvailable(available bool) {
	if r != nil {
		r.webPushAvailable.Store(available)
	}
}

func registryTargetSelected(msg Message, channel Channel, source string, switchs []string) bool {
	if msg.Channel != "" && msg.Channel != channel {
		return false
	}
	if msg.Source != "" && msg.Source != source {
		return false
	}
	if msg.UserID == "" && msg.Source == "" && msg.Type != "" &&
		!notificationconfig.SwitchMatches(switchs, string(msg.Type)) {
		return false
	}
	return true
}

func switchValues(values []any) []string {
	result := make([]string, 0, len(values))
	for _, value := range values {
		if item, ok := value.(string); ok {
			result = append(result, item)
		}
	}
	return result
}

// Replace builds configured senders before atomically publishing the new
// immutable runtime snapshot. A failed Telegram build leaves the current state intact.
func (r *Registry) Replace(snapshot notificationconfig.Snapshot) error {
	if r == nil || r.factory == nil {
		return errRegistryUnavailable
	}

	next := &registryState{
		snapshot: snapshot.Clone(),
		senders:  make(map[string]ChannelSender, len(snapshot.Telegrams)),
		channels: make(map[Channel]map[string]ChannelSender),
	}
	telegramSenders := make(map[string]ChannelSender, len(snapshot.Telegrams))
	for _, config := range next.snapshot.Telegrams {
		if config.Name == "" {
			return errRegistryBuild
		}
		if _, exists := next.senders[config.Name]; exists {
			return errRegistryBuild
		}
		sender, err := r.factory(r.logger, config)
		if err != nil || sender == nil || sender.Name() != ChannelTelegram {
			return errRegistryBuild
		}
		next.senders[config.Name] = sender
		telegramSenders[config.Name] = sender
	}
	next.channels[ChannelTelegram] = telegramSenders
	for _, config := range next.snapshot.Configs {
		channel := Channel(config.Type)
		if channel == ChannelTelegram {
			continue
		}
		if !config.Enabled || config.Name == "" {
			continue
		}
		sender, ok := newConfiguredChannelSender(r.logger, channel, config)
		if !ok {
			continue
		}
		if next.channels[channel] == nil {
			next.channels[channel] = make(map[string]ChannelSender)
		}
		next.channels[channel][config.Name] = sender
	}
	r.current.Store(next)
	return nil
}

// Send delivers through the Telegram senders selected from one atomic state.
// Broadcast failures do not prevent the remaining selected targets from being
// attempted; callers receive only a stable, redacted delivery error.
func (r *Registry) Send(ctx context.Context, msg Message) error {
	if ctx == nil {
		return errors.New("notification context is nil")
	}
	state := r.load()
	if state == nil {
		return errRegistryUnavailable
	}

	targets := targetsForState(state, msg)
	if len(targets) == 0 {
		return errTargetUnavailable
	}

	var sendFailed bool
	for _, target := range targets {
		sender, ok := state.channels[target.Channel][target.Source]
		if !ok {
			return errTargetUnavailable
		}
		if err := sender.Send(ctx, msg); err != nil {
			if ctxErr := ctx.Err(); ctxErr != nil {
				return ctxErr
			}
			sendFailed = true
			r.logger.ErrorContext(ctx, "notification delivery failed",
				slog.String("channel", string(target.Channel)),
				slog.String("source", target.Source))
		}
	}
	if sendFailed {
		return errRegistrySend
	}
	return nil
}

func targetsForState(state *registryState, msg Message) []Target {
	if state == nil {
		return nil
	}
	configs := state.snapshot.Configs
	if len(configs) == 0 {
		targets := make([]Target, 0, len(state.snapshot.Telegrams))
		for _, config := range state.snapshot.Telegrams {
			if registryTargetSelected(msg, ChannelTelegram, config.Name, config.Switchs) {
				if _, ok := state.senders[config.Name]; ok {
					targets = append(targets, Target{Channel: ChannelTelegram, Source: config.Name})
				}
			}
		}
		return targets
	}
	targets := make([]Target, 0, len(configs))
	for _, config := range configs {
		channel := Channel(config.Type)
		if !config.Enabled || config.Name == "" || !registryTargetSelected(msg, channel, config.Name, switchValues(config.Switchs)) {
			continue
		}
		if _, ok := state.channels[channel][config.Name]; ok {
			targets = append(targets, Target{Channel: channel, Source: config.Name})
		}
	}
	return targets
}

func (r *Registry) load() *registryState {
	if r == nil {
		return nil
	}
	return r.current.Load()
}
