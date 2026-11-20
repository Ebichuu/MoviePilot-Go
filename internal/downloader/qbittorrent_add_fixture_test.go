package downloader

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

type qbAddFixtureCase struct {
	Name  string `json:"name"`
	Input struct {
		Episodes    []int   `json:"episodes"`
		ForceResume bool    `json:"force_resume"`
		RaiseAt     *string `json:"raise_at"`
	} `json:"input"`
	Expected struct {
		Calls []struct {
			Method string `json:"method"`
			Hash   string `json:"hash"`
		} `json:"calls"`
	} `json:"expected"`
}

// TestQBAddPathMatchesFrozenEmptyTargetCases drives the frozen
// `empty_targets_skip_file_query` and `empty_targets_force_start` cases through
// the Go Add path. In V2 both cases have no target episodes, so they skip file
// selection and land in `app/modules/qbittorrent/__init__.py:275–278`: force
// start only when `force_resume` is enabled, and no plain start otherwise.
func TestQBAddPathMatchesFrozenEmptyTargetCases(t *testing.T) {
	const fixtureHash = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	payload, err := os.ReadFile("../../testdata/contracts/fixtures/r08/qb-file-selection-v2.json")
	require.NoError(t, err)
	var fixture struct {
		Cases []qbAddFixtureCase `json:"cases"`
	}
	require.NoError(t, json.Unmarshal(payload, &fixture))

	compared := 0
	for _, tc := range fixture.Cases {
		if len(tc.Input.Episodes) != 0 || tc.Input.RaiseAt != nil {
			continue
		}
		compared++
		t.Run(tc.Name, func(t *testing.T) {
			var forceCalls int
			var forceForm url.Values
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				switch r.URL.Path {
				case "/api/v2/app/webapiVersion":
					_, _ = io.WriteString(w, "2.11.0")
				case "/api/v2/torrents/add":
					w.Header().Set("Content-Type", "text/plain")
					_, _ = io.WriteString(w, "Ok.")
				case "/api/v2/torrents/info":
					label := r.URL.Query().Get("tag")
					w.Header().Set("Content-Type", "application/json")
					_, _ = io.WriteString(w, `[{"hash":"`+strings.ToLower(fixtureHash)+
						`","name":"fixture","total_size":1,"progress":1,"state":"uploading","tags":"`+label+`"}]`)
				case "/api/v2/torrents/setForceStart":
					forceCalls++
					_ = r.ParseForm()
					forceForm = r.Form
					w.WriteHeader(http.StatusOK)
				default:
					t.Errorf("unexpected request: %s", r.URL.Path)
					w.WriteHeader(http.StatusNotFound)
				}
			}))
			t.Cleanup(server.Close)

			candidate := Candidate{Key: "qb-add-fixture", SavePath: "/fixture/downloads"}
			intent := mustTorrentBytesIntent(t, "qb-add-fixture", candidate)
			intent, err := intent.Transition(IntentSubmitting)
			require.NoError(t, err)
			client := newTestQBittorrent(server)
			client.forceResume = tc.Input.ForceResume

			updated, err := NewTorrentBytesSubmitter(client).AddOnce(
				t.Context(), intent, candidate, []byte(identityMetainfo),
			)
			require.NoError(t, err)
			require.Equal(t, IntentDownloading, updated.State)
			require.Equal(t, strings.ToUpper(fixtureHash), updated.TorrentHash)

			wantForce := 0
			for _, call := range tc.Expected.Calls {
				if call.Method == "torrents_set_force_start" {
					wantForce++
				}
			}
			require.Equal(t, wantForce, forceCalls)
			if wantForce == 1 {
				require.NotNil(t, forceForm)
				require.Equal(t, strings.ToUpper(fixtureHash), strings.ToUpper(forceForm.Get("hashes")))
				require.Equal(t, "true", forceForm.Get("value"))
			}
		})
	}
	require.Equal(t, 2, compared)
}
