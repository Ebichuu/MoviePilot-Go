package subscriptionflow

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"strings"
	"time"

	"github.com/moviepilot-go/moviepilot-go/internal/subscription"
)

type v2DownloadFailureStore interface {
	ListActiveV2DownloadFailureFingerprints(
		context.Context,
		[]string,
		time.Time,
	) (map[string]struct{}, error)
	RecordV2DownloadFailure(
		context.Context,
		subscription.V2DownloadFailure,
		time.Time,
		time.Duration,
	) error
}

type v2TVDownloadFailureSession struct {
	store        v2DownloadFailureStore
	target       subscription.V2DownloadFailureTarget
	records      map[string]v2TVCandidateRecord
	fingerprints map[string]string
	active       map[string]struct{}
	source       string
}

func newV2TVDownloadFailureSession(
	ctx context.Context,
	store TargetStore,
	target persistedSubscription,
	records map[string]v2TVCandidateRecord,
	candidates []subscription.V2TVCandidate,
) (*v2TVDownloadFailureSession, []subscription.V2TVCandidate) {
	session := &v2TVDownloadFailureSession{
		target: v2DownloadFailureTarget(target), records: records,
		fingerprints: make(map[string]string, len(candidates)),
		active:       make(map[string]struct{}), source: v2SubscriptionSource(target),
	}
	if failureStore, ok := store.(v2DownloadFailureStore); ok {
		session.store = failureStore
	}
	fingerprints := make([]string, 0, len(candidates))
	for _, candidate := range candidates {
		record, exists := records[candidate.Key]
		if !exists {
			continue
		}
		failure, ok := subscription.NewV2DownloadFailure(
			session.target, record.candidate, nil, "", session.source, "",
		)
		if !ok {
			continue
		}
		session.fingerprints[candidate.Key] = failure.Fingerprint
		fingerprints = append(fingerprints, failure.Fingerprint)
	}
	if session.store != nil && len(fingerprints) > 0 {
		active, err := session.store.ListActiveV2DownloadFailureFingerprints(
			ctx, fingerprints, time.Now(),
		)
		if err != nil {
			slog.WarnContext(ctx, "查询 V2 下载失败冷却失败，按原版放行")
		} else {
			session.active = active
			if session.active == nil {
				session.active = make(map[string]struct{})
			}
		}
	}
	filtered := make([]subscription.V2TVCandidate, 0, len(candidates))
	for _, candidate := range candidates {
		if !session.isActive(candidate.Key) {
			filtered = append(filtered, candidate)
		}
	}
	return session, filtered
}

func (s *v2TVDownloadFailureSession) isActive(candidateKey string) bool {
	if s == nil {
		return false
	}
	fingerprint := s.fingerprints[candidateKey]
	if fingerprint == "" {
		return false
	}
	_, active := s.active[fingerprint]
	return active
}

func (s *v2TVDownloadFailureSession) record(
	ctx context.Context,
	candidateKey string,
	selected []subscription.Episode,
	fingerprintEpisodes []subscription.Episode,
	downloader string,
	errorMessage string,
) {
	if s == nil {
		return
	}
	record, exists := s.records[candidateKey]
	if !exists {
		return
	}
	candidate := record.candidate
	if len(fingerprintEpisodes) > 0 {
		begin, end := fingerprintEpisodes[0].Episode, fingerprintEpisodes[0].Episode
		for _, episode := range fingerprintEpisodes[1:] {
			begin = min(begin, episode.Episode)
			end = max(end, episode.Episode)
		}
		candidate.Parsed.BeginEpisode = begin
		candidate.Parsed.EndEpisode = end
	}
	failure, ok := subscription.NewV2DownloadFailure(
		s.target,
		candidate,
		selected,
		downloader,
		s.source,
		errorMessage,
	)
	if !ok {
		return
	}
	s.fingerprints[candidateKey] = failure.Fingerprint
	s.active[failure.Fingerprint] = struct{}{}
	if s.store == nil {
		return
	}
	if err := s.store.RecordV2DownloadFailure(
		ctx,
		failure,
		time.Now(),
		subscription.V2DownloadFailureTTL(errorMessage),
	); err != nil {
		slog.WarnContext(ctx, "记录 V2 下载失败冷却失败，按原版继续")
	}
}

func v2DownloadFailureTarget(target persistedSubscription) subscription.V2DownloadFailureTarget {
	return subscription.V2DownloadFailureTarget{
		MediaType:   target.MediaType,
		Title:       target.Name,
		Year:        target.Year,
		MediaSource: target.MediaSource,
		MediaID:     target.MediaID,
		TMDBID:      firstConfigText(target.Config, "tmdbid", "tmdb_id"),
		DoubanID:    firstConfigText(target.Config, "doubanid", "douban_id"),
		BangumiID:   firstConfigText(target.Config, "bangumiid", "bangumi_id"),
		AniListID:   firstConfigText(target.Config, "anilistid", "anilist_id"),
		IMDBID:      firstConfigText(target.Config, "imdbid", "imdb_id"),
		TVDBID:      firstConfigText(target.Config, "tvdbid", "tvdb_id"),
		Season:      target.Season,
	}
}

func firstConfigText(config map[string]any, keys ...string) string {
	for _, key := range keys {
		if value := configText(config, key); value != "" {
			return value
		}
	}
	return ""
}

func v2SubscriptionSource(target persistedSubscription) string {
	fields := []struct {
		name  string
		value any
	}{
		{name: "id", value: target.ID},
		{name: "name", value: target.Name},
		{name: "year", value: target.Year},
		{name: "type", value: target.MediaType},
		{name: "season", value: target.Season},
		{name: "episode_group", value: firstConfigValue(target.Config, "episode_group")},
		{name: "tmdbid", value: firstConfigValue(target.Config, "tmdbid", "tmdb_id")},
		{name: "imdbid", value: firstConfigValue(target.Config, "imdbid", "imdb_id")},
		{name: "tvdbid", value: firstConfigValue(target.Config, "tvdbid", "tvdb_id")},
		{name: "doubanid", value: firstConfigValue(target.Config, "doubanid", "douban_id")},
		{name: "bangumiid", value: firstConfigValue(target.Config, "bangumiid", "bangumi_id")},
		{name: "anilistid", value: firstConfigValue(target.Config, "anilistid", "anilist_id")},
		{name: "media_source", value: target.MediaSource},
		{name: "media_id", value: target.MediaID},
	}
	parts := make([]string, 0, len(fields))
	for _, field := range fields {
		encoded, ok := encodeV2SourceValue(field.value)
		if !ok {
			return "Subscribe|"
		}
		parts = append(parts, `"`+field.name+`": `+encoded)
	}
	return "Subscribe|{" + strings.Join(parts, ", ") + "}"
}

func firstConfigValue(config map[string]any, keys ...string) any {
	for _, key := range keys {
		if value, exists := config[key]; exists {
			return value
		}
	}
	return nil
}

func encodeV2SourceValue(value any) (string, bool) {
	var buffer bytes.Buffer
	encoder := json.NewEncoder(&buffer)
	encoder.SetEscapeHTML(false)
	if err := encoder.Encode(value); err != nil {
		return "", false
	}
	return strings.TrimSuffix(buffer.String(), "\n"), true
}
