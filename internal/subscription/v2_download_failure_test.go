package subscription

import (
	"testing"
	"time"

	"github.com/moviepilot-go/moviepilot-go/internal/indexer"
	"github.com/moviepilot-go/moviepilot-go/internal/media"
)

func TestNewV2DownloadFailureMatchesPythonFingerprint(t *testing.T) {
	t.Parallel()

	season := 1
	candidate := Candidate{
		Resource: indexer.Resource{
			SiteID: 7, Site: "Fixture", Title: "Example.Show.S01E01-E03",
			TorrentID: "456", DownloadID: "private-locator", SizeBytes: 1024,
		},
		Parsed: media.Parse("Example.Show.S01E01-E03", media.Options{}),
	}
	got, ok := NewV2DownloadFailure(
		V2DownloadFailureTarget{
			MediaType: string(media.TypeTV), Title: "Example Show", Year: "2026",
			MediaSource: "tmdb", MediaID: "123", Season: &season,
		},
		candidate, nil, "qb", "Subscribe|fixture", "下载种子内容为空",
	)
	if !ok {
		t.Fatal("NewV2DownloadFailure() rejected valid fixture")
	}
	const wantFingerprint = "49aac6f742f8f42d5b24aade533ce1657d0ceafc8925f49b98abf90fe162e1a3"
	if got.Fingerprint != wantFingerprint || got.Season != "S01" || got.Episodes != "E01-E03" ||
		got.ResourceKey != "456" || got.SiteID != 7 {
		t.Fatalf("NewV2DownloadFailure() = %#v", got)
	}
}

func TestNewV2DownloadFailureStoresSelectedEpisodesWithoutChangingFingerprint(t *testing.T) {
	t.Parallel()

	candidate := Candidate{
		Resource: indexer.Resource{
			Site: "Fixture", Title: "Example.Show.S01", SizeBytes: 2048,
			DetailURL:  "https://tracker.example/details.php?id=88&passkey=private",
			DownloadID: "aWQ9ODgKZG93bmhhc2g9cHJpdmF0ZQ",
		},
		Parsed: media.Parse("Example.Show.S01", media.Options{}),
	}
	got, ok := NewV2DownloadFailure(
		V2DownloadFailureTarget{MediaType: string(media.TypeTV), Title: "Example", Year: "2026", IMDBID: "tt123"},
		candidate,
		[]Episode{{Season: 1, Episode: 4}, {Season: 1, Episode: 2}, {Season: 1, Episode: 3}},
		"", "", "temporary",
	)
	if !ok {
		t.Fatal("NewV2DownloadFailure() rejected URL fixture")
	}
	if got.Episodes != "E02-E04" || got.ResourceKey != "tracker.example:id=88" {
		t.Fatalf("failure projection = %#v", got)
	}
	if got.Fingerprint == "" || got.ErrorMessage != "temporary" {
		t.Fatalf("failure fact = %#v", got)
	}
	withoutSelection, ok := NewV2DownloadFailure(
		V2DownloadFailureTarget{MediaType: string(media.TypeTV), Title: "Example", Year: "2026", IMDBID: "tt123"},
		candidate, nil, "", "", "temporary",
	)
	if !ok || got.Fingerprint != withoutSelection.Fingerprint || withoutSelection.Episodes != "" {
		t.Fatalf("selected episodes changed fingerprint: selected=%#v parsed=%#v", got, withoutSelection)
	}
}

func TestNewV2DownloadFailureUsesPythonJSONEncoding(t *testing.T) {
	t.Parallel()

	season := 1
	candidate := Candidate{
		Resource: indexer.Resource{Site: "站点<&", Title: "剧集<&", TorrentID: "资源<&", DownloadID: "private"},
		Parsed:   media.Parse("剧集 S01E01", media.Options{}),
	}
	got, ok := NewV2DownloadFailure(
		V2DownloadFailureTarget{
			MediaType: string(media.TypeTV), Title: "剧集", MediaSource: "tmdb", MediaID: "42", Season: &season,
		},
		candidate, nil, "", "", "",
	)
	if !ok {
		t.Fatal("NewV2DownloadFailure() rejected Unicode fixture")
	}
	const wantFingerprint = "461fc6445d0c7423ed7fa1f91b9649b91621c16b3b6ff724f8ae188a416ef578"
	if got.Fingerprint != wantFingerprint {
		t.Fatalf("fingerprint = %q, want Python digest %q", got.Fingerprint, wantFingerprint)
	}
}

func TestNewV2DownloadFailureResolvesV2LegacyMediaIdentity(t *testing.T) {
	t.Parallel()

	candidate := Candidate{
		Resource: indexer.Resource{SiteID: 7, Title: "Show.S01E01", TorrentID: "42", DownloadID: "private"},
		Parsed:   media.Parse("Show.S01E01", media.Options{}),
	}
	tests := []struct {
		name   string
		target V2DownloadFailureTarget
		want   string
	}{
		{
			name:   "tmdb alias is canonical",
			target: V2DownloadFailureTarget{MediaType: string(media.TypeTV), MediaSource: "TMDB", MediaID: "123"},
			want:   "fc4f182a20758a338c89f766fc43e5037ca9e8605ee2dfddc05b9b9416ce7d4e",
		},
		{
			name:   "legacy provider precedes imdb",
			target: V2DownloadFailureTarget{MediaType: string(media.TypeTV), TMDBID: "123", IMDBID: "tt123"},
			want:   "fc4f182a20758a338c89f766fc43e5037ca9e8605ee2dfddc05b9b9416ce7d4e",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got, ok := NewV2DownloadFailure(tt.target, candidate, nil, "", "", "")
			if !ok || got.Fingerprint != tt.want {
				t.Fatalf("failure = %#v, want fingerprint %q", got, tt.want)
			}
		})
	}
}

func TestV2DownloadFailureTTL(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		err  string
		want time.Duration
	}{
		{name: "empty torrent content", err: "下载种子内容为空", want: 24 * time.Hour},
		{name: "case insensitive not found", err: "TORRENT NOT FOUND", want: 24 * time.Hour},
		{name: "http 404", err: "upstream returned 404", want: 24 * time.Hour},
		{name: "downloader rejection", err: "qBittorrent 拒绝了下载任务", want: time.Hour},
		{name: "empty error", want: time.Hour},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			if got := V2DownloadFailureTTL(test.err); got != test.want {
				t.Fatalf("V2DownloadFailureTTL(%q) = %s, want %s", test.err, got, test.want)
			}
		})
	}
}
