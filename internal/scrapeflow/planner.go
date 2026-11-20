package scrapeflow

import (
	"context"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/google/uuid"

	"github.com/moviepilot-go/moviepilot-go/internal/media"
	"github.com/moviepilot-go/moviepilot-go/internal/organizer"
)

// Planner derives NFO requests from persisted subscription identity and an
// immutable organization plan.
type Planner struct {
	sources SourceStore
	enabled bool
}

// NewPlanner creates a planner. Disabled planners still emit terminal skipped
// rows so downstream media indexing has an explicit prerequisite result.
func NewPlanner(sources SourceStore, enabled bool) (*Planner, error) {
	if sources == nil {
		return nil, ErrInvalidPlan
	}
	return &Planner{sources: sources, enabled: enabled}, nil
}

// Plan builds a deterministic request snapshot for one completed batch.
func (p *Planner) Plan(
	ctx context.Context,
	batchID uuid.UUID,
	intentID uuid.UUID,
	plan organizer.Plan,
) (Plan, error) {
	if ctx == nil || p == nil || p.sources == nil || batchID == uuid.Nil || intentID == uuid.Nil {
		return Plan{}, ErrInvalidPlan
	}
	if err := ctx.Err(); err != nil {
		return Plan{}, err
	}
	source, err := p.sources.LoadScrapeSource(ctx, intentID)
	if err != nil {
		return Plan{}, ErrPersistence
	}
	result := Plan{BatchID: batchID, IntentID: intentID, Username: strings.TrimSpace(source.Username)}
	videos := videoTargets(plan)
	if len(videos) == 0 {
		if !result.Valid() {
			return Plan{}, ErrInvalidPlan
		}
		return result, nil
	}

	tmdbID, identityReady := p.tmdbIdentity(source)
	switch normalizeMediaType(source.MediaType) {
	case media.TypeMovie:
		for _, target := range videos {
			request := RequestPlan{Kind: KindMovie, MediaPath: target, TMDBID: tmdbID}
			if !identityReady {
				request.TMDBID = 0
				request.SkipReason = p.skipReason(source)
			}
			result.Requests = append(result.Requests, finishRequest(batchID, request))
		}
	case media.TypeTV:
		tvshow := RequestPlan{Kind: KindTVShow, MediaPath: tvShowNFOPath(videos), TMDBID: tmdbID}
		if !identityReady {
			tvshow.TMDBID = 0
			tvshow.SkipReason = p.skipReason(source)
		}
		result.Requests = append(result.Requests, finishRequest(batchID, tvshow))
		bound := episodeSet(source.Episodes)
		for _, target := range videos {
			season, episode, mapped := mapEpisode(target, source.Season, source.Episodes, len(videos), bound)
			request := RequestPlan{Kind: KindEpisode, MediaPath: target, TMDBID: tmdbID, Season: season, Episode: episode}
			if !identityReady {
				request.TMDBID = 0
				request.Season = 0
				request.Episode = 0
				request.SkipReason = p.skipReason(source)
			} else if !mapped {
				request.TMDBID = 0
				request.Season = 0
				request.Episode = 0
				request.SkipReason = "episode_mapping_unavailable"
			}
			result.Requests = append(result.Requests, finishRequest(batchID, request))
		}
	default:
		for _, target := range videos {
			result.Requests = append(result.Requests, finishRequest(batchID, RequestPlan{
				Kind: KindUnknown, MediaPath: target, SkipReason: "unsupported_media_type",
			}))
		}
	}
	if !result.Valid() {
		return Plan{}, ErrInvalidPlan
	}
	return result, nil
}

func (p *Planner) tmdbIdentity(source Source) (int, bool) {
	if !p.enabled {
		return 0, false
	}
	provider := strings.ToLower(strings.TrimSpace(source.MediaSource))
	if provider != "tmdb" && provider != "themoviedb" {
		return 0, false
	}
	id, err := strconv.Atoi(strings.TrimSpace(source.MediaID))
	return id, err == nil && id > 0
}

func (p *Planner) skipReason(source Source) string {
	if !p.enabled {
		return "scraping_disabled"
	}
	return "tmdb_identity_unavailable"
}

func normalizeMediaType(value string) media.MediaType {
	switch strings.ToLower(strings.TrimSpace(value)) {
	case "电影", "movie":
		return media.TypeMovie
	case "电视剧", "tv", "series":
		return media.TypeTV
	default:
		return media.TypeUnknown
	}
}

func videoTargets(plan organizer.Plan) []string {
	result := make([]string, 0, len(plan.Operations))
	for _, operation := range plan.Operations {
		switch strings.ToLower(filepath.Ext(operation.Target)) {
		case ".mkv", ".mp4", ".avi", ".mov", ".ts", ".m2ts", ".wmv", ".flv", ".webm", ".iso":
			result = append(result, filepath.Clean(operation.Target))
		}
	}
	return result
}

func tvShowNFOPath(videos []string) string {
	root := filepath.Dir(videos[0])
	for _, video := range videos[1:] {
		directory := filepath.Dir(video)
		for !pathWithinDirectory(root, directory) {
			parent := filepath.Dir(root)
			if parent == root {
				break
			}
			root = parent
		}
	}
	if isSeasonDirectory(filepath.Base(root)) {
		root = filepath.Dir(root)
	}
	return filepath.Join(root, "tvshow.nfo")
}

func pathWithinDirectory(root, candidate string) bool {
	relative, err := filepath.Rel(root, candidate)
	return err == nil && relative != ".." && !strings.HasPrefix(relative, ".."+string(filepath.Separator))
}

func isSeasonDirectory(name string) bool {
	value := strings.ToLower(strings.TrimSpace(name))
	if value == "special" || value == "specials" {
		return true
	}
	if strings.HasPrefix(value, "第") && strings.HasSuffix(value, "季") {
		return decimalString(strings.TrimSuffix(strings.TrimPrefix(value, "第"), "季"))
	}
	for _, prefix := range []string{"season", "s"} {
		if !strings.HasPrefix(value, prefix) {
			continue
		}
		number := strings.TrimLeft(strings.TrimPrefix(value, prefix), " ._-")
		if decimalString(number) {
			return true
		}
	}
	return false
}

func decimalString(value string) bool {
	if value == "" {
		return false
	}
	for _, character := range value {
		if character < '0' || character > '9' {
			return false
		}
	}
	return true
}

func mapEpisode(path string, sourceSeason int, episodes []Episode, videoCount int, bound map[Episode]struct{}) (int, int, bool) {
	parsed := media.Parse(filepath.Base(path), media.Options{})
	season := parsed.BeginSeason
	if season == 0 {
		season = sourceSeason
	}
	if season == 0 {
		season = 1
	}
	if parsed.BeginEpisode > 0 && (parsed.EndEpisode == 0 || parsed.EndEpisode == parsed.BeginEpisode) {
		coordinate := Episode{Season: season, Episode: parsed.BeginEpisode}
		_, found := bound[coordinate]
		return coordinate.Season, coordinate.Episode, found
	}
	if videoCount == 1 && len(episodes) == 1 {
		coordinate := episodes[0]
		return coordinate.Season, coordinate.Episode, coordinate.Season >= 0 && coordinate.Episode > 0
	}
	return 0, 0, false
}

func episodeSet(episodes []Episode) map[Episode]struct{} {
	result := make(map[Episode]struct{}, len(episodes))
	for _, episode := range episodes {
		result[episode] = struct{}{}
	}
	return result
}

func finishRequest(batchID uuid.UUID, request RequestPlan) RequestPlan {
	request.MediaPath = filepath.Clean(request.MediaPath)
	request.ID = uuid.NewSHA1(batchID, []byte(string(request.Kind)+"\x00"+request.MediaPath))
	return request
}
