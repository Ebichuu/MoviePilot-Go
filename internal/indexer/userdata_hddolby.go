package indexer

import (
	"bytes"
	"context"
	"errors"
	"net/http"
	"strings"

	"github.com/PuerkitoBio/goquery"
)

var hddolbyUserLevels = map[string]string{
	"0": "Peasant", "1": "User", "2": "Power User", "3": "Elite User", "4": "Crazy User",
	"5": "Insane User", "6": "Veteran User", "7": "Extreme User", "8": "Ultimate User",
	"9": "Nexus Master", "10": "VIP", "11": "Retiree", "12": "Helper", "13": "Seeder",
	"14": "Transferrer", "15": "Uploader", "16": "Torrent Manager", "17": "Forum Moderator",
	"18": "Coder", "19": "Moderator", "20": "Administrator", "21": "Sysop", "22": "Staff Leader",
}

// FetchSiteUserData reproduces HDDolbySiteUserInfo from V2 2.15.6.
func (a *HDDolbyAdapter) FetchSiteUserData(ctx context.Context) (SiteUserData, error) {
	result := newSiteUserData(a.site)
	apiHeaders := a.headers.Clone()
	apiHeaders.Del("Cookie")

	userEndpoint, err := resolveSiteURL(a.api, "/api/v1/user/data")
	if err != nil {
		return SiteUserData{}, err
	}
	body, err := hddolbyUserDataPage(ctx, a.client, userEndpoint, apiHeaders)
	if err != nil {
		return SiteUserData{}, err
	}
	if err := parseHDDolbyUser(&result, body); err != nil {
		return SiteUserData{}, err
	}

	peersEndpoint, err := resolveSiteURL(a.api, "/api/v1/user/peers")
	if err != nil {
		return SiteUserData{}, err
	}
	body, err = hddolbyUserDataPage(ctx, a.client, peersEndpoint, apiHeaders)
	if err != nil {
		return SiteUserData{}, err
	}
	if err := parseHDDolbyPeers(&result, body); err != nil {
		return SiteUserData{}, err
	}

	if siteUserDataMessagesEnabled(ctx) && result.MessageUnread > 0 && a.headers.Get("Cookie") != "" {
		if err := a.fetchHDDolbyMessages(ctx, &result); err != nil {
			return SiteUserData{}, err
		}
	}
	return result, nil
}

func hddolbyUserDataPage(ctx context.Context, client *HTTPClient, endpoint string, headers http.Header) ([]byte, error) {
	response, err := client.getResponseLimited(ctx, endpoint, headers, maxProbeBodyBytes)
	if err != nil {
		return nil, err
	}
	switch response.StatusCode {
	case http.StatusOK, http.StatusForbidden, http.StatusInternalServerError:
		return response.Body, nil
	default:
		return nil, nil
	}
}

func parseHDDolbyUser(result *SiteUserData, body []byte) error {
	document, err := decodeJSONObject(body)
	if err != nil {
		return err
	}
	if valueString(document["status"]) != "0" {
		return nil
	}
	items, ok := document["data"].([]any)
	if !ok || len(items) == 0 {
		return nil
	}
	user, ok := items[0].(map[string]any)
	if !ok {
		return nil
	}
	result.UserID = userDataID(user["id"])
	result.Username = userDataString(user["username"])
	level := hddolbyUserLevels[valueString(user["class"])]
	if level == "" {
		level = hddolbyUserLevels["1"]
	}
	result.UserLevel = &level
	result.JoinAt = userDataString(user["added"])
	result.Upload = userDataInt64(user["uploaded"])
	result.Download = userDataInt64(user["downloaded"])
	if result.Download > 0 {
		result.Ratio = roundUserData(float64(result.Upload)/float64(result.Download), 2)
	}
	result.Bonus = userDataFloat(user["seedbonus"])
	result.MessageUnread = userDataInt(user["unread_messages"])
	return nil
}

func parseHDDolbyPeers(result *SiteUserData, body []byte) error {
	document, err := decodeJSONObject(body)
	if err != nil {
		return err
	}
	if valueString(document["status"]) != "0" {
		return nil
	}
	items, ok := document["data"].([]any)
	if !ok {
		return nil
	}
	for _, raw := range items {
		item, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		size := userDataInt64(item["size"])
		seeders := userDataInt64(item["seeders"])
		if seeders == 0 {
			seeders = 1
		}
		result.Seeding++
		result.SeedingSize += size
		result.SeedingInfo = append(result.SeedingInfo, []int64{seeders, size})
	}
	return nil
}

func (a *HDDolbyAdapter) fetchHDDolbyMessages(ctx context.Context, result *SiteUserData) error {
	links := make([]string, 0, result.MessageUnread)
	for _, path := range []string{
		"/messages.php?action=viewmailbox&box=1&unread=yes",
		"/messages.php?action=viewmailbox&box=-2&unread=yes",
	} {
		next := path
		for pages := 0; next != "" && pages < 100; pages++ {
			endpoint, err := resolveSiteURL(a.site, next)
			if err != nil {
				return err
			}
			body, err := hddolbyMessagePage(ctx, a, endpoint)
			if err != nil {
				return err
			}
			pageLinks, pageNext := parseHDDolbyMessageLinks(body)
			links = append(links, pageLinks...)
			next = pageNext
		}
	}
	if len(links) > 0 && result.MessageUnread == 0 {
		result.MessageUnread = len(links)
	}
	for _, link := range links {
		endpoint, err := resolveSiteURL(a.site, link)
		if err != nil {
			return err
		}
		body, err := hddolbyMessagePage(ctx, a, endpoint)
		if err != nil {
			return err
		}
		title, date, content := parseHDDolbyMessage(body)
		result.MessageUnreadContents = append(result.MessageUnreadContents, []any{title, date, content})
	}
	return nil
}

func hddolbyMessagePage(ctx context.Context, adapter *HDDolbyAdapter, endpoint string) ([]byte, error) {
	headers := adapter.headers.Clone()
	headers.Del("x-api-key")
	headers.Set("Accept", "text/html,application/xhtml+xml")
	response, err := adapter.client.getResponseLimited(ctx, endpoint, headers, maxProbeBodyBytes)
	if err != nil {
		if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
			return nil, err
		}
		return nil, nil
	}
	if response.StatusCode != http.StatusOK && response.StatusCode != http.StatusForbidden && response.StatusCode != http.StatusInternalServerError {
		return nil, nil
	}
	return response.Body, nil
}

func parseHDDolbyMessageLinks(body []byte) ([]string, string) {
	document, err := goquery.NewDocumentFromReader(bytes.NewReader(body))
	if err != nil {
		return nil, ""
	}
	links := make([]string, 0)
	document.Find("tr").Each(func(_ int, row *goquery.Selection) {
		if row.Find(`td img[alt="Read"]`).Length() > 0 {
			return
		}
		row.Find(`td a[href*="viewmessage"]`).Each(func(_ int, link *goquery.Selection) {
			if href, ok := link.Attr("href"); ok {
				links = append(links, href)
			}
		})
	})
	var next string
	document.Find("a").Each(func(_ int, link *goquery.Selection) {
		text := link.Text()
		if strings.Contains(text, "下一页") || strings.Contains(text, "下一頁") {
			next, _ = link.Attr("href")
			next = strings.TrimSpace(next)
		}
	})
	return links, next
}

func parseHDDolbyMessage(body []byte) (any, any, any) {
	document, err := goquery.NewDocumentFromReader(bytes.NewReader(body))
	if err != nil {
		return nil, nil, nil
	}
	var title, date, content any
	if value := strings.TrimSpace(document.Find("h1").Last().Text()); value != "" {
		title = value
	} else if value = strings.TrimSpace(document.Find("div.layui-card-header span").First().Text()); value != "" {
		title = value
	}
	if header := document.Find("h1").Last(); header.Length() > 0 {
		table := header.NextAllFiltered("table").FilterFunction(func(_ int, item *goquery.Selection) bool {
			return item.Find("tr td.colhead").Length() > 0
		}).First()
		if value := strings.TrimSpace(table.Find("tr").Eq(1).Find("td").Eq(1).Text()); value != "" {
			date = value
		}
		if value := strings.TrimSpace(table.Find("tr").Eq(2).Find("td").First().Text()); value != "" {
			content = value
		}
	}
	if date == nil {
		if value := strings.TrimSpace(document.Find("div.layui-card-header span").Eq(1).Find("span").Eq(1).Text()); value != "" {
			date = value
		}
	}
	if content == nil {
		if value := strings.TrimSpace(document.Find("div.layui-card-body").First().Text()); value != "" {
			content = value
		}
	}
	return title, date, content
}

func roundUserData(value float64, precision int) float64 {
	factor := 1.0
	for range precision {
		factor *= 10
	}
	return float64(int64(value*factor+0.5)) / factor
}
