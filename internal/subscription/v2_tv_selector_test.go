package subscription

import (
	"context"
	"errors"
	"reflect"
	"testing"

	"github.com/moviepilot-go/moviepilot-go/internal/indexer"
	"github.com/moviepilot-go/moviepilot-go/internal/media"
)

func TestSelectV2TVCandidates(t *testing.T) {
	t.Parallel()

	episodes := func(season int, values ...int) []Episode {
		result := make([]Episode, len(values))
		for index, value := range values {
			result[index] = Episode{Season: season, Episode: value}
		}
		return result
	}
	fullNeed := func(state string, strict bool, candidates ...V2TVCandidate) V2TVSelectionRequest {
		return V2TVSelectionRequest{
			SubscriptionState: state,
			Seasons: []V2TVSeasonNeed{{
				Season: 1, MissingEpisodes: episodes(1, 1, 2, 3, 4, 5),
				TotalEpisodes: 5, FullSeason: true, RequireCompleteCoverage: strict,
			}},
			Candidates: candidates,
		}
	}
	candidate := func(key string, titleEpisodes ...int) V2TVCandidate {
		return V2TVCandidate{
			Key: key, Seasons: []int{1}, TitleEpisodes: episodes(1, titleEpisodes...),
		}
	}

	tests := []struct {
		name          string
		request       V2TVSelectionRequest
		projections   map[string]V2TVEpisodeProjection
		attempts      map[string][]V2TVCandidateAttemptResult
		attemptErrors map[string]error
		wantCalls     []v2TVTestCall
		wantAccepted  []string
		wantRemaining []Episode
		wantState     string
		wantStopped   bool
		wantErr       error
	}{
		{
			name:        "TV-FULL-01 accepts a complete real season and stops",
			request:     fullNeed(V2TVSubscriptionStateReady, false, candidate("full"), candidate("episode", 1)),
			projections: map[string]V2TVEpisodeProjection{"full": {Episodes: episodes(1, 1, 2, 3, 4, 5)}},
			wantCalls: []v2TVTestCall{{
				key: "full", stage: V2TVStageFullSeason,
				expected: episodes(1, 1, 2, 3, 4, 5),
			}},
			wantAccepted: []string{"full"},
			wantState:    V2TVSubscriptionStateCompleted,
		},
		{
			name:        "TV-FULL-02 preserves V2 count-only full-season acceptance",
			request:     fullNeed(V2TVSubscriptionStateReady, false, candidate("shifted")),
			projections: map[string]V2TVEpisodeProjection{"shifted": {Episodes: episodes(1, 2, 3, 4, 5, 6)}},
			wantCalls: []v2TVTestCall{{
				key: "shifted", stage: V2TVStageFullSeason,
				expected: episodes(1, 1, 2, 3, 4, 5),
			}},
			wantAccepted: []string{"shifted"},
			wantState:    V2TVSubscriptionStateCompleted,
		},
		{
			name:    "TV-FULL-03 strict coverage skips a shifted season",
			request: fullNeed(V2TVSubscriptionStateReady, true, candidate("shifted"), candidate("complete")),
			projections: map[string]V2TVEpisodeProjection{
				"shifted":  {Episodes: episodes(1, 2, 3, 4, 5, 6)},
				"complete": {Episodes: episodes(1, 1, 2, 3, 4, 5)},
			},
			wantCalls: []v2TVTestCall{{
				key: "complete", stage: V2TVStageFullSeason,
				expected: episodes(1, 1, 2, 3, 4, 5),
			}},
			wantAccepted: []string{"complete"},
			wantState:    V2TVSubscriptionStateCompleted,
		},
		{
			name:        "failed full-season count updates metadata before the title pass",
			request:     fullNeed(V2TVSubscriptionStateReady, false, candidate("partial")),
			projections: map[string]V2TVEpisodeProjection{"partial": {Episodes: episodes(1, 1, 3)}},
			wantCalls: []v2TVTestCall{{
				key: "partial", stage: V2TVStageTitleEpisodes, expected: episodes(1, 1, 2, 3),
			}},
			wantAccepted:  []string{"partial"},
			wantRemaining: episodes(1, 4, 5),
			wantState:     V2TVSubscriptionStateReady,
		},
		{
			name: "strict title coverage accepts a superset of the target range",
			request: V2TVSelectionRequest{
				SubscriptionState: V2TVSubscriptionStateReady,
				Seasons: []V2TVSeasonNeed{{
					Season: 1, MissingEpisodes: episodes(1, 1, 2, 3, 4, 5),
					TotalEpisodes: 5, RequireCompleteCoverage: true,
				}},
				Candidates: []V2TVCandidate{candidate("superset", 1, 2, 3, 4, 5, 6)},
			},
			wantCalls: []v2TVTestCall{{
				key: "superset", stage: V2TVStageTitleEpisodes,
				expected: episodes(1, 1, 2, 3, 4, 5),
			}},
			wantAccepted: []string{"superset"},
			wantState:    V2TVSubscriptionStateCompleted,
		},
		{
			name: "TV-EP-01 greedily accepts two disjoint title ranges",
			request: V2TVSelectionRequest{
				SubscriptionState: V2TVSubscriptionStateReady,
				Seasons:           []V2TVSeasonNeed{{Season: 1, MissingEpisodes: episodes(1, 1, 2, 3, 4), TotalEpisodes: 4}},
				Candidates:        []V2TVCandidate{candidate("first", 1, 2), candidate("second", 3, 4)},
			},
			wantCalls: []v2TVTestCall{
				{key: "first", stage: V2TVStageTitleEpisodes, expected: episodes(1, 1, 2)},
				{key: "second", stage: V2TVStageTitleEpisodes, expected: episodes(1, 3, 4)},
			},
			wantAccepted: []string{"first", "second"},
			wantState:    V2TVSubscriptionStateCompleted,
		},
		{
			name: "TV-EP-02 moves a partially overlapping range to unpack",
			request: V2TVSelectionRequest{
				SubscriptionState: V2TVSubscriptionStateReady,
				Seasons:           []V2TVSeasonNeed{{Season: 1, MissingEpisodes: episodes(1, 1, 2, 3), TotalEpisodes: 3}},
				Candidates:        []V2TVCandidate{candidate("first", 1, 2), candidate("overlap", 2, 3)},
			},
			projections: map[string]V2TVEpisodeProjection{"overlap": {Episodes: episodes(1, 2, 3)}},
			wantCalls: []v2TVTestCall{
				{key: "first", stage: V2TVStageTitleEpisodes, expected: episodes(1, 1, 2)},
				{key: "overlap", stage: V2TVStageUnpack, selected: episodes(1, 3), expected: episodes(1, 3)},
			},
			wantAccepted: []string{"first", "overlap"},
			wantState:    V2TVSubscriptionStateCompleted,
		},
		{
			name: "fixed allowed episodes rejects whole title and selects only the improvement",
			request: V2TVSelectionRequest{
				SubscriptionState: V2TVSubscriptionStateReady,
				Seasons:           []V2TVSeasonNeed{{Season: 1, MissingEpisodes: episodes(1, 1, 2), TotalEpisodes: 2}},
				Candidates: []V2TVCandidate{{
					Key: "mixed", Seasons: []int{1}, TitleEpisodes: episodes(1, 1, 2), AllowedEpisodes: episodes(1, 2),
				}},
			},
			projections: map[string]V2TVEpisodeProjection{"mixed": {Episodes: episodes(1, 1, 2)}},
			wantCalls: []v2TVTestCall{{
				key: "mixed", stage: V2TVStageUnpack, selected: episodes(1, 2), expected: episodes(1, 2),
			}},
			wantAccepted:  []string{"mixed"},
			wantRemaining: episodes(1, 1),
			wantState:     V2TVSubscriptionStateReady,
		},
		{
			name: "explicit empty allowed episodes does not inherit unconstrained need",
			request: V2TVSelectionRequest{
				SubscriptionState: V2TVSubscriptionStateReady,
				Seasons:           []V2TVSeasonNeed{{Season: 1, MissingEpisodes: episodes(1, 1), TotalEpisodes: 1}},
				Candidates: []V2TVCandidate{{
					Key: "rejected", Seasons: []int{1}, TitleEpisodes: episodes(1, 1), AllowedEpisodes: []Episode{},
				}},
			},
			projections:   map[string]V2TVEpisodeProjection{"rejected": {Episodes: episodes(1, 1)}},
			wantCalls:     []v2TVTestCall{},
			wantRemaining: episodes(1, 1),
			wantState:     V2TVSubscriptionStateReady,
		},
		{
			name: "TV-UNPACK-02 skips a real-file miss and continues in input order",
			request: V2TVSelectionRequest{
				SubscriptionState: V2TVSubscriptionStateReady,
				Seasons:           []V2TVSeasonNeed{{Season: 1, MissingEpisodes: episodes(1, 3), TotalEpisodes: 3}},
				Candidates:        []V2TVCandidate{candidate("miss"), candidate("hit")},
			},
			projections: map[string]V2TVEpisodeProjection{
				"miss": {Episodes: episodes(1, 1, 2)}, "hit": {Episodes: episodes(1, 3)},
			},
			wantCalls: []v2TVTestCall{{
				key: "hit", stage: V2TVStageUnpack,
				selected: episodes(1, 3), expected: episodes(1, 3),
			}},
			wantAccepted: []string{"hit"},
			wantState:    V2TVSubscriptionStateCompleted,
		},
		{
			name: "TV-FAIL-01 explicit failure continues to the next sorted candidate",
			request: V2TVSelectionRequest{
				SubscriptionState: V2TVSubscriptionStateReady,
				Seasons:           []V2TVSeasonNeed{{Season: 1, MissingEpisodes: episodes(1, 1), TotalEpisodes: 1}},
				Candidates:        []V2TVCandidate{candidate("B", 1), candidate("A", 1)},
			},
			attempts: map[string][]V2TVCandidateAttemptResult{
				"B": {{HashAcquired: false}},
			},
			wantCalls: []v2TVTestCall{
				{key: "B", stage: V2TVStageTitleEpisodes, expected: episodes(1, 1)},
				{key: "A", stage: V2TVStageTitleEpisodes, expected: episodes(1, 1)},
			},
			wantAccepted: []string{"A"},
			wantState:    V2TVSubscriptionStateCompleted,
		},
		{
			name: "terminal result stops before the next candidate",
			request: V2TVSelectionRequest{
				SubscriptionState: V2TVSubscriptionStateReady,
				Seasons:           []V2TVSeasonNeed{{Season: 1, MissingEpisodes: episodes(1, 1), TotalEpisodes: 1}},
				Candidates:        []V2TVCandidate{candidate("terminal", 1), candidate("later", 1)},
			},
			attempts: map[string][]V2TVCandidateAttemptResult{
				"terminal": {{Stop: true}},
			},
			wantCalls:     []v2TVTestCall{{key: "terminal", stage: V2TVStageTitleEpisodes, expected: episodes(1, 1)}},
			wantRemaining: episodes(1, 1),
			wantState:     V2TVSubscriptionStateReady,
			wantStopped:   true,
		},
		{
			name: "unknown attempt error stops before the next candidate",
			request: V2TVSelectionRequest{
				SubscriptionState: V2TVSubscriptionStateReady,
				Seasons:           []V2TVSeasonNeed{{Season: 1, MissingEpisodes: episodes(1, 1), TotalEpisodes: 1}},
				Candidates:        []V2TVCandidate{candidate("unknown", 1), candidate("later", 1)},
			},
			attemptErrors: map[string]error{"unknown": errV2TVTestUnknown},
			wantCalls:     []v2TVTestCall{{key: "unknown", stage: V2TVStageTitleEpisodes, expected: episodes(1, 1)}},
			wantRemaining: episodes(1, 1),
			wantState:     V2TVSubscriptionStateReady,
			wantStopped:   true,
			wantErr:       errV2TVTestUnknown,
		},
		{
			name:        "TV-STATE-P keeps P after all episodes are accepted",
			request:     fullNeed(V2TVSubscriptionStatePending, false, candidate("full")),
			projections: map[string]V2TVEpisodeProjection{"full": {Episodes: episodes(1, 1, 2, 3, 4, 5)}},
			wantCalls: []v2TVTestCall{{
				key: "full", stage: V2TVStageFullSeason,
				expected: episodes(1, 1, 2, 3, 4, 5),
			}},
			wantAccepted: []string{"full"},
			wantState:    V2TVSubscriptionStatePending,
		},
	}

	for _, test := range tests {
		test := test
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			calls := make([]v2TVTestCall, 0)
			attemptIndex := make(map[string]int)
			project := func(_ context.Context, item V2TVCandidate) (V2TVEpisodeProjection, error) {
				return test.projections[item.Key], nil
			}
			attempt := func(_ context.Context, item V2TVCandidateAttempt) (V2TVCandidateAttemptResult, error) {
				calls = append(calls, v2TVTestCall{
					key: item.Candidate.Key, stage: item.Stage,
					selected: cloneEpisodes(item.SelectedEpisodes), expected: cloneEpisodes(item.ExpectedCoverage),
				})
				if err := test.attemptErrors[item.Candidate.Key]; err != nil {
					return V2TVCandidateAttemptResult{}, err
				}
				index := attemptIndex[item.Candidate.Key]
				attemptIndex[item.Candidate.Key]++
				if scripted := test.attempts[item.Candidate.Key]; index < len(scripted) {
					return scripted[index], nil
				}
				return V2TVCandidateAttemptResult{
					HashAcquired: true, CoveredEpisodes: cloneEpisodes(item.ExpectedCoverage),
				}, nil
			}

			got, err := SelectV2TVCandidates(context.Background(), test.request, project, attempt)
			if !errors.Is(err, test.wantErr) {
				t.Fatalf("SelectV2TVCandidates() error = %v, want %v", err, test.wantErr)
			}
			if !reflect.DeepEqual(calls, test.wantCalls) {
				t.Fatalf("attempt calls = %#v, want %#v", calls, test.wantCalls)
			}
			if !reflect.DeepEqual(got.AcceptedCandidateKeys, nonNilStrings(test.wantAccepted)) {
				t.Fatalf("accepted keys = %#v, want %#v", got.AcceptedCandidateKeys, test.wantAccepted)
			}
			if !reflect.DeepEqual(got.RemainingEpisodes, nonNilEpisodes(test.wantRemaining)) {
				t.Fatalf("remaining episodes = %#v, want %#v", got.RemainingEpisodes, test.wantRemaining)
			}
			if got.SubscriptionState != test.wantState || got.Stopped != test.wantStopped {
				t.Fatalf("result state/stopped = %q/%t, want %q/%t", got.SubscriptionState, got.Stopped, test.wantState, test.wantStopped)
			}
		})
	}
}

func TestSelectV2TVCandidatesDoesNotRetryProjectionFailureInLaterStages(t *testing.T) {
	t.Parallel()
	projectCalls := 0
	attemptCalls := 0
	request := V2TVSelectionRequest{
		SubscriptionState: V2TVSubscriptionStateReady,
		Seasons: []V2TVSeasonNeed{{
			Season: 1, MissingEpisodes: []Episode{{Season: 1, Episode: 1}},
			TotalEpisodes: 1, FullSeason: true,
		}},
		Candidates: []V2TVCandidate{{Key: "failed-projection", Seasons: []int{1}}},
	}
	got, err := SelectV2TVCandidates(
		context.Background(), request,
		func(context.Context, V2TVCandidate) (V2TVEpisodeProjection, error) {
			projectCalls++
			return V2TVEpisodeProjection{Failed: true}, nil
		},
		func(context.Context, V2TVCandidateAttempt) (V2TVCandidateAttemptResult, error) {
			attemptCalls++
			return V2TVCandidateAttemptResult{}, nil
		},
	)
	if err != nil {
		t.Fatal(err)
	}
	if projectCalls != 1 || attemptCalls != 0 {
		t.Fatalf("projection/attempt calls = %d/%d, want 1/0", projectCalls, attemptCalls)
	}
	if !reflect.DeepEqual(got.RemainingEpisodes, []Episode{{Season: 1, Episode: 1}}) {
		t.Fatalf("remaining episodes = %#v", got.RemainingEpisodes)
	}
}

func TestSelectV2TVCandidatesRejectsInvalidContracts(t *testing.T) {
	t.Parallel()
	validRequest := V2TVSelectionRequest{
		SubscriptionState: V2TVSubscriptionStateReady,
		Seasons:           []V2TVSeasonNeed{{Season: 1, MissingEpisodes: []Episode{{Season: 1, Episode: 1}}, TotalEpisodes: 1}},
		Candidates:        []V2TVCandidate{{Key: "candidate", Seasons: []int{1}, TitleEpisodes: []Episode{{Season: 1, Episode: 1}}}},
	}
	project := func(context.Context, V2TVCandidate) (V2TVEpisodeProjection, error) {
		return V2TVEpisodeProjection{}, nil
	}
	attempt := func(_ context.Context, item V2TVCandidateAttempt) (V2TVCandidateAttemptResult, error) {
		return V2TVCandidateAttemptResult{HashAcquired: true, CoveredEpisodes: item.ExpectedCoverage}, nil
	}
	tests := []struct {
		name    string
		request V2TVSelectionRequest
		project V2TVProjectEpisodesFunc
		attempt V2TVAttemptCandidateFunc
	}{
		{name: "unknown subscription state", request: func() V2TVSelectionRequest { value := validRequest; value.SubscriptionState = "N"; return value }(), project: project, attempt: attempt},
		{name: "missing projector", request: validRequest, attempt: attempt},
		{name: "missing attempter", request: validRequest, project: project},
		{name: "duplicate season", request: func() V2TVSelectionRequest {
			value := validRequest
			value.Seasons = append(value.Seasons, value.Seasons[0])
			return value
		}(), project: project, attempt: attempt},
		{name: "title episode outside candidate season", request: func() V2TVSelectionRequest {
			value := validRequest
			value.Candidates = []V2TVCandidate{{Key: "candidate", Seasons: []int{1}, TitleEpisodes: []Episode{{Season: 2, Episode: 1}}}}
			return value
		}(), project: project, attempt: attempt},
	}
	for _, test := range tests {
		test := test
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			if _, err := SelectV2TVCandidates(context.Background(), test.request, test.project, test.attempt); !errors.Is(err, ErrInvalidV2TVSelection) {
				t.Fatalf("SelectV2TVCandidates() error = %v", err)
			}
		})
	}
}

func TestNewV2TVCandidatePreservesFullTitleEpisodeRange(t *testing.T) {
	t.Parallel()

	parsed := media.ParseResult{
		Type: media.TypeTV, BeginSeason: 1, EndSeason: 1, BeginEpisode: 1, EndEpisode: 2,
	}
	got, err := NewV2TVCandidate(Candidate{
		Key:             "range",
		Resource:        indexer.Resource{Title: "Example.S01E01-E02"},
		Parsed:          parsed,
		CoveredEpisodes: []Episode{{Season: 1, Episode: 1}},
	})
	if err != nil {
		t.Fatal(err)
	}
	want := []Episode{{Season: 1, Episode: 1}, {Season: 1, Episode: 2}}
	if !reflect.DeepEqual(got.TitleEpisodes, want) {
		t.Fatalf("TitleEpisodes = %#v, want %#v", got.TitleEpisodes, want)
	}
}

func TestV2TVEpisodeCoordinatesPreservesSpecialSeason(t *testing.T) {
	t.Parallel()

	got, err := V2TVEpisodeCoordinates("Example.S00E03", media.ParseResult{
		Type: media.TypeTV, BeginSeason: 0, BeginEpisode: 3, EndEpisode: 3,
	})
	if err != nil {
		t.Fatal(err)
	}
	want := []Episode{{Season: 0, Episode: 3}}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("coordinates = %#v, want %#v", got, want)
	}
}

type v2TVTestCall struct {
	key      string
	stage    V2TVStage
	selected []Episode
	expected []Episode
}

var errV2TVTestUnknown = errors.New("unknown attempt failure")

func cloneEpisodes(items []Episode) []Episode {
	return append([]Episode(nil), items...)
}

func nonNilEpisodes(items []Episode) []Episode {
	if items == nil {
		return []Episode{}
	}
	return items
}

func nonNilStrings(items []string) []string {
	if items == nil {
		return []string{}
	}
	return items
}
