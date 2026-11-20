package httpapi

import (
	"net/http"
)

// 本文件补齐 V2 里由 Python 插件运行时/调度器/远端版本服务承担的系统接口。Go 侧没有
// 对应子系统时按 V2 的真实失败分支返回（`success:false` + 原因），而不是 404，避免
// 前端整页报错。

// systemVersions 对应 V2 `GET /system/versions`：拉取 GitHub Release 列表。运行环境
// 无外网出口时 V2 同样返回 `success:false`，这里保持同一分支。
func systemVersions(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, Response{
		Success: false, Message: "无法获取版本列表", Data: map[string]any{},
	})
}

// systemProgress 对应 V2 `GET /system/progress/{process_type}`：查询整理/刮削进度。
// 本实现没有异步进度总线，按「没有正在执行的任务」返回。
func systemProgress(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, Response{
		Success: false, Message: "没有正在执行的任务", Data: map[string]any{},
	})
}

// systemModuleList 对应 V2 `GET /system/modulelist`：列出 Python 模块与插件模块。
// 首版不装载 Python 运行时，返回空列表。
func systemModuleList(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, []any{})
}

// systemModuleTest 对应 V2 `GET /system/moduletest/{moduleid}`：测试模块能否安装。
func systemModuleTest(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, Response{
		Success: false, Message: "当前版本不支持模块安装", Data: map[string]any{},
	})
}

// systemRestart 对应 V2 `GET /system/restart`：重启后端。容器由编排负责重启，
// 直接在进程内退出会丢弃正在执行的任务，因此这里明确拒绝并提示手工重启。
func systemRestart(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, Response{
		Success: false, Message: "请通过容器编排重启服务", Data: map[string]any{},
	})
}
