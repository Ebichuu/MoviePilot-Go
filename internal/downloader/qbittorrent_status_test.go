package downloader

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	qbittorrent "github.com/autobrr/go-qbittorrent"
)

func TestQBittorrentObserveTasksUsesOneBoundedRequestAndClosedStates(t *testing.T) {
	requested := []string{
		strings.Repeat("a", 40),
		strings.Repeat("b", 40),
		strings.Repeat("c", 40),
		strings.Repeat("d", 40),
		strings.Repeat("e", 40),
		strings.Repeat("f", 40),
		strings.Repeat("1", 40),
	}
	response := `[` +
		`{"hash":"` + requested[0] + `","progress":0.5,"state":"stalledDL"},` +
		`{"hash":"` + requested[1] + `","progress":0.4,"state":"pausedDL"},` +
		`{"hash":"` + requested[2] + `","progress":1,"state":"uploading"},` +
		`{"hash":"` + requested[3] + `","progress":0,"state":"error"},` +
		`{"hash":"` + requested[4] + `","progress":1,"state":"missingFiles"},` +
		`{"hash":"` + requested[5] + `","progress":1,"state":"futureState"}` +
		`]`
	requestCh := make(chan *http.Request, 1)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/v2/app/webapiVersion" {
			_, _ = io.WriteString(w, "5.0.0")
			return
		}
		requestCh <- r.Clone(context.Background())
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, response)
	}))
	t.Cleanup(server.Close)

	got, err := newTestQBittorrent(server).ObserveTasks(t.Context(), requested)
	if err != nil {
		t.Fatal(err)
	}
	wantStates := []TaskObservationState{
		TaskObservationActive,
		TaskObservationPaused,
		TaskObservationCompleted,
		TaskObservationError,
		TaskObservationMissing,
		TaskObservationUnknown,
		TaskObservationMissing,
	}
	for index, rawHash := range requested {
		hash := strings.ToUpper(rawHash)
		observation, ok := got[hash]
		if !ok || observation.Hash != hash || observation.State != wantStates[index] {
			t.Fatalf("observation[%s] = %#v, found %v, want %s", hash, observation, ok, wantStates[index])
		}
	}
	request := <-requestCh
	if request.URL.Path != "/api/v2/torrents/info" || request.URL.Query().Get("filter") != "all" {
		t.Fatalf("request = %s?%s", request.URL.Path, request.URL.RawQuery)
	}
	if request.URL.Query().Get("limit") != "7" {
		t.Fatalf("limit = %q", request.URL.Query().Get("limit"))
	}
	for _, rawHash := range requested {
		if !strings.Contains(strings.ToUpper(request.URL.Query().Get("hashes")), strings.ToUpper(rawHash)) {
			t.Fatalf("hashes query missing %s: %s", rawHash, request.URL.RawQuery)
		}
	}
}

func FuzzObserveQBTorrentStateNeverCompletesUnsafeStates(f *testing.F) {
	f.Add("fixture", 0.0)
	f.Add("future", 1.0)
	f.Add("mixed", -1.0)
	f.Fuzz(func(t *testing.T, suffix string, progress float64) {
		if got := observeQBTorrentState(qbittorrent.TorrentState("error"), progress); got != TaskObservationError {
			t.Fatalf("error state with progress %v = %s", progress, got)
		}
		if got := observeQBTorrentState(qbittorrent.TorrentState("missingFiles"), progress); got != TaskObservationMissing {
			t.Fatalf("missing state with progress %v = %s", progress, got)
		}
		unknown := qbittorrent.TorrentState("unknown-" + suffix)
		if got := observeQBTorrentState(unknown, progress); got != TaskObservationUnknown {
			t.Fatalf("unknown state %q with progress %v = %s", unknown, progress, got)
		}
	})
}

func TestQBittorrentObserveTasksValidatesBoundsAndCancellation(t *testing.T) {
	t.Parallel()

	client := &QBittorrent{}
	cancelled, cancel := context.WithCancel(context.Background())
	cancel()
	tests := []struct {
		name    string
		ctx     context.Context
		hashes  []string
		wantErr error
	}{
		{name: "空批次", ctx: t.Context(), hashes: nil},
		{name: "超过上限", ctx: t.Context(), hashes: make([]string, maxObservedTasks+1)},
		{name: "无效 Hash", ctx: t.Context(), hashes: []string{"not-a-hash"}},
		{name: "取消", ctx: cancelled, hashes: []string{strings.Repeat("a", 40)}, wantErr: context.Canceled},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			_, err := client.ObserveTasks(tt.ctx, tt.hashes)
			if err == nil {
				t.Fatal("ObserveTasks() error = nil")
			}
			if tt.wantErr != nil && !errors.Is(err, tt.wantErr) {
				t.Fatalf("ObserveTasks() error = %v, want %v", err, tt.wantErr)
			}
		})
	}
}
