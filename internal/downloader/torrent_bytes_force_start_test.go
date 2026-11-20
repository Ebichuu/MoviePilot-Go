package downloader

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/require"
)

type resumeRecordingTorrentBytesClient struct {
	*scriptedTorrentBytesClient
	resumeCalls int
	resumeHash  string
	resumeErr   error
}

func (c *resumeRecordingTorrentBytesClient) ResumeAddedTorrent(_ context.Context, torrentHash string) error {
	c.resumeCalls++
	c.resumeHash = torrentHash
	return c.resumeErr
}

// TestTorrentBytesSubmitterResumeAfterFreshAdd covers V2
// `app/modules/qbittorrent/__init__.py:275–278`: only a freshly added torrent
// that is not paused for episode selection is resumed, an add that merely matched
// an existing torrent returns early in V2, and a client without the capability
// stays untouched.
func TestTorrentBytesSubmitterResumeAfterFreshAdd(t *testing.T) {
	tests := []struct {
		name      string
		candidate Candidate
		addErr    error
		resumeErr error
		capable   bool
		wantCalls int
		wantErr   error
	}{
		{
			name:      "新增加且未暂停时恢复",
			candidate: Candidate{Key: "opaque-bytes-candidate", Paused: false},
			capable:   true,
			wantCalls: 1,
		},
		{
			name:      "暂停添加等待选集时不在 Add 阶段恢复",
			candidate: Candidate{Key: "opaque-bytes-candidate", Paused: true},
			capable:   true,
			wantCalls: 0,
		},
		{
			name: "带目标集的添加不在 Add 阶段恢复",
			candidate: Candidate{
				Key: "opaque-bytes-candidate", Paused: false,
				TargetEpisodes: []Episode{{Season: 1, Episode: 1}},
			},
			capable:   true,
			wantCalls: 0,
		},
		{
			name:      "已存在任务命中时不恢复",
			candidate: Candidate{Key: "opaque-bytes-candidate", Paused: false},
			addErr:    ErrDefinitiveAddFailure,
			capable:   true,
			wantCalls: 0,
		},
		{
			name:      "下载器不具备 force_resume 能力时保持原行为",
			candidate: Candidate{Key: "opaque-bytes-candidate", Paused: false},
			wantCalls: 0,
		},
		{
			name:      "恢复失败上抛稳定错误",
			candidate: Candidate{Key: "opaque-bytes-candidate", Paused: false},
			resumeErr: ErrEpisodeSelectionFailed,
			capable:   true,
			wantCalls: 1,
			wantErr:   ErrTorrentResumeFailed,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var submitter *TorrentBytesSubmitter
			var recorder *resumeRecordingTorrentBytesClient
			if tt.capable {
				recorder = &resumeRecordingTorrentBytesClient{
					scriptedTorrentBytesClient: &scriptedTorrentBytesClient{},
					resumeErr:                  tt.resumeErr,
				}
				submitter = NewTorrentBytesSubmitter(recorder)
			} else {
				submitter = NewTorrentBytesSubmitter(&scriptedTorrentBytesClient{})
			}

			err := submitter.resumeAfterFreshAdd(
				t.Context(), tt.candidate,
				Intent{TorrentHash: torrentBytesTestHash}, tt.addErr,
			)

			if !errors.Is(err, tt.wantErr) {
				t.Fatalf("resumeAfterFreshAdd() error = %v, want %v", err, tt.wantErr)
			}
			if recorder == nil {
				return
			}
			require.Equal(t, tt.wantCalls, recorder.resumeCalls)
			if tt.wantCalls == 1 {
				require.Equal(t, torrentBytesTestHash, recorder.resumeHash)
			}
		})
	}
}

func TestTorrentBytesSubmitterResumeAfterFreshAddPropagatesCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	client := &resumeRecordingTorrentBytesClient{
		scriptedTorrentBytesClient: &scriptedTorrentBytesClient{},
		resumeErr:                  errors.New("upstream cancelled"),
	}
	err := NewTorrentBytesSubmitter(client).resumeAfterFreshAdd(
		ctx, Candidate{Key: "opaque-bytes-candidate"},
		Intent{TorrentHash: torrentBytesTestHash}, nil,
	)
	require.ErrorIs(t, err, context.Canceled)
}

// V2 `app/modules/qbittorrent/qbittorrent.py:354–365` 在底层客户端捕获并记录
// setForceStart 的异常后继续：强制开始失败不能让已经确认新增的任务失败。
func TestQBittorrentResumeAddedTorrentIgnoresForceStartFailure(t *testing.T) {
	const hash = "ffffffffffffffffffffffffffffffffffffffff"
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/v2/app/webapiVersion":
			_, _ = io.WriteString(w, "2.11.0")
		case "/api/v2/torrents/setForceStart":
			http.Error(w, "boom", http.StatusInternalServerError)
		default:
			http.Error(w, "unexpected request", http.StatusNotFound)
		}
	}))
	t.Cleanup(server.Close)

	client := newTestQBittorrent(server)
	client.forceResume = true
	require.NoError(t, client.ResumeAddedTorrent(t.Context(), hash))
}

func TestQBittorrentResumeAddedTorrentPolicy(t *testing.T) {
	const hash = "ffffffffffffffffffffffffffffffffffffffff"
	var requests atomic.Int64
	var forceForm url.Values
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		switch r.URL.Path {
		case "/api/v2/app/webapiVersion":
			_, _ = io.WriteString(w, "2.11.0")
		case "/api/v2/torrents/setForceStart":
			_ = r.ParseForm()
			forceForm = r.Form
			w.WriteHeader(http.StatusOK)
		default:
			http.Error(w, "unexpected request", http.StatusNotFound)
		}
	}))
	t.Cleanup(server.Close)

	enabled := newTestQBittorrent(server)
	enabled.forceResume = true
	require.NoError(t, enabled.ResumeAddedTorrent(t.Context(), hash))
	require.NotNil(t, forceForm)
	require.Equal(t, strings.ToUpper(hash), strings.ToUpper(forceForm.Get("hashes")))
	require.Equal(t, "true", forceForm.Get("value"))

	beforeDisabled := requests.Load()
	disabled := newTestQBittorrent(server)
	require.NoError(t, disabled.ResumeAddedTorrent(t.Context(), hash))
	require.Equal(t, beforeDisabled, requests.Load(), "force_resume=false 时不得请求 setForceStart")

	require.ErrorIs(t, enabled.ResumeAddedTorrent(t.Context(), " "), ErrEpisodeSelectionFailed)
}
