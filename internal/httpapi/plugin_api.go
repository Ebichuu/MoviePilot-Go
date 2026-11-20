package httpapi

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"

	"github.com/go-chi/chi/v5"

	"github.com/moviepilot-go/moviepilot-go/internal/pluginengine"
)

// 本文件是原生插件的 HTTP 边界。返回对象对齐 V2 `schemas.Plugin` 的字段
// （见 web/dist 的插件页与 PluginConfigDialog）：
// id / plugin_name / plugin_desc / plugin_icon / plugin_version / plugin_label /
// plugin_author / author_url / plugin_config_prefix / plugin_order / auth_level /
// installed / state / has_page / has_update / system_version_compatible /
// system_version_message / system_version / release / is_local / repo_url /
// install_count / history / add_time / plugin_public_key。
//
// 与 V2 的差别只有一处，且是「做不到就别假装」：本项目的插件没有 Python 渲染的
// 页面（Vue 组件），所以 has_page 恒为 false；配置表单用 render_mode=vuetify + conf
// 「该插件没有配置界面」，而不是渲染一个空白页或报错。

// pluginView 把注册表快照转成前端消费的对象。
func pluginView(snapshot pluginengine.Plugin) map[string]any {
	// `state` 必须是 bool 的「运行中」：前端状态点的颜色（success/secondary）
	// 和「运行中」筛选都按布尔判断。早先这里给的是 "installed"/"not_installed"
	// 字符串，在 JS 里恒为真，结果禁用中的插件也显示为运行中。
	return map[string]any{
		"id":                   snapshot.ID,
		"plugin_name":          snapshot.Name,
		"plugin_desc":          snapshot.Description,
		"plugin_icon":          snapshot.Icon,
		"plugin_version":       snapshot.Version,
		"plugin_label":         snapshot.Label,
		"plugin_author":        snapshot.Author,
		"author_url":           "",
		"plugin_config_prefix": snapshot.ID,
		"plugin_order":         0,
		"auth_level":           snapshot.Level,
		"installed":            snapshot.Installed,
		"state":                snapshot.Enabled,
		// 内置插件不跟随远端仓库：既没有可更新版本，也没有可渲染的页面。
		"has_page":                  false,
		"has_update":                false,
		"system_version_compatible": true,
		"system_version_message":    "",
		"system_version":            "",
		"release":                   false,
		"is_local":                  true,
		"repo_url":                  "",
		"install_count":             0,
		"history":                   map[string]any{},
		"add_time":                  0,
		"plugin_public_key":         "",
	}
}

// pluginFieldView 是配置项声明的 HTTP 表示，键名与前端表单常用命名一致。
func pluginFieldView(field pluginengine.Field) map[string]any {
	options := make([]map[string]string, 0, len(field.Options))
	for _, option := range field.Options {
		options = append(options, map[string]string{"value": option.Value, "label": option.Label})
	}
	return map[string]any{
		"key":      field.Key,
		"label":    field.Label,
		"type":     string(field.Type),
		"required": field.Required,
		"help":     field.Help,
		"default":  field.Default,
		"options":  options,
	}
}

// pluginRegistry 取出注册表；未装配时按「插件体系未启用」处理，
// 让路由在禁用插件的部署里仍然可用（只是返回空列表）。
func (d Dependencies) pluginRegistry(w http.ResponseWriter) (*pluginengine.Registry, bool) {
	if d.Plugins == nil {
		writeJSON(w, http.StatusOK, Response{Success: false, Message: "插件体系未启用"})
		return nil, false
	}
	return d.Plugins, true
}

// pluginFailure 把注册表错误映射成 V2 的 {success:false,message}。
// 这些都是可预期的业务结果，不该用 4xx/5xx 让前端弹「网络错误」。
func pluginFailure(w http.ResponseWriter, err error) {
	message := "插件操作失败"
	switch {
	case errors.Is(err, pluginengine.ErrPluginNotFound):
		message = "插件不存在"
	case errors.Is(err, pluginengine.ErrPluginExists):
		message = "插件已注册"
	case errors.Is(err, pluginengine.ErrPluginNotInstalled):
		message = "插件未安装"
	case errors.Is(err, pluginengine.ErrPluginDisabled):
		message = "插件未启用"
	case errors.Is(err, pluginengine.ErrInvalidConfig):
		message = "插件配置不合法"
	case errors.Is(err, pluginengine.ErrUnknownAction):
		message = "插件动作不存在"
	case errors.Is(err, pluginengine.ErrServiceUnavailable):
		message = "插件能力未接线"
	}
	writeJSON(w, http.StatusOK, Response{Success: false, Message: message + "：" + err.Error()})
}

// pluginID 读出路径里的插件 ID。
func pluginID(r *http.Request) string {
	return strings.TrimSpace(chi.URLParam(r, "plugin_id"))
}

// decodePluginBody 读取插件相关的 JSON 请求体，限制体积避免把超大 JSON 灌进注册表。
func decodePluginBody(r *http.Request, target any) error {
	if r == nil || r.Body == nil {
		return errors.New("缺少请求体")
	}
	decoder := json.NewDecoder(io.LimitReader(r.Body, 1<<20))
	return decoder.Decode(target)
}

// listPlugins 对应 V2 `GET /plugin/?state=installed|market`。
//
// market 也返回内置清单：本项目的「市场」就是这 5 个内置插件（没有远端插件包），
// 若 market 返回空数组，前端插件页的「安装」入口会整个消失——用户只能靠调接口安装，
// 那不是「诚实地少做」，是把功能藏起来了。installed 字段照实回填，前端据此显示
// 「安装」或「已安装」。
func (d Dependencies) listPlugins(w http.ResponseWriter, r *http.Request) {
	if d.Plugins == nil {
		writeJSON(w, http.StatusOK, []any{})
		return
	}
	state := strings.ToLower(strings.TrimSpace(r.URL.Query().Get("state")))
	plugins := make([]map[string]any, 0, 8)
	for _, snapshot := range d.Plugins.List() {
		if state == "installed" && !snapshot.Installed {
			continue
		}
		plugins = append(plugins, pluginView(snapshot))
	}
	writeJSON(w, http.StatusOK, plugins)
}

// listInstalledPlugins 对应 V2 `GET /plugin/installed`：已安装插件的 ID 列表。
func (d Dependencies) listInstalledPlugins(w http.ResponseWriter, _ *http.Request) {
	if d.Plugins == nil {
		writeJSON(w, http.StatusOK, []string{})
		return
	}
	ids := make([]string, 0, 8)
	for _, snapshot := range d.Plugins.List() {
		if snapshot.Installed {
			ids = append(ids, snapshot.ID)
		}
	}
	writeJSON(w, http.StatusOK, ids)
}

// getPlugin 对应 V2 `GET /plugin/{plugin_id}`。
func (d Dependencies) getPlugin(w http.ResponseWriter, r *http.Request) {
	registry, ok := d.pluginRegistry(w)
	if !ok {
		return
	}
	snapshot, err := registry.Get(pluginID(r))
	if err != nil {
		pluginFailure(w, err)
		return
	}
	writeJSON(w, http.StatusOK, pluginView(snapshot))
}

// installPlugin 对应 V2 `GET /plugin/install/{plugin_id}`。
//
// 安装即启用：本项目的插件没有独立启用开关（V2 那个开关属于 Python 插件页），
// 若只置 installed，插件会永远停在「装好了但不运行」，这是最难排查的状态。
func (d Dependencies) installPlugin(w http.ResponseWriter, r *http.Request) {
	registry, ok := d.pluginRegistry(w)
	if !ok {
		return
	}
	id := pluginID(r)
	ctx := r.Context()
	if err := registry.Install(ctx, id); err != nil {
		pluginFailure(w, err)
		return
	}
	if err := registry.Enable(ctx, id); err != nil {
		pluginFailure(w, err)
		return
	}
	writeJSON(w, http.StatusOK, Response{Success: true, Message: "插件已安装并启用"})
}

// uninstallPlugin 对应 V2 `DELETE /plugin/{plugin_id}`。
func (d Dependencies) uninstallPlugin(w http.ResponseWriter, r *http.Request) {
	registry, ok := d.pluginRegistry(w)
	if !ok {
		return
	}
	if err := registry.Uninstall(r.Context(), pluginID(r)); err != nil {
		pluginFailure(w, err)
		return
	}
	writeJSON(w, http.StatusOK, Response{Success: true, Message: "插件已卸载"})
}

// resetPlugin 对应 V2 `GET /plugin/reset/{plugin_id}`：把配置恢复成清单默认值。
// 用「空配置更新」实现：缺省项取默认值，没有默认值的可选项被丢弃。
func (d Dependencies) resetPlugin(w http.ResponseWriter, r *http.Request) {
	registry, ok := d.pluginRegistry(w)
	if !ok {
		return
	}
	if err := registry.UpdateConfig(r.Context(), pluginID(r), map[string]any{}); err != nil {
		pluginFailure(w, err)
		return
	}
	writeJSON(w, http.StatusOK, Response{Success: true, Message: "插件配置已重置"})
}

// savePluginConfig 对应 V2 `PUT /plugin/{plugin_id}`：请求体就是配置模型本身，
// 前端把表单值原样 PUT 上来，没有外层包装。
func (d Dependencies) savePluginConfig(w http.ResponseWriter, r *http.Request) {
	registry, ok := d.pluginRegistry(w)
	if !ok {
		return
	}
	var config map[string]any
	if err := decodePluginBody(r, &config); err != nil {
		writeJSON(w, http.StatusOK, Response{Success: false, Message: "参数错误：" + err.Error()})
		return
	}
	if err := registry.UpdateConfig(r.Context(), pluginID(r), config); err != nil {
		pluginFailure(w, err)
		return
	}
	writeJSON(w, http.StatusOK, Response{Success: true, Message: "配置已保存"})
}

// pluginForm 对应 V2 `GET /plugin/form/{plugin_id}`。
//
// render_mode 必须是 "vuetify"：前端 `PluginConfigDialog` 拿到空串会直接判定
// 「插件配置加载失败」，而不是「没有配置界面」——这一条是真实点击时发现的，
// 光看接口返回体看不出来。给 "vuetify" 时前端渲染 conf 里的组件描述，
// 并用 model 作为表单初值，保存时整份 model 回传给 PUT /plugin/{id}。
func (d Dependencies) pluginForm(w http.ResponseWriter, r *http.Request) {
	registry, ok := d.pluginRegistry(w)
	if !ok {
		return
	}
	snapshot, err := registry.Get(pluginID(r))
	if err != nil {
		pluginFailure(w, err)
		return
	}
	fields := make([]map[string]any, 0, len(snapshot.Fields))
	for _, field := range snapshot.Fields {
		fields = append(fields, pluginFieldView(field))
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"render_mode": "vuetify",
		"model":       snapshot.Config,
		"conf":        pluginFormConf(snapshot.Fields),
		"fields":      fields,
	})
}

// pluginFormConf 把字段声明翻译成 V2 前端 FormRender 认识的组件描述。
//
// FormRender 的约定（见 `web/dist/assets/FormRender*.js`）：
//   - 每一项是 `{component, props, content}`，component 用组件名由前端 resolve；
//   - props 里键为 `model` 时会被绑成 `v-model`（值就是 model 的字段名）；
//   - 值是字符串且以 `{{...}}` 包裹时按表达式求值，值恰好等于某个 model 键时取该值，
//     其它值原样透传（所以 items 这类数组可以直接内联）。
//
// 组件选择按字段类型：布尔用 VSwitch，多行用 VTextarea，下拉用 VSelect，
// 其余（含整数/小数）用 VTextField；数字输入框交回后端的是字符串，
// 由 `pluginengine` 的归一逻辑负责转成整数/浮点。
func pluginFormConf(fields []pluginengine.Field) []map[string]any {
	items := make([]map[string]any, 0, len(fields))
	for _, field := range fields {
		component := "VTextField"
		props := map[string]any{
			"model":       field.Key,
			"label":       field.Label,
			"variant":     "outlined",
			"density":     "comfortable",
			"hideDetails": "auto",
		}
		if field.Help != "" {
			props["hint"] = field.Help
			props["persistentHint"] = true
		}
		switch field.Type {
		case pluginengine.FieldBoolean:
			component = "VSwitch"
			props["color"] = "primary"
			delete(props, "variant")
			delete(props, "density")
			delete(props, "hideDetails")
		case pluginengine.FieldTextarea:
			component = "VTextarea"
			props["rows"] = 3
		case pluginengine.FieldPassword:
			props["type"] = "password"
		case pluginengine.FieldInteger:
			props["type"] = "number"
		case pluginengine.FieldNumber:
			props["type"] = "number"
			props["step"] = "any"
		case pluginengine.FieldSelect:
			component = "VSelect"
			options := make([]map[string]any, 0, len(field.Options))
			for _, option := range field.Options {
				options = append(options, map[string]any{"title": option.Label, "value": option.Value})
			}
			props["items"] = options
		}
		items = append(items, map[string]any{"component": component, "props": props})
	}
	// 外面套一层 flex 容器：否则每个字段各自贴边，字段多了很难看。
	return []map[string]any{{
		"component": "div",
		"props":     map[string]any{"class": "d-flex flex-column ga-3"},
		"content":   items,
	}}
}

// reloadPlugin 对应 V2 `GET /plugin/reload/{plugin_id}`：从设置表重新读回状态。
// 没有 Python 模块可重载，这一操作在这里的真实含义就是「以库里为准刷新内存」。
func (d Dependencies) reloadPlugin(w http.ResponseWriter, r *http.Request) {
	registry, ok := d.pluginRegistry(w)
	if !ok {
		return
	}
	if _, err := registry.Get(pluginID(r)); err != nil {
		pluginFailure(w, err)
		return
	}
	if err := registry.Restore(r.Context()); err != nil {
		pluginFailure(w, err)
		return
	}
	writeJSON(w, http.StatusOK, Response{Success: true, Message: "插件状态已重新载入"})
}

// runPluginAction 是本项目的扩展端点（V2 没有手工触发插件的能力）：
// `POST /plugin/{plugin_id}/action?action=run`。
// 动作只做「触发既有后台任务」，因此成功只代表已入队，不代表已经执行完。
func (d Dependencies) runPluginAction(w http.ResponseWriter, r *http.Request) {
	registry, ok := d.pluginRegistry(w)
	if !ok {
		return
	}
	action := strings.TrimSpace(r.URL.Query().Get("action"))
	if action == "" {
		action = pluginengine.ActionRun
	}
	var args map[string]any
	if r.Body != nil && r.ContentLength != 0 {
		if err := decodePluginBody(r, &args); err != nil {
			writeJSON(w, http.StatusOK, Response{Success: false, Message: "参数错误：" + err.Error()})
			return
		}
	}
	result, err := registry.Run(r.Context(), pluginID(r), action, args)
	if err != nil {
		pluginFailure(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"success": true,
		"message": result.Message,
		"details": result.Details,
	})
}
