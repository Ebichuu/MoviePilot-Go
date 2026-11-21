package httpapi

import (
	"bufio"
	"context"
	"crypto/subtle"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"github.com/prometheus/client_golang/prometheus"

	"github.com/moviepilot-go/moviepilot-go/internal/auth"
)

type contextKey string

const (
	claimsContextKey    contextKey = "claims"
	requestIDContextKey contextKey = "request_id"
)

type metrics struct {
	requests *prometheus.CounterVec
	duration *prometheus.HistogramVec
	inflight prometheus.Gauge
}

type UserLookup interface {
	UserByID(context.Context, int64) (auth.User, error)
}

func newMetrics(registerer prometheus.Registerer) *metrics {
	m := &metrics{
		requests: prometheus.NewCounterVec(prometheus.CounterOpts{Name: "moviepilot_http_requests_total", Help: "HTTP 请求总数。"}, []string{"method", "route", "status"}),
		duration: prometheus.NewHistogramVec(prometheus.HistogramOpts{Name: "moviepilot_http_request_duration_seconds", Help: "HTTP 请求耗时。"}, []string{"method", "route"}),
		inflight: prometheus.NewGauge(prometheus.GaugeOpts{Name: "moviepilot_http_inflight_requests", Help: "当前处理中的 HTTP 请求数。"}),
	}
	registerer.MustRegister(m.requests, m.duration, m.inflight)
	return m
}

func requestContext(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requestID := strings.TrimSpace(r.Header.Get("X-Request-ID"))
		if requestID == "" || len(requestID) > 128 {
			requestID = uuid.NewString()
		}
		w.Header().Set("X-Request-ID", requestID)
		ctx := context.WithValue(r.Context(), requestIDContextKey, requestID)
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

func recoverer(logger *slog.Logger) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			defer func() {
				if recovered := recover(); recovered != nil {
					logger.Error("HTTP 处理发生 panic", "request_id", requestID(r.Context()), "error", fmt.Sprint(recovered))
					writeError(w, http.StatusInternalServerError, "服务器内部错误")
				}
			}()
			next.ServeHTTP(w, r)
		})
	}
}

func limitBody(maxBytes int64) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.Body != nil && r.Method != http.MethodGet && r.Method != http.MethodHead {
				r.Body = http.MaxBytesReader(w, r.Body, maxBytes)
			}
			next.ServeHTTP(w, r)
		})
	}
}

func observe(m *metrics) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			started := time.Now()
			m.inflight.Inc()
			defer m.inflight.Dec()
			writer := &statusWriter{ResponseWriter: w, status: http.StatusOK}
			next.ServeHTTP(writer, r)
			route := chi.RouteContext(r.Context()).RoutePattern()
			if route == "" {
				route = "unmatched"
			}
			m.requests.WithLabelValues(r.Method, route, strconv.Itoa(writer.status)).Inc()
			m.duration.WithLabelValues(r.Method, route).Observe(time.Since(started).Seconds())
		})
	}
}

func authenticate(service *auth.Service) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			header := strings.TrimSpace(r.Header.Get("Authorization"))
			parts := strings.Fields(header)
			if len(parts) != 2 || !strings.EqualFold(parts[0], "Bearer") {
				w.Header().Set("WWW-Authenticate", "Bearer")
				writeError(w, http.StatusUnauthorized, "Not authenticated")
				return
			}
			claims, err := service.VerifyAccessToken(parts[1])
			if err != nil {
				writeError(w, http.StatusForbidden, "token校验不通过")
				return
			}
			refreshResourceCookie(service, w, r, claims)
			ctx := context.WithValue(r.Context(), claimsContextKey, claims)
			next.ServeHTTP(w, r.WithContext(ctx))
		})
	}
}

// resourceCookieName 是资源令牌 Cookie 名，V2 取 `settings.PROJECT_NAME`（默认 MoviePilot）。
const resourceCookieName = "MoviePilot"

// resourceTokenLifetime 对齐 V2 `RESOURCE_ACCESS_TOKEN_EXPIRE_SECONDS = 60 * 30`。
const resourceTokenLifetime = 30 * time.Minute

// refreshResourceCookie 复刻 V2 `set_or_refresh_resource_token_cookie`：
// 每个带 Bearer 的鉴权请求都检查一次 Cookie，缺失 / 剩余不足 1/3 / 身份不符就重签。
//
// 为什么必须刷新：`<img src>`、`<video src>` 这类请求只会带 Cookie、不会带 Authorization，
// 海报、演员头像、流媒体都走它们。只在登录时发一次 30 分钟令牌的话，用户在页面上逛半小时后
// 图片会整片 403（豆瓣图片必须过 `/system/img`，没有直连兜底）。
func refreshResourceCookie(service *auth.Service, w http.ResponseWriter, r *http.Request, claims auth.Claims) {
	if cookie, err := r.Cookie(resourceCookieName); err == nil && cookie.Value != "" {
		existing, verifyErr := service.VerifyResourceToken(cookie.Value)
		if verifyErr == nil &&
			existing.Subject == claims.Subject && existing.Username == claims.Username &&
			existing.ExpiresAt != nil && time.Until(existing.ExpiresAt.Time) > resourceTokenLifetime/3 {
			return
		}
	}
	token, issueErr := service.ResourceTokenFromClaims(claims, resourceTokenLifetime)
	if issueErr != nil {
		// Cookie 只服务于图片/流媒体鉴权，签发失败不该让主请求失败。
		return
	}
	secure := r.TLS != nil || strings.EqualFold(r.Header.Get("X-Forwarded-Proto"), "https")
	http.SetCookie(w, &http.Cookie{
		Name: resourceCookieName, Value: token, Path: "/", HttpOnly: true,
		Secure: secure, SameSite: http.SameSiteLaxMode,
	})
}

func authenticateResource(service *auth.Service) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			cookie, err := r.Cookie(resourceCookieName)
			if err != nil || cookie.Value == "" {
				writeError(w, http.StatusForbidden, "resource token not found")
				return
			}
			claims, err := service.VerifyResourceToken(cookie.Value)
			if err != nil {
				writeError(w, http.StatusForbidden, "token校验不通过")
				return
			}
			ctx := context.WithValue(r.Context(), claimsContextKey, claims)
			next.ServeHTTP(w, r.WithContext(ctx))
		})
	}
}

// authenticateLoggingSuperuser 对齐 V2 `_verify_log_resource_superuser`：
// 同时支持 Authorization Bearer（axios 纯文本模式）与资源 Cookie / ?token=（浏览器 EventSource），
// 且强制校验超级管理员身份。
func (d Dependencies) authenticateLoggingSuperuser(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if d.Auth == nil {
			writeError(w, http.StatusUnauthorized, "Not authenticated")
			return
		}
		var (
			claims auth.Claims
			err    error
		)
		header := strings.TrimSpace(r.Header.Get("Authorization"))
		parts := strings.Fields(header)
		if len(parts) == 2 && strings.EqualFold(parts[0], "Bearer") {
			claims, err = d.Auth.VerifyAccessToken(parts[1])
		} else if cookie, cookieErr := r.Cookie(resourceCookieName); cookieErr == nil && cookie.Value != "" {
			claims, err = d.Auth.VerifyResourceToken(cookie.Value)
		} else if queryToken := strings.TrimSpace(r.URL.Query().Get("token")); queryToken != "" {
			claims, err = d.Auth.VerifyResourceToken(queryToken)
			if err != nil {
				claims, err = d.Auth.VerifyAccessToken(queryToken)
			}
		} else {
			writeError(w, http.StatusUnauthorized, "Not authenticated")
			return
		}
		if err != nil {
			writeError(w, http.StatusForbidden, "token校验不通过")
			return
		}
		rWithClaims := r.WithContext(context.WithValue(r.Context(), claimsContextKey, claims))
		if d.Users != nil {
			user, ok := d.authorizedUser(rWithClaims)
			if !ok || !user.SuperUser {
				writeError(w, http.StatusForbidden, "用户权限不足")
				return
			}
		} else if !claims.SuperUser {
			writeError(w, http.StatusForbidden, "用户权限不足")
			return
		}
		next.ServeHTTP(w, rWithClaims)
	})
}

// requireAPIToken 复刻 V2 `verify_apitoken`：`?token=` 必须等于配置的 API_TOKEN。
// 没有配置 API_TOKEN 时一律拒绝——V2 用空串比较同样匹配不到任何令牌。
func (d Dependencies) requireAPIToken(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		expected := strings.TrimSpace(d.Config.APIToken)
		provided := strings.TrimSpace(r.URL.Query().Get("token"))
		if expected == "" || provided == "" ||
			subtle.ConstantTimeCompare([]byte(provided), []byte(expected)) != 1 {
			writeError(w, http.StatusForbidden, "API_TOKEN 校验失败")
			return
		}
		next.ServeHTTP(w, r)
	})
}

// requireAPITokenOrBearer 同时接受外部媒体服务器常用的 `?token=API_TOKEN`
// 与已登录用户的 `Authorization: Bearer <jwt>`。
func (d Dependencies) requireAPITokenOrBearer(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		expected := strings.TrimSpace(d.Config.APIToken)
		provided := strings.TrimSpace(r.URL.Query().Get("token"))
		if expected != "" && provided != "" &&
			subtle.ConstantTimeCompare([]byte(provided), []byte(expected)) == 1 {
			next.ServeHTTP(w, r)
			return
		}
		if d.Auth != nil {
			header := strings.TrimSpace(r.Header.Get("Authorization"))
			parts := strings.Fields(header)
			if len(parts) == 2 && strings.EqualFold(parts[0], "Bearer") {
				if claims, err := d.Auth.VerifyAccessToken(parts[1]); err == nil {
					ctx := context.WithValue(r.Context(), claimsContextKey, claims)
					next.ServeHTTP(w, r.WithContext(ctx))
					return
				}
			}
		}
		writeError(w, http.StatusForbidden, "API_TOKEN 校验失败")
	})
}

func (d Dependencies) requireAdmin(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		user, ok := d.authorizedUser(r)
		if !ok || !user.SuperUser {
			writeError(w, http.StatusForbidden, "用户权限不足")
			return
		}
		next.ServeHTTP(w, r)
	})
}

func (d Dependencies) requireManage(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		user, ok := d.authorizedUser(r)
		if !ok {
			writeError(w, http.StatusForbidden, "用户权限不足")
			return
		}
		if user.SuperUser {
			next.ServeHTTP(w, r)
			return
		}
		manage, _ := user.Permissions["manage"].(bool)
		if !manage {
			writeError(w, http.StatusForbidden, "用户权限不足")
			return
		}
		next.ServeHTTP(w, r)
	})
}

func (d Dependencies) authorizedUser(r *http.Request) (auth.User, bool) {
	claims, ok := claimsFromContext(r.Context())
	if !ok {
		return auth.User{}, false
	}
	id, err := strconv.ParseInt(claims.Subject, 10, 64)
	if err != nil {
		return auth.User{}, false
	}
	lookup := d.Users
	if lookup == nil {
		if d.Store == nil {
			return auth.User{}, false
		}
		lookup = d.Store
	}
	user, err := lookup.UserByID(r.Context(), id)
	if err != nil {
		return auth.User{}, false
	}
	return user, true
}

func claimsFromContext(ctx context.Context) (auth.Claims, bool) {
	claims, ok := ctx.Value(claimsContextKey).(auth.Claims)
	return claims, ok
}

func requestID(ctx context.Context) string {
	value, _ := ctx.Value(requestIDContextKey).(string)
	return value
}

type statusWriter struct {
	http.ResponseWriter
	status int
}

func (w *statusWriter) WriteHeader(status int) {
	w.status = status
	w.ResponseWriter.WriteHeader(status)
}

func (w *statusWriter) Flush() {
	if flusher, ok := w.ResponseWriter.(http.Flusher); ok {
		flusher.Flush()
	}
}

func (w *statusWriter) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	hijacker, ok := w.ResponseWriter.(http.Hijacker)
	if !ok {
		return nil, nil, errorsUnsupported("hijacking")
	}
	return hijacker.Hijack()
}

func errorsUnsupported(feature string) error {
	return fmt.Errorf("response writer 不支持 %s", feature)
}
