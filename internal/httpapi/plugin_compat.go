package httpapi

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"sort"
	"strings"

	"github.com/go-chi/chi/v5"
)

// pluginFoldersSettingKey 复刻 V2 `SystemConfigKey.PluginFolders`，形状是
// `{文件夹名: [插件ID]}`。Go 侧没有 Python 插件运行时，但文件夹分组本身是普通配置，
// 可以真实读写，避免插件页每次刷新都 404。
const pluginFoldersSettingKey = "PluginFolders"

// pluginStatistic 对应 V2 `GET /plugin/statistic`：插件安装统计。V2 在未开启统计
// 分享或中心端不可用时返回 `{}`，这里保持同一分支。
func pluginStatistic(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{})
}

func (d Dependencies) pluginFolders(w http.ResponseWriter, r *http.Request) {
	folders, err := d.loadPluginFolders(r)
	if err != nil {
		writeJSON(w, http.StatusOK, map[string][]string{})
		return
	}
	writeJSON(w, http.StatusOK, folders)
}

func (d Dependencies) savePluginFolders(w http.ResponseWriter, r *http.Request) {
	var folders map[string][]string
	if err := decodePluginFoldersBody(r, &folders); err != nil {
		writeJSON(w, http.StatusOK, Response{Success: false, Message: "参数错误"})
		return
	}
	if err := d.storePluginFolders(r, folders); err != nil {
		writeJSON(w, http.StatusOK, Response{Success: false, Message: "保存插件文件夹失败"})
		return
	}
	writeJSON(w, http.StatusOK, Response{Success: true})
}

func (d Dependencies) createPluginFolder(w http.ResponseWriter, r *http.Request) {
	name := strings.TrimSpace(chi.URLParam(r, "folder_name"))
	if name == "" {
		writeJSON(w, http.StatusOK, Response{Success: false, Message: "文件夹名称不能为空"})
		return
	}
	folders, err := d.loadPluginFolders(r)
	if err != nil {
		writeJSON(w, http.StatusOK, Response{Success: false, Message: "读取插件文件夹失败"})
		return
	}
	if _, exists := folders[name]; exists {
		writeJSON(w, http.StatusOK, Response{Success: false, Message: "文件夹 '" + name + "' 已存在"})
		return
	}
	folders[name] = []string{}
	if err := d.storePluginFolders(r, folders); err != nil {
		writeJSON(w, http.StatusOK, Response{Success: false, Message: "保存插件文件夹失败"})
		return
	}
	writeJSON(w, http.StatusOK, Response{Success: true, Message: "文件夹 '" + name + "' 创建成功"})
}

func (d Dependencies) deletePluginFolder(w http.ResponseWriter, r *http.Request) {
	name := strings.TrimSpace(chi.URLParam(r, "folder_name"))
	folders, err := d.loadPluginFolders(r)
	if err != nil {
		writeJSON(w, http.StatusOK, Response{Success: false, Message: "读取插件文件夹失败"})
		return
	}
	if _, exists := folders[name]; !exists {
		writeJSON(w, http.StatusOK, Response{Success: false, Message: "文件夹 '" + name + "' 不存在"})
		return
	}
	delete(folders, name)
	if err := d.storePluginFolders(r, folders); err != nil {
		writeJSON(w, http.StatusOK, Response{Success: false, Message: "保存插件文件夹失败"})
		return
	}
	writeJSON(w, http.StatusOK, Response{Success: true, Message: "文件夹 '" + name + "' 删除成功"})
}

func (d Dependencies) updateFolderPlugins(w http.ResponseWriter, r *http.Request) {
	name := strings.TrimSpace(chi.URLParam(r, "folder_name"))
	var pluginIDs []string
	if err := decodePluginFoldersBody(r, &pluginIDs); err != nil {
		writeJSON(w, http.StatusOK, Response{Success: false, Message: "参数错误"})
		return
	}
	folders, err := d.loadPluginFolders(r)
	if err != nil {
		writeJSON(w, http.StatusOK, Response{Success: false, Message: "读取插件文件夹失败"})
		return
	}
	folders[name] = pluginIDs
	if err := d.storePluginFolders(r, folders); err != nil {
		writeJSON(w, http.StatusOK, Response{Success: false, Message: "保存插件文件夹失败"})
		return
	}
	writeJSON(w, http.StatusOK, Response{Success: true, Message: "文件夹 '" + name + "' 中的插件已更新"})
}

func decodePluginFoldersBody(r *http.Request, target any) error {
	if r == nil || r.Body == nil {
		return errors.New("缺少请求体")
	}
	decoder := json.NewDecoder(io.LimitReader(r.Body, 1<<20))
	if err := decoder.Decode(target); err != nil {
		return err
	}
	return nil
}

func (d Dependencies) loadPluginFolders(r *http.Request) (map[string][]string, error) {
	folders := map[string][]string{}
	if d.Store == nil {
		return folders, nil
	}
	found, err := d.Store.GetSetting(r.Context(), pluginFoldersSettingKey, &folders)
	if err != nil {
		return nil, err
	}
	if !found || folders == nil {
		return map[string][]string{}, nil
	}
	return folders, nil
}

func (d Dependencies) storePluginFolders(r *http.Request, folders map[string][]string) error {
	if d.Store == nil {
		return errors.New("设置存储不可用")
	}
	if folders == nil {
		folders = map[string][]string{}
	}
	for name, plugins := range folders {
		if plugins == nil {
			folders[name] = []string{}
		}
	}
	return d.Store.SetSetting(r.Context(), pluginFoldersSettingKey, folders, false)
}

// sortedPluginFolderNames 只用于稳定输出，便于测试与日志阅读。
func sortedPluginFolderNames(folders map[string][]string) []string {
	names := make([]string, 0, len(folders))
	for name := range folders {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}
