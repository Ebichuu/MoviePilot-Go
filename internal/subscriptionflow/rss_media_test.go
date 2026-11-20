package subscriptionflow

import (
	"context"
	"errors"
	"reflect"
	"testing"
	"time"

	"github.com/moviepilot-go/moviepilot-go/internal/database"
	"github.com/moviepilot-go/moviepilot-go/internal/indexer"
	"github.com/moviepilot-go/moviepilot-go/internal/media"
	"github.com/moviepilot-go/moviepilot-go/internal/metadata"
	"github.com/moviepilot-go/moviepilot-go/internal/subscription"
)

func TestRSSMediaRecognizerStoresCanonicalTitleAndBuildsV2Query(t *testing.T) {
	t.Parallel()
	recognizer := &rssMediaRecognizerFake{
		match: metadata.Match{Media: metadata.Media{Title: "Canonical Show"}},
		found: true,
	}
	handler := &RSSHandler{mediaRecognizer: recognizer}
	items := []indexer.RSSItem{{Resource: indexer.Resource{
		Title:    "Release.Name.S01E01.2026",
		Category: "电视剧",
	}}}
	if err := handler.recognizeRSSResources(t.Context(), items); err != nil {
		t.Fatalf("recognizeRSSResources() error = %v", err)
	}
	if items[0].Resource.RecognizedMediaTitle != "Canonical Show" {
		t.Fatalf("recognized title = %q, want Canonical Show", items[0].Resource.RecognizedMediaTitle)
	}

	// An already recognized item is not requested again.
	if err := handler.recognizeRSSResources(t.Context(), items); err != nil {
		t.Fatalf("second recognizeRSSResources() error = %v", err)
	}
	if len(recognizer.queries) != 1 {
		t.Fatalf("recognizer calls = %d, want 1 after title persistence", len(recognizer.queries))
	}
	want := metadata.Query{Keyword: "Release Name", Type: "tv", Year: 2026}
	if !reflect.DeepEqual(recognizer.queries[0], want) {
		t.Fatalf("recognizer query = %#v, want %#v", recognizer.queries[0], want)
	}
}

func TestRSSMediaRecognizerMutatesRSSItemsAndPreservesExistingTitle(t *testing.T) {
	t.Parallel()
	recognizer := &rssMediaRecognizerFake{
		match: metadata.Match{Media: metadata.Media{Title: "Fresh Canonical"}},
		found: true,
	}
	handler := &RSSHandler{mediaRecognizer: recognizer}
	items := []indexer.RSSItem{{Resource: indexer.Resource{
		Title:                "Release.S01E01",
		RecognizedMediaTitle: "Already Canonical",
	}}, {Resource: indexer.Resource{Title: "Fresh.S01E01"}}}
	if err := handler.recognizeRSSResources(t.Context(), items); err != nil {
		t.Fatalf("recognizeRSSResources() error = %v", err)
	}
	if items[0].Resource.RecognizedMediaTitle != "Already Canonical" ||
		items[1].Resource.RecognizedMediaTitle != "Fresh Canonical" {
		t.Fatalf("recognized titles = %#v", []string{
			items[0].Resource.RecognizedMediaTitle,
			items[1].Resource.RecognizedMediaTitle,
		})
	}
	if len(recognizer.queries) != 1 {
		t.Fatalf("recognizer calls = %d, want 1 for existing-title skip", len(recognizer.queries))
	}
}

func TestRSSMediaRecognizerIgnoresProviderFailureButPropagatesCancellation(t *testing.T) {
	t.Parallel()
	providerErr := errors.New("provider unavailable")
	handler := &RSSHandler{mediaRecognizer: &rssMediaRecognizerFake{err: providerErr}}
	items := []indexer.RSSItem{{Resource: indexer.Resource{Title: "Release"}}}
	if err := handler.recognizeRSSResources(t.Context(), items); err != nil {
		t.Fatalf("provider failure should be fail-open, got %v", err)
	}
	cancelled, cancel := context.WithCancel(t.Context())
	cancel()
	if err := handler.recognizeRSSResources(cancelled, items); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled recognition error = %v, want context.Canceled", err)
	}
}

func TestRefreshRSSCandidateMediaTitlesUsesCustomWordRecognition(t *testing.T) {
	t.Parallel()
	recognizer := &rssMediaRecognizerFake{
		match: metadata.Match{Media: metadata.Media{Title: "Custom Canonical"}},
		found: true,
	}
	handler := &RSSHandler{mediaRecognizer: recognizer}
	parsed := media.Parse("Custom.Show.S01E01", media.Options{CustomWords: []string{"Custom"}})
	parsed.AppliedWords = []string{"Custom"}
	candidates := []subscription.Candidate{{
		Resource: indexer.Resource{Title: "Custom.Show.S01E01"},
		Parsed:   parsed,
	}}
	if err := handler.refreshRSSCandidateMediaTitles(t.Context(), candidates); err != nil {
		t.Fatalf("refreshRSSCandidateMediaTitles() error = %v", err)
	}
	if candidates[0].MediaTitle != "Custom Canonical" ||
		candidates[0].Resource.RecognizedMediaTitle != "Custom Canonical" {
		t.Fatalf("candidate titles = (%q, %q)", candidates[0].MediaTitle, candidates[0].Resource.RecognizedMediaTitle)
	}
}

func TestRSSMediaRecognitionRetryPersistsBoundedCountAndTitle(t *testing.T) {
	t.Parallel()
	failedStore := &rssFlowStore{items: []database.RSSItem{{
		ID: 71, SiteID: 7, ItemKey: "sha256:retry-item", Title: "Release.S01E01",
		DownloadID: "download-71", MediaRecognizeFailCount: 1,
	}}}
	failingHandler := &RSSHandler{
		store:           failedStore,
		mediaRecognizer: &rssMediaRecognizerFake{err: errors.New("provider unavailable")},
	}
	resource := indexer.Resource{SiteID: 7, Title: "Release.S01E01", DownloadID: "download-71"}
	attempted := make(map[int64]struct{})
	if err := failingHandler.retryRSSMediaRecognition(t.Context(), failedStore.items[0], &resource, attempted); err != nil {
		t.Fatalf("first retry error = %v", err)
	}
	if failedStore.items[0].MediaRecognizeFailCount != 2 {
		t.Fatalf("first retry count = %d, want 2", failedStore.items[0].MediaRecognizeFailCount)
	}
	failedStore.items[0].MediaRecognizeFailCount = 2
	attempted = make(map[int64]struct{})
	if err := failingHandler.retryRSSMediaRecognition(t.Context(), failedStore.items[0], &resource, attempted); err != nil {
		t.Fatalf("second retry error = %v", err)
	}
	if failedStore.items[0].MediaRecognizeFailCount != 3 {
		t.Fatalf("second retry count = %d, want 3", failedStore.items[0].MediaRecognizeFailCount)
	}
	queries := len(failingHandler.mediaRecognizer.(*rssMediaRecognizerFake).queries)
	attempted = make(map[int64]struct{})
	if err := failingHandler.retryRSSMediaRecognition(t.Context(), failedStore.items[0], &resource, attempted); err != nil {
		t.Fatalf("third retry error = %v", err)
	}
	if got := len(failingHandler.mediaRecognizer.(*rssMediaRecognizerFake).queries); got != queries {
		t.Fatalf("count>=3 issued another recognition: before %d after %d", queries, got)
	}

	successStore := &rssFlowStore{items: []database.RSSItem{{
		ID: 72, SiteID: 7, ItemKey: "sha256:success-item", Title: "Release.S01E01",
		DownloadID: "download-72", MediaRecognizeFailCount: 2,
	}}}
	successHandler := &RSSHandler{
		store: successStore,
		mediaRecognizer: &rssMediaRecognizerFake{
			match: metadata.Match{Media: metadata.Media{Title: "Canonical Show"}}, found: true,
		},
	}
	successResource := indexer.Resource{SiteID: 7, Title: "Release.S01E01", DownloadID: "download-72"}
	if err := successHandler.retryRSSMediaRecognition(t.Context(), successStore.items[0], &successResource, make(map[int64]struct{})); err != nil {
		t.Fatalf("successful retry error = %v", err)
	}
	if successStore.items[0].MediaRecognizeFailCount != 0 || successStore.items[0].RecognizedMediaTitle != "Canonical Show" ||
		successResource.RecognizedMediaTitle != "Canonical Show" {
		t.Fatalf("successful retry state = item:%#v resource:%q", successStore.items[0], successResource.RecognizedMediaTitle)
	}
}

func TestRSSMatchRetriesCacheWithoutActiveTargets(t *testing.T) {
	t.Parallel()
	store := newRSSFlowStore(subscriptionFlowTarget())
	store.subscriptionFlowStore.list = func(context.Context, time.Time, time.Time, int64, int) ([]database.Subscription, error) {
		return nil, nil
	}
	store.items = []database.RSSItem{{
		ID: 91, SiteID: 7, ItemKey: "sha256:no-target-retry", Title: "Release.S01E01",
		DownloadID: "download-91", MediaRecognizeFailCount: 1,
		Resource: []byte(`{"title":"Release.S01E01"}`),
	}}
	adapter := rssFlowAdapter{subscriptionFlowAdapter: subscriptionFlowAdapter{id: 7, name: "rss", kind: "declarative-rss", version: 1}, key: "sha256:no-target-retry"}
	handler := &RSSHandler{
		store:           store,
		mediaRecognizer: &rssMediaRecognizerFake{err: errors.New("provider unavailable")},
	}
	if err := handler.matchRSSSubscriptions(t.Context(), time.Now().UTC(), rssSources([]indexer.Adapter{adapter})); err != nil {
		t.Fatal(err)
	}
	if store.items[0].MediaRecognizeFailCount != 2 {
		t.Fatalf("no-target retry count = %d, want 2", store.items[0].MediaRecognizeFailCount)
	}
}

type rssMediaRecognizerFake struct {
	match   metadata.Match
	found   bool
	err     error
	queries []metadata.Query
}

func (f *rssMediaRecognizerFake) Recognize(_ context.Context, query metadata.Query) (metadata.Match, bool, error) {
	f.queries = append(f.queries, query)
	return f.match, f.found, f.err
}
