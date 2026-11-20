package indexer

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// RSSRequest carries conditional validators and the bounded item limit for a feed poll.
type RSSRequest struct {
	ETag         string
	LastModified string
	Limit        int
}

// RSSItem is one parsed feed item with a stable, site-scoped identity.
type RSSItem struct {
	ItemKey  string
	GUID     string
	Resource Resource
}

// RSSResponse is the bounded result of one feed request.
type RSSResponse struct {
	NotModified  bool
	ETag         string
	LastModified string
	Items        []RSSItem
}

func rssDomainUsesNoCacheKey(site *url.URL) bool {
	return site != nil && strings.Contains(strings.ToLower(site.Hostname()), "m-team")
}

// RSSFetcher is an optional adapter capability. It deliberately does not
// extend Adapter so existing site implementations remain source-compatible.
type RSSFetcher interface {
	FetchRSS(ctx context.Context, request RSSRequest) (RSSResponse, error)
}

// RSSCursorSource exposes the opaque identity of a feed endpoint. The key
// changes when the configured endpoint changes, while never persisting the
// original URL or query credentials in the cursor table.
type RSSCursorSource interface {
	RSSFetcher
	RSSCursorKey() string
}

// RSSNoCacheSource marks feeds whose successful non-empty refresh replaces the
// persisted item window instead of reusing prior items. This mirrors V2's
// NO_CACHE_SITE_KEY behavior without exposing site credentials or URLs.
type RSSNoCacheSource interface {
	RSSNoCache() bool
}

// RSSItemKey derives a versioned opaque key without exposing feed locators.
// Identity precedence is GUID, detail URL, enclosure URL, then a bounded tuple.
func RSSItemKey(siteID int, guid, detailURL, downloadURL, title string, publishedAt time.Time, sizeBytes int64) string {
	identity := strings.TrimSpace(guid)
	if identity != "" {
		identity = "guid\x00" + identity
	} else if normalized := normalizeRSSURL(detailURL); normalized != "" {
		identity = "detail\x00" + normalized
	} else if normalized := normalizeRSSURL(downloadURL); normalized != "" {
		identity = "download\x00" + normalized
	} else {
		identity = "tuple\x00" + strings.ToLower(strings.Join(strings.Fields(title), " ")) + "\x00" + publishedAt.UTC().Format(time.RFC3339Nano) + "\x00" + strconv.FormatInt(sizeBytes, 10)
	}
	if siteID < 1 || strings.TrimSpace(identity) == "" {
		return ""
	}
	digest := sha256.Sum256([]byte("moviepilot:rss-item:v1\x00" + strconv.Itoa(siteID) + "\x00" + identity))
	return "sha256:" + hex.EncodeToString(digest[:])
}

func normalizeRSSURL(raw string) string {
	parsed, err := url.Parse(strings.TrimSpace(raw))
	if err != nil || parsed.Host == "" || (parsed.Scheme != "http" && parsed.Scheme != "https") {
		return strings.TrimSpace(raw)
	}
	parsed.Scheme = strings.ToLower(parsed.Scheme)
	parsed.Host = strings.ToLower(parsed.Host)
	parsed.Fragment = ""
	return parsed.String()
}
