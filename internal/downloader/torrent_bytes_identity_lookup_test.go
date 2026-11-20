package downloader

import (
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/moviepilot-go/moviepilot-go/internal/torrentmeta"
)

func TestTorrentBytesSubmitterSkipsIdentityLookupWithoutMetainfo(t *testing.T) {
	candidate := Candidate{Key: "opaque-identity-invalid"}
	intent := mustTorrentBytesIntent(t, "admin", candidate)
	intent, err := intent.Transition(IntentSubmitting)
	if err != nil {
		t.Fatalf("Transition() error = %v", err)
	}
	client := &identityAndResumeTorrentBytesClient{
		scriptedTorrentBytesClient: &scriptedTorrentBytesClient{
			labelResults: []bytesLookupResult{{}},
			addErrors:    []error{ErrDefinitiveAddFailure},
		},
		identityResults: []identityResult{{hash: torrentBytesTestHash, found: true}},
	}

	got, err := NewTorrentBytesSubmitter(client).AddOnce(t.Context(), intent, candidate, []byte("not-bencode"))
	if !errors.Is(err, ErrDefinitiveAddFailure) || got.State != IntentFailed {
		t.Fatalf("AddOnce() = %#v, %v, want failed/ErrDefinitiveAddFailure", got, err)
	}
	if client.identityCalls != 0 {
		t.Fatalf("不可解析 metainfo 不得触发名称与大小查询，got %d", client.identityCalls)
	}
}

// TestTorrentmetaIdentityProjection pins the name/total_size projection that the
// duplicate lookup consumes against V2 `torrent_from_file`.
func TestTorrentmetaIdentityProjection(t *testing.T) {
	inspection, err := torrentmeta.Inspect([]byte(identityMetainfo))
	require.NoError(t, err)
	require.Equal(t, identityMetaName, inspection.Name)
	require.Equal(t, identityMetaSize, inspection.TotalSize)
}

func TestQBittorrentFindByNameAndSize(t *testing.T) {
	const (
		hashA = "1111111111111111111111111111111111111111"
		hashB = "2222222222222222222222222222222222222222"
		hashC = "3333333333333333333333333333333333333333"
	)
	var requests atomic.Int64
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/v2/app/webapiVersion":
			_, _ = io.WriteString(w, "2.11.0")
		case "/api/v2/torrents/info":
			requests.Add(1)
			w.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(w, `[`+
				`{"hash":"`+hashA+`","name":"other","total_size":7},`+
				`{"hash":"not-a-hash","name":"fixture","total_size":7},`+
				`{"hash":"`+hashB+`","name":"fixture","total_size":7},`+
				`{"hash":"`+hashC+`","name":"fixture","total_size":8}]`)
		default:
			http.Error(w, "unexpected request", http.StatusNotFound)
		}
	}))
	t.Cleanup(server.Close)
	client := newTestQBittorrent(server)

	hash, found, err := client.FindByNameAndSize(t.Context(), identityMetaName, identityMetaSize)
	require.NoError(t, err)
	require.True(t, found)
	require.Equal(t, hashB, hash)

	_, found, err = client.FindByNameAndSize(t.Context(), identityMetaName, 99)
	require.NoError(t, err)
	require.False(t, found)

	before := requests.Load()
	_, _, err = client.FindByNameAndSize(t.Context(), "", identityMetaSize)
	require.Error(t, err)
	_, _, err = client.FindByNameAndSize(t.Context(), identityMetaName, -1)
	require.Error(t, err)
	require.Equal(t, before, requests.Load(), "非法输入不得发起查询")
}

func TestQBittorrentFindByNameAndSizeRedactsErrors(t *testing.T) {
	const responseSecret = "identity-response-secret"
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, responseSecret, http.StatusInternalServerError)
	}))
	t.Cleanup(server.Close)

	_, _, err := newTestQBittorrent(server).FindByNameAndSize(t.Context(), identityMetaName, identityMetaSize)
	require.Error(t, err)
	require.NotContains(t, err.Error(), responseSecret)
	require.NotContains(t, err.Error(), server.URL)
}
