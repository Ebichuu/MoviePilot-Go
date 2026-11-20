package indexer

import (
	"context"
	"encoding/base64"
	"errors"
	"net/http"
	"net/url"
	"strings"

	"github.com/PuerkitoBio/goquery"
)

const (
	maxSiteDiscoveryHTMLBytes = 2 << 20
	maxSiteFaviconBytes       = 128 << 10
)

// DiscoverSiteFavicon follows the site's declared icon link, falling back to
// /favicon.ico, and returns its URL plus optional base64 data.
func (c *HTTPClient) DiscoverSiteFavicon(
	ctx context.Context,
	baseURL, cookie, userAgent string,
) (string, string, error) {
	base, document, headers, err := c.siteDiscoveryDocument(ctx, baseURL, cookie, userAgent)
	if err != nil {
		return "", "", err
	}
	rawIconURL := "favicon.ico"
	document.Find("head link[rel]").EachWithBreak(func(_ int, link *goquery.Selection) bool {
		rel, _ := link.Attr("rel")
		if !siteIconRel(rel) {
			return true
		}
		href, _ := link.Attr("href")
		if strings.TrimSpace(href) == "" {
			return true
		}
		rawIconURL = strings.TrimSpace(href)
		return false
	})
	iconURL, err := resolveSiteURL(base, rawIconURL)
	if err != nil {
		return "", "", nil
	}
	response, err := c.getResponseLimited(ctx, iconURL, headers, maxSiteFaviconBytes)
	if err != nil {
		return iconURL, "", err
	}
	if response.StatusCode < http.StatusOK || response.StatusCode >= http.StatusMultipleChoices || len(response.Body) == 0 {
		return iconURL, "", nil
	}
	return iconURL, base64.StdEncoding.EncodeToString(response.Body), nil
}

// DiscoverSiteRSS reads an advertised RSS or Atom feed from the site page.
// It returns an empty URL when the page does not advertise a feed.
func (c *HTTPClient) DiscoverSiteRSS(
	ctx context.Context,
	baseURL, cookie, userAgent string,
) (string, error) {
	base, document, _, err := c.siteDiscoveryDocument(ctx, baseURL, cookie, userAgent)
	if err != nil {
		return "", err
	}
	var feedURL string
	document.Find("head link[rel]").EachWithBreak(func(_ int, link *goquery.Selection) bool {
		rel, _ := link.Attr("rel")
		mediaType, _ := link.Attr("type")
		if !siteFeedLink(rel, mediaType) {
			return true
		}
		href, _ := link.Attr("href")
		if strings.TrimSpace(href) == "" {
			return true
		}
		resolved, resolveErr := resolveSiteURL(base, strings.TrimSpace(href))
		if resolveErr != nil {
			return true
		}
		feedURL = resolved
		return false
	})
	return feedURL, nil
}

// TestSiteLoginPage performs the bounded generic login-page probe used when a
// configured adapter has no site-specific TestSite implementation.
func (c *HTTPClient) TestSiteLoginPage(ctx context.Context, baseURL, cookie, userAgent string) error {
	if c == nil || ctx == nil || strings.ContainsAny(cookie, "\r\n") || strings.ContainsAny(userAgent, "\r\n") {
		return errors.New("站点测试参数无效")
	}
	base, err := validateBaseURL(baseURL, c.allowPrivate)
	if err != nil {
		return err
	}
	headers := make(http.Header)
	if strings.TrimSpace(cookie) != "" {
		headers.Set("Cookie", cookie)
	}
	if strings.TrimSpace(userAgent) != "" {
		headers.Set("User-Agent", userAgent)
	}
	return probeLoginPage(ctx, c, base, headers)
}

// TestSiteAvailability checks the same bounded homepage response codes accepted
// by V2's CookieCloud site-add flow for public sites.
func (c *HTTPClient) TestSiteAvailability(ctx context.Context, baseURL, cookie, userAgent string) error {
	if c == nil || ctx == nil || strings.ContainsAny(cookie, "\r\n") || strings.ContainsAny(userAgent, "\r\n") {
		return errors.New("站点测试参数无效")
	}
	base, err := validateBaseURL(baseURL, c.allowPrivate)
	if err != nil {
		return err
	}
	headers := make(http.Header)
	headers.Set("Accept", "text/html,application/xhtml+xml")
	if strings.TrimSpace(cookie) != "" {
		headers.Set("Cookie", cookie)
	}
	if strings.TrimSpace(userAgent) != "" {
		headers.Set("User-Agent", userAgent)
	}
	response, err := c.getResponseLimited(ctx, base.String(), headers, maxSiteDiscoveryHTMLBytes)
	if err != nil {
		return err
	}
	switch response.StatusCode {
	case http.StatusOK, http.StatusForbidden, http.StatusInternalServerError:
		return nil
	default:
		return &HTTPStatusError{StatusCode: response.StatusCode}
	}
}

func (c *HTTPClient) siteDiscoveryDocument(
	ctx context.Context,
	baseURL, cookie, userAgent string,
) (*url.URL, *goquery.Document, http.Header, error) {
	if c == nil || ctx == nil {
		return nil, nil, nil, errors.New("站点发现参数无效")
	}
	if strings.ContainsAny(cookie, "\r\n") || strings.ContainsAny(userAgent, "\r\n") {
		return nil, nil, nil, errors.New("站点请求头无效")
	}
	base, err := validateBaseURL(baseURL, c.allowPrivate)
	if err != nil {
		return nil, nil, nil, err
	}
	headers := make(http.Header)
	headers.Set("Accept", "text/html,application/xhtml+xml")
	if strings.TrimSpace(cookie) != "" {
		headers.Set("Cookie", cookie)
	}
	if strings.TrimSpace(userAgent) != "" {
		headers.Set("User-Agent", userAgent)
	}
	response, err := c.getResponseLimited(ctx, base.String(), headers, maxSiteDiscoveryHTMLBytes)
	if err != nil {
		return nil, nil, nil, err
	}
	if response.StatusCode < http.StatusOK || response.StatusCode >= http.StatusMultipleChoices {
		return nil, nil, nil, &HTTPStatusError{StatusCode: response.StatusCode}
	}
	document, err := goquery.NewDocumentFromReader(strings.NewReader(string(response.Body)))
	if err != nil {
		return nil, nil, nil, errors.New("站点首页 HTML 无效")
	}
	return base, document, headers, nil
}

func siteIconRel(raw string) bool {
	for _, rel := range strings.Fields(strings.ToLower(raw)) {
		if rel == "icon" || rel == "shortcut" || rel == "apple-touch-icon" {
			return true
		}
	}
	return false
}

func siteFeedLink(rel, mediaType string) bool {
	alternate := false
	for _, token := range strings.Fields(strings.ToLower(rel)) {
		if token == "alternate" {
			alternate = true
			break
		}
	}
	if !alternate {
		return false
	}
	mediaType = strings.ToLower(strings.TrimSpace(strings.SplitN(mediaType, ";", 2)[0]))
	return mediaType == "application/rss+xml" || mediaType == "application/atom+xml" ||
		mediaType == "application/feed+json"
}
