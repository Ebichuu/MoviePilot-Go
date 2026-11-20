package httpapi

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"

	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5"

	"github.com/moviepilot-go/moviepilot-go/internal/database"
	"github.com/moviepilot-go/moviepilot-go/internal/indexer"
)

// SiteUserDataStore defines the read contract consumed by V2 site-user APIs.
type SiteUserDataStore interface {
	LatestSiteUserData(context.Context) ([]database.SiteUserData, error)
	SiteUserDataByDomain(context.Context, string, string) ([]database.SiteUserData, error)
}

// SiteUserDataWriter is the write contract consumed only by the refresh endpoint.
type SiteUserDataWriter interface {
	SaveSiteUserData(context.Context, string, string, database.SiteUserData) error
}

// SiteUserDataEffects owns V2 side effects after a successful daily snapshot save.
type SiteUserDataEffects interface {
	AfterSiteUserDataRefresh(context.Context, int, string, string, indexer.SiteUserData) error
}

type latestSiteUserDataResponse struct {
	Domain                string          `json:"domain"`
	Username              *string         `json:"username"`
	UserID                *string         `json:"userid"`
	UserLevel             *string         `json:"user_level"`
	JoinAt                *string         `json:"join_at"`
	Bonus                 *float64        `json:"bonus"`
	Upload                *float64        `json:"upload"`
	Download              *float64        `json:"download"`
	Ratio                 *float64        `json:"ratio"`
	Seeding               *float64        `json:"seeding"`
	Leeching              *float64        `json:"leeching"`
	SeedingSize           *float64        `json:"seeding_size"`
	LeechingSize          *float64        `json:"leeching_size"`
	SeedingInfo           json.RawMessage `json:"seeding_info"`
	MessageUnread         *int            `json:"message_unread"`
	MessageUnreadContents json.RawMessage `json:"message_unread_contents"`
	ErrMsg                *string         `json:"err_msg"`
	UpdatedDay            string          `json:"updated_day"`
	UpdatedTime           string          `json:"updated_time"`
}

type latestSiteUserDataV2Response struct {
	Success bool                         `json:"success"`
	Message string                       `json:"message"`
	Data    []latestSiteUserDataResponse `json:"data"`
}

type siteUserDataHistoryResponse struct {
	Success     bool                    `json:"success"`
	Message     *string                 `json:"message"`
	MessageI18N *string                 `json:"message_i18n"`
	Data        []database.SiteUserData `json:"data"`
}

type siteUserDataRefreshResponse struct {
	Success     bool    `json:"success"`
	Message     *string `json:"message"`
	MessageI18N *string `json:"message_i18n"`
	Data        any     `json:"data"`
}

func (d Dependencies) requireSiteUserDataManage(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		user, ok := d.authorizedUser(r)
		if !ok {
			writeError(w, http.StatusBadRequest, "用户权限不足")
			return
		}
		if user.SuperUser {
			next.ServeHTTP(w, r)
			return
		}
		manage, _ := user.Permissions["manage"].(bool)
		if !manage {
			writeError(w, http.StatusBadRequest, "用户权限不足")
			return
		}
		next.ServeHTTP(w, r)
	})
}

func (d Dependencies) latestSiteUserData(w http.ResponseWriter, r *http.Request) {
	store := d.siteUserDataStore()
	if store == nil {
		d.writeLatestSiteUserData(w, r, []latestSiteUserDataResponse{})
		return
	}
	rows, err := store.LatestSiteUserData(r.Context())
	if err != nil {
		writeError(w, http.StatusInternalServerError, "查询站点用户数据失败")
		return
	}
	result := make([]latestSiteUserDataResponse, 0, len(rows))
	for _, row := range rows {
		result = append(result, newLatestSiteUserDataResponse(row))
	}
	d.writeLatestSiteUserData(w, r, result)
}

func (d Dependencies) siteUserDataHistory(w http.ResponseWriter, r *http.Request) {
	siteID, err := strconv.Atoi(chi.URLParam(r, "site_id"))
	if err != nil {
		writeError(w, http.StatusUnprocessableEntity, "站点 ID 无效")
		return
	}
	sites := d.siteStore()
	if sites == nil {
		writeError(w, http.StatusServiceUnavailable, "站点存储未初始化")
		return
	}
	site, err := sites.SiteByID(r.Context(), siteID)
	if errors.Is(err, pgx.ErrNoRows) {
		writeError(w, http.StatusNotFound, fmt.Sprintf("站点 %d 不存在", siteID))
		return
	}
	if err != nil {
		writeError(w, http.StatusInternalServerError, "查询站点失败")
		return
	}
	store := d.siteUserDataStore()
	if store == nil {
		writeJSON(w, http.StatusOK, siteUserDataHistoryResponse{Data: []database.SiteUserData{}})
		return
	}
	rows, err := store.SiteUserDataByDomain(r.Context(), site.Domain, r.URL.Query().Get("workdate"))
	if err != nil {
		writeError(w, http.StatusInternalServerError, "查询站点用户数据失败")
		return
	}
	if rows == nil {
		rows = []database.SiteUserData{}
	}
	writeJSON(w, http.StatusOK, siteUserDataHistoryResponse{Success: len(rows) > 0, Data: rows})
}

func (d Dependencies) refreshSiteUserData(w http.ResponseWriter, r *http.Request) {
	siteID, err := strconv.Atoi(chi.URLParam(r, "site_id"))
	if err != nil {
		writeError(w, http.StatusUnprocessableEntity, "站点 ID 无效")
		return
	}
	sites := d.siteStore()
	if sites == nil {
		writeError(w, http.StatusServiceUnavailable, "站点存储未初始化")
		return
	}
	site, err := sites.SiteByID(r.Context(), siteID)
	if errors.Is(err, pgx.ErrNoRows) {
		writeError(w, http.StatusNotFound, fmt.Sprintf("站点 %d 不存在", siteID))
		return
	}
	if err != nil {
		writeError(w, http.StatusInternalServerError, "查询站点失败")
		return
	}

	adapter, ok := d.siteUserDataAdapter(r.Context(), sites, site)
	if !ok {
		d.writeSiteUserDataRefreshFailure(w, "站点不支持索引或未通过用户认证！")
		return
	}
	if _, ok := adapter.(indexer.SiteUserDataFetcher); !ok {
		d.writeSiteUserDataRefreshSuccess(w, map[string]any{})
		return
	}
	if d.Search == nil {
		writeError(w, http.StatusServiceUnavailable, "站点刷新服务未初始化")
		return
	}
	userData, err := d.Search.FetchSiteUserData(r.Context(), adapter, indexer.SiteUserDataOptions{
		FetchMessages: d.siteMessagesEnabled(),
	})
	if err != nil {
		if d.Logger != nil {
			d.Logger.WarnContext(r.Context(), "刷新站点用户数据失败",
				"request_id", requestID(r.Context()), "site_id", siteID)
		}
		d.writeSiteUserDataRefreshSuccess(w, map[string]any{})
		return
	}
	writer := d.siteUserDataWriter()
	if writer == nil {
		writeError(w, http.StatusServiceUnavailable, "站点用户数据存储未初始化")
		return
	}
	if err := writer.SaveSiteUserData(r.Context(), site.Domain, site.Name, databaseSiteUserData(userData)); err != nil {
		writeError(w, http.StatusInternalServerError, "保存站点用户数据失败")
		return
	}
	if d.SiteUserDataEffects != nil {
		if err := d.SiteUserDataEffects.AfterSiteUserDataRefresh(
			r.Context(), site.ID, site.Name, site.URL, userData,
		); err != nil {
			writeError(w, http.StatusInternalServerError, "处理站点刷新通知失败")
			return
		}
	}
	d.writeSiteUserDataRefreshSuccess(w, userData)
}

func (d Dependencies) siteMessagesEnabled() bool {
	if settings, ok := d.EnvironmentSettings.(interface{ SiteMessagesEnabled() bool }); ok {
		return settings.SiteMessagesEnabled()
	}
	return d.Config.SiteMessage
}

func (d Dependencies) siteUserDataAdapter(
	ctx context.Context,
	sites SiteStore,
	site database.Site,
) (indexer.Adapter, bool) {
	if d.BuildSiteAdapter != nil {
		runtimeSite, err := sites.RuntimeSiteByID(ctx, site.ID)
		if err != nil {
			return nil, false
		}
		adapter, err := d.BuildSiteAdapter(runtimeSite)
		return adapter, err == nil && adapter != nil
	}
	adapters, err := selectSearchAdapters(d.adapters(), strconv.Itoa(site.ID))
	if err != nil || len(adapters) == 0 {
		return nil, false
	}
	return adapters[0], true
}

func (d Dependencies) writeSiteUserDataRefreshSuccess(w http.ResponseWriter, data any) {
	writeJSON(w, http.StatusOK, siteUserDataRefreshResponse{Success: true, Data: data})
}

func (d Dependencies) writeSiteUserDataRefreshFailure(w http.ResponseWriter, message string) {
	writeJSON(w, http.StatusOK, siteUserDataRefreshResponse{
		Success: false, Message: &message, MessageI18N: &message, Data: map[string]any{},
	})
}

func (d Dependencies) siteUserDataWriter() SiteUserDataWriter {
	if writer, ok := d.Sites.(SiteUserDataWriter); ok {
		return writer
	}
	if d.Store != nil {
		return d.Store
	}
	return nil
}

func databaseSiteUserData(data indexer.SiteUserData) database.SiteUserData {
	bonus, upload, download, ratio := data.Bonus, float64(data.Upload), float64(data.Download), data.Ratio
	seeding, leeching := float64(data.Seeding), float64(data.Leeching)
	seedingSize, leechingSize := float64(data.SeedingSize), float64(data.LeechingSize)
	messageUnread := data.MessageUnread
	seedingInfo, _ := json.Marshal(data.SeedingInfo)
	messageContents, _ := json.Marshal(data.MessageUnreadContents)
	var userID *string
	if data.UserID != nil {
		value := fmt.Sprint(data.UserID)
		userID = &value
	}
	updatedTime := ""
	if data.UpdatedTime != nil {
		updatedTime = *data.UpdatedTime
	}
	return database.SiteUserData{
		Domain: data.Domain, Username: data.Username, UserID: userID, UserLevel: data.UserLevel, JoinAt: data.JoinAt,
		Bonus: &bonus, Upload: &upload, Download: &download, Ratio: &ratio,
		Seeding: &seeding, Leeching: &leeching, SeedingSize: &seedingSize, LeechingSize: &leechingSize,
		SeedingInfo: seedingInfo, MessageUnread: &messageUnread, MessageUnreadContents: messageContents,
		ErrMsg: data.ErrMsg, UpdatedDay: data.UpdatedDay, UpdatedTime: updatedTime,
	}
}

func (d Dependencies) siteUserDataStore() SiteUserDataStore {
	if store, ok := d.Sites.(SiteUserDataStore); ok {
		return store
	}
	if d.Store != nil {
		return d.Store
	}
	return nil
}

func (d Dependencies) writeLatestSiteUserData(
	w http.ResponseWriter,
	r *http.Request,
	data []latestSiteUserDataResponse,
) {
	if strings.HasPrefix(r.URL.Path, "/api/v2/") {
		writeJSON(w, http.StatusOK, latestSiteUserDataV2Response{Success: true, Message: "", Data: data})
		return
	}
	writeJSON(w, http.StatusOK, data)
}

func newLatestSiteUserDataResponse(row database.SiteUserData) latestSiteUserDataResponse {
	return latestSiteUserDataResponse{
		Domain: row.Domain, Username: row.Username, UserID: row.UserID, UserLevel: row.UserLevel,
		JoinAt: row.JoinAt, Bonus: row.Bonus, Upload: row.Upload, Download: row.Download, Ratio: row.Ratio,
		Seeding: row.Seeding, Leeching: row.Leeching, SeedingSize: row.SeedingSize,
		LeechingSize: row.LeechingSize, SeedingInfo: row.SeedingInfo, MessageUnread: row.MessageUnread,
		MessageUnreadContents: row.MessageUnreadContents, ErrMsg: row.ErrMsg,
		UpdatedDay: row.UpdatedDay, UpdatedTime: row.UpdatedTime,
	}
}
