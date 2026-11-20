package downloadflow

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"slices"
	"testing"

	"github.com/moviepilot-go/moviepilot-go/internal/downloader"
	"github.com/moviepilot-go/moviepilot-go/internal/jobs"
)

func TestQBNoMatchResumesBeforeHandlerAcceptance(t *testing.T) {
	t.Parallel()
	for _, state := range []downloader.IntentState{downloader.IntentPlanned, downloader.IntentSubmitting, downloader.IntentDownloading} {
		for _, batch := range []bool{false, true} {
			name := string(state) + "/worker"
			if batch {
				name = string(state) + "/batch"
			}
			t.Run(name, func(t *testing.T) {
				t.Parallel()
				fixture, store := newHandlerFileSelectionPolicyFixture(t, state)
				store.episodes = []downloader.Episode{{Season: 1, Episode: 1}}
				coverage := slices.Clone(store.input.Episodes)
				addEvent := func(event string) {
					fixture.store.mu.Lock()
					defer fixture.store.mu.Unlock()
					fixture.store.events = append(fixture.store.events, event)
				}
				submitter := &qbNoMatchHandlerSubmitter{handlerTestSubmitter: fixture.submitter, selector: newQBNoMatchSelector(t, addEvent)}
				fixture.handler = newHandlerTestHandler(t, store, fixture.resolver, submitter)
				if batch {
					got, err := fixture.handler.ExecuteIntent(t.Context(), fixture.args.Username, fixture.args.IntentID)
					if err != nil || got.Outcome != IntentExecutionAccepted {
						t.Fatalf("batch result=%+v error=%v", got, err)
					}
				} else {
					got, err := fixture.handler.HandleDownload(t.Context(), fixture.args)
					if err != nil || got.Action != jobs.DownloadActionComplete {
						t.Fatalf("worker result=%+v error=%v", got, err)
					}
				}
				events := fixture.store.eventsSnapshot()
				start, accept := slices.Index(events, "qb:start"), slices.Index(events, "accept")
				if start < 0 || accept <= start || slices.Contains(events, "qb:priority") || len(store.acceptanceSnapshot()) != 1 {
					t.Fatalf("resume/accept ordering=%v", events)
				}
				if !slices.Equal(store.input.Episodes, coverage) {
					t.Fatal("file selection changed captured quality/notification episodes")
				}
				wantAdds := int32(0)
				if state == downloader.IntentPlanned {
					wantAdds = 1
				}
				if fixture.submitter.addCalls.Load() != wantAdds {
					t.Fatalf("AddOnce=%d want=%d", fixture.submitter.addCalls.Load(), wantAdds)
				}
			})
		}
	}
}

func TestQBNoMatchResumesBeforePeriodicAcceptance(t *testing.T) {
	t.Parallel()
	for _, state := range []downloader.IntentState{downloader.IntentSubmitting, downloader.IntentDownloading} {
		for _, observed := range []downloader.TaskObservationState{downloader.TaskObservationActive, downloader.TaskObservationPaused, downloader.TaskObservationCompleted} {
			t.Run(string(state)+"/"+string(observed), func(t *testing.T) {
				t.Parallel()
				store, target, cutoff := newReconciliationFileSelectionPolicyFixture(state)
				store.episodes = []downloader.Episode{{Season: 1, Episode: 1}}
				coverage := slices.Clone(store.input.Episodes)
				addEvent := func(event string) {
					store.mu.Lock()
					defer store.mu.Unlock()
					store.events = append(store.events, event)
				}
				observer := &reconciliationTestSubmitter{states: map[string]downloader.TaskObservationState{target.TorrentHash: observed}}
				submitter := &qbNoMatchReconciliationSubmitter{reconciliationTestSubmitter: observer, selector: newQBNoMatchSelector(t, addEvent)}
				handler := newReconciliationTestHandler(t, store, map[string]TorrentSubmitter{target.DownloaderID: submitter}, &reconciliationCompletionHookFake{})
				if err := handler.HandleDownloadReconciliation(t.Context(), cutoff); err != nil {
					t.Fatal(err)
				}
				events := store.eventSnapshot()
				start, accept := slices.Index(events, "qb:start"), slices.Index(events, "accept")
				if start < 0 || accept <= start || slices.Contains(events, "qb:priority") || len(store.acceptanceSnapshot()) != 1 {
					t.Fatalf("resume/accept ordering=%v", events)
				}
				if !slices.Equal(store.input.Episodes, coverage) {
					t.Fatal("periodic selection changed captured quality/notification episodes")
				}
			})
		}
	}
}

type qbNoMatchHandlerSubmitter struct {
	*handlerTestSubmitter
	selector *downloader.TorrentBytesSubmitter
}

func (s *qbNoMatchHandlerSubmitter) SelectEpisodeFiles(ctx context.Context, intent downloader.Intent, episodes []downloader.Episode) error {
	return s.selector.SelectEpisodeFiles(ctx, intent, episodes)
}

type qbNoMatchReconciliationSubmitter struct {
	*reconciliationTestSubmitter
	selector *downloader.TorrentBytesSubmitter
}

func (s *qbNoMatchReconciliationSubmitter) SelectEpisodeFiles(ctx context.Context, intent downloader.Intent, episodes []downloader.Episode) error {
	return s.selector.SelectEpisodeFiles(ctx, intent, episodes)
}

func newQBNoMatchSelector(t *testing.T, addEvent func(string)) *downloader.TorrentBytesSubmitter {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/v2/app/webapiVersion":
			_, _ = io.WriteString(w, "2.11.0")
		case "/api/v2/torrents/stop":
			addEvent("qb:stop")
			w.WriteHeader(http.StatusNoContent)
		case "/api/v2/torrents/files":
			addEvent("qb:files")
			w.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(w, `[{"index":0,"name":"Show.S01E02.mkv","priority":1},{"index":1,"name":"readme.txt","priority":1}]`)
		case "/api/v2/torrents/start":
			addEvent("qb:start")
			w.WriteHeader(http.StatusNoContent)
		case "/api/v2/torrents/filePrio":
			addEvent("qb:priority")
			w.WriteHeader(http.StatusOK)
		default:
			t.Errorf("unexpected qB request: %s %s", r.Method, r.URL.Path)
			http.Error(w, "unexpected request", http.StatusNotFound)
		}
	}))
	t.Cleanup(server.Close)
	client, err := downloader.NewQBittorrent(downloader.QBittorrentConfig{URL: server.URL, APIKey: "fixture-qb-no-match"})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(client.CloseIdleConnections)
	return downloader.NewTorrentBytesSubmitter(client)
}
