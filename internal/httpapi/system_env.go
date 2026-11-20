package httpapi

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"sort"
	"strings"

	"github.com/moviepilot-go/moviepilot-go/internal/config"
)

const defaultSystemEnvRequestBodyLimit int64 = 1 << 20

var (
	errSystemEnvRequestInvalid  = errors.New("系统配置请求 JSON 无效")
	errSystemEnvRequestTooLarge = errors.New("系统配置请求体过大")
)

// EnvironmentSettings 是系统环境配置 HTTP 接口消费的最小契约。
type EnvironmentSettings interface {
	Snapshot() map[string]any
	Update(context.Context, map[string]any) (map[string]config.EnvironmentUpdateResult, error)
}

func (d Dependencies) requireSystemEnvSuperuser(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		user, ok := d.authorizedUser(r)
		if !ok {
			writeError(w, http.StatusForbidden, "用户权限不足")
			return
		}
		if !user.SuperUser {
			// V2 对已登录但非超级管理员的用户返回 400。
			writeError(w, http.StatusBadRequest, "用户权限不足")
			return
		}
		next.ServeHTTP(w, r)
	})
}

func (d Dependencies) systemEnv(w http.ResponseWriter, _ *http.Request) {
	if d.EnvironmentSettings == nil {
		writeError(w, http.StatusInternalServerError, "读取系统配置失败")
		return
	}

	info := cloneSystemEnvSnapshot(d.EnvironmentSettings.Snapshot())
	info["VERSION"] = backendVersion
	info["FRONTEND_VERSION"] = frontendVersion
	info["AUTH_VERSION"] = ""
	info["INDEXER_VERSION"] = ""
	info["RUST_ACCEL_AVAILABLE"] = false
	info["RUST_ACCEL_ENABLED"] = false
	// 前端 PersonCard 用这个域名拼 TMDB 头像：
	// `https://${TMDB_IMAGE_DOMAIN}/t/p/w600_and_h900_bestv2${profile_path}`。
	// 缺了它，TMDB 演职员卡片就只剩名字没有头像。
	if _, exists := info["TMDB_IMAGE_DOMAIN"]; !exists {
		domain := strings.TrimSpace(d.Config.TMDBImageDomain)
		if domain == "" {
			domain = "image.tmdb.org"
		}
		info["TMDB_IMAGE_DOMAIN"] = domain
	}
	writeSuccess(w, info)
}

func (d Dependencies) setSystemEnv(w http.ResponseWriter, r *http.Request) {
	env, err := readSystemEnvRequest(r, d.Config.MaxRequestBody)
	if err != nil {
		if errors.Is(err, errSystemEnvRequestTooLarge) {
			writeError(w, http.StatusRequestEntityTooLarge, errSystemEnvRequestTooLarge.Error())
			return
		}
		writeError(w, http.StatusUnprocessableEntity, errSystemEnvRequestInvalid.Error())
		return
	}
	if d.EnvironmentSettings == nil {
		writeError(w, http.StatusInternalServerError, "更新系统配置失败")
		return
	}

	results, err := d.EnvironmentSettings.Update(r.Context(), env)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "更新系统配置失败")
		return
	}
	writeSystemEnvUpdateResponse(w, results)
}

func cloneSystemEnvSnapshot(snapshot map[string]any) map[string]any {
	info := make(map[string]any, len(snapshot)+6)
	for key, value := range snapshot {
		info[key] = value
	}
	for _, key := range []string{
		"SECRET_KEY",
		"RESOURCE_SECRET_KEY",
		"MOVIEPILOT_JWT_SECRET",
		"MOVIEPILOT_RESOURCE_SECRET",
		"MOVIEPILOT_MASTER_KEY",
		"MOVIEPILOT_BOOTSTRAP_PASSWORD",
		"MOVIEPILOT_DATABASE_URL",
		"MOVIEPILOT_REDIS_URL",
	} {
		delete(info, key)
	}
	return info
}

func readSystemEnvRequest(r *http.Request, maxBytes int64) (map[string]any, error) {
	if maxBytes <= 0 {
		maxBytes = defaultSystemEnvRequestBodyLimit
	}
	body, err := io.ReadAll(io.LimitReader(r.Body, maxBytes+1))
	var maxBytesError *http.MaxBytesError
	if errors.As(err, &maxBytesError) || int64(len(body)) > maxBytes {
		return nil, errSystemEnvRequestTooLarge
	}
	if err != nil {
		return nil, errSystemEnvRequestInvalid
	}

	decoder := json.NewDecoder(bytes.NewReader(body))
	decoder.UseNumber()
	var value any
	if err := decoder.Decode(&value); err != nil {
		return nil, errSystemEnvRequestInvalid
	}
	env, ok := value.(map[string]any)
	if !ok || env == nil {
		return nil, errSystemEnvRequestInvalid
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		return nil, errSystemEnvRequestInvalid
	}
	return env, nil
}

func writeSystemEnvUpdateResponse(w http.ResponseWriter, results map[string]config.EnvironmentUpdateResult) {
	successUpdates := make(map[string][2]any)
	failedUpdates := make(map[string][2]any)
	failureKeys := make([]string, 0)
	for key, result := range results {
		message := nullableSystemEnvMessage(result.Message)
		if result.Changed {
			successUpdates[key] = [2]any{true, message}
		}
		if result.Failed {
			failedUpdates[key] = [2]any{false, message}
			failureKeys = append(failureKeys, key)
		}
	}

	if len(failureKeys) != 0 {
		sort.Strings(failureKeys)
		messages := make([]string, 0, len(failureKeys))
		for _, key := range failureKeys {
			messages = append(messages, results[key].Message)
		}
		writeJSON(w, http.StatusOK, Response{
			Success: false,
			Message: strings.Join(messages, ", "),
			Data: map[string]any{
				"success_updates": successUpdates,
				"failed_updates":  failedUpdates,
			},
		})
		return
	}

	writeJSON(w, http.StatusOK, Response{
		Success: true,
		Message: "所有配置项更新成功",
		Data:    map[string]any{"success_updates": successUpdates},
	})
}

func nullableSystemEnvMessage(message string) any {
	if message == "" {
		return nil
	}
	return message
}
