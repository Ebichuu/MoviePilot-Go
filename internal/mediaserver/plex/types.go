// Package plex 提供纯 Go 的 Plex Media Server HTTP JSON 客户端，
// 对齐 MoviePilot V2 的媒体库查询、条目同步、路径/ProviderID 检索、刷新与播放链接能力。
package plex

import (
	"errors"
	"fmt"
	"time"
)

// Config 配置单个 Plex Media Server 客户端连接参数。
type Config struct {
	BaseURL  string
	PlayHost string
	Token    string
	Timeout  time.Duration
}

// Library 表示 Plex 的单个媒体库分区（Directory）。
type Library struct {
	Key       string
	Title     string
	Type      string // "movie" | "show" | ...
	Locations []string
}

// Item 表示 Plex 中的影片、剧集或单集条目。
type Item struct {
	RatingKey     string
	Type          string // "movie" | "show" | "episode"
	Title         string
	OriginalTitle string
	Year          int
	LibraryKey    string
	ParentIndex   *int // 季号（单集）
	Index         *int // 集号（单集）
	Path          string
	ProviderIDs   map[string]string // {"Tmdb": "...", "Imdb": "...", "Tvdb": "..."}
}

// ItemsPage 表示一次有界分页查询结果。
type ItemsPage struct {
	Items      []Item
	StartIndex int
	TotalSize  int
}

// ErrorCode 对齐 Emby 客户端的错误分类语义。
type ErrorCode string

const (
	ErrorCodeInvalidConfig   ErrorCode = "invalid_config"
	ErrorCodeInvalidArgument ErrorCode = "invalid_argument"
	ErrorCodeTransport       ErrorCode = "transport"
	ErrorCodeHTTPStatus      ErrorCode = "http_status"
	ErrorCodeRateLimited     ErrorCode = "rate_limited"
	ErrorCodeInvalidResponse ErrorCode = "invalid_response"
	ErrorCodeCanceled        ErrorCode = "canceled"
	ErrorCodeOutcomeUnknown  ErrorCode = "outcome_unknown"
)

// ErrOutcomeUnknown 表示刷新请求发出后结果未知。
var ErrOutcomeUnknown = errors.New("plex refresh outcome unknown")

// Error 是 Plex 客户端的结构化错误（不含 Token 秘密）。
type Error struct {
	Code       ErrorCode
	StatusCode int
	cause      error
}

func (e *Error) Error() string {
	if e == nil {
		return "<nil>"
	}
	if e.StatusCode > 0 {
		return fmt.Sprintf("plex %s (status %d)", e.Code, e.StatusCode)
	}
	return fmt.Sprintf("plex %s", e.Code)
}

func (e *Error) Unwrap() error {
	if e == nil {
		return nil
	}
	if e.Code == ErrorCodeOutcomeUnknown {
		return ErrOutcomeUnknown
	}
	return e.cause
}
