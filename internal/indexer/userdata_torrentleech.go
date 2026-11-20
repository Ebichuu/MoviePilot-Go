package indexer

import (
	"bytes"
	"context"
	"errors"
	"net/http"
	"regexp"
	"strings"

	"github.com/PuerkitoBio/goquery"
)

var (
	torrentLeechProfilePattern     = regexp.MustCompile(`/profile/([^/]+)/`)
	torrentLeechProfileViewPattern = regexp.MustCompile(`/profile/([^/]+)/view`)
)

// FetchSiteUserData reproduces TorrentLeechSiteUserInfo from V2 2.15.6.
func (a *TorrentLeechAdapter) FetchSiteUserData(ctx context.Context) (SiteUserData, error) {
	result := newSiteUserData(a.base)
	home, err := torrentLeechUserDataPage(ctx, a, a.base.String())
	if err != nil {
		return SiteUserData{}, err
	}
	document, loggedIn := torrentLeechLoggedIn(home)
	if !loggedIn {
		message := "未检测到已登陆，请检查cookies是否过期"
		result.ErrMsg = &message
		return result, nil
	}

	userID := torrentLeechUserID(document, string(home))
	if userID == "" {
		message := "未获取到用户ID"
		result.ErrMsg = &message
		return result, nil
	}
	result.UserID = userID
	result.Username = userDataString(userID)

	profileURL, err := resolveSiteURL(a.base, "/profile/"+userID+"/view")
	if err != nil {
		return SiteUserData{}, err
	}
	profile, err := torrentLeechUserDataPage(ctx, a, profileURL)
	if err != nil {
		return SiteUserData{}, err
	}
	if err := parseTorrentLeechProfile(&result, profile); err != nil {
		return SiteUserData{}, err
	}

	seedingURL, err := resolveSiteURL(a.base, "/profile/"+userID+"/seeding")
	if err != nil {
		return SiteUserData{}, err
	}
	seeding, err := torrentLeechUserDataPage(ctx, a, seedingURL)
	if err != nil {
		return SiteUserData{}, err
	}
	if err := parseTorrentLeechSeeding(&result, seeding); err != nil {
		return SiteUserData{}, err
	}
	return result, nil
}

func torrentLeechUserDataPage(ctx context.Context, adapter *TorrentLeechAdapter, endpoint string) ([]byte, error) {
	headers := adapter.headers.Clone()
	headers.Set("Accept", "text/html,application/xhtml+xml")
	response, err := adapter.client.getResponseLimited(ctx, endpoint, headers, maxProbeBodyBytes)
	if err != nil {
		if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
			return nil, err
		}
		return nil, nil
	}
	switch response.StatusCode {
	case http.StatusOK, http.StatusForbidden, http.StatusInternalServerError:
		return response.Body, nil
	default:
		return nil, nil
	}
}

func torrentLeechLoggedIn(body []byte) (*goquery.Document, bool) {
	document, err := goquery.NewDocumentFromReader(bytes.NewReader(body))
	if err != nil {
		return nil, false
	}
	if document.Find(`input[type="password"]`).Length() > 0 {
		return document, false
	}
	loggedIn := document.Find("div.user-info-side, a#myitem").Length() > 0 ||
		selectionAttributeContains(document.Find("a"), "href", "logout") ||
		selectionAttributeContains(document.Find("a"), "data-url", "logout") ||
		selectionAttributeContains(document.Find("a"), "href", "mybonus") ||
		selectionAttributeContains(document.Find("a"), "onclick", "logout") ||
		selectionAttributeContains(document.Find("a"), "href", "usercp") ||
		selectionAttributeContains(document.Find("a"), "lay-on", "logout") ||
		selectionAttributeContains(document.Find("form"), "action", "logout")
	return document, loggedIn
}

func torrentLeechUserID(document *goquery.Document, raw string) string {
	if document != nil {
		var userID string
		document.Find("span.centerTopBar [onclick]").EachWithBreak(func(_ int, item *goquery.Selection) bool {
			route, _ := item.Attr("onclick")
			match := torrentLeechProfileViewPattern.FindStringSubmatch(route)
			if len(match) > 1 {
				userID = strings.TrimSpace(match[1])
			}
			return userID == ""
		})
		if userID != "" {
			return userID
		}
	}
	match := torrentLeechProfilePattern.FindStringSubmatch(raw)
	if len(match) > 1 {
		return strings.TrimSpace(match[1])
	}
	return ""
}

func parseTorrentLeechProfile(result *SiteUserData, body []byte) error {
	document, err := goquery.NewDocumentFromReader(bytes.NewReader(body))
	if err != nil {
		return nil
	}
	if username := strings.TrimSpace(document.Find("div.profile-username").First().Text()); username != "" {
		result.Username = &username
	} else if username = torrentLeechTableValue(document, "Username"); username != "" {
		result.Username = &username
	}
	if upload := strings.TrimSpace(document.Find("div.profile-uploaded span").First().Text()); upload != "" {
		result.Upload = parseSize(upload)
	}
	if download := strings.TrimSpace(document.Find("div.profile-downloaded span").First().Text()); download != "" {
		result.Download = parseSize(download)
	}
	if ratio := strings.TrimSpace(document.Find("div.profile-ratio span").First().Text()); ratio != "" {
		result.Ratio = userDataFloat(strings.ReplaceAll(ratio, "∞", "0"))
	}
	if level := torrentLeechTableValue(document, "Class"); level != "" {
		result.UserLevel = &level
	}
	if joined := torrentLeechTableValue(document, "Registration date"); joined != "" {
		result.JoinAt = userDataDateTime(joined)
	}
	if bonus := strings.TrimSpace(document.Find("span.total-TL-points").First().Text()); bonus != "" {
		result.Bonus = userDataFloat(bonus)
	}
	return nil
}

func torrentLeechTableValue(document *goquery.Document, label string) string {
	var value string
	document.Find("table.profileViewTable tr").EachWithBreak(func(_ int, row *goquery.Selection) bool {
		cells := row.Find("td")
		if cells.Length() >= 2 && strings.TrimSpace(cells.Eq(0).Text()) == label {
			value = strings.TrimSpace(cells.Eq(1).Text())
		}
		return value == ""
	})
	return value
}

func parseTorrentLeechSeeding(result *SiteUserData, body []byte) error {
	document, err := goquery.NewDocumentFromReader(bytes.NewReader(body))
	if err != nil {
		return nil
	}
	document.Find("tbody tr").Each(func(_ int, row *goquery.Selection) {
		cells := row.Find("td")
		if cells.Length() < 7 {
			return
		}
		size := parseSize(strings.TrimSpace(cells.Eq(1).Text()))
		seeders := int64(parseInt(strings.TrimSpace(cells.Eq(6).Text())))
		result.Seeding++
		result.SeedingSize += size
		result.SeedingInfo = append(result.SeedingInfo, []int64{seeders, size})
	})
	return nil
}
