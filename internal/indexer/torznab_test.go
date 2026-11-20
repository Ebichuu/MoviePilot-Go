package indexer

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestTorznabAdapterSearchCapsDownloadAndRSS(t *testing.T) {
	t.Parallel()

	const capsXML = `<?xml version="1.0" encoding="UTF-8"?>
<caps><server title="Prowlarr" /><searching><search available="yes" /></searching></caps>`

	const searchXML = `<?xml version="1.0" encoding="UTF-8"?>
<rss version="2.0" xmlns:torznab="http://torznab.com/schemas/2015/feed">
<channel><title>Prowlarr</title>
  <item>
    <title>Oppenheimer.2023.2160p.BluRay.REMUX</title>
    <guid>https://tracker.example/details/888</guid>
    <comments>https://tracker.example/details/888</comments>
    <pubDate>Fri, 20 Nov 2026 08:00:00 +0000</pubDate>
    <size>68719476736</size>
    <category>2000</category>
    <enclosure url="/1/download?id=888" length="68719476736" type="application/x-bittorrent" />
    <torznab:attr name="seeders" value="95" />
    <torznab:attr name="peers" value="110" />
    <torznab:attr name="grabs" value="430" />
    <torznab:attr name="imdb" value="15398776" />
    <torznab:attr name="downloadvolumefactor" value="0" />
    <torznab:attr name="uploadvolumefactor" value="2" />
    <torznab:attr name="tag" value="freeleech" />
  </item>
</channel></rss>`

	var gotAction, gotIMDb, gotCat string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/1/download" {
			_, _ = w.Write([]byte("d8:announce1:ye"))
			return
		}
		if r.URL.Query().Get("apikey") != "prowlarr-key" {
			w.Header().Set("Content-Type", "application/xml")
			_, _ = w.Write([]byte(`<error code="100" description="Incorrect user credentials" />`))
			return
		}
		gotAction = r.URL.Query().Get("t")
		gotIMDb = r.URL.Query().Get("imdbid")
		gotCat = r.URL.Query().Get("cat")
		w.Header().Set("Content-Type", "application/xml")
		switch gotAction {
		case "caps":
			_, _ = w.Write([]byte(capsXML))
		case "search":
			_, _ = w.Write([]byte(searchXML))
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	httpClient, err := NewHTTPClient(2, 5*time.Second, 1<<20)
	if err != nil {
		t.Fatalf("NewHTTPClient() error = %v", err)
	}
	httpClient.SetAllowPrivateHosts(true)

	adapterAny, err := NewAdapterFromConfig(httpClient, 9, "MyProwlarr", server.URL+"/1/api", "prowlarr", map[string]any{
		"apikey":     "prowlarr-key",
		"categories": "2000,5000",
	})
	if err != nil {
		t.Fatalf("NewAdapterFromConfig() error = %v", err)
	}
	adapter, ok := adapterAny.(*TorznabAdapter)
	if !ok {
		t.Fatalf("adapter type = %T, want *TorznabAdapter", adapterAny)
	}

	ctx := context.Background()
	if err := adapter.TestSite(ctx); err != nil {
		t.Fatalf("TestSite() error = %v", err)
	}

	resources, err := adapter.Search(ctx, Query{Keyword: "tt15398776", Limit: 10})
	if err != nil {
		t.Fatalf("Search() error = %v", err)
	}
	if gotIMDb != "15398776" || gotCat != "2000,5000" {
		t.Fatalf("query params imdb=%q cat=%q", gotIMDb, gotCat)
	}
	if len(resources) != 1 {
		t.Fatalf("len(resources) = %d, want 1", len(resources))
	}
	res := resources[0]
	if res.Title != "Oppenheimer.2023.2160p.BluRay.REMUX" || res.SizeBytes != 68719476736 {
		t.Fatalf("unexpected title/size: %+v", res)
	}
	if res.Seeders != 95 || res.Peers != 15 || res.Grabs != 430 || res.IMDBID != "tt15398776" {
		t.Fatalf("unexpected stats/imdb: %+v", res)
	}
	if res.DownloadVolumeFactor == nil || *res.DownloadVolumeFactor != 0 || res.UploadVolumeFactor == nil || *res.UploadVolumeFactor != 2 {
		t.Fatalf("unexpected volume factors: down=%v up=%v", res.DownloadVolumeFactor, res.UploadVolumeFactor)
	}

	dl, err := adapter.ResolveDownload(ctx, res.DownloadID)
	if err != nil || string(dl.Body) != "d8:announce1:ye" {
		t.Fatalf("ResolveDownload() body=%q err=%v", dl.Body, err)
	}

	rssResp, err := adapter.FetchRSS(ctx, RSSRequest{Limit: 10})
	if err != nil || len(rssResp.Items) != 1 || adapter.RSSCursorKey() == "" {
		t.Fatalf("FetchRSS() items=%d key=%q err=%v", len(rssResp.Items), adapter.RSSCursorKey(), err)
	}
}
