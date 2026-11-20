package httpapi

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strconv"
	"strings"

	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5"

	"github.com/moviepilot-go/moviepilot-go/internal/database"
)

type SiteStore interface {
	ListSites(context.Context) ([]database.Site, error)
	SiteByID(context.Context, int) (database.Site, error)
	RuntimeSiteByID(context.Context, int) (database.Site, error)
	UpsertSite(context.Context, database.Site) (int, error)
	UpdateSite(context.Context, database.Site) error
	DeleteSite(context.Context, int) (bool, error)
}

type siteRequest struct {
	ID            int            `json:"id"`
	Name          string         `json:"name"`
	Domain        string         `json:"domain"`
	URL           string         `json:"url"`
	Adapter       string         `json:"adapter"`
	Parser        string         `json:"parser"`
	Pri           *int           `json:"pri"`
	RSS           *string        `json:"rss"`
	Cookie        *string        `json:"cookie"`
	UA            *string        `json:"ua"`
	APIKey        *string        `json:"apikey"`
	Token         *string        `json:"token"`
	Proxy         siteFlag       `json:"proxy"`
	Filter        *string        `json:"filter"`
	Render        siteFlag       `json:"render"`
	Public        siteFlag       `json:"public"`
	Note          any            `json:"note"`
	Timeout       *int           `json:"timeout"`
	LimitInterval *int           `json:"limit_interval"`
	LimitCount    *int           `json:"limit_count"`
	LimitSeconds  *int           `json:"limit_seconds"`
	IsActive      *bool          `json:"is_active"`
	Enabled       *bool          `json:"enabled"`
	Downloader    *string        `json:"downloader"`
	Config        map[string]any `json:"config"`
}

func (d Dependencies) siteStore() SiteStore {
	if d.Sites != nil {
		return d.Sites
	}
	// 避免把类型化 nil 指针（*database.Store）装进接口后逃过 `store == nil` 检查。
	if d.Store == nil {
		return nil
	}
	return d.Store
}

func (d Dependencies) listSites(w http.ResponseWriter, r *http.Request) {
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
	result := make([]map[string]any, 0, len(sites))
	for _, site := range sites {
		result = append(result, siteView(site))
	}
	writeJSON(w, http.StatusOK, result)
}

func (d Dependencies) getSite(w http.ResponseWriter, r *http.Request) {
	store := d.siteStore()
	if store == nil {
		writeError(w, http.StatusServiceUnavailable, "站点存储未初始化")
		return
	}
	id, err := parseSiteID(chi.URLParam(r, "siteid"))
	if err != nil {
		writeError(w, http.StatusBadRequest, "站点 ID 无效")
		return
	}
	site, err := store.SiteByID(r.Context(), id)
	if errors.Is(err, pgx.ErrNoRows) {
		writeError(w, http.StatusNotFound, "站点不存在")
		return
	}
	if err != nil {
		writeError(w, http.StatusInternalServerError, "查询站点失败")
		return
	}
	writeJSON(w, http.StatusOK, siteView(site))
}

func (d Dependencies) createSite(w http.ResponseWriter, r *http.Request) {
	input, err := decodeSiteRequest(r)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	site, err := input.toSite(true, database.Site{})
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	if err := d.validateSite(site); err != nil {
		writeError(w, http.StatusBadRequest, "站点适配器配置无效")
		return
	}
	store := d.siteStore()
	if store == nil {
		writeError(w, http.StatusServiceUnavailable, "站点存储未初始化")
		return
	}
	id, err := store.UpsertSite(r.Context(), site)
	if err != nil {
		if errors.Is(err, database.ErrSiteConflict) {
			if reloadErr := d.reloadSites(r.Context()); reloadErr != nil {
				writeSiteReloadFailure(w, 0)
				return
			}
			writeUnsupportedSite(w, "站点已存在")
			return
		}
		writeError(w, http.StatusInternalServerError, "保存站点失败")
		return
	}
	if err := d.reloadSites(r.Context()); err != nil {
		writeSiteReloadFailure(w, id)
		return
	}
	writeSuccess(w, map[string]any{"id": id})
}

func (d Dependencies) updateSite(w http.ResponseWriter, r *http.Request) {
	input, err := decodeSiteRequest(r)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	id := input.ID
	routeID := strings.TrimSpace(chi.URLParam(r, "siteid"))
	if routeID != "" {
		parsedRouteID, routeErr := parseSiteID(routeID)
		if routeErr != nil {
			writeError(w, http.StatusBadRequest, "站点 ID 无效")
			return
		}
		if id > 0 && id != parsedRouteID {
			writeError(w, http.StatusBadRequest, "请求体站点 ID 与路径不一致")
			return
		}
		id = parsedRouteID
	}
	if id <= 0 {
		id, err = parseSiteID(routeID)
		if err != nil {
			writeError(w, http.StatusBadRequest, "站点 ID 无效")
			return
		}
	}
	store := d.siteStore()
	if store == nil {
		writeError(w, http.StatusServiceUnavailable, "站点存储未初始化")
		return
	}
	current, err := store.RuntimeSiteByID(r.Context(), id)
	if errors.Is(err, pgx.ErrNoRows) {
		writeError(w, http.StatusNotFound, "站点不存在")
		return
	}
	if err != nil {
		writeError(w, http.StatusInternalServerError, "查询站点失败")
		return
	}
	site, err := input.toSite(false, current)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	if err := d.validateSite(site); err != nil {
		writeError(w, http.StatusBadRequest, "站点适配器配置无效")
		return
	}
	if err := store.UpdateSite(r.Context(), site); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			writeError(w, http.StatusNotFound, "站点不存在")
			return
		}
		if errors.Is(err, database.ErrSiteConflict) {
			writeUnsupportedSite(w, "站点名称或域名已存在")
			return
		}
		writeError(w, http.StatusInternalServerError, "更新站点失败")
		return
	}
	if err := d.reloadSites(r.Context()); err != nil {
		writeSiteReloadFailure(w, id)
		return
	}
	writeSuccess(w, nil)
}

func (d Dependencies) deleteSite(w http.ResponseWriter, r *http.Request) {
	id, err := parseSiteID(chi.URLParam(r, "siteid"))
	if err != nil {
		writeError(w, http.StatusBadRequest, "站点 ID 无效")
		return
	}
	store := d.siteStore()
	if store == nil {
		writeError(w, http.StatusServiceUnavailable, "站点存储未初始化")
		return
	}
	deleted, err := store.DeleteSite(r.Context(), id)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "删除站点失败")
		return
	}
	if !deleted {
		if reloadErr := d.reloadSites(r.Context()); reloadErr != nil {
			writeSiteReloadFailure(w, id)
			return
		}
		writeError(w, http.StatusNotFound, "站点不存在")
		return
	}
	if err := d.reloadSites(r.Context()); err != nil {
		writeSiteReloadFailure(w, id)
		return
	}
	writeSuccess(w, nil)
}

func decodeSiteRequest(r *http.Request) (siteRequest, error) {
	decoder := json.NewDecoder(r.Body)
	decoder.UseNumber()
	var input siteRequest
	if err := decoder.Decode(&input); err != nil {
		return siteRequest{}, errors.New("站点请求体必须是有效 JSON")
	}
	var trailing any
	if err := decoder.Decode(&trailing); err != io.EOF {
		return siteRequest{}, errors.New("站点请求体包含多余内容")
	}
	return input, nil
}

func (input siteRequest) toSite(create bool, current database.Site) (database.Site, error) {
	site := current
	if !create && input.ID > 0 {
		site.ID = input.ID
	}
	if input.Name != "" {
		site.Name = strings.TrimSpace(input.Name)
	}
	if input.URL != "" {
		site.URL = strings.TrimSpace(input.URL)
	}
	adapter := strings.TrimSpace(input.Adapter)
	if adapter == "" {
		adapter = strings.TrimSpace(input.Parser)
	}
	if adapter == "" {
		adapter = configString(input.Config, "adapter")
	}
	if adapter == "" && create {
		adapter = inferSiteAdapter(input.URL, input.Name)
		if adapter == "" && configString(input.Config, "format") != "" {
			adapter = "declarative"
		}
	}
	if adapter != "" {
		site.Adapter = adapter
	}
	if input.IsActive != nil {
		site.Enabled = *input.IsActive
	} else if input.Enabled != nil {
		site.Enabled = *input.Enabled
	} else if create {
		site.Enabled = true
	}
	if input.Config != nil {
		site.Config = make(map[string]any, len(input.Config))
		for key, value := range input.Config {
			site.Config[key] = value
		}
		for key, value := range current.Config {
			if sensitiveSiteInputKey(key) {
				if _, supplied := site.Config[key]; !supplied {
					site.Config[key] = value
				}
			}
		}
	} else if site.Config == nil {
		site.Config = make(map[string]any)
	}
	applySiteConfigFields(site.Config, input)
	if site.Name == "" {
		site.Name = inferSiteName(site.URL)
	}
	site.Domain = normalizedSiteDomain(site.URL)
	if site.Domain == "" {
		return database.Site{}, errors.New("站点地址必须是有效的 HTTP 或 HTTPS URL")
	}
	if site.Name == "" || site.URL == "" || site.Adapter == "" {
		return database.Site{}, errors.New("站点名称、地址和适配器不能为空")
	}
	return site, nil
}

func siteView(site database.Site) map[string]any {
	domain := site.Domain
	if domain == "" {
		domain = normalizedSiteDomain(site.URL)
	}
	publicConfig := make(map[string]any, len(site.Config))
	for key, value := range site.Config {
		if sensitiveSiteInputKey(key) {
			continue
		}
		publicConfig[key] = value
	}
	ua, _ := configValue(site.Config, "ua")
	return map[string]any{
		"id": site.ID, "name": site.Name, "url": site.URL, "domain": domain,
		"adapter": site.Adapter, "parser": site.Adapter, "enabled": site.Enabled, "is_active": site.Enabled,
		"pri": configInt(site.Config, "pri", 0), "rss": configOrNil(site.Config, "rss"),
		"cookie": nil, "ua": ua, "apikey": nil, "token": nil,
		"proxy": configInt(site.Config, "proxy", 0), "filter": configOrNil(site.Config, "filter"),
		"render": configInt(site.Config, "render", 0), "public": configInt(site.Config, "public", 0),
		"note": configOrNil(site.Config, "note"), "timeout": configInt(site.Config, "timeout", 15),
		"limit_interval": configOrNil(site.Config, "limit_interval"),
		"limit_count":    configOrNil(site.Config, "limit_count"),
		"limit_seconds":  configOrNil(site.Config, "limit_seconds"),
		"downloader":     configOrNil(site.Config, "downloader"), "config": publicConfig,
	}
}

func applySiteConfigFields(config map[string]any, input siteRequest) {
	if input.Pri != nil {
		config["pri"] = *input.Pri
	}
	applySiteString(config, "rss", input.RSS)
	applySiteString(config, "ua", input.UA)
	applySiteString(config, "cookie", input.Cookie)
	applySiteString(config, "apikey", input.APIKey)
	applySiteString(config, "token", input.Token)
	if input.Proxy.Set {
		config["proxy"] = input.Proxy.Value
	}
	applySiteString(config, "filter", input.Filter)
	if input.Render.Set {
		config["render"] = input.Render.Value
	}
	if input.Public.Set {
		config["public"] = input.Public.Value
	}
	if input.Note != nil {
		config["note"] = input.Note
	}
	if input.Timeout != nil {
		config["timeout"] = *input.Timeout
	}
	if input.LimitInterval != nil {
		config["limit_interval"] = *input.LimitInterval
	}
	if input.LimitCount != nil {
		config["limit_count"] = *input.LimitCount
	}
	if input.LimitSeconds != nil {
		config["limit_seconds"] = *input.LimitSeconds
	}
	applySiteString(config, "downloader", input.Downloader)
}

func applySiteString(config map[string]any, key string, value *string) {
	if value == nil {
		return
	}
	deleteSiteConfigKey(config, key)
	if *value != "" {
		config[key] = *value
	}
}

func deleteSiteConfigKey(config map[string]any, key string) {
	for existing := range config {
		if strings.EqualFold(strings.TrimSpace(existing), key) {
			delete(config, existing)
		}
	}
}

func parseSiteID(value string) (int, error) {
	id, err := strconv.Atoi(strings.TrimSpace(value))
	if err != nil || id <= 0 {
		return 0, errors.New("invalid site ID")
	}
	return id, nil
}

func (d Dependencies) validateSite(site database.Site) error {
	if d.SiteValidator == nil {
		return nil
	}
	return d.SiteValidator(site)
}

func (d Dependencies) reloadSites(ctx context.Context) error {
	if d.ReloadSites == nil {
		return nil
	}
	return d.ReloadSites(ctx)
}

func writeSiteReloadFailure(w http.ResponseWriter, id int) {
	writeJSON(w, http.StatusServiceUnavailable, Response{
		Success: false,
		Message: "站点已持久化，但运行态重载失败，请稍后重试",
		Data:    map[string]any{"id": id, "reload": false},
	})
}
