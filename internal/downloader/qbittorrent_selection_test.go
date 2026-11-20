package downloader

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestQBittorrentSelectEpisodeFiles(t *testing.T) {
	const hash = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	var requests []string
	var priorityForm url.Values
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests = append(requests, r.Method+" "+r.URL.Path)
		switch r.URL.Path {
		case "/api/v2/app/webapiVersion":
			_, _ = io.WriteString(w, "2.11.0")
		case "/api/v2/torrents/stop", "/api/v2/torrents/start":
			w.WriteHeader(http.StatusNoContent)
		case "/api/v2/torrents/files":
			if got := r.URL.Query().Get("hash"); got != strings.ToUpper(hash) {
				t.Errorf("files hash = %q, want %q", got, strings.ToUpper(hash))
			}
			w.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(w, `[{"index":0,"name":"Show.S01E01.mkv","priority":1},{"index":1,"name":"Show.S01E02.mkv","priority":1},{"index":2,"name":"Show.S01E01-E02.mkv","priority":1},{"index":3,"name":"Show.jpg","priority":1}]`)
		case "/api/v2/torrents/filePrio":
			_ = r.ParseForm()
			priorityForm = r.Form
			w.WriteHeader(http.StatusOK)
		default:
			http.Error(w, "unexpected request", http.StatusNotFound)
		}
	}))
	t.Cleanup(server.Close)

	q := newTestQBittorrent(server)
	err := q.SelectEpisodeFiles(context.Background(), hash, []Episode{{Season: 1, Episode: 1}})
	require.NoError(t, err)
	require.Equal(t, []string{
		"GET /api/v2/app/webapiVersion",
		"POST /api/v2/torrents/stop",
		"GET /api/v2/torrents/files",
		"POST /api/v2/torrents/filePrio",
		"POST /api/v2/torrents/start",
	}, requests)
	require.Equal(t, strings.ToUpper(hash), priorityForm.Get("hash"))
	require.Equal(t, "1|2|3", priorityForm.Get("id"))
	require.Equal(t, "0", priorityForm.Get("priority"))
}

// TestQBittorrentSelectEpisodeFilesForceResumeUsesForceStart covers V2's
// `is_force_resume()` branch: with force_resume the torrent is force started and
// plain start is never called, both with and without a target match.
func TestQBittorrentSelectEpisodeFilesForceResumeUsesForceStart(t *testing.T) {
	tests := []struct {
		name     string
		files    string
		episodes []Episode
		wantPrio bool
	}{
		{
			name:     "matched target disables non-target files then force starts",
			files:    `[{"index":0,"name":"Show.S01E01.mkv"},{"index":1,"name":"Show.S01E02.mkv"},{"index":2,"name":"Show.jpg"}]`,
			episodes: []Episode{{Season: 1, Episode: 1}},
			wantPrio: true,
		},
		{
			name:     "no match still force starts without priority changes",
			files:    `[{"index":0,"name":"Show.S01E02.mkv"}]`,
			episodes: []Episode{{Season: 1, Episode: 1}},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			const hash = "eeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeee"
			var forceForm url.Values
			var priorityCalled bool
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				switch r.URL.Path {
				case "/api/v2/app/webapiVersion":
					_, _ = io.WriteString(w, "2.11.0")
				case "/api/v2/torrents/stop":
					w.WriteHeader(http.StatusNoContent)
				case "/api/v2/torrents/files":
					w.Header().Set("Content-Type", "application/json")
					_, _ = io.WriteString(w, tt.files)
				case "/api/v2/torrents/filePrio":
					priorityCalled = true
					w.WriteHeader(http.StatusOK)
				case "/api/v2/torrents/setForceStart":
					_ = r.ParseForm()
					forceForm = r.Form
					w.WriteHeader(http.StatusOK)
				case "/api/v2/torrents/start":
					t.Error("force_resume must not call the plain start endpoint")
					w.WriteHeader(http.StatusNoContent)
				default:
					http.Error(w, "unexpected request", http.StatusNotFound)
				}
			}))
			t.Cleanup(server.Close)

			q := newTestQBittorrent(server)
			q.forceResume = true
			require.NoError(t, q.SelectEpisodeFiles(context.Background(), hash, tt.episodes))
			require.NotNil(t, forceForm, "force start was not called")
			require.Equal(t, strings.ToUpper(hash), strings.ToUpper(forceForm.Get("hashes")))
			require.Equal(t, "true", forceForm.Get("value"))
			require.Equal(t, tt.wantPrio, priorityCalled)
		})
	}
}

func TestQBittorrentSelectEpisodeFilesNoMatchResumesWithoutPriorityChanges(t *testing.T) {
	const hash = "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
	var resumed bool
	var priorityChanged bool
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/v2/app/webapiVersion":
			_, _ = io.WriteString(w, "2.11.0")
		case "/api/v2/torrents/stop":
			w.WriteHeader(http.StatusNoContent)
		case "/api/v2/torrents/files":
			w.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(w, `[{"index":0,"name":"Show.S01E02.mkv"}]`)
		case "/api/v2/torrents/start":
			resumed = true
			w.WriteHeader(http.StatusNoContent)
		case "/api/v2/torrents/filePrio":
			priorityChanged = true
			w.WriteHeader(http.StatusOK)
		default:
			http.Error(w, "unexpected request", http.StatusNotFound)
		}
	}))
	t.Cleanup(server.Close)

	q := newTestQBittorrent(server)
	err := q.SelectEpisodeFiles(context.Background(), hash, []Episode{{Season: 1, Episode: 1}})
	require.NoError(t, err)
	require.True(t, resumed)
	require.False(t, priorityChanged)
}

func TestQBittorrentSelectEpisodeFilesMapsMetadataPending(t *testing.T) {
	const hash = "cccccccccccccccccccccccccccccccccccccccc"
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/v2/app/webapiVersion":
			_, _ = io.WriteString(w, "2.11.0")
		case "/api/v2/torrents/stop":
			w.WriteHeader(http.StatusNoContent)
		case "/api/v2/torrents/files":
			w.WriteHeader(http.StatusConflict)
		default:
			http.Error(w, "unexpected request", http.StatusNotFound)
		}
	}))
	t.Cleanup(server.Close)

	q := newTestQBittorrent(server)
	err := q.SelectEpisodeFiles(context.Background(), hash, []Episode{{Season: 1, Episode: 1}})
	require.ErrorIs(t, err, ErrEpisodeSelectionPending)
}

func TestQBittorrentSelectEpisodeFilesEmptyTargetsDoesNotCallAPI(t *testing.T) {
	called := false
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		called = true
		http.Error(w, "unexpected request", http.StatusInternalServerError)
	}))
	t.Cleanup(server.Close)

	q := newTestQBittorrent(server)
	require.NoError(t, q.SelectEpisodeFiles(context.Background(), strings.Repeat("d", 40), nil))
	require.False(t, called)
}

func TestQBittorrentSelectEpisodeFilesRejectsInvalidFileIndexes(t *testing.T) {
	tests := []struct {
		name  string
		files string
	}{
		{
			name:  "负索引",
			files: `[{"index":-1,"name":"Show.S01E01.mkv"}]`,
		},
		{
			name:  "重复索引",
			files: `[{"index":1,"name":"Show.S01E01.mkv"},{"index":1,"name":"Show.S01E02.mkv"}]`,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var paths []string
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				paths = append(paths, r.Method+" "+r.URL.Path)
				switch r.URL.Path {
				case "/api/v2/app/webapiVersion":
					_, _ = io.WriteString(w, "2.11.0")
				case "/api/v2/torrents/stop":
					w.WriteHeader(http.StatusNoContent)
				case "/api/v2/torrents/files":
					w.Header().Set("Content-Type", "application/json")
					_, _ = io.WriteString(w, tt.files)
				case "/api/v2/torrents/start":
					t.Errorf("无效文件清单不应恢复 torrent")
					w.WriteHeader(http.StatusNoContent)
				default:
					http.Error(w, "unexpected request", http.StatusNotFound)
				}
			}))
			t.Cleanup(server.Close)

			q := newTestQBittorrent(server)
			err := q.SelectEpisodeFiles(t.Context(), strings.Repeat("e", 40), []Episode{{Season: 1, Episode: 1}})
			require.ErrorIs(t, err, ErrEpisodeSelectionFailed)
			require.NotContains(t, paths, "POST /api/v2/torrents/start")
		})
	}
}

func TestQBittorrentSelectEpisodeFilesRejectsTooManyFiles(t *testing.T) {
	var builder strings.Builder
	builder.WriteByte('[')
	for index := 0; index <= maxTaskContentFiles; index++ {
		if index > 0 {
			builder.WriteByte(',')
		}
		fmt.Fprintf(&builder, `{"index":%d,"name":"Show.S01E01.part%d.mkv"}`, index, index)
	}
	builder.WriteByte(']')

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/v2/app/webapiVersion":
			_, _ = io.WriteString(w, "2.11.0")
		case "/api/v2/torrents/stop":
			w.WriteHeader(http.StatusNoContent)
		case "/api/v2/torrents/files":
			w.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(w, builder.String())
		default:
			http.Error(w, "unexpected request", http.StatusNotFound)
		}
	}))
	t.Cleanup(server.Close)

	q := newTestQBittorrent(server)
	err := q.SelectEpisodeFiles(t.Context(), strings.Repeat("f", 40), []Episode{{Season: 1, Episode: 1}})
	require.ErrorIs(t, err, ErrEpisodeSelectionFailed)
}

func TestQBittorrentSelectEpisodeFilesContextCancellation(t *testing.T) {
	called := false
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		called = true
		http.Error(w, "unexpected request", http.StatusInternalServerError)
	}))
	t.Cleanup(server.Close)

	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	q := newTestQBittorrent(server)
	err := q.SelectEpisodeFiles(ctx, strings.Repeat("a", 40), []Episode{{Season: 1, Episode: 1}})
	require.ErrorIs(t, err, context.Canceled)
	require.False(t, called)
}

func TestQBittorrentSelectEpisodeFilesRedactsPriorityAndResumeErrors(t *testing.T) {
	const marker = "QBIT_SELECTION_SECRET_MARKER"
	tests := []struct {
		name          string
		files         string
		priorityError bool
	}{
		{
			name:          "filePrio 错误",
			files:         `[{"index":0,"name":"Show.S01E01.mkv"},{"index":1,"name":"Show.S01E02.mkv"}]`,
			priorityError: true,
		},
		{
			name:  "resume 错误",
			files: `[{"index":0,"name":"Show.S01E01.mkv"}]`,
		},
		{
			name:  "无匹配时 resume 错误",
			files: `[{"index":0,"name":"Show.S01E02.mkv"}]`,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				switch r.URL.Path {
				case "/api/v2/app/webapiVersion":
					_, _ = io.WriteString(w, "2.11.0")
				case "/api/v2/torrents/stop":
					w.WriteHeader(http.StatusNoContent)
				case "/api/v2/torrents/files":
					w.Header().Set("Content-Type", "application/json")
					_, _ = io.WriteString(w, tt.files)
				case "/api/v2/torrents/filePrio":
					if !tt.priorityError {
						http.Error(w, "unexpected priority request", http.StatusInternalServerError)
						return
					}
					w.WriteHeader(http.StatusInternalServerError)
					_, _ = io.WriteString(w, marker)
				case "/api/v2/torrents/start":
					w.WriteHeader(http.StatusInternalServerError)
					_, _ = io.WriteString(w, marker)
				default:
					http.Error(w, "unexpected request", http.StatusNotFound)
				}
			}))
			t.Cleanup(server.Close)

			q := newTestQBittorrent(server)
			err := q.SelectEpisodeFiles(t.Context(), strings.Repeat("b", 40), []Episode{{Season: 1, Episode: 1}})
			require.ErrorIs(t, err, ErrEpisodeSelectionFailed)
			require.NotContains(t, err.Error(), marker)
		})
	}
}

func TestTorrentFileMatchesEpisodesExtensionAndSpecialSeasonBoundary(t *testing.T) {
	targetRegular := map[Episode]struct{}{{Season: 1, Episode: 1}: {}}
	require.True(t, torrentFileMatchesEpisodes(`Show\Season 01\Show.S01E01.MKV`, targetRegular))

	// An explicit S00 is V2's special season and must not be folded into S01.
	targetSpecial := map[Episode]struct{}{{Season: 0, Episode: 1}: {}}
	require.True(t, torrentFileMatchesEpisodes("Show.S00E01.mkv", targetSpecial))
	require.False(t, torrentFileMatchesEpisodes("Show.S00E01.mkv", targetRegular))
}
