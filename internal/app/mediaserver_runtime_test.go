package app

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/moviepilot-go/moviepilot-go/internal/httpapi"
	"github.com/moviepilot-go/moviepilot-go/internal/mediaconfig"
	"github.com/moviepilot-go/moviepilot-go/internal/mediaindex"
	"github.com/moviepilot-go/moviepilot-go/internal/organizer"
)

const (
	mediaServerRuntimeTestID          = "2a46de31-c7bc-42d3-b65f-5691e9478cf3"
	mediaServerRuntimeTestFingerprint = "f55c01449df2554326e01d51b3cf5a964f2f84fc8f1e52ee73a702ebf08f5695"
)

func TestMediaServerRuntimeMediaIndexCompletionPlan(t *testing.T) {
	t.Parallel()

	const (
		videoSHA = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
		nfoSHA   = "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
		imageSHA = "cccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccc"
	)
	tests := []struct {
		name       string
		servers    []mediaconfig.Server
		plan       organizer.Plan
		cancel     bool
		want       mediaindex.CompletionPlan
		wantErr    error
		wantAnyErr bool
	}{
		{
			name: "无 required 返回空计划",
			plan: organizer.Plan{Operations: []organizer.Operation{{
				Target: "/mnt/media/Movie.mkv", ExpectedSize: 1024, ExpectedSHA256: videoSHA,
			}}},
			want: mediaindex.CompletionPlan{},
		},
		{
			name: "只选择视频文件",
			servers: []mediaconfig.Server{mediaServerRuntimeTestServer([]mediaconfig.PathMapping{{
				LocalRoot: "/mnt/media", EmbyRoot: "/library",
			}})},
			plan: organizer.Plan{Operations: []organizer.Operation{
				{Target: "/mnt/media/Movie.MKV", ExpectedSize: 4096, ExpectedSHA256: videoSHA},
				{Target: "/mnt/media/Movie.nfo", ExpectedSize: 512, ExpectedSHA256: nfoSHA},
				{Target: "/mnt/media/poster.jpg", ExpectedSize: 256, ExpectedSHA256: imageSHA},
			}},
			want: mediaindex.CompletionPlan{
				RequiredServer: &mediaindex.ServerRef{
					ID: mediaServerRuntimeTestID, Name: "Fixture Emby", Type: "emby",
					Fingerprint: mediaServerRuntimeTestFingerprint,
				},
				Files: []mediaindex.TargetFile{{
					LocalPath: "/mnt/media/Movie.MKV", RemotePath: "/library/Movie.MKV",
					PathStyle: "posix", CaseSensitive: true,
					ExpectedSize: 4096, ExpectedSHA256: videoSHA, MediaType: "video",
				}},
			},
		},
		{
			name: "最长映射保留远端风格和大小写策略",
			servers: []mediaconfig.Server{mediaServerRuntimeTestServer([]mediaconfig.PathMapping{
				{LocalRoot: "/mnt/media", EmbyRoot: "/library"},
				{
					LocalRoot: "/mnt/media/movies", EmbyRoot: `Z:\Movies`,
					RemoteStyle: mediaconfig.PathStyleWindows, CasePolicy: mediaconfig.CasePolicyInsensitive,
				},
			})},
			plan: organizer.Plan{Operations: []organizer.Operation{{
				Target: "/mnt/media/movies/Film.mkv", ExpectedSize: 8192, ExpectedSHA256: videoSHA,
			}}},
			want: mediaindex.CompletionPlan{
				RequiredServer: &mediaindex.ServerRef{
					ID: mediaServerRuntimeTestID, Name: "Fixture Emby", Type: "emby",
					Fingerprint: mediaServerRuntimeTestFingerprint,
				},
				Files: []mediaindex.TargetFile{{
					LocalPath: "/mnt/media/movies/Film.mkv", RemotePath: `Z:\Movies\Film.mkv`,
					PathStyle: "windows", CaseSensitive: false,
					ExpectedSize: 8192, ExpectedSHA256: videoSHA, MediaType: "video",
				}},
			},
		},
		{
			name: "required 存在但目标无映射",
			servers: []mediaconfig.Server{mediaServerRuntimeTestServer([]mediaconfig.PathMapping{{
				LocalRoot: "/mnt/media", EmbyRoot: "/library",
			}})},
			plan: organizer.Plan{Operations: []organizer.Operation{{
				Target: "/other/Movie.mkv", ExpectedSize: 1024, ExpectedSHA256: videoSHA,
			}}},
			wantAnyErr: true,
		},
		{
			name: "取消 context",
			servers: []mediaconfig.Server{mediaServerRuntimeTestServer([]mediaconfig.PathMapping{{
				LocalRoot: "/mnt/media", EmbyRoot: "/library",
			}})},
			plan: organizer.Plan{Operations: []organizer.Operation{{
				Target: "/mnt/media/Movie.mkv", ExpectedSize: 1024, ExpectedSHA256: videoSHA,
			}}},
			cancel:  true,
			wantErr: context.Canceled,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			runtime := newMediaServerRuntimeCompletionPlanTest(t, tt.servers)
			ctx := context.Background()
			if tt.cancel {
				cancelled, cancel := context.WithCancel(ctx)
				cancel()
				ctx = cancelled
			}

			got, err := runtime.MediaIndexCompletionPlan(ctx, tt.plan)
			if tt.wantErr != nil {
				if !errors.Is(err, tt.wantErr) {
					t.Fatalf("MediaIndexCompletionPlan() error = %v, want %v", err, tt.wantErr)
				}
				return
			}
			if tt.wantAnyErr {
				if err == nil {
					t.Fatal("MediaIndexCompletionPlan() error = nil")
				}
				return
			}
			if err != nil {
				t.Fatalf("MediaIndexCompletionPlan() error = %v", err)
			}
			if !reflect.DeepEqual(got, tt.want) {
				t.Fatalf("MediaIndexCompletionPlan() = %#v, want %#v", got, tt.want)
			}
		})
	}
}

func TestMediaServerRuntimeResolveRequiresEndpointFingerprint(t *testing.T) {
	t.Parallel()
	runtime := newMediaServerRuntimeCompletionPlanTest(t, []mediaconfig.Server{mediaServerRuntimeTestServer(nil)})
	valid := mediaindex.ServerRef{
		ID: mediaServerRuntimeTestID, Name: "old display name", Type: "emby",
		Fingerprint: mediaServerRuntimeTestFingerprint,
	}
	if server, found := runtime.Resolve(valid); !found || server == nil {
		t.Fatal("Resolve() rejected matching endpoint identity")
	}
	tests := []struct {
		name   string
		mutate func(*mediaindex.ServerRef)
	}{
		{name: "different type", mutate: func(ref *mediaindex.ServerRef) { ref.Type = "plex" }},
		{name: "different endpoint", mutate: func(ref *mediaindex.ServerRef) { ref.Fingerprint = strings.Repeat("0", 64) }},
		{name: "missing fingerprint", mutate: func(ref *mediaindex.ServerRef) { ref.Fingerprint = "" }},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			ref := valid
			tt.mutate(&ref)
			if server, found := runtime.Resolve(ref); found || server != nil {
				t.Fatalf("Resolve(%#v) unexpectedly matched", ref)
			}
		})
	}
}

func TestMediaIndexEmbyServerMapsUnavailableHTTPStatus(t *testing.T) {
	t.Parallel()
	var calls int
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls++
		http.Error(w, "sensitive upstream response", http.StatusServiceUnavailable)
	}))
	t.Cleanup(server.Close)

	setting := mediaServerRuntimeTestServer(nil)
	setting.Config.Host = server.URL
	runtime := newMediaServerRuntimeCompletionPlanTest(t, []mediaconfig.Server{setting})
	ref := mediaindex.ServerRef{
		ID: setting.ID, Name: setting.Name, Type: setting.Type,
		Fingerprint: mediaServerEndpointFingerprint(setting),
	}
	resolved, found := runtime.Resolve(ref)
	if !found || resolved == nil {
		t.Fatal("Resolve() rejected the unavailable fixture endpoint")
	}

	err := resolved.RefreshAll(t.Context())
	if !errors.Is(err, mediaindex.ErrServerUnavailable) {
		t.Fatalf("RefreshAll() error = %v, want ErrServerUnavailable", err)
	}
	if strings.Contains(err.Error(), "sensitive upstream response") || strings.Contains(err.Error(), server.URL) {
		t.Fatalf("RefreshAll() leaked upstream details: %q", err)
	}
	if calls != 1 {
		t.Fatalf("RefreshAll() requests = %d, want 1", calls)
	}
}

func TestMediaServerEndpointFingerprintExcludesSecret(t *testing.T) {
	t.Parallel()
	server := mediaServerRuntimeTestServer(nil)
	if got := mediaServerEndpointFingerprint(server); got != mediaServerRuntimeTestFingerprint {
		t.Fatalf("mediaServerEndpointFingerprint() = %q", got)
	}
	changedSecret := server
	changedSecret.Config.APIKey = strings.Repeat("r", 16)
	if mediaServerEndpointFingerprint(changedSecret) != mediaServerRuntimeTestFingerprint {
		t.Fatal("API key rotation changed endpoint fingerprint")
	}
	changedHost := server
	changedHost.Config.Host = "https://other.example.test"
	if mediaServerEndpointFingerprint(changedHost) == mediaServerRuntimeTestFingerprint {
		t.Fatal("host change did not change endpoint fingerprint")
	}
}

func TestMediaServerRuntimeExistingEpisodesPaginatesAndDeduplicates(t *testing.T) {
	t.Parallel()
	starts := make([]string, 0, 2)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		query := r.URL.Query()
		starts = append(starts, query.Get("StartIndex"))
		if query.Get("Limit") != "200" || query.Get("Path") != "/library/Show" ||
			query.Get("AnyProviderIdEquals") != "tmdb.123" {
			t.Errorf("unexpected query: %v", query)
		}
		w.Header().Set("Content-Type", "application/json")
		switch query.Get("StartIndex") {
		case "":
			_, _ = io.WriteString(w, `{"Items":[{"Id":"1","Type":"Episode","ParentIndexNumber":1,"IndexNumber":1},{"Id":"3","Type":"Episode","ParentIndexNumber":1,"IndexNumber":3}],"TotalRecordCount":4,"StartIndex":0}`)
		case "2":
			_, _ = io.WriteString(w, `{"Items":[{"Id":"2","Type":"Episode","ParentIndexNumber":1,"IndexNumber":2},{"Id":"3-copy","Type":"Episode","ParentIndexNumber":1,"IndexNumber":3}],"TotalRecordCount":4,"StartIndex":2}`)
		default:
			http.Error(w, "unexpected page", http.StatusBadRequest)
		}
	}))
	t.Cleanup(server.Close)

	setting := mediaServerRuntimeTestServer(nil)
	setting.Config.Host = server.URL
	runtime := newMediaServerRuntimeCompletionPlanTest(t, []mediaconfig.Server{setting})
	got, err := runtime.ExistingEpisodes(t.Context(), httpapi.RemoteMediaInfo{
		Path: "/library/Show", TMDBID: 123,
	})
	if err != nil {
		t.Fatalf("ExistingEpisodes() error = %v", err)
	}
	want := map[int][]int{1: {1, 2, 3}}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("ExistingEpisodes() = %#v, want %#v", got, want)
	}
	if !reflect.DeepEqual(starts, []string{"", "2"}) {
		t.Fatalf("StartIndex sequence = %#v", starts)
	}
}

func mediaServerRuntimeTestServer(mappings []mediaconfig.PathMapping) mediaconfig.Server {
	return mediaconfig.Server{
		ID: mediaServerRuntimeTestID, Name: "Fixture Emby", Type: "emby", Enabled: true,
		PathMapping: mappings,
		Config:      mediaconfig.Config{Host: "https://emby.example.test", APIKey: "fixture-token"},
	}
}

func newMediaServerRuntimeCompletionPlanTest(
	t *testing.T,
	servers []mediaconfig.Server,
) *mediaServerRuntime {
	t.Helper()

	snapshot, _, err := mediaconfig.Normalize(servers)
	if err != nil {
		t.Fatalf("mediaconfig.Normalize() error = %v", err)
	}
	if err := mediaconfig.Validate(snapshot.Servers); err != nil {
		t.Fatalf("mediaconfig.Validate() error = %v", err)
	}
	runtime, err := buildMediaServerRuntime(
		snapshot,
		time.Second,
		slog.New(slog.NewTextHandler(io.Discard, nil)),
	)
	if err != nil {
		t.Fatalf("buildMediaServerRuntime() error = %v", err)
	}
	t.Cleanup(runtime.CloseIdleConnections)
	return runtime
}
