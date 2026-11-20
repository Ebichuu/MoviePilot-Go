package httpapi

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5"

	"github.com/moviepilot-go/moviepilot-go/internal/database"
	"github.com/moviepilot-go/moviepilot-go/internal/indexer"
)

const siteStatisticWriteTimeout = 2 * time.Second
const rssSitesSetting = "RssSites"

// SiteStatisticStore 定义站点连接测试所需的最小统计持久化能力。
type SiteStatisticStore interface {
	RecordSiteTest(context.Context, string, bool, time.Duration) error
	ListSiteStatistics(context.Context) ([]database.SiteStatistic, error)
	SiteStatisticByDomain(context.Context, string) (database.SiteStatistic, bool, error)
}

// registerSiteCompatibility 注册 V2 站点域中尚未接入外部站点能力的兼容端点。
// 这些端点必须返回稳定的 V2 响应形状，避免前端把延后能力误判为路由故障。
func (d Dependencies) registerSiteCompatibility(router chi.Router) {
	router.Get("/site/auth", d.listSiteAuth)
	router.With(d.requireAdmin).Post("/site/auth", d.updateSiteAuth)
	router.With(d.requireAdmin).Get("/site/cookiecloud", d.cookieCloudSync)
	router.With(d.requireAdmin).Get("/site/reset", d.resetSites)
	router.With(d.requireManage).Post("/site/priorities", d.updateSitePriorities)
	router.With(d.requireManage).Get("/site/cookie/{site_id}", d.updateSiteCookie)
	router.With(d.requireManage).Post("/site/cookie/{site_id}", d.updateSiteCookie)
	router.Get("/site/category/{site_id}", d.siteCategories)
	router.Get("/site/domain/{site_url}", d.siteByDomain)
	router.Get("/site/icon/{site_id}", d.siteIcon)
	router.With(d.requireAdmin).Get("/site/mapping", d.siteMapping)
	router.With(d.requireManage).Get("/site/resource/{site_id}", d.siteResources)
	router.Get("/site/rss", d.rssSites)
	router.Get("/site/statistic", d.siteStatistics)
	router.Get("/site/statistic/{site_url}", d.siteStatistic)
	router.With(d.requireAdmin).Get("/site/supporting", d.supportingSites)
	router.Get("/site/test/{site_id}", d.testSite)
	router.With(d.requireSiteUserDataManage).Get("/site/userdata/latest", d.latestSiteUserData)
	router.With(d.requireSiteUserDataManage).Get("/site/userdata/{site_id}", d.siteUserDataHistory)
	router.With(d.requireSiteUserDataManage).Post("/site/userdata/{site_id}", d.refreshSiteUserData)
}

func (d Dependencies) listSiteAuth(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{})
}

// CookieCloudSyncer 触发一次 CookieCloud 同步，对应 V2 `Scheduler().start(job_id="cookiecloud")`。
type CookieCloudSyncer interface {
	SyncCookieCloud(ctx context.Context) (string, error)
}

// cookieCloudSync 复刻 V2 `/site/cookiecloud`（app/api/endpoints/site.py:120–129）：
// 管理员触发后台同步并返回 `{success:true,message:"CookieCloud同步任务已启动！"}`。
func (d Dependencies) cookieCloudSync(w http.ResponseWriter, r *http.Request) {
	if d.CookieCloud == nil {
		writeUnsupportedSite(w, "CookieCloud 同步能力尚未接入")
		return
	}
	message, err := d.CookieCloud.SyncCookieCloud(r.Context())
	if err != nil {
		if contextErr := r.Context().Err(); contextErr != nil {
			writeError(w, http.StatusServiceUnavailable, "CookieCloud 同步已取消")
			return
		}
		writeError(w, http.StatusInternalServerError, "CookieCloud 同步失败")
		return
	}
	if message == "" {
		message = "CookieCloud同步任务已启动！"
	}
	writeJSON(w, http.StatusOK, Response{Success: true, Message: message})
}

func (d Dependencies) updateSiteAuth(w http.ResponseWriter, _ *http.Request) {
	writeUnsupportedSite(w, "站点认证能力尚未接入")
}

type sitePriorityRequest struct {
	ID       int  `json:"id"`
	Priority *int `json:"pri"`
}

func (d Dependencies) updateSitePriorities(w http.ResponseWriter, r *http.Request) {
	store := d.siteStore()
	updater, ok := store.(interface {
		UpdateSitePriorities(context.Context, []database.SitePriority) error
	})
	if !ok {
		writeUnsupportedSite(w, "站点优先级能力尚未接入")
		return
	}
	decoder := json.NewDecoder(r.Body)
	var input []sitePriorityRequest
	if err := decoder.Decode(&input); err != nil {
		writeError(w, http.StatusBadRequest, "站点优先级请求无效")
		return
	}
	var trailing any
	if err := decoder.Decode(&trailing); err != io.EOF {
		writeError(w, http.StatusBadRequest, "站点优先级请求包含多余内容")
		return
	}
	if len(input) > 1000 {
		writeError(w, http.StatusBadRequest, "站点优先级请求过大")
		return
	}
	priorities := make([]database.SitePriority, 0, len(input))
	for _, item := range input {
		if item.ID <= 0 || item.Priority == nil || *item.Priority < 0 {
			writeError(w, http.StatusBadRequest, "站点优先级参数无效")
			return
		}
		priorities = append(priorities, database.SitePriority{ID: item.ID, Priority: *item.Priority})
	}
	if err := updater.UpdateSitePriorities(r.Context(), priorities); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			writeError(w, http.StatusNotFound, "站点不存在")
			return
		}
		writeError(w, http.StatusInternalServerError, "更新站点优先级失败")
		return
	}
	if err := d.reloadSites(r.Context()); err != nil {
		writeSiteReloadFailure(w, 0)
		return
	}
	writeSuccess(w, map[string]any{"updated": len(priorities)})
}

// siteCategories 复刻 V2 `/site/category/{site_id}`（app/api/endpoints/site.py:357–388）：
// 站点不存在 → 404「站点 {id} 不存在」；索引器不支持 → 404「站点 {domain} 不支持」；
// 无分类 → []；否则按索引器定义顺序去重后返回分类对象列表。
func (d Dependencies) siteCategories(w http.ResponseWriter, r *http.Request) {
	store := d.siteStore()
	if store == nil {
		// 兼容路由最小装配下返回稳定的 V2 空分类数组。
		writeJSON(w, http.StatusOK, []any{})
		return
	}
	siteID, err := strconv.Atoi(strings.TrimSpace(chi.URLParam(r, "site_id")))
	if err != nil {
		writeError(w, http.StatusUnprocessableEntity, "站点 ID 无效")
		return
	}
	site, err := store.RuntimeSiteByID(r.Context(), siteID)
	if errors.Is(err, pgx.ErrNoRows) {
		writeError(w, http.StatusNotFound, fmt.Sprintf("站点 %d 不存在", siteID))
		return
	}
	if err != nil {
		writeError(w, http.StatusInternalServerError, "查询站点失败")
		return
	}
	domain := siteStatisticDomain(site)
	catalog := d.SiteCatalog
	if catalog == nil || domain == "" {
		writeJSON(w, http.StatusOK, []any{})
		return
	}
	categories, supported, err := catalog.SiteCategories(r.Context(), domain)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "查询站点分类失败")
		return
	}
	if !supported {
		writeError(w, http.StatusNotFound, fmt.Sprintf("站点 %s 不支持", domain))
		return
	}
	if categories == nil {
		categories = []map[string]any{}
	}
	writeJSON(w, http.StatusOK, categories)
}

// siteIcon 复刻 V2 `/site/icon/{site_id}`（app/api/endpoints/site.py:334–354）：
// 站点不存在 → 404；无图标 → `{success:false,message:"站点图标不存在！"}`；
// 有图标 → `{success:true,data:{icon: base64或url}}`。
func (d Dependencies) siteIcon(w http.ResponseWriter, r *http.Request) {
	store := d.siteStore()
	if store == nil {
		writeError(w, http.StatusServiceUnavailable, "站点存储未初始化")
		return
	}
	siteID, err := strconv.Atoi(strings.TrimSpace(chi.URLParam(r, "site_id")))
	if err != nil {
		writeError(w, http.StatusUnprocessableEntity, "站点 ID 无效")
		return
	}
	site, err := store.RuntimeSiteByID(r.Context(), siteID)
	if errors.Is(err, pgx.ErrNoRows) {
		writeError(w, http.StatusNotFound, fmt.Sprintf("站点 %d 不存在", siteID))
		return
	}
	if err != nil {
		writeError(w, http.StatusInternalServerError, "查询站点失败")
		return
	}
	icons := d.SiteIcons
	if icons == nil {
		writeJSON(w, http.StatusOK, Response{Success: false, Message: "站点图标不存在！"})
		return
	}
	iconBase64, iconURL, found, err := icons.SiteIconByDomain(r.Context(), siteStatisticDomain(site))
	if err != nil {
		writeError(w, http.StatusInternalServerError, "查询站点图标失败")
		return
	}
	if !found {
		writeJSON(w, http.StatusOK, Response{Success: false, Message: "站点图标不存在！"})
		return
	}
	icon := iconBase64
	if strings.TrimSpace(icon) == "" {
		icon = iconURL
	}
	writeJSON(w, http.StatusOK, Response{Success: true, Data: map[string]any{"icon": icon}})
}

// resetSites 复刻 V2 `/site/reset`（app/api/endpoints/site.py:132–146）：清空所有
// 站点，随后重新触发 CookieCloud 同步，返回 `{success:true,message:"站点已重置！"}`。
func (d Dependencies) resetSites(w http.ResponseWriter, r *http.Request) {
	resetter, ok := d.Sites.(interface {
		DeleteAllSites(context.Context) (int64, error)
	})
	if !ok {
		writeUnsupportedSite(w, "站点重置能力尚未接入")
		return
	}
	if _, err := resetter.DeleteAllSites(r.Context()); err != nil {
		writeError(w, http.StatusInternalServerError, "重置站点失败")
		return
	}
	if d.CookieCloud != nil {
		if _, err := d.CookieCloud.SyncCookieCloud(r.Context()); err != nil {
			if contextErr := r.Context().Err(); contextErr != nil {
				writeError(w, http.StatusServiceUnavailable, "站点重置已取消")
				return
			}
			// V2 先返回重置成功，同步失败在下一次调度重试。
		}
	}
	writeJSON(w, http.StatusOK, Response{Success: true, Message: "站点已重置！"})
}

// SiteCookieUpdater 执行 V2 `SiteChain.update_cookie`：用用户密码登录并更新站点
// Cookie/UA。未接入时端点返回稳定的未成功响应。
type SiteCookieUpdater interface {
	UpdateSiteCookie(ctx context.Context, siteID int, username, password, code string) (bool, string, error)
}

type siteCookieUpdateRequest struct {
	Username string `json:"username"`
	Password string `json:"password"`
	Code     string `json:"code"`
}

// updateSiteCookie 复刻 V2 `/site/cookie/{site_id}`（app/api/endpoints/site.py:167–242）：
// 站点不存在 404；缺少用户名/密码 422；成功或失败都以 `{success,message}` 返回。
func (d Dependencies) updateSiteCookie(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.Atoi(strings.TrimSpace(chi.URLParam(r, "site_id")))
	if err != nil {
		writeError(w, http.StatusUnprocessableEntity, "站点 ID 无效")
		return
	}
	store := d.siteStore()
	if store == nil {
		writeError(w, http.StatusServiceUnavailable, "站点存储未初始化")
		return
	}
	if _, err := store.RuntimeSiteByID(r.Context(), id); errors.Is(err, pgx.ErrNoRows) {
		writeError(w, http.StatusNotFound, fmt.Sprintf("站点 %d 不存在！", id))
		return
	} else if err != nil {
		writeError(w, http.StatusInternalServerError, "查询站点失败")
		return
	}
	username := strings.TrimSpace(r.URL.Query().Get("username"))
	password := r.URL.Query().Get("password")
	code := r.URL.Query().Get("code")
	if r.Method == http.MethodPost {
		decoder := json.NewDecoder(r.Body)
		var input siteCookieUpdateRequest
		if err := decoder.Decode(&input); err != nil {
			writeError(w, http.StatusBadRequest, "站点 Cookie 更新请求无效")
			return
		}
		username = strings.TrimSpace(input.Username)
		password = input.Password
		code = input.Code
	}
	if username == "" || password == "" {
		writeError(w, http.StatusUnprocessableEntity, "站点用户名和密码不能为空")
		return
	}
	if d.SiteCookieUpdater == nil {
		writeJSON(w, http.StatusOK, Response{Success: false, Message: "站点 Cookie 更新能力尚未接入"})
		return
	}
	success, message, err := d.SiteCookieUpdater.UpdateSiteCookie(r.Context(), id, username, password, code)
	if err != nil {
		if contextErr := r.Context().Err(); contextErr != nil {
			writeError(w, http.StatusServiceUnavailable, "站点 Cookie 更新已取消")
			return
		}
		writeError(w, http.StatusInternalServerError, "站点 Cookie 更新失败")
		return
	}
	writeJSON(w, http.StatusOK, Response{Success: success, Message: message})
}

// SiteCategoryCatalog 提供站点分类目录，对应 V2 `SitesHelper.get_indexer` 的
// `category` 字段；supported=false 表示该域名不是已知索引器（V2 返回 404）。
type SiteCategoryCatalog interface {
	SiteCategories(ctx context.Context, domain string) ([]map[string]any, bool, error)
}

// SiteIconStore 提供站点图标，对应 V2 `SiteIcon` 表。
type SiteIconStore interface {
	SiteIconByDomain(ctx context.Context, domain string) (base64Data string, iconURL string, found bool, err error)
}

func writeUnsupportedSite(w http.ResponseWriter, message string) {
	writeJSON(w, http.StatusOK, Response{Success: false, Message: message, Data: map[string]any{}})
}

func (d Dependencies) testSite(w http.ResponseWriter, r *http.Request) {
	store := d.siteStore()
	if store == nil {
		writeError(w, http.StatusServiceUnavailable, "站点存储未初始化")
		return
	}
	siteID, err := strconv.Atoi(strings.TrimSpace(chi.URLParam(r, "site_id")))
	if err != nil {
		writeError(w, http.StatusUnprocessableEntity, "站点 ID 无效")
		return
	}
	site, err := store.RuntimeSiteByID(r.Context(), siteID)
	if errors.Is(err, pgx.ErrNoRows) {
		writeError(w, http.StatusNotFound, fmt.Sprintf("站点 %d 不存在", siteID))
		return
	}
	if err != nil {
		writeError(w, http.StatusInternalServerError, "查询站点失败")
		return
	}
	if configInt(site.Config, "render", 0) != 0 {
		writeUnsupportedSite(w, "仿真渲染站点连接测试尚未支持")
		return
	}
	if configInt(site.Config, "proxy", 0) != 0 {
		writeUnsupportedSite(w, "代理站点连接测试尚未支持")
		return
	}
	var adapter indexer.Adapter
	if d.BuildSiteAdapter != nil {
		adapter, err = d.BuildSiteAdapter(site)
		if err != nil {
			writeUnsupportedSite(w, "站点适配器不支持连接测试")
			return
		}
	}
	if adapter == nil {
		adapters, selectErr := selectSearchAdapters(d.adapters(), strconv.Itoa(siteID))
		if selectErr != nil {
			writeError(w, http.StatusBadRequest, "站点 ID 无效")
			return
		}
		if len(adapters) > 0 {
			adapter = adapters[0]
		}
	}
	if adapter == nil {
		writeUnsupportedSite(w, "站点适配器未加载")
		return
	}
	if d.Search == nil {
		writeError(w, http.StatusServiceUnavailable, "站点测试服务未初始化")
		return
	}
	timeoutSeconds := configInt(site.Config, "timeout", 15)
	if timeoutSeconds < 1 {
		timeoutSeconds = 1
	}
	if timeoutSeconds > 60 {
		timeoutSeconds = 60
	}
	timeout := time.Duration(timeoutSeconds) * time.Second
	requestTimeout := d.Config.RequestTimeout
	if requestTimeout <= 0 {
		requestTimeout = 30 * time.Second
	}
	if requestTimeout < timeout {
		timeout = requestTimeout
	}
	ctx, cancel := context.WithTimeout(r.Context(), timeout)
	defer cancel()
	startedAt := time.Now()
	result := d.Search.TestSite(ctx, adapter)
	if result.Recordable {
		d.recordSiteTestStatistic(r, site, result.Success, time.Since(startedAt))
	}
	writeJSON(w, http.StatusOK, Response{Success: result.Success, Message: result.Message, Data: map[string]any{}})
}

func (d Dependencies) recordSiteTestStatistic(r *http.Request, site database.Site, success bool, elapsed time.Duration) {
	store := d.siteStatisticStore()
	if store == nil {
		return
	}
	domain := siteStatisticDomain(site)
	if domain == "" {
		if d.Logger != nil {
			d.Logger.Warn("跳过缺少域名的站点连接统计", "request_id", requestID(r.Context()), "site_id", site.ID)
		}
		return
	}

	writeCtx, cancel := context.WithTimeout(context.WithoutCancel(r.Context()), siteStatisticWriteTimeout)
	defer cancel()
	if err := store.RecordSiteTest(writeCtx, domain, success, elapsed); err != nil && d.Logger != nil {
		d.Logger.Warn("记录站点连接统计失败",
			"request_id", requestID(r.Context()),
			"site_id", site.ID,
			"domain", domain,
			"error", err,
		)
	}
}

func (d Dependencies) siteByDomain(w http.ResponseWriter, r *http.Request) {
	store := d.siteStore()
	if store == nil {
		writeError(w, http.StatusServiceUnavailable, "站点存储未初始化")
		return
	}
	raw := chi.URLParam(r, "site_url")
	if decoded, err := url.PathUnescape(raw); err == nil {
		raw = decoded
	}
	wanted := strings.ToLower(strings.TrimSpace(raw))
	if parsed, err := url.Parse(wanted); err == nil && parsed.Hostname() != "" {
		wanted = strings.ToLower(parsed.Hostname())
	}
	sites, err := store.ListSites(r.Context())
	if err != nil {
		writeError(w, http.StatusInternalServerError, "查询站点失败")
		return
	}
	for _, site := range sites {
		view := siteView(site)
		if strings.EqualFold(view["domain"].(string), wanted) {
			writeJSON(w, http.StatusOK, view)
			return
		}
	}
	writeError(w, http.StatusNotFound, "站点不存在")
}

func (d Dependencies) siteMapping(w http.ResponseWriter, r *http.Request) {
	store := d.siteStore()
	if store == nil {
		writeError(w, http.StatusServiceUnavailable, "站点存储未初始化")
		return
	}
	sites, err := store.ListSites(r.Context())
	if err != nil {
		writeError(w, http.StatusInternalServerError, "查询站点失败")
		return
	}
	mapping := make(map[string]string, len(sites))
	for _, site := range sites {
		view := siteView(site)
		domain, _ := view["domain"].(string)
		if domain != "" {
			mapping[domain] = site.Name
		}
	}
	writeSuccess(w, mapping)
}

func (d Dependencies) siteResources(w http.ResponseWriter, r *http.Request) {
	siteID, err := parseSiteID(chi.URLParam(r, "site_id"))
	if err != nil {
		writeError(w, http.StatusBadRequest, "站点 ID 无效")
		return
	}
	adapters, err := selectSearchAdapters(d.adapters(), strconv.Itoa(siteID))
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	if d.Search == nil || len(adapters) == 0 {
		writeJSON(w, http.StatusOK, []any{})
		return
	}
	resources, err := d.Search.Search(r.Context(), adapters, indexer.Query{
		Keyword: strings.TrimSpace(r.URL.Query().Get("keyword")),
		Page:    parseSearchPage(r.URL.Query().Get("page")),
		Limit:   100,
	}, nil)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "浏览站点资源失败")
		return
	}
	contexts := searchContexts(resources, d.Config.Location)
	result := make([]map[string]any, 0, len(contexts))
	for _, item := range contexts {
		if torrent, ok := item["torrent_info"].(map[string]any); ok {
			result = append(result, torrent)
		}
	}
	writeJSON(w, http.StatusOK, result)
}

func (d Dependencies) rssSites(w http.ResponseWriter, r *http.Request) {
	store := d.siteStore()
	if store == nil {
		writeError(w, http.StatusServiceUnavailable, "站点存储未初始化")
		return
	}
	sites, err := store.ListSites(r.Context())
	if err != nil {
		writeError(w, http.StatusInternalServerError, "查询站点失败")
		return
	}
	if settings, ok := store.(interface {
		GetSetting(context.Context, string, any) (bool, error)
	}); ok {
		selected := []int{}
		if _, err := settings.GetSetting(r.Context(), rssSitesSetting, &selected); err != nil {
			writeError(w, http.StatusInternalServerError, "查询订阅站点配置失败")
			return
		}
		if len(selected) > 0 {
			selectedIDs := make(map[int]struct{}, len(selected))
			for _, siteID := range selected {
				if siteID > 0 {
					selectedIDs[siteID] = struct{}{}
				}
			}
			filtered := make([]database.Site, 0, len(selectedIDs))
			for _, site := range sites {
				if _, include := selectedIDs[site.ID]; include {
					filtered = append(filtered, site)
				}
			}
			sites = filtered
		}
	}
	result := make([]map[string]any, 0, len(sites))
	for _, site := range sites {
		result = append(result, siteView(site))
	}
	writeJSON(w, http.StatusOK, result)
}

func (d Dependencies) siteStatistics(w http.ResponseWriter, r *http.Request) {
	store := d.siteStatisticStore()
	if store == nil {
		writeJSON(w, http.StatusOK, []database.SiteStatistic{})
		return
	}
	statistics, err := store.ListSiteStatistics(r.Context())
	if err != nil {
		writeError(w, http.StatusInternalServerError, "查询站点统计失败")
		return
	}
	result := make([]siteStatisticResponse, 0, len(statistics))
	for _, statistic := range statistics {
		result = append(result, newSiteStatisticResponse(statistic, true))
	}
	writeJSON(w, http.StatusOK, result)
}

func (d Dependencies) siteStatistic(w http.ResponseWriter, r *http.Request) {
	domain := siteStatisticDomainParam(chi.URLParam(r, "site_url"))
	store := d.siteStatisticStore()
	if store == nil {
		writeJSON(w, http.StatusOK, newSiteStatisticResponse(database.SiteStatistic{Domain: domain}, false))
		return
	}
	statistic, found, err := store.SiteStatisticByDomain(r.Context(), domain)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "查询站点统计失败")
		return
	}
	writeJSON(w, http.StatusOK, newSiteStatisticResponse(statistic, found))
}

type siteStatisticResponse struct {
	Domain       string         `json:"domain"`
	Success      int            `json:"success"`
	Fail         int            `json:"fail"`
	Seconds      int            `json:"seconds"`
	LastState    int            `json:"lst_state"`
	LastModified *string        `json:"lst_mod_date"`
	Note         map[string]int `json:"note"`
}

func newSiteStatisticResponse(statistic database.SiteStatistic, found bool) siteStatisticResponse {
	result := siteStatisticResponse{
		Domain: statistic.Domain, Success: statistic.Success, Fail: statistic.Fail,
		Seconds: statistic.Seconds, LastState: statistic.LastState,
	}
	if !found {
		return result
	}
	result.Note = statistic.Note
	if statistic.LastModified != "" {
		modified := statistic.LastModified
		result.LastModified = &modified
	}
	return result
}

func (d Dependencies) siteStatisticStore() SiteStatisticStore {
	if d.SiteStatistics != nil {
		return d.SiteStatistics
	}
	if d.Store != nil {
		return d.Store
	}
	return nil
}

func siteStatisticDomain(site database.Site) string {
	domain := strings.TrimSuffix(strings.ToLower(strings.TrimSpace(site.Domain)), ".")
	if domain != "" {
		return domain
	}
	return normalizedSiteDomain(site.URL)
}

func siteStatisticDomainParam(raw string) string {
	if decoded, err := url.PathUnescape(raw); err == nil {
		raw = decoded
	}
	domain := strings.TrimSpace(raw)
	if parsed, err := url.Parse(domain); err == nil && parsed.Hostname() != "" {
		domain = parsed.Hostname()
	}
	return strings.TrimSuffix(strings.ToLower(strings.TrimSpace(domain)), ".")
}

func (d Dependencies) supportingSites(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{})
}
