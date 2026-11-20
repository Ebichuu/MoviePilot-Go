package httpapi

import "net/http"

// webhookProbe 对应 V2 `GET /webhook/`：作为连通性探测返回成功。
func webhookProbe(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, Response{Success: true})
}

// webhookReceive 对应 V2 `POST /webhook/`：本项目没有配置外部 Webhook 处理器。
func webhookReceive(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, Response{Success: false, Message: "未配置 Webhook 处理"})
}

// downloadSubtitle 对应 V2 `POST /download/subtitle`：需要字幕站点，本项目未接入。
func downloadSubtitle(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, Response{Success: false, Message: "未配置字幕站点，无法下载字幕"})
}

// authExchange 对应 V2 `POST /auth/exchange`：V2 用临时令牌换取长期令牌。
// 本项目的登录令牌本身可直接续用，无需交换。
func authExchange(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, Response{Success: false, Message: "请直接使用登录令牌（access_token）"})
}
