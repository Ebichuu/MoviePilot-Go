package httpapi

import (
	"crypto/subtle"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"

	"github.com/moviepilot-go/moviepilot-go/internal/database"
)

type seerrWebhookRequest struct {
	NotificationType string `json:"notification_type"`
	Subject          string `json:"subject"`
	Media            struct {
		MediaType string `json:"media_type"`
		TMDBID    any    `json:"tmdbId"`
	} `json:"media"`
	Request struct {
		RequestedByUsername string `json:"requestedBy_username"`
	} `json:"request"`
	Extra []struct {
		Name  string `json:"name"`
		Value string `json:"value"`
	} `json:"extra"`
}

// seerrSubscribe 对应 V2 `POST /api/v1/subscribe/seerr`：
// 接收 Overseerr / Jellyseerr 审批通知并自动创建电影/分季剧集订阅。
func (d Dependencies) seerrSubscribe(w http.ResponseWriter, r *http.Request) {
	if !d.authorizeSeerr(r) {
		writeError(w, http.StatusBadRequest, "授权失败")
		return
	}
	if d.Store == nil {
		writeJSON(w, http.StatusOK, Response{Success: false, Message: "订阅存储未初始化"})
		return
	}

	limit := d.Config.MaxRequestBody
	if limit <= 0 {
		limit = 1 << 20
	}
	bodyBytes, err := io.ReadAll(io.LimitReader(r.Body, limit))
	if err != nil || len(bodyBytes) == 0 {
		writeError(w, http.StatusInternalServerError, "报文内容为空")
		return
	}

	var req seerrWebhookRequest
	if err := json.Unmarshal(bodyBytes, &req); err != nil {
		writeJSON(w, http.StatusOK, Response{Success: false, Message: "报文 JSON 格式无效"})
		return
	}

	nType := strings.ToUpper(strings.TrimSpace(req.NotificationType))
	if nType == "TEST_NOTIFICATION" {
		writeJSON(w, http.StatusOK, Response{Success: true, Message: "Seerr 测试通知接收成功"})
		return
	}
	if nType != "MEDIA_APPROVED" && nType != "MEDIA_AUTO_APPROVED" {
		writeJSON(w, http.StatusOK, Response{Success: false, Message: "不支持的通知类型"})
		return
	}

	subject := strings.TrimSpace(req.Subject)
	tmdbID := normalizeSeerrTMDBID(req.Media.TMDBID)
	if subject == "" || tmdbID == "" {
		writeJSON(w, http.StatusOK, Response{Success: false, Message: "请求参数不正确"})
		return
	}

	mediaType := "电视剧"
	if strings.EqualFold(strings.TrimSpace(req.Media.MediaType), "movie") {
		mediaType = "电影"
	}

	title, year := splitTitleYear(subject)
	username := strings.TrimSpace(req.Request.RequestedByUsername)
	if username == "" {
		username = strings.TrimSpace(d.Config.BootstrapUsername)
	}
	if username == "" {
		username = "admin"
	}

	createdIDs := make([]int64, 0, 4)
	if mediaType == "电影" {
		id, createErr := d.Store.CreateSubscription(r.Context(), database.NewSubscription{
			Username:    username,
			Name:        title,
			MediaType:   mediaType,
			Year:        year,
			MediaSource: "themoviedb",
			MediaID:     tmdbID,
			State:       "R",
			Config: map[string]any{
				"source": "seerr",
			},
		})
		if createErr != nil {
			writeJSON(w, http.StatusOK, Response{Success: false, Message: "创建 Seerr 电影订阅失败"})
			return
		}
		createdIDs = append(createdIDs, id)
	} else {
		seasons := parseSeerrSeasons(req.Extra)
		if len(seasons) == 0 {
			seasons = []int{1}
		}
		for _, season := range seasons {
			s := season
			id, createErr := d.Store.CreateSubscription(r.Context(), database.NewSubscription{
				Username:    username,
				Name:        title,
				MediaType:   mediaType,
				Year:        year,
				Season:      &s,
				MediaSource: "themoviedb",
				MediaID:     tmdbID,
				State:       "R",
				Config: map[string]any{
					"source": "seerr",
				},
			})
			if createErr != nil {
				writeJSON(w, http.StatusOK, Response{Success: false, Message: "创建 Seerr 剧集订阅失败"})
				return
			}
			createdIDs = append(createdIDs, id)
		}
	}

	if d.Scheduler != nil {
		_, _ = d.Scheduler.RunScheduler(r.Context(), "subscribe_search")
	}

	writeJSON(w, http.StatusOK, Response{
		Success: true,
		Message: "已添加 Seerr 订阅",
		Data: map[string]any{
			"ids":     createdIDs,
			"tmdbid":  tmdbID,
			"type":    mediaType,
			"subject": subject,
		},
	})
}

func (d Dependencies) authorizeSeerr(r *http.Request) bool {
	// 1. 兼容 V2 Overseerr/Jellyseerr：Authorization 头直接放 API_TOKEN，或 Bearer <API_TOKEN>，或 ?token=。
	expected := strings.TrimSpace(d.Config.APIToken)
	authHeader := strings.TrimSpace(r.Header.Get("Authorization"))
	if expected != "" {
		candidates := []string{
			authHeader,
			strings.TrimSpace(r.URL.Query().Get("token")),
		}
		if strings.HasPrefix(strings.ToLower(authHeader), "bearer ") {
			candidates = append(candidates, strings.TrimSpace(authHeader[7:]))
		}
		for _, candidate := range candidates {
			if candidate != "" && subtle.ConstantTimeCompare([]byte(candidate), []byte(expected)) == 1 {
				return true
			}
		}
	}
	// 2. 也支持已登录用户的 Bearer JWT。
	if d.Auth != nil && strings.HasPrefix(strings.ToLower(authHeader), "bearer ") {
		token := strings.TrimSpace(authHeader[7:])
		if _, err := d.Auth.VerifyAccessToken(token); err == nil {
			return true
		}
	}
	return false
}

func normalizeSeerrTMDBID(raw any) string {
	if raw == nil {
		return ""
	}
	var text string
	switch v := raw.(type) {
	case string:
		text = strings.TrimSpace(v)
	case float64:
		text = strconv.FormatInt(int64(v), 10)
	case json.Number:
		text = strings.TrimSpace(v.String())
	default:
		text = strings.TrimSpace(fmt.Sprint(v))
	}
	if n, err := strconv.Atoi(text); err == nil && n > 0 {
		return strconv.Itoa(n)
	}
	return ""
}

func parseSeerrSeasons(extras []struct {
	Name  string `json:"name"`
	Value string `json:"value"`
}) []int {
	var seasons []int
	for _, extra := range extras {
		if !strings.EqualFold(strings.TrimSpace(extra.Name), "Requested Seasons") {
			continue
		}
		for _, part := range strings.Split(extra.Value, ",") {
			if n, err := strconv.Atoi(strings.TrimSpace(part)); err == nil && n >= 0 {
				seasons = append(seasons, n)
			}
		}
		break
	}
	return seasons
}
