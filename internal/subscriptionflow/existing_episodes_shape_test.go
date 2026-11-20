package subscriptionflow

import (
	"context"
	"errors"
	"reflect"
	"testing"

	"github.com/moviepilot-go/moviepilot-go/internal/subscription"
)

func TestHandlerExcludeExistingEpisodesWithShape(t *testing.T) {
	t.Parallel()
	pending := []subscription.Episode{
		{Season: 1, Episode: 1}, {Season: 1, Episode: 2}, {Season: 2, Episode: 1},
	}
	overLimit := make([]subscription.Episode, maxTrackedEpisodes+1)
	for i := range overLimit {
		overLimit[i] = subscription.Episode{Season: 1, Episode: 1}
	}
	tests := []struct {
		name          string
		episodes      []subscription.Episode
		sourceErr     error
		withoutSource bool
		season        *int
		config        map[string]any
		cancel        bool
		want          []subscription.Episode
		wantSeasons   map[int]bool
		wantErr       error
	}{
		{
			name: "optional source leaves pending and shape unchanged", withoutSource: true,
			want: pending,
		},
		{
			name: "empty library leaves no explicit seasons", want: pending,
		},
		{
			name: "library records all populated seasons and subtracts exact episodes",
			episodes: []subscription.Episode{
				{Season: 1, Episode: 2}, {Season: 1, Episode: 2}, {Season: 3, Episode: 1},
			},
			want:        []subscription.Episode{{Season: 1, Episode: 1}, {Season: 2, Episode: 1}},
			wantSeasons: map[int]bool{1: true, 3: true},
		},
		{
			name:     "same season library episode outside pending still gives explicit shape",
			episodes: []subscription.Episode{{Season: 1, Episode: 99}},
			want:     pending, wantSeasons: map[int]bool{1: true},
		},
		{
			name:     "other season library does not mark target season",
			episodes: []subscription.Episode{{Season: 3, Episode: 99}},
			season:   intPointer(1), want: pending, wantSeasons: map[int]bool{3: true},
		},
		{
			name:     "valid boundary library values preserve their seasons",
			episodes: []subscription.Episode{{Season: 0, Episode: 1}, {Season: maxTrackedSeason, Episode: maxTrackedEpisode}},
			want:     pending, wantSeasons: map[int]bool{0: true, maxTrackedSeason: true},
		},
		{
			name:     "invalid negative season discards whole library batch",
			episodes: []subscription.Episode{{Season: 1, Episode: 1}, {Season: -1, Episode: 1}, {Season: 2, Episode: 1}},
			want:     pending,
		},
		{
			name:     "invalid oversized season discards whole library batch",
			episodes: []subscription.Episode{{Season: 1, Episode: 1}, {Season: maxTrackedSeason + 1, Episode: 1}},
			want:     pending,
		},
		{
			name:     "invalid zero episode discards whole library batch",
			episodes: []subscription.Episode{{Season: 1, Episode: 1}, {Season: 2, Episode: 0}},
			want:     pending,
		},
		{
			name:     "invalid oversized episode discards whole library batch",
			episodes: []subscription.Episode{{Season: 1, Episode: 1}, {Season: 2, Episode: maxTrackedEpisode + 1}},
			want:     pending,
		},
		{
			name:     "over limit library batch supplies neither subtraction nor shape",
			episodes: overLimit, want: pending,
		},
		{
			name:        "library batch at limit retains subtraction and shape",
			episodes:    overLimit[:maxTrackedEpisodes],
			want:        []subscription.Episode{{Season: 1, Episode: 2}, {Season: 2, Episode: 1}},
			wantSeasons: map[int]bool{1: true},
		},
		{
			name:          "note uses default season and existing parsing",
			withoutSource: true, config: map[string]any{"note": []any{" 2 ", "invalid", float64(1)}},
			want: []subscription.Episode{{Season: 2, Episode: 1}}, wantSeasons: map[int]bool{1: true},
		},
		{
			name:   "note applies only to explicit note season",
			season: intPointer(2), config: map[string]any{"note": []int{1}},
			want:        []subscription.Episode{{Season: 1, Episode: 1}, {Season: 1, Episode: 2}},
			wantSeasons: map[int]bool{2: true},
		},
		{
			name:   "note outside pending range still marks its season",
			season: intPointer(2), config: map[string]any{"note": []int{99}},
			want: pending, wantSeasons: map[int]bool{2: true},
		},
		{
			name:   "empty note does not supply shape",
			config: map[string]any{"note": []int{}}, want: pending,
		},
		{
			name:   "invalid note entries do not supply shape",
			config: map[string]any{"note": []any{"bad", 0, -1, maxTrackedEpisode + 1, 1.5}}, want: pending,
		},
		{
			name:   "unsupported note shape does not supply shape",
			config: map[string]any{"note": "1"}, want: pending,
		},
		{
			name:     "library and note shapes combine",
			episodes: []subscription.Episode{{Season: 3, Episode: 99}},
			season:   intPointer(2), config: map[string]any{"note": []int{99}},
			want: pending, wantSeasons: map[int]bool{2: true, 3: true},
		},
		{
			name:     "bad library batch still applies valid note",
			episodes: []subscription.Episode{{Season: 1, Episode: 1}, {Season: 3, Episode: 0}},
			season:   intPointer(2), config: map[string]any{"note": []int{1}},
			want:        []subscription.Episode{{Season: 1, Episode: 1}, {Season: 1, Episode: 2}},
			wantSeasons: map[int]bool{2: true},
		},
		{
			name:     "over limit library batch still applies valid note",
			episodes: overLimit, season: intPointer(2), config: map[string]any{"note": []int{1}},
			want:        []subscription.Episode{{Season: 1, Episode: 1}, {Season: 1, Episode: 2}},
			wantSeasons: map[int]bool{2: true},
		},
		{
			name:     "source failure ignores partial library results",
			episodes: []subscription.Episode{{Season: 1, Episode: 1}}, sourceErr: errors.New("source unavailable"),
			want: pending,
		},
		{
			name:     "source failure still applies note",
			episodes: []subscription.Episode{{Season: 3, Episode: 99}}, sourceErr: errors.New("source unavailable"),
			season: intPointer(2), config: map[string]any{"note": []int{1}},
			want:        []subscription.Episode{{Season: 1, Episode: 1}, {Season: 1, Episode: 2}},
			wantSeasons: map[int]bool{2: true},
		},
		{
			name:     "canceled context preserves cancellation and returns no partial shape",
			episodes: []subscription.Episode{{Season: 1, Episode: 1}}, sourceErr: errors.New("source unavailable"),
			config: map[string]any{"note": []int{2}}, cancel: true, wantErr: context.Canceled,
		},
		{
			name:      "source cancellation preserves cancellation",
			episodes:  []subscription.Episode{{Season: 1, Episode: 1}},
			sourceErr: errors.Join(errors.New("source unavailable"), context.Canceled), wantErr: context.Canceled,
		},
		{
			name:      "source deadline preserves deadline",
			episodes:  []subscription.Episode{{Season: 1, Episode: 1}},
			sourceErr: errors.Join(errors.New("source unavailable"), context.DeadlineExceeded), wantErr: context.DeadlineExceeded,
		},
		{
			name:     "successful lookup retains original behavior with canceled context",
			episodes: []subscription.Episode{{Season: 1, Episode: 1}}, cancel: true,
			want:        []subscription.Episode{{Season: 1, Episode: 2}, {Season: 2, Episode: 1}},
			wantSeasons: map[int]bool{1: true},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			ctx := t.Context()
			if tt.cancel {
				canceled, cancel := context.WithCancel(ctx)
				cancel()
				ctx = canceled
			}
			source := &existingEpisodesSourceFake{episodes: tt.episodes, err: tt.sourceErr}
			handler := &Handler{}
			wantCalls := 0
			if !tt.withoutSource {
				handler.existingEpisodes = source
				wantCalls = 1
			}
			got, err := handler.excludeExistingEpisodesWithShape(ctx, ExistingEpisodesQuery{
				Title: " Show ", Year: " 2026 ", MediaSource: " TMDB ", MediaID: " 123 ",
			}, tt.season, tt.config, pending)
			if !errors.Is(err, tt.wantErr) {
				t.Fatalf("excludeExistingEpisodesWithShape() error = %v, want %v", err, tt.wantErr)
			}
			if !reflect.DeepEqual(got.Episodes, tt.want) || !reflect.DeepEqual(got.ExplicitSeasons, tt.wantSeasons) {
				t.Fatalf("excludeExistingEpisodesWithShape() = %#v, want episodes=%#v seasons=%#v", got, tt.want, tt.wantSeasons)
			}
			if source.calls != wantCalls {
				t.Fatalf("source calls = %d, want %d", source.calls, wantCalls)
			}
			wantQuery := ExistingEpisodesQuery{Title: "Show", Year: "2026", MediaSource: "TMDB", MediaID: "123"}
			if source.calls > 0 && source.query != wantQuery {
				t.Fatalf("source query = %#v, want %#v", source.query, wantQuery)
			}
		})
	}
}

func TestHandlerExcludeExistingEpisodesWithShapeBypass(t *testing.T) {
	t.Parallel()
	pending := []subscription.Episode{{Season: 1, Episode: 1}}
	tests := []struct {
		name       string
		pending    []subscription.Episode
		config     map[string]any
		nilHandler bool
	}{
		{name: "nil pending bypasses library and note", config: map[string]any{"note": []int{1}}},
		{name: "empty pending bypasses library and note", pending: []subscription.Episode{}, config: map[string]any{"note": []int{1}}},
		{name: "best version bool bypasses library and note", pending: pending, config: map[string]any{"best_version": true, "note": []int{1}}},
		{name: "best version integer bypasses library and note", pending: pending, config: map[string]any{"best_version": float64(1), "note": []int{1}}},
		{name: "nil handler bypasses note", pending: pending, config: map[string]any{"note": []int{1}}, nilHandler: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			source := &existingEpisodesSourceFake{episodes: pending, err: context.Canceled}
			handler := &Handler{existingEpisodes: source}
			if tt.nilHandler {
				handler = nil
			}
			got, err := handler.excludeExistingEpisodesWithShape(t.Context(), ExistingEpisodesQuery{}, nil, tt.config, tt.pending)
			want := append([]subscription.Episode(nil), tt.pending...)
			if err != nil || !reflect.DeepEqual(got.Episodes, want) || len(got.ExplicitSeasons) != 0 || source.calls != 0 {
				t.Fatalf("excludeExistingEpisodesWithShape() = (%#v, %v), calls=%d; want episodes=%#v and no shape or lookup", got, err, source.calls, want)
			}
			if len(got.Episodes) > 0 {
				got.Episodes[0].Episode = 99
				if tt.pending[0].Episode != 1 {
					t.Fatal("bypass result aliases pending episodes")
				}
			}
		})
	}
}
