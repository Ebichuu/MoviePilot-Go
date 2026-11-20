package httpapi

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/moviepilot-go/moviepilot-go/internal/database"
	"github.com/moviepilot-go/moviepilot-go/internal/subscription"
)

func newShareKey() string {
	buffer := make([]byte, 8)
	if _, err := rand.Read(buffer); err != nil {
		return strconv.FormatInt(time.Now().UnixNano(), 16)
	}
	return hex.EncodeToString(buffer)
}

func subscribeShareV2(share database.SubscribeShare) map[string]any {
	season := any(nil)
	if share.Season != nil {
		season = *share.Season
	}
	return map[string]any{
		"id":           share.ID,
		"share_key":    share.ShareKey,
		"username":     share.Username,
		"title":        share.Title,
		"media_type":   share.MediaType,
		"year":         share.Year,
		"season":       season,
		"media_source": share.MediaSource,
		"media_id":     share.MediaID,
		"tmdbid":       share.MediaID,
		"sites":        share.Sites,
		"filter":       share.Filter,
		"description":  share.Description,
		"poster":       share.Poster,
		"follow_count": share.FollowCount,
		"created_at":   share.CreatedAt.Format(time.RFC3339),
	}
}

func readShareID(r *http.Request) int64 {
	value := strings.TrimSpace(r.URL.Query().Get("share_id"))
	if value == "" {
		var body struct {
			ShareID json.Number `json:"share_id"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err == nil {
			value = body.ShareID.String()
		}
	}
	id, _ := strconv.ParseInt(value, 10, 64)
	return id
}

// createSubscribeShare 对应 V2 `POST /subscribe/share`，把一条订阅的媒体信息固化成分享。
func (d Dependencies) createSubscribeShare(w http.ResponseWriter, r *http.Request) {
	claims, ok := claimsFromContext(r.Context())
	if !ok {
		writeJSON(w, http.StatusOK, Response{Success: false, Message: "未登录"})
		return
	}
	if d.Store == nil {
		writeJSON(w, http.StatusOK, Response{Success: false, Message: "存储未配置"})
		return
	}
	var body struct {
		SubscribeID json.Number `json:"subscribe_id"`
		Description string      `json:"description"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeJSON(w, http.StatusOK, Response{Success: false, Message: "请求体不是合法 JSON"})
		return
	}
	subscribeID, err := body.SubscribeID.Int64()
	if err != nil || subscribeID <= 0 {
		writeJSON(w, http.StatusOK, Response{Success: false, Message: "subscribe_id 无效"})
		return
	}
	items, err := d.Store.ListSubscriptions(r.Context(), claims.Username, claims.SuperUser)
	if err != nil {
		writeJSON(w, http.StatusOK, Response{Success: false, Message: "读取订阅失败：" + err.Error()})
		return
	}
	var matched *database.Subscription
	for index := range items {
		if items[index].ID == subscribeID {
			matched = &items[index]
			break
		}
	}
	if matched == nil {
		writeJSON(w, http.StatusOK, Response{Success: false, Message: "订阅不存在"})
		return
	}
	sites := []int{}
	if raw, exists := matched.Config["sites"]; exists {
		switch value := raw.(type) {
		case []any:
			for _, item := range value {
				if number, isNumber := item.(float64); isNumber {
					sites = append(sites, int(number))
				}
			}
		case []int:
			sites = append(sites, value...)
		}
	}
	filter := ""
	if raw, exists := matched.Config["filter"]; exists {
		if text, isText := raw.(string); isText {
			filter = text
		}
	}
	poster := ""
	if raw, exists := matched.Config["poster"]; exists {
		if text, isText := raw.(string); isText {
			poster = text
		}
	}
	created, err := d.Store.CreateSubscribeShare(r.Context(), database.SubscribeShare{
		ShareKey:    newShareKey(),
		Username:    claims.Username,
		Title:       matched.Name,
		MediaType:   matched.MediaType,
		Year:        matched.Year,
		Season:      matched.Season,
		MediaSource: matched.MediaSource,
		MediaID:     matched.MediaID,
		Sites:       sites,
		Filter:      filter,
		Description: strings.TrimSpace(body.Description),
		Poster:      poster,
	})
	if err != nil {
		writeJSON(w, http.StatusOK, Response{Success: false, Message: "创建分享失败：" + err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"success": true,
		"message": "分享创建成功",
		"data":    subscribeShareV2(created),
	})
}

// listSubscribeShares 对应 V2 `GET /subscribe/shares`。
func (d Dependencies) listSubscribeShares(w http.ResponseWriter, r *http.Request) {
	claims, ok := claimsFromContext(r.Context())
	if !ok {
		writeJSON(w, http.StatusOK, Response{Success: false, Message: "未登录"})
		return
	}
	if d.Store == nil {
		writeJSON(w, http.StatusOK, []any{})
		return
	}
	shares, err := d.Store.ListSubscribeShares(r.Context(), claims.Username)
	if err != nil {
		writeJSON(w, http.StatusOK, Response{Success: false, Message: "读取分享失败：" + err.Error()})
		return
	}
	items := make([]map[string]any, 0, len(shares))
	for _, share := range shares {
		items = append(items, subscribeShareV2(share))
	}
	writeJSON(w, http.StatusOK, items)
}

// deleteSubscribeShare 对应 V2 `DELETE /subscribe/share/{share_id}`。
func (d Dependencies) deleteSubscribeShare(w http.ResponseWriter, r *http.Request) {
	claims, ok := claimsFromContext(r.Context())
	if !ok {
		writeJSON(w, http.StatusOK, Response{Success: false, Message: "未登录"})
		return
	}
	if d.Store == nil {
		writeJSON(w, http.StatusOK, Response{Success: false, Message: "存储未配置"})
		return
	}
	shareID, _ := strconv.ParseInt(chi.URLParam(r, "share_id"), 10, 64)
	if shareID <= 0 {
		writeJSON(w, http.StatusOK, Response{Success: false, Message: "share_id 无效"})
		return
	}
	removed, err := d.Store.DeleteSubscribeShare(r.Context(), claims.Username, shareID)
	if err != nil {
		writeJSON(w, http.StatusOK, Response{Success: false, Message: "删除分享失败：" + err.Error()})
		return
	}
	if !removed {
		writeJSON(w, http.StatusOK, Response{Success: false, Message: "分享不存在或无权删除"})
		return
	}
	writeJSON(w, http.StatusOK, Response{Success: true, Message: "分享已删除"})
}

// subscribeShareStatistics 对应 V2 `GET /subscribe/share/statistics`。
func (d Dependencies) subscribeShareStatistics(w http.ResponseWriter, r *http.Request) {
	claims, ok := claimsFromContext(r.Context())
	if !ok {
		writeJSON(w, http.StatusOK, Response{Success: false, Message: "未登录"})
		return
	}
	if d.Store == nil {
		writeJSON(w, http.StatusOK, map[string]any{"total": 0, "follow_total": 0, "follow_count": 0})
		return
	}
	shares, err := d.Store.ListSubscribeShares(r.Context(), claims.Username)
	if err != nil {
		writeJSON(w, http.StatusOK, Response{Success: false, Message: "读取分享失败：" + err.Error()})
		return
	}
	follows := int64(0)
	for _, share := range shares {
		follows += share.FollowCount
	}
	following, _ := d.Store.ListSubscribeFollows(r.Context(), claims.Username)
	writeJSON(w, http.StatusOK, map[string]any{
		"total":        len(shares),
		"follow_total": follows,
		"follow_count": len(following),
	})
}

// listSubscribeFollows 对应 V2 `GET /subscribe/follow`。
func (d Dependencies) listSubscribeFollows(w http.ResponseWriter, r *http.Request) {
	claims, ok := claimsFromContext(r.Context())
	if !ok {
		writeJSON(w, http.StatusOK, Response{Success: false, Message: "未登录"})
		return
	}
	if d.Store == nil {
		writeJSON(w, http.StatusOK, []any{})
		return
	}
	shares, err := d.Store.ListSubscribeFollows(r.Context(), claims.Username)
	if err != nil {
		writeJSON(w, http.StatusOK, Response{Success: false, Message: "读取关注失败：" + err.Error()})
		return
	}
	items := make([]map[string]any, 0, len(shares))
	for _, share := range shares {
		items = append(items, subscribeShareV2(share))
	}
	writeJSON(w, http.StatusOK, items)
}

// followSubscribeShare 对应 V2 `POST /subscribe/follow`。
func (d Dependencies) followSubscribeShare(w http.ResponseWriter, r *http.Request) {
	claims, ok := claimsFromContext(r.Context())
	if !ok {
		writeJSON(w, http.StatusOK, Response{Success: false, Message: "未登录"})
		return
	}
	if d.Store == nil {
		writeJSON(w, http.StatusOK, Response{Success: false, Message: "存储未配置"})
		return
	}
	shareID := readShareID(r)
	if shareID <= 0 {
		writeJSON(w, http.StatusOK, Response{Success: false, Message: "share_id 无效"})
		return
	}
	if _, err := d.Store.FollowSubscribeShare(r.Context(), claims.Username, shareID); err != nil {
		writeJSON(w, http.StatusOK, Response{Success: false, Message: "关注失败：" + err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, Response{Success: true, Message: "已关注该分享"})
}

// unfollowSubscribeShare 对应 V2 `DELETE /subscribe/follow`。
func (d Dependencies) unfollowSubscribeShare(w http.ResponseWriter, r *http.Request) {
	claims, ok := claimsFromContext(r.Context())
	if !ok {
		writeJSON(w, http.StatusOK, Response{Success: false, Message: "未登录"})
		return
	}
	if d.Store == nil {
		writeJSON(w, http.StatusOK, Response{Success: false, Message: "存储未配置"})
		return
	}
	shareID := readShareID(r)
	if shareID <= 0 {
		writeJSON(w, http.StatusOK, Response{Success: false, Message: "share_id 无效"})
		return
	}
	removed, err := d.Store.UnfollowSubscribeShare(r.Context(), claims.Username, shareID)
	if err != nil {
		writeJSON(w, http.StatusOK, Response{Success: false, Message: "取消关注失败：" + err.Error()})
		return
	}
	if !removed {
		writeJSON(w, http.StatusOK, Response{Success: false, Message: "尚未关注该分享"})
		return
	}
	writeJSON(w, http.StatusOK, Response{Success: true, Message: "已取消关注"})
}

// forkSubscribeShare 对应 V2 `POST /subscribe/fork`，把分享固化成自己的订阅。
func (d Dependencies) forkSubscribeShare(w http.ResponseWriter, r *http.Request) {
	claims, ok := claimsFromContext(r.Context())
	if !ok {
		writeJSON(w, http.StatusOK, Response{Success: false, Message: "未登录"})
		return
	}
	if d.Store == nil {
		writeJSON(w, http.StatusOK, Response{Success: false, Message: "存储未配置"})
		return
	}
	shareID := readShareID(r)
	if shareID <= 0 {
		writeJSON(w, http.StatusOK, Response{Success: false, Message: "share_id 无效"})
		return
	}
	share, err := d.Store.GetSubscribeShare(r.Context(), shareID)
	if err != nil {
		writeJSON(w, http.StatusOK, Response{Success: false, Message: "分享不存在"})
		return
	}
	config := map[string]any{"sites": share.Sites}
	if share.Filter != "" {
		config["filter"] = share.Filter
	}
	id, err := d.Store.CreateSubscription(r.Context(), database.NewSubscription{
		Username:    claims.Username,
		Name:        share.Title,
		MediaType:   share.MediaType,
		Year:        share.Year,
		Season:      share.Season,
		MediaSource: share.MediaSource,
		MediaID:     share.MediaID,
		State:       subscription.PendingSearch,
		Config:      config,
	})
	if err != nil {
		writeJSON(w, http.StatusOK, Response{Success: false, Message: "创建订阅失败：" + err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"success": true,
		"message": "已将分享转为订阅",
		"data":    map[string]any{"id": id},
	})
}

// checkSubscribeShare 对应 V2 `GET /subscribe/check`，按 share_key 查分享。
func (d Dependencies) checkSubscribeShare(w http.ResponseWriter, r *http.Request) {
	key := strings.TrimSpace(r.URL.Query().Get("share_key"))
	if key == "" {
		key = strings.TrimSpace(r.URL.Query().Get("sharekey"))
	}
	if key == "" {
		writeJSON(w, http.StatusOK, Response{Success: false, Message: "share_key 不能为空"})
		return
	}
	if d.Store == nil {
		writeJSON(w, http.StatusOK, Response{Success: false, Message: "存储未配置"})
		return
	}
	share, err := d.Store.GetSubscribeShareByKey(r.Context(), key)
	if err != nil {
		writeJSON(w, http.StatusOK, Response{Success: false, Message: "分享不存在"})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"success": true, "data": subscribeShareV2(share)})
}

// refreshSubscribeShare 对应 V2 `GET /subscribe/refresh`，重新读取分享最新状态。
func (d Dependencies) refreshSubscribeShare(w http.ResponseWriter, r *http.Request) {
	shareID := readShareID(r)
	if shareID <= 0 {
		writeJSON(w, http.StatusOK, Response{Success: false, Message: "share_id 无效"})
		return
	}
	if d.Store == nil {
		writeJSON(w, http.StatusOK, Response{Success: false, Message: "存储未配置"})
		return
	}
	share, err := d.Store.GetSubscribeShare(r.Context(), shareID)
	if err != nil {
		writeJSON(w, http.StatusOK, Response{Success: false, Message: "分享不存在"})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"success": true, "data": subscribeShareV2(share)})
}

// subscribeFileList 对应 V2 `GET /subscribe/files/{subscribe_id}`：
// 从 download_files 与 download_intents 里按订阅读真实文件清单。
func (d Dependencies) subscribeFileList(w http.ResponseWriter, r *http.Request) {
	subscribeID, err := strconv.ParseInt(chi.URLParam(r, "subscribe_id"), 10, 64)
	if err != nil || subscribeID <= 0 {
		writeJSON(w, http.StatusOK, []any{})
		return
	}
	if d.Store == nil {
		writeJSON(w, http.StatusOK, []any{})
		return
	}
	files, listErr := d.Store.ListSubscriptionDownloadFiles(r.Context(), subscribeID)
	if listErr != nil {
		writeJSON(w, http.StatusOK, Response{Success: false, Message: "读取订阅文件失败：" + listErr.Error()})
		return
	}
	items := make([]map[string]any, 0, len(files))
	for _, file := range files {
		items = append(items, map[string]any{
			"path":         file.Path,
			"size":         file.SizeBytes,
			"selected":     file.Selected,
			"torrent_hash": file.TorrentHash,
			"state":        file.IntentState,
		})
	}
	writeJSON(w, http.StatusOK, items)
}
