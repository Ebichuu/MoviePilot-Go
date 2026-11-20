package subscriptionflow

import (
	"errors"
	"reflect"
	"testing"

	"github.com/google/uuid"

	"github.com/moviepilot-go/moviepilot-go/internal/indexer"
	"github.com/moviepilot-go/moviepilot-go/internal/media"
	"github.com/moviepilot-go/moviepilot-go/internal/metadata"
	"github.com/moviepilot-go/moviepilot-go/internal/organization"
	"github.com/moviepilot-go/moviepilot-go/internal/subscription"
)

func TestSubscriptionSuccessInput(t *testing.T) {
	t.Parallel()

	intentID := uuid.New()
	tests := []struct {
		name       string
		target     subscriptionSuccessTarget
		candidate  subscription.Candidate
		wantMedia  organization.MediaContext
		wantTerm   string
		wantEvents []organization.Episode
	}{
		{
			name: "subscription identity and category win over site resource",
			target: subscriptionSuccessTarget{
				Username: "alice", Name: "Target Show", Year: "2026", MediaType: string(media.TypeTV),
				MediaSource: "themoviedb", MediaID: "42",
				VoteAverage: 8.6, PosterURL: "https://image.example/poster.jpg",
				BackdropURL: "https://image.example/backdrop.jpg",
				Config:      map[string]any{"media_category": "欧美", "custom_words": "REMOVE=>"},
			},
			candidate: subscription.Candidate{
				Resource: indexer.Resource{
					Title: "Release.Show.S02E03.WEB-DL", Description: "HDR10+ 2160p {[doubanid=999]} ",
					Category: "站点分类",
				},
				CoveredEpisodes: []subscription.Episode{{Season: 2, Episode: 3}, {Season: 2, Episode: 5}},
			},
			wantMedia: organization.MediaContext{
				Source: "themoviedb", MediaID: "42", TitleYear: "Target Show (2026)",
				MediaType: string(media.TypeTV), Category: "欧美", VoteAverage: 8.6,
				PosterURL: "https://image.example/poster.jpg", BackdropURL: "https://image.example/backdrop.jpg",
			},
			wantTerm:   "WEB-DL HDR10+ 2160p",
			wantEvents: []organization.Episode{{Season: 2, Episode: 3}, {Season: 2, Episode: 5}},
		},
		{
			name: "movie keeps empty optional metadata and has no episode expansion",
			target: subscriptionSuccessTarget{
				Username: "bob", Name: "Target Movie", MediaType: string(media.TypeMovie), Config: map[string]any{},
			},
			candidate: subscription.Candidate{Resource: indexer.Resource{
				Title: "Target.Movie.BluRay", Description: "1080p",
			}},
			wantMedia: organization.MediaContext{TitleYear: "Target Movie", MediaType: string(media.TypeMovie)},
			wantTerm:  "BluRay 1080p",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			got := subscriptionSuccessInput(tt.target, tt.candidate, intentID)
			if got.IntentID != intentID || got.Version != organization.CurrentSuccessVersion || got.Legacy || got.Manual || !got.Background {
				t.Fatalf("submission flags = %#v", got)
			}
			if got.Username != tt.target.Username {
				t.Fatalf("username = %q, want %q", got.Username, tt.target.Username)
			}
			if got.Media != tt.wantMedia {
				t.Fatalf("media = %#v, want %#v", got.Media, tt.wantMedia)
			}
			if got.ResourceTerm != tt.wantTerm {
				t.Fatalf("resource term = %q, want %q", got.ResourceTerm, tt.wantTerm)
			}
			if !reflect.DeepEqual(got.Episodes, tt.wantEvents) {
				t.Fatalf("episodes = %#v, want %#v", got.Episodes, tt.wantEvents)
			}
			if _, err := organization.NormalizeSuccessInput(got); err != nil {
				t.Fatalf("NormalizeSuccessInput() error = %v", err)
			}
		})
	}
}

func TestBuildSubscriptionSuccessInputFallsBackToStoredMetadata(t *testing.T) {
	t.Parallel()

	resolver := &subscriptionFlowMediaDetails{err: errors.New("metadata unavailable")}
	handler := &Handler{mediaDetails: resolver}
	intentID := uuid.New()
	target := subscriptionSuccessTarget{
		Username: "alice", Name: "Fallback", Year: "2026", MediaType: string(media.TypeTV),
		MediaSource: "douban", MediaID: "42", VoteAverage: 7.9,
		PosterURL:   "https://image.example/stored-poster.jpg",
		BackdropURL: "https://image.example/stored-backdrop.jpg",
	}

	got := handler.buildSubscriptionSuccessInput(t.Context(), target, subscription.Candidate{}, intentID)
	if got.Media.VoteAverage != target.VoteAverage || got.Media.PosterURL != target.PosterURL ||
		got.Media.BackdropURL != target.BackdropURL {
		t.Fatalf("fallback media = %#v", got.Media)
	}
	if resolver.calls != 1 || resolver.source != "douban" || resolver.id != "tv:42" {
		t.Fatalf("metadata detail = calls:%d source:%q id:%q", resolver.calls, resolver.source, resolver.id)
	}
}

func TestSubscriptionMetadataDetailID(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name      string
		source    string
		id        string
		mediaType string
		want      string
	}{
		{name: "TMDB movie", source: "themoviedb", id: "42", mediaType: string(media.TypeMovie), want: "movie:42"},
		{name: "TMDB alias TV", source: " TMDB ", id: "42", mediaType: string(media.TypeTV), want: "tv:42"},
		{name: "Douban movie", source: "douban", id: "db-1", mediaType: string(media.TypeMovie), want: "movie:db-1"},
		{name: "typed identity stays stable", source: "themoviedb", id: "tv:42", mediaType: string(media.TypeTV), want: "tv:42"},
		{name: "Bangumi uses native ID", source: "bangumi", id: "42", mediaType: string(media.TypeTV), want: "42"},
		{name: "unknown type uses native ID", source: "themoviedb", id: "42", mediaType: "", want: "42"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if got := subscriptionMetadataDetailID(tt.source, tt.id, tt.mediaType); got != tt.want {
				t.Fatalf("subscriptionMetadataDetailID() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestBuildSubscriptionSuccessInputRejectsInvalidProviderMetadata(t *testing.T) {
	t.Parallel()

	target := subscriptionSuccessTarget{
		Username: "alice", Name: "Fallback", MediaType: string(media.TypeMovie),
		MediaSource: "themoviedb", MediaID: "42", VoteAverage: 7.5,
		PosterURL: "https://image.example/stored.jpg",
	}
	handler := &Handler{mediaDetails: &subscriptionFlowMediaDetails{item: metadata.Media{
		VoteAverage: 8.5, PosterURL: "file:///private/poster.jpg",
	}}}

	got := handler.buildSubscriptionSuccessInput(t.Context(), target, subscription.Candidate{}, uuid.New())
	if got.Media.VoteAverage != target.VoteAverage || got.Media.PosterURL != target.PosterURL {
		t.Fatalf("invalid provider metadata replaced fallback: %#v", got.Media)
	}
}
