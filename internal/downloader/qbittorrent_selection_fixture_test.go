package downloader

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestQBSelectionMatchesFrozenNormalResumeCalls(t *testing.T) {
	var fixture struct {
		Cases []struct {
			Name  string `json:"name"`
			Input struct {
				Episodes []int `json:"episodes"`
				Files    []struct {
					ID       int    `json:"id"`
					Name     string `json:"name"`
					Priority int    `json:"priority"`
				} `json:"files"`
				ForceResume    bool    `json:"force_resume"`
				SetFilesResult bool    `json:"set_files_result"`
				StartResult    bool    `json:"start_result"`
				RaiseAt        *string `json:"raise_at"`
			} `json:"input"`
			Expected struct {
				Calls []struct {
					Method  string `json:"method"`
					FileIDs []int  `json:"file_ids"`
				} `json:"calls"`
			} `json:"expected"`
		} `json:"cases"`
	}
	payload, err := os.ReadFile("../../testdata/contracts/fixtures/r08/qb-file-selection-v2.json")
	require.NoError(t, err)
	require.NoError(t, json.Unmarshal(payload, &fixture))
	compared := 0
	for _, tc := range fixture.Cases {
		// Force-start false-return, exception paths, and the empty-target Add
		// branch (V2 276–277) remain separate ports and stay excluded.
		if !tc.Input.SetFilesResult || !tc.Input.StartResult || tc.Input.RaiseAt != nil ||
			len(tc.Input.Episodes) == 0 {
			continue
		}
		compared++
		t.Run(tc.Name, func(t *testing.T) {
			var priorityIDs []int
			var effects []string
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				switch r.URL.Path {
				case "/api/v2/app/webapiVersion":
					_, _ = io.WriteString(w, "2.11.0")
				case "/api/v2/torrents/stop":
					w.WriteHeader(http.StatusNoContent)
				case "/api/v2/torrents/files":
					files := make([]map[string]any, 0, len(tc.Input.Files))
					for _, file := range tc.Input.Files {
						files = append(files, map[string]any{"index": file.ID, "name": file.Name, "priority": file.Priority})
					}
					w.Header().Set("Content-Type", "application/json")
					_ = json.NewEncoder(w).Encode(files)
				case "/api/v2/torrents/filePrio":
					effects = append(effects, "set_files")
					if err := r.ParseForm(); err != nil {
						t.Error(err)
					}
					for _, id := range strings.Split(r.Form.Get("id"), "|") {
						value, err := strconv.Atoi(id)
						if err != nil {
							t.Error(err)
						}
						priorityIDs = append(priorityIDs, value)
					}
					if r.Form.Get("priority") != "0" {
						t.Errorf("priority=%s", r.Form.Get("priority"))
					}
					w.WriteHeader(http.StatusOK)
				case "/api/v2/torrents/start":
					effects = append(effects, "start_torrents")
					w.WriteHeader(http.StatusNoContent)
				case "/api/v2/torrents/setForceStart":
					if err := r.ParseForm(); err != nil {
						t.Error(err)
					}
					if r.Form.Get("value") != "true" {
						t.Errorf("setForceStart value=%s", r.Form.Get("value"))
					}
					effects = append(effects, "torrents_set_force_start")
					w.WriteHeader(http.StatusOK)
				default:
					t.Errorf("unexpected request: %s", r.URL.Path)
					w.WriteHeader(http.StatusNotFound)
				}
			}))
			t.Cleanup(server.Close)
			var target []Episode
			for _, episode := range tc.Input.Episodes {
				target = append(target, Episode{Season: 1, Episode: episode})
			}
			client := newTestQBittorrent(server)
			client.forceResume = tc.Input.ForceResume
			err := client.SelectEpisodeFiles(t.Context(), strings.Repeat("a", 40), target)
			if len(target) > 0 && len(tc.Input.Files) == 0 {
				// V2 returns its hash without resuming an empty list. Go retains
				// its existing error; only the side-effect trace is compared here.
				require.ErrorIs(t, err, ErrEpisodeSelectionFailed)
			} else {
				require.NoError(t, err)
			}
			var expectedEffects []string
			var expectedIDs []int
			for _, call := range tc.Expected.Calls {
				if call.Method == "set_files" || call.Method == "start_torrents" ||
					call.Method == "torrents_set_force_start" {
					expectedEffects = append(expectedEffects, call.Method)
				}
				if call.Method == "set_files" {
					expectedIDs = call.FileIDs
				}
			}
			require.True(t, slices.Equal(expectedEffects, effects), "effects=%v source=%v", effects, expectedEffects)
			require.True(t, slices.Equal(expectedIDs, priorityIDs), "ids=%v source=%v", priorityIDs, expectedIDs)
		})
	}
	require.Equal(t, 10, compared)
}

func TestQBNoMatchCancellationDuringFileQueryDoesNotResume(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	t.Cleanup(cancel)
	var resumed bool
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/v2/app/webapiVersion":
			_, _ = io.WriteString(w, "2.11.0")
		case "/api/v2/torrents/stop":
			w.WriteHeader(http.StatusNoContent)
		case "/api/v2/torrents/files":
			cancel()
			w.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(w, `[{"index":0,"name":"Show.S01E02.mkv"}]`)
		case "/api/v2/torrents/start", "/api/v2/torrents/filePrio":
			resumed = true
			w.WriteHeader(http.StatusOK)
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(server.Close)
	err := newTestQBittorrent(server).SelectEpisodeFiles(ctx, strings.Repeat("a", 40), []Episode{{Season: 1, Episode: 1}})
	require.ErrorIs(t, err, context.Canceled)
	require.False(t, resumed)
}
