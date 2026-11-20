package subscriptionflow

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strconv"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/moviepilot-go/moviepilot-go/internal/database"
	"github.com/moviepilot-go/moviepilot-go/internal/downloader"
	"github.com/moviepilot-go/moviepilot-go/internal/downloadflow"
	"github.com/moviepilot-go/moviepilot-go/internal/indexer"
	"github.com/moviepilot-go/moviepilot-go/internal/media"
	"github.com/moviepilot-go/moviepilot-go/internal/metadata"
	"github.com/moviepilot-go/moviepilot-go/internal/organization"
	"github.com/moviepilot-go/moviepilot-go/internal/subscription"
)

type recoveredTVNativeCase struct {
	Name  string
	Input struct {
		State, Entrypoint string
		Total             int              `json:"total_episode"`
		Start             int              `json:"start_episode"`
		Manual            bool             `json:"manual_total_episode"`
		Initial           map[string][]int `json:"initial_seasons"`
		Retry             map[string][]int `json:"retry_seasons"`
		Library           map[string][]int `json:"library_seasons"`
		Note              []int
		Priorities        map[string]int `json:"episode_priority"`
		Priority          int            `json:"current_priority"`
		Lack              int            `json:"initial_lack_episode"`
		Cancel            bool           `json:"completion_cancel"`
		Candidates        []struct {
			Name     string
			Meta     []int `json:"meta_episodes"`
			Files    []int `json:"file_episodes"`
			Success  bool
			Priority int
		}
	}
	Expected struct {
		Lack           int `json:"lack_episode"`
		Completed      bool
		RetryCalls     int     `json:"retry_calls"`
		LibraryCalls   int     `json:"library_calls"`
		RemainingState *string `json:"remaining_state"`
		Downloads      []struct {
			Name     string
			Selected []int `json:"selected_episodes"`
			Meta     []int `json:"meta_episodes"`
		}
	}
}

func TestRecoveredTVLibraryAndRSSKeepSeparateMediaFacts(t *testing.T) {
	t.Parallel()
	for _, title := range []string{"First title", ""} {
		t.Run("initial title "+title, func(t *testing.T) {
			t.Parallel()
			target := database.Subscription{ID: 123, Username: "owner", Name: "Configured title", Year: "2026", MediaType: string(media.TypeTV), MediaSource: "themoviedb", MediaID: "123", Season: intPointer(1), InternalState: subscription.Active, V2State: "R", UpdatedAt: time.Now().UTC(), Config: map[string]any{"total_episode": 0, "start_episode": 4}}
			store := &recoveredTVFlowStore{rssFlowStore: newRSSFlowStore(target), current: target}
			initial := metadata.Media{Type: "tv", Title: title}
			retry := nativeMetadataDetail(map[string][]int{"1": {1, 2, 3}})
			retry.Title, retry.Year, retry.TMDBID = "Retry title", 2025, "456"
			reader := &tvMetadataBranchReader{initial: initial, retry: retry}
			library := &existingEpisodesSourceFake{}
			handler := mustSubscriptionFlowHandler(t, store, &subscriptionFlowSearcher{}, subscriptionFlowAdapters{}, &subscriptionFlowSubmission{})
			handler.SetMediaDetailResolver(reader)
			handler.SetExistingEpisodesSource(library)
			refreshed, resolution, state, err := handler.prepareTVSearchTarget(t.Context(), target)
			if err != nil {
				t.Fatal(err)
			}
			query, _, err := candidateQuery(refreshed)
			if err != nil {
				t.Fatal(err)
			}
			query, err = handler.withRecoveredTVMissingEpisodes(t.Context(), refreshed, resolution, state.RecoveredTotal, query, state.RecoveredLibraryQuery)
			if err != nil {
				t.Fatal(err)
			}
			if library.calls != 1 || library.query.Title != "Retry title" || library.query.Year != "2025" || library.query.MediaSource != "themoviedb" || library.query.MediaID != "456" || len(query.MissingEpisodes) != 0 {
				t.Fatalf("library=%+v calls=%d query=%+v", library.query, library.calls, query)
			}
			query.TVMediaRecognized, query.TVMediaTitle = state.InitialRecognized, state.InitialMediaTitle
			rss := &RSSHandler{subscription: handler, mediaRecognizer: &rssMediaRecognizerFake{}}
			if got := rss.rssTargetMediaTitle(t.Context(), refreshed, query); got != title || reader.freshCalls != 1 || reader.retryCalls != 1 {
				t.Fatalf("title=%q fresh/detail=%d/%d", got, reader.freshCalls, reader.retryCalls)
			}
		})
	}
}

func TestRecoveredTVRetainsTotalWhenOnlyEarlyEpisodeMissing(t *testing.T) {
	t.Parallel()
	target := database.Subscription{Season: intPointer(1), Config: map[string]any{"total_episode": 0, "start_episode": 1}}
	query := subscription.Query{MediaType: media.TypeTV, MissingEpisodes: v2TVTestEpisodes(1, 2), FullSeasonMissing: map[int]bool{}, TVRecoveredTarget: &subscription.TVRecoveredTarget{TotalEpisode: 12, MissingEpisodes: v2TVTestEpisodes(1, 2)}}
	needs, err := v2TVSeasonNeeds(target, query)
	if err != nil || len(needs) != 1 || needs[0].TotalEpisodes != 12 {
		t.Fatalf("needs=%+v err=%v", needs, err)
	}
}

func TestRecoveredTVProgressDoesNotHideStorageFailure(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name           string
		err, errorWant error
	}{
		{"storage", errors.New("storage unavailable"), ErrSubscriptionSearchStorage},
		{"version", database.ErrSubscriptionPreciseSearchStateConflict, ErrSubscriptionTargetChanged},
		{"cancel", context.Canceled, context.Canceled},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			target := database.Subscription{ID: 1, Username: "owner", InternalState: subscription.Active, UpdatedAt: time.Now().UTC()}
			store := &recoveredTVFlowStore{rssFlowStore: newRSSFlowStore(target), current: target, progressErr: tc.err}
			handler := &Handler{store: store}
			if err := handler.finishRecoveredTVTarget(t.Context(), target, 3); !errors.Is(err, tc.errorWant) {
				t.Fatalf("err=%v want=%v", err, tc.errorWant)
			}
		})
	}
}

func TestRecoveredTVStopsAfterAcceptedParentConfigurationChanges(t *testing.T) {
	t.Parallel()
	missing := v2TVTestEpisodes(1, 1, 2, 3)
	fixture := newV2TVExecutionFixture(t, subscription.PendingSearch, missing)
	fixture.target.Config["total_episode"] = 0
	fixture.target.UpdatedAt = time.Now().UTC()
	current := cloneV2TVTestTarget(fixture.target)
	fixture.store.activeTarget = &current
	fixture.query.TVRecoveredTarget = &subscription.TVRecoveredTarget{TotalEpisode: 3, MissingEpisodes: missing}
	fixture.execution.retainEpisodes = true
	fixture.execution.afterExecution = func(uuid.UUID, downloadflow.IntentExecutionResult) {
		current.Config["total_episode"] = 4
		current.UpdatedAt = current.UpdatedAt.Add(time.Second)
	}
	result, err := fixture.handler.executeV2TVCandidates(t.Context(), fixture.target, fixture.query,
		[]subscription.Candidate{v2TVExecutionCandidate("first", 1, 1), v2TVExecutionCandidate("second", 1, 2)}, []indexer.Adapter{fixture.adapter}, false)
	if err != nil || !result.Stopped || fixture.execution.calls != 1 || len(fixture.submission.calls) != 1 {
		t.Fatalf("result=%+v err=%v calls=%d", result, err, fixture.execution.calls)
	}
	if fixture.store.casCalls != 0 {
		t.Fatal("changed parent overwritten after stop")
	}
}

func recoveredTVNativeCases(t *testing.T) []recoveredTVNativeCase {
	t.Helper()
	payload, err := os.ReadFile("../../testdata/contracts/fixtures/r08/tv-recovered-target-v2.json")
	if err != nil {
		t.Fatal(err)
	}
	var corpus struct {
		Sources []struct {
			ContractPath string `json:"contract_path"`
			SHA256       string `json:"sha256"`
		}
		Cases []recoveredTVNativeCase
	}
	if err := json.Unmarshal(payload, &corpus); err != nil {
		t.Fatal(err)
	}
	for _, source := range corpus.Sources {
		payload, err := os.ReadFile(filepath.Join("..", "..", "testdata", "contracts", filepath.FromSlash(source.ContractPath)))
		if os.IsNotExist(err) {
			continue
		}
		if err != nil {
			t.Fatal(err)
		}
		digest := sha256.Sum256(payload)
		if hex.EncodeToString(digest[:]) != source.SHA256 {
			t.Fatalf("frozen source changed: %s", source.ContractPath)
		}
	}
	return corpus.Cases
}

type recoveredTVFlowStore struct {
	*rssFlowStore
	current        database.Subscription
	completed      bool
	progress       []int
	progressErr    error
	beforeProgress func()
}

type recoveredTVSubmission struct{ *v2TVTestSubmission }

func (s *recoveredTVSubmission) SubmitRSSSubscriptionEpisodes(context.Context, string, int64, subscription.State, downloader.Intent, database.DownloadIntentPlan, organization.SuccessInput, []subscription.Episode) (uuid.UUID, error) {
	return uuid.Nil, fmt.Errorf("unexpected async RSS")
}
func (s *recoveredTVSubmission) SubmitRSSTargetEpisodes(context.Context, string, int64, subscription.State, time.Time, downloader.Intent, database.DownloadIntentPlan, organization.SuccessInput, []subscription.Episode) (uuid.UUID, error) {
	return uuid.Nil, fmt.Errorf("unexpected async RSS target")
}

func (s *recoveredTVFlowStore) GetActiveSubscription(context.Context, string, int64) (database.Subscription, error) {
	if s.completed {
		return database.Subscription{}, database.ErrSubscriptionNotFound
	}
	return s.current, nil
}

func (s *recoveredTVFlowStore) ReconcileSubscriptionTarget(ctx context.Context, input database.SubscriptionTargetReconcile) (time.Time, error) {
	version, err := s.subscriptionFlowStore.ReconcileSubscriptionTarget(ctx, input)
	if err == nil {
		s.current.Config = applyTVTargetSnapshot(s.current.Config, input.Target.Snapshot)
		s.current.UpdatedAt = version.Add(time.Second)
	}
	return s.current.UpdatedAt, err
}

func (s *recoveredTVFlowStore) RecordRecoveredTVSubscriptionProgress(_ context.Context, owner string, id int64, state subscription.State, version time.Time, lack int) error {
	if s.beforeProgress != nil {
		s.beforeProgress()
	}
	if owner != s.current.Username || id != s.current.ID || !version.Equal(s.current.UpdatedAt) || state != s.current.InternalState {
		return database.ErrSubscriptionPreciseSearchStateConflict
	}
	s.progress = append(s.progress, lack)
	if s.progressErr != nil {
		return s.progressErr
	}
	s.current.Config["lack_episode"] = lack
	if state != subscription.PendingIdentification {
		s.completed = lack == 0
		s.current.InternalState = subscription.Active
		s.current.V2State = "R"
	}
	return nil
}

// The storage boundary consumes a frozen accepted plan and returns its next
// observation version. Database tests separately exercise the production
// settlement and its SQL transaction; this fixture never edits history rows.
func (s *recoveredTVFlowStore) accepted(plan database.DownloadIntentPlan) {
	s.current.Config["lack_episode"] = len(plan.TVRecoveredTarget.MissingEpisodes) - len(plan.TVRecoveredTarget.ExpectedCoverage)
	s.current.UpdatedAt = s.current.UpdatedAt.Add(time.Second)
	if s.current.InternalState != subscription.PendingIdentification {
		s.completed = s.current.Config["lack_episode"].(int) == 0
		s.current.InternalState = subscription.PendingSearch
		s.current.V2State = "N"
	}
}

func TestRecoveredTVTargetMatchesFrozenSearchAndRSS(t *testing.T) {
	t.Parallel()
	covered := 0
	for _, tc := range recoveredTVNativeCases(t) {
		// Completion-event veto has a separate event migration boundary.
		if tc.Input.Cancel {
			continue
		}
		covered++
		t.Run(tc.Name, func(t *testing.T) {
			t.Parallel()
			state := subscription.PendingSearch
			if tc.Input.State == "R" {
				state = subscription.Active
			}
			if tc.Input.State == "P" {
				state = subscription.PendingIdentification
			}
			target := database.Subscription{ID: 123, Username: "owner", Name: "Fixture TV", Year: "2026", Season: intPointer(1),
				MediaType: string(media.TypeTV), MediaSource: "themoviedb", MediaID: "123", InternalState: state, V2State: tc.Input.State,
				UpdatedAt: time.Date(2026, 10, 4, 0, 0, 0, 0, time.UTC), Config: map[string]any{
					"total_episode": tc.Input.Total, "start_episode": tc.Input.Start, "manual_total_episode": tc.Input.Manual,
					"note": tc.Input.Note, "episode_priority": tc.Input.Priorities, "current_priority": tc.Input.Priority, "lack_episode": tc.Input.Lack,
				}}
			store := &recoveredTVFlowStore{rssFlowStore: newRSSFlowStore(target), current: target}
			// Old download history and unrelated missing coordinates must not
			// substitute for the zero-total V2 writer's untouched note.
			oldIntent := uuid.New()
			store.episodes = []subscription.SubscriptionEpisode{{Season: 1, Episode: 1, State: subscription.SubscriptionEpisodeIndexed, DownloadIntentID: &oldIntent},
				{Season: 9, Episode: 9, State: subscription.SubscriptionEpisodeMissing}}
			oldRows := append([]subscription.SubscriptionEpisode{}, store.episodes...)
			reader := &tvMetadataBranchReader{initial: nativeMetadataDetail(tc.Input.Initial), retry: nativeMetadataDetail(tc.Input.Retry)}
			library := &existingEpisodesSourceFake{}
			for season, episodes := range tc.Input.Library {
				number, _ := strconv.Atoi(season)
				library.episodes = append(library.episodes, v2TVTestEpisodes(number, episodes...)...)
			}
			adapter := &tvMetadataRSSAdapter{v2TVTestAdapter: &v2TVTestAdapter{id: 7, bodies: map[string][]byte{}}}
			resources := []indexer.Resource{}
			success := map[string]bool{}
			for i, candidate := range tc.Input.Candidates {
				id := fmt.Sprintf("resource-%d", i)
				title := "Fixture.TV.S01"
				if len(candidate.Meta) > 0 {
					title += fmt.Sprintf("E%02d", candidate.Meta[0])
					if len(candidate.Meta) > 1 {
						title += fmt.Sprintf("-E%02d", candidate.Meta[len(candidate.Meta)-1])
					}
				}
				resources = append(resources, indexer.Resource{SiteID: 7, Site: "Fixture", Title: title + ".2026.1080p.WEB-DL", DownloadID: id, TorrentID: id, PriOrder: candidate.Priority})
				names := []string{}
				files := candidate.Files
				if len(files) == 0 {
					files = candidate.Meta
				}
				for _, ep := range files {
					names = append(names, fmt.Sprintf("Fixture.TV.S01E%02d.mkv", ep))
				}
				adapter.bodies[id] = v2TVTestMetainfo(t, names...)
				success[id] = candidate.Success
			}
			submission := &recoveredTVSubmission{v2TVTestSubmission: &v2TVTestSubmission{staged: map[uuid.UUID][]subscription.Episode{}}}
			execution := &v2TVTestExecution{store: store.subscriptionFlowStore, submission: submission.v2TVTestSubmission, retainEpisodes: true}
			for _, candidate := range tc.Input.Candidates {
				if !candidate.Success {
					execution.results = append(execution.results, downloadflow.IntentExecutionResult{Outcome: downloadflow.IntentExecutionDefinitiveFailureRecovered})
				}
			}
			execution.afterExecution = func(id uuid.UUID, result downloadflow.IntentExecutionResult) {
				if result.Outcome == downloadflow.IntentExecutionAccepted {
					store.accepted(submission.calls[len(submission.calls)-1].plan)
				} else {
					store.current.UpdatedAt = store.current.UpdatedAt.Add(time.Second)
				}
			}
			searcher := &subscriptionFlowSearcher{search: func(_ context.Context, _ []indexer.Adapter, _ indexer.Query, event func(indexer.Event)) ([]indexer.Resource, error) {
				event(indexer.Event{Resources: resources})
				return resources, nil
			}}
			handler, err := NewHandler(store, searcher, subscriptionFlowAdapters{adapter}, submission, execution)
			if err != nil {
				t.Fatal(err)
			}
			handler.SetMediaDetailResolver(reader)
			handler.SetExistingEpisodesSource(library)
			// Go's scheduled precise entry handles N; R/P source variants run
			// through RSS's shared preparation/selector using their legal state.
			if tc.Input.Entrypoint == "precise" && state == subscription.PendingSearch {
				err = handler.handleTarget(t.Context(), target)
			} else {
				rss, createErr := NewRSSHandler(store, handler, subscriptionFlowAdapters{adapter}, &rssFlowEnqueuer{})
				if createErr != nil {
					t.Fatal(createErr)
				}
				rss.SetMediaRecognizer(&rssMediaRecognizerFake{})
				_, err = rss.matchRSSTarget(t.Context(), rssSources([]indexer.Adapter{adapter}), map[int][]indexer.Resource{7: resources}, target)
			}
			if err != nil {
				t.Fatal(err)
			}
			// SuccessInput enrichment uses its existing Detail lookup per
			// staged candidate; it is independent of DownloadChain's retry.
			wantDetailCalls := tc.Expected.RetryCalls + len(submission.calls)
			if reader.freshCalls != 1 || reader.retryCalls != wantDetailCalls || library.calls != tc.Expected.LibraryCalls {
				t.Fatalf("fresh/detail/library=%d/%d/%d source+enrichment=1/%d/%d", reader.freshCalls, reader.retryCalls, library.calls, wantDetailCalls, tc.Expected.LibraryCalls)
			}
			acceptedCalls := []v2TVTestStageCall{}
			for _, call := range submission.calls {
				if success[call.plan.ResourceSecret] {
					acceptedCalls = append(acceptedCalls, call)
				}
			}
			if len(acceptedCalls) != len(tc.Expected.Downloads) {
				t.Fatalf("accepted=%d source=%d stages=%d", len(acceptedCalls), len(tc.Expected.Downloads), len(submission.calls))
			}
			for i, call := range acceptedCalls {
				want := v2TVTestEpisodes(1, tc.Expected.Downloads[i].Meta...)
				if call.plan.Paused {
					want = v2TVTestEpisodes(1, tc.Expected.Downloads[i].Selected...)
				}
				if !reflect.DeepEqual(call.plan.TVCoverage, want) {
					t.Fatalf("quality coverage=%v source=%v", call.plan.TVCoverage, want)
				}
				if call.plan.TVRecoveredTarget == nil || call.plan.TVRecoveredTarget.TotalEpisode != len(tc.Input.Retry["1"]) || !reflect.DeepEqual(call.plan.TVRecoveredTarget.ExpectedCoverage, call.episodes) {
					t.Fatalf("runtime plan=%+v expected=%v", call.plan.TVRecoveredTarget, call.episodes)
				}
				if call.expectedUpdatedAt.IsZero() {
					t.Fatal("lost target version")
				}
			}
			if got, _ := integerValue(store.current.Config["lack_episode"]); got != tc.Expected.Lack {
				t.Fatalf("lack=%d source=%d", got, tc.Expected.Lack)
			}
			if total, _ := integerValue(store.current.Config["total_episode"]); total != 0 {
				t.Fatalf("config total rewritten=%d", total)
			}
			if !reflect.DeepEqual(subscriptionNoteEpisodes(store.current.Config), subscriptionNoteEpisodes(map[string]any{"note": tc.Input.Note})) || !reflect.DeepEqual(jsonRoundTripV2TVTestConfig(t, map[string]any{"p": store.current.Config["episode_priority"]}), jsonRoundTripV2TVTestConfig(t, map[string]any{"p": tc.Input.Priorities})) || store.current.Config["current_priority"] != tc.Input.Priority {
				t.Fatal("zero-target writer changed download facts")
			}
			if store.completed != tc.Expected.Completed {
				t.Fatalf("completed=%v source=%v", store.completed, tc.Expected.Completed)
			}
			if !store.completed && tc.Expected.RemainingState != nil && store.current.V2State != *tc.Expected.RemainingState {
				t.Fatalf("state=%s source=%s", store.current.V2State, *tc.Expected.RemainingState)
			}
			if !reflect.DeepEqual(store.episodes, oldRows) {
				t.Fatal("changed immutable history")
			}
		})
	}
	if covered < 35 {
		t.Fatalf("too few native reachable cases=%d", covered)
	}
	t.Logf("verified %d frozen cases through search/RSS", covered)
}
