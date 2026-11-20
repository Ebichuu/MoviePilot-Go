package indexer

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"

	"github.com/PuerkitoBio/goquery"
)

const maxProbeBodyBytes = 1 << 20

// SiteTester 由支持只读连通性与鉴权探测的站点适配器实现。
// 探测不得创建下载凭据、修改远端状态或使用空关键词搜索代替健康检查。
type SiteTester interface {
	TestSite(context.Context) error
}

// SiteTestResult 是可以安全返回给 V2 前端的脱敏探测结果。
type SiteTestResult struct {
	Success    bool
	Message    string
	Recordable bool
}

type siteTestFailure string

func (e siteTestFailure) Error() string { return string(e) }

// TestSite 在调用方 goroutine 中执行探测，并与搜索共享同一个单站并发闸门。
func (c *Coordinator) TestSite(ctx context.Context, adapter Adapter) SiteTestResult {
	if c == nil || adapter == nil {
		return SiteTestResult{Message: "站点测试服务未初始化"}
	}
	if err := ctx.Err(); err != nil {
		return siteTestResult(err)
	}
	tester, ok := adapter.(SiteTester)
	if !ok {
		return SiteTestResult{Message: "当前站点适配器不支持连接测试"}
	}
	select {
	case c.probeSlots <- struct{}{}:
		defer func() { <-c.probeSlots }()
	default:
		return SiteTestResult{Message: "站点测试繁忙"}
	case <-ctx.Done():
		return siteTestResult(ctx.Err())
	}
	if err := ctx.Err(); err != nil {
		return siteTestResult(err)
	}
	gate := c.siteGate(adapterGateKey(adapter))
	select {
	case gate <- struct{}{}:
		defer func() { <-gate }()
	default:
		return SiteTestResult{Message: "站点正在执行其他请求"}
	case <-ctx.Done():
		return siteTestResult(ctx.Err())
	}
	if err := ctx.Err(); err != nil {
		return siteTestResult(err)
	}
	probeErr := tester.TestSite(withImmediateHTTPAdmission(ctx))
	result := siteTestResult(probeErr)
	result.Recordable = !errors.Is(probeErr, context.Canceled) && !errors.Is(probeErr, errHTTPRequestNotStarted)
	return result
}

func adapterGateKey(adapter Adapter) string {
	if identified, ok := adapter.(SiteIDAdapter); ok && identified.SiteID() > 0 {
		return "site:" + strconv.Itoa(identified.SiteID())
	}
	return "name:" + strings.ToLower(strings.TrimSpace(adapter.Name()))
}

func siteTestResult(err error) SiteTestResult {
	if err == nil {
		return SiteTestResult{Success: true, Message: "连接成功"}
	}
	if errors.Is(err, errHTTPRequestNotStarted) {
		if errors.Is(err, context.Canceled) {
			return SiteTestResult{Message: "连接测试已取消"}
		}
		return SiteTestResult{Message: "站点测试繁忙"}
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return SiteTestResult{Message: "连接超时"}
	}
	if errors.Is(err, context.Canceled) {
		return SiteTestResult{Message: "连接测试已取消"}
	}
	var statusError *HTTPStatusError
	if errors.As(err, &statusError) {
		if statusError.StatusCode == http.StatusUnauthorized || statusError.StatusCode == http.StatusForbidden {
			return SiteTestResult{Message: "站点鉴权失败"}
		}
		return SiteTestResult{Message: fmt.Sprintf("站点返回 HTTP %d", statusError.StatusCode)}
	}
	var safeFailure siteTestFailure
	if errors.As(err, &safeFailure) {
		return SiteTestResult{Message: safeFailure.Error()}
	}
	return SiteTestResult{Message: "站点连接失败"}
}

func (a *TNodeAdapter) TestSite(ctx context.Context) error {
	headers := a.headers.Clone()
	token, err := a.search.csrf(ctx, headers)
	if err != nil {
		return err
	}
	if token == "" {
		return siteTestFailure("站点 CSRF Token 缺失")
	}
	headers.Set(a.search.def.CSRFHeader, token)
	refreshed := false
	for index, path := range []string{"/api/user/getInfo", "/api/user/getMainInfo"} {
		var document map[string]any
		for {
			endpoint, resolveErr := resolveSiteURL(a.base, path)
			if resolveErr != nil {
				return resolveErr
			}
			body, requestErr := a.client.getLimited(ctx, endpoint, headers, maxProbeBodyBytes)
			if csrfRefreshRequired(requestErr) && !refreshed {
				a.search.invalidateCSRF(token)
				token, requestErr = a.search.csrf(ctx, headers)
				if requestErr != nil {
					return requestErr
				}
				headers.Set(a.search.def.CSRFHeader, token)
				refreshed = true
				continue
			}
			if requestErr != nil {
				var statusError *HTTPStatusError
				if index == 0 && errors.As(requestErr, &statusError) &&
					(statusError.StatusCode == http.StatusNotFound || statusError.StatusCode == http.StatusMethodNotAllowed) {
					break
				}
				return requestErr
			}
			document, requestErr = decodeProbeJSON(body)
			if requestErr != nil {
				return requestErr
			}
			if path == "/api/user/getInfo" {
				if probeDataObject(document["data"]) {
					return nil
				}
				return siteTestFailure("Cookie 已失效")
			}
			if strings.TrimSpace(valueString(document["status"])) == "200" && probeDataObject(document["data"]) {
				return nil
			}
			return siteTestFailure("Cookie 已失效")
		}
	}
	return siteTestFailure("站点用户接口不可用")
}

func (a *TorrentLeechAdapter) TestSite(ctx context.Context) error {
	return probeLoginPage(ctx, a.client, a.base, a.headers)
}

func (a *MTorrentAdapter) TestSite(ctx context.Context) error {
	endpoint, err := resolveSiteURL(a.api, "/api/member/profile")
	if err != nil {
		return err
	}
	body, err := a.client.doLimited(ctx, http.MethodPost, endpoint, a.headers.Clone(), nil, maxProbeBodyBytes)
	if err != nil {
		return err
	}
	document, err := decodeProbeJSON(body)
	if err != nil {
		return err
	}
	if !probeZero(document["code"]) || !probeDataObject(document["data"]) {
		return siteTestFailure("API Key 已失效")
	}
	return nil
}

func (a *SunnyPTAdapter) TestSite(ctx context.Context) error {
	body, err := a.client.getLimited(ctx, sunnyPTEndpoint(a.api, "profile"), a.headers.Clone(), maxProbeBodyBytes)
	if err != nil {
		return err
	}
	document, err := decodeProbeJSON(body)
	if err != nil {
		return err
	}
	data, ok := document["data"].(map[string]any)
	if !probeZero(document["code"]) || !ok || len(data) == 0 {
		return siteTestFailure("API Key 已失效")
	}
	if allowed, exists := data["download_allowed"]; exists && !valueBool(allowed) {
		return siteTestFailure("当前账号没有下载权限")
	}
	return nil
}

func (a *YemaAdapter) TestSite(ctx context.Context) error {
	endpoint, err := resolveSiteURL(a.base, "/openApi/user/fetchBasicInfo.json")
	if err != nil {
		return err
	}
	body, err := a.client.postJSONLimited(ctx, endpoint, a.headers.Clone(), map[string]any{}, maxProbeBodyBytes)
	if err != nil {
		return err
	}
	document, err := decodeProbeJSON(body)
	if err != nil {
		return err
	}
	success, ok := document["success"].(bool)
	if !ok || !success || !probeDataObject(document["data"]) {
		return siteTestFailure("API AuthKey 已失效")
	}
	return nil
}

func (a *HaidanAdapter) TestSite(ctx context.Context) error {
	return probeLoginPage(ctx, a.client, a.base, a.headers)
}

func (a *HDDolbyAdapter) TestSite(ctx context.Context) error {
	endpoint, err := resolveSiteURL(a.api, "/api/v1/user/data")
	if err != nil {
		return err
	}
	headers := a.headers.Clone()
	headers.Del("Cookie")
	body, err := a.client.getLimited(ctx, endpoint, headers, maxProbeBodyBytes)
	if err != nil {
		return err
	}
	document, err := decodeProbeJSON(body)
	if err != nil {
		return err
	}
	if !probeZero(document["status"]) || !probeDataContainer(document["data"]) {
		return siteTestFailure("API Key 已失效")
	}
	return nil
}

func (a *RousiAdapter) TestSite(ctx context.Context) error {
	endpoint, err := resolveSiteURL(a.base, "/api/v1/profile")
	if err != nil {
		return err
	}
	parsed, _ := url.Parse(endpoint)
	query := parsed.Query()
	query.Set("include_fields[user]", "seeding_leeching_data")
	parsed.RawQuery = query.Encode()
	body, err := a.client.getLimited(ctx, parsed.String(), a.headers.Clone(), maxProbeBodyBytes)
	if err != nil {
		return err
	}
	document, err := decodeProbeJSON(body)
	if err != nil {
		return err
	}
	if !probeZero(document["code"]) || !probeDataObject(document["data"]) {
		return siteTestFailure("Passkey 已失效")
	}
	return nil
}

func decodeProbeJSON(body []byte) (map[string]any, error) {
	if len(bytes.TrimSpace(body)) == 0 {
		return nil, siteTestFailure("站点响应为空")
	}
	document, err := decodeJSONObject(body)
	if err != nil {
		return nil, siteTestFailure("站点响应格式无效")
	}
	return document, nil
}

func probeZero(value any) bool {
	return strings.TrimSpace(valueString(value)) == "0"
}

func probeDataObject(value any) bool {
	object, ok := value.(map[string]any)
	return ok && len(object) > 0
}

func probeDataContainer(value any) bool {
	if probeDataObject(value) {
		return true
	}
	items, ok := value.([]any)
	return ok && len(items) > 0
}

func probeLoginPage(ctx context.Context, client *HTTPClient, base *url.URL, headers http.Header) error {
	requestHeaders := headers.Clone()
	requestHeaders.Set("Accept", "text/html,application/xhtml+xml")
	response, err := client.getResponseLimited(ctx, base.String(), requestHeaders, maxProbeBodyBytes)
	if err != nil {
		return err
	}
	if response.StatusCode != http.StatusOK && response.StatusCode != http.StatusForbidden && response.StatusCode != http.StatusInternalServerError {
		return &HTTPStatusError{StatusCode: response.StatusCode}
	}
	body := response.Body
	if len(body) == 0 {
		return siteTestFailure("站点响应为空")
	}
	lower := strings.ToLower(string(body))
	for _, marker := range []string{"cf-chl-", "challenge-platform", "just a moment...", "cloudflare ray id"} {
		if strings.Contains(lower, marker) {
			return siteTestFailure("站点受 Cloudflare 挑战保护")
		}
	}
	document, err := goquery.NewDocumentFromReader(bytes.NewReader(body))
	if err != nil {
		return siteTestFailure("站点响应格式无效")
	}
	hasPassword := false
	document.Find("input").EachWithBreak(func(_ int, selection *goquery.Selection) bool {
		inputType, _ := selection.Attr("type")
		hasPassword = strings.EqualFold(strings.TrimSpace(inputType), "password")
		return !hasPassword
	})
	if hasPassword {
		return siteTestFailure("Cookie 已失效")
	}
	if response.StatusCode != http.StatusOK {
		return &HTTPStatusError{StatusCode: response.StatusCode}
	}
	if document.Find("div.user-info-side, a#myitem").Length() > 0 ||
		selectionAttributeContains(document.Find("a"), "href", "logout") ||
		selectionAttributeContains(document.Find("a"), "data-url", "logout") ||
		selectionAttributeContains(document.Find("a"), "href", "mybonus") ||
		selectionAttributeContains(document.Find("a"), "onclick", "logout") ||
		selectionAttributeContains(document.Find("a"), "href", "usercp") ||
		selectionAttributeContains(document.Find("a"), "lay-on", "logout") ||
		selectionAttributeContains(document.Find("form"), "action", "logout") {
		return nil
	}
	return siteTestFailure("无法确认站点登录状态")
}

func selectionAttributeContains(selection *goquery.Selection, attribute, marker string) bool {
	found := false
	selection.EachWithBreak(func(_ int, item *goquery.Selection) bool {
		value, exists := item.Attr(attribute)
		found = exists && strings.Contains(strings.ToLower(value), marker)
		return !found
	})
	return found
}
