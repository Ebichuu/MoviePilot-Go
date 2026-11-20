package httpapi

import (
	"encoding/json"
	"net/http"
)

// Response 是与 MoviePilot V2 v1 接口兼容的通用响应。
type Response struct {
	Success     bool   `json:"success"`
	Message     string `json:"message,omitempty"`
	MessageI18N string `json:"message_i18n,omitempty"`
	Data        any    `json:"data"`
}

func writeJSON(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(value)
}

func writeSuccess(w http.ResponseWriter, data any) {
	if data == nil {
		data = map[string]any{}
	}
	writeJSON(w, http.StatusOK, Response{Success: true, Data: data})
}

func writeError(w http.ResponseWriter, status int, message string) {
	writeJSON(w, status, Response{Success: false, Message: message, Data: map[string]any{}})
}
