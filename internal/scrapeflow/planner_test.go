package scrapeflow

import (
	"context"
	"errors"
	"path/filepath"
	"testing"

	"github.com/google/uuid"

	"github.com/moviepilot-go/moviepilot-go/internal/organizer"
)

func TestPlannerPlan(t *testing.T) {
	t.Parallel()
	batchID := uuid.New()
	intentID := uuid.New()
	root := t.TempDir()
	tests := []struct {
		name    string
		enabled bool
		source  Source
		targets []string
		assert  func(*testing.T, Plan)
	}{
		{
			name: "movie uses subscription TMDB identity", enabled: true,
			source:  Source{Username: "alice", MediaType: "电影", MediaSource: "themoviedb", MediaID: "42"},
			targets: []string{filepath.Join(root, "Movie.mkv")},
			assert: func(t *testing.T, plan Plan) {
				if len(plan.Requests) != 1 || plan.Requests[0].Kind != KindMovie || plan.Requests[0].TMDBID != 42 || plan.Requests[0].SkipReason != "" {
					t.Fatalf("movie requests = %#v", plan.Requests)
				}
			},
		},
		{
			name: "episode filename must match bound coordinate", enabled: true,
			source: Source{Username: "alice", MediaType: "电视剧", MediaSource: "tmdb", MediaID: "1399", Season: 1,
				Episodes: []Episode{{Season: 1, Episode: 2}}},
			targets: []string{filepath.Join(root, "Show", "Season 01", "Show.S01E02.mkv")},
			assert: func(t *testing.T, plan Plan) {
				if len(plan.Requests) != 2 || plan.Requests[0].Kind != KindTVShow || plan.Requests[1].Kind != KindEpisode ||
					plan.Requests[0].MediaPath != filepath.Join(root, "Show", "tvshow.nfo") ||
					plan.Requests[1].Season != 1 || plan.Requests[1].Episode != 2 || plan.Requests[1].TMDBID != 1399 {
					t.Fatalf("TV requests = %#v", plan.Requests)
				}
			},
		},
		{
			name: "unbound parsed episode is skipped", enabled: true,
			source: Source{Username: "alice", MediaType: "电视剧", MediaSource: "tmdb", MediaID: "1399", Season: 1,
				Episodes: []Episode{{Season: 1, Episode: 3}}},
			targets: []string{filepath.Join(root, "Show.S01E02.mkv")},
			assert: func(t *testing.T, plan Plan) {
				request := plan.Requests[1]
				if request.SkipReason != "episode_mapping_unavailable" || request.TMDBID != 0 {
					t.Fatalf("episode request = %#v", request)
				}
			},
		},
		{
			name: "disabled scraper is terminal skip", enabled: false,
			source:  Source{Username: "alice", MediaType: "电影", MediaSource: "tmdb", MediaID: "42"},
			targets: []string{filepath.Join(root, "Movie.mkv")},
			assert: func(t *testing.T, plan Plan) {
				if plan.Requests[0].SkipReason != "scraping_disabled" || plan.Requests[0].TMDBID != 0 {
					t.Fatalf("disabled request = %#v", plan.Requests[0])
				}
			},
		},
		{
			name: "disabled TV episode remains a valid terminal skip", enabled: false,
			source: Source{Username: "alice", MediaType: "电视剧", MediaSource: "tmdb", MediaID: "1399", Season: 1,
				Episodes: []Episode{{Season: 1, Episode: 2}}},
			targets: []string{filepath.Join(root, "Show", "S01", "Show.S01E02.mkv")},
			assert: func(t *testing.T, plan Plan) {
				if len(plan.Requests) != 2 || plan.Requests[0].MediaPath != filepath.Join(root, "Show", "tvshow.nfo") {
					t.Fatalf("disabled TV requests = %#v", plan.Requests)
				}
				for _, request := range plan.Requests {
					if request.SkipReason != "scraping_disabled" || request.TMDBID != 0 || !request.Valid() {
						t.Fatalf("disabled request = %#v", request)
					}
				}
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			planner, err := NewPlanner(sourceStoreFake{source: tt.source}, tt.enabled)
			if err != nil {
				t.Fatal(err)
			}
			plan, err := planner.Plan(t.Context(), batchID, intentID, organizationPlan(tt.targets))
			if err != nil {
				t.Fatal(err)
			}
			if !plan.Valid() || plan.BatchID != batchID || plan.IntentID != intentID || plan.Username != "alice" {
				t.Fatalf("invalid plan: %#v", plan)
			}
			tt.assert(t, plan)
		})
	}
}

func TestPlannerPlanSanitizesSourceFailure(t *testing.T) {
	t.Parallel()
	planner, err := NewPlanner(sourceStoreFake{err: errors.New("postgres://secret")}, true)
	if err != nil {
		t.Fatal(err)
	}
	_, err = planner.Plan(t.Context(), uuid.New(), uuid.New(), organizationPlan([]string{filepath.Join(t.TempDir(), "a.mkv")}))
	if !errors.Is(err, ErrPersistence) || err.Error() == "postgres://secret" {
		t.Fatalf("Plan() error = %v", err)
	}
}

func organizationPlan(targets []string) organizer.Plan {
	operations := make([]organizer.Operation, 0, len(targets))
	for _, target := range targets {
		operations = append(operations, organizer.Operation{Target: target})
	}
	return organizer.Plan{ID: "plan", TargetRoot: filepath.Dir(targets[0]), Operations: operations}
}

type sourceStoreFake struct {
	source Source
	err    error
}

func (s sourceStoreFake) LoadScrapeSource(context.Context, uuid.UUID) (Source, error) {
	return s.source, s.err
}
