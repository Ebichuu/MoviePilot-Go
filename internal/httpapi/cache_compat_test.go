package httpapi

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/moviepilot-go/moviepilot-go/internal/command"
)

type compatPayload struct {
	Success bool           `json:"success"`
	Message string         `json:"message"`
	Data    map[string]any `json:"data"`
}

func runCompatHandler(t *testing.T, handler http.HandlerFunc, method, target string) (int, compatPayload) {
	t.Helper()
	recorder := httptest.NewRecorder()
	handler(recorder, httptest.NewRequest(method, target, nil))
	var payload compatPayload
	if err := json.Unmarshal(recorder.Body.Bytes(), &payload); err != nil {
		t.Fatalf("响应不是 JSON: %v (%s)", err, recorder.Body.String())
	}
	return recorder.Code, payload
}

func TestTorrentCacheListIsHonestlyEmpty(t *testing.T) {
	status, payload := runCompatHandler(t, torrentCacheList, http.MethodGet, "/api/v1/torrent/cache")
	if status != http.StatusOK {
		t.Fatalf("状态码 = %d", status)
	}
	// V2 有种子搜索缓存；本项目不做缓存，所以必须如实返回 0 条，而不是编造数据。
	if !payload.Success {
		t.Fatalf("应返回成功: %+v", payload)
	}
	if payload.Data["count"] != float64(0) || payload.Data["sites"] != float64(0) {
		t.Fatalf("缓存计数 = %+v", payload.Data)
	}
	items, ok := payload.Data["data"].([]any)
	if !ok || len(items) != 0 {
		t.Fatalf("data 应为空数组，实际 %#v", payload.Data["data"])
	}
}

func TestTorrentCacheReidentifyRefuses(t *testing.T) {
	status, payload := runCompatHandler(t, torrentCacheReidentify, http.MethodPost, "/api/v1/torrent/cache/reidentify/site/abcd")
	if status != http.StatusOK {
		t.Fatalf("状态码 = %d", status)
	}
	// 没有缓存条目就不能回「成功」，否则前端会以为重新识别过。
	if payload.Success {
		t.Fatal("不应返回成功")
	}
	if payload.Message != "未找到缓存条目" {
		t.Fatalf("消息 = %q", payload.Message)
	}
}

func TestRefusalAndSuccessOnlyShapes(t *testing.T) {
	status, payload := runCompatHandler(t, refusal("该接口未实现"), http.MethodGet, "/api/v1/whatever")
	if status != http.StatusOK || payload.Success || payload.Message != "该接口未实现" {
		t.Fatalf("拒绝响应 = %d %+v", status, payload)
	}
	status, payload = runCompatHandler(t, successOnly, http.MethodPost, "/api/v1/whatever/clear")
	if status != http.StatusOK || !payload.Success {
		t.Fatalf("幂等成功响应 = %d %+v", status, payload)
	}
	if payload.Data != nil {
		t.Fatalf("幂等成功不应带数据: %+v", payload.Data)
	}
}

func TestListCommandsHandlers(t *testing.T) {
	var deps Dependencies
	// 注册表未初始化时明确 503，而不是返回空列表让调用方以为没有命令。
	status, _ := runCompatHandler(t, deps.listCommands(nil), http.MethodGet, "/api/v1/command/list")
	if status != http.StatusServiceUnavailable {
		t.Fatalf("未初始化状态码 = %d", status)
	}

	registry, err := command.NewRegistry(command.Command{
		Name: "dummy", Aliases: []string{"dm"}, Title: "示例", Description: "示例命令",
		Category: "测试", AdminOnly: true,
		Handler: func(context.Context, command.Request) (command.Result, error) {
			return command.Result{Text: "ok"}, nil
		},
	})
	if err != nil {
		t.Fatalf("构造注册表失败: %v", err)
	}
	recorder := httptest.NewRecorder()
	deps.listCommands(registry)(recorder, httptest.NewRequest(http.MethodGet, "/api/v1/command/list", nil))
	if recorder.Code != http.StatusOK {
		t.Fatalf("状态码 = %d", recorder.Code)
	}
	var envelope struct {
		Success bool             `json:"success"`
		Data    []map[string]any `json:"data"`
	}
	if err := json.Unmarshal(recorder.Body.Bytes(), &envelope); err != nil {
		t.Fatalf("响应不是 JSON: %v", err)
	}
	if !envelope.Success || len(envelope.Data) != 1 {
		t.Fatalf("响应 = %+v", envelope)
	}
	descriptor := envelope.Data[0]
	if descriptor["name"] != "dummy" || descriptor["title"] != "示例" {
		t.Fatalf("命令描述 = %+v", descriptor)
	}
	// 管理员限定与别名必须体现在描述里，前端据此决定是否显示。
	if descriptor["admin_only"] != true {
		t.Fatalf("admin_only = %+v", descriptor["admin_only"])
	}
	aliases, ok := descriptor["aliases"].([]any)
	if !ok || len(aliases) != 1 || aliases[0] != "dm" {
		t.Fatalf("aliases = %#v", descriptor["aliases"])
	}
}
