package httpapi

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strings"

	"github.com/go-chi/chi/v5"

	"github.com/moviepilot-go/moviepilot-go/internal/command"
	"github.com/moviepilot-go/moviepilot-go/internal/database"
	"github.com/moviepilot-go/moviepilot-go/internal/doctor"
)

// newCommandRegistry 装配预设命令。
//
// 命名与 V2 `app/command.py` 的预设命令对齐（help/doctor/sites/subscribes），
// 但本轮只提供只读命令：会改状态的动作继续留在各自的 HTTP 端点上。
// 注册表是编译期常量，构建失败属于编程错误，直接 panic 让启动期暴露问题，
// 而不是运行期悄悄退化成「没有命令」。
func newCommandRegistry(d Dependencies) *command.Registry {
	var registry *command.Registry
	built, err := command.NewRegistry(
		command.Command{
			Name: "help", Aliases: []string{"?", "h"}, Title: "命令列表",
			Description: "列出全部可用命令", Category: "系统",
			Handler: func(_ context.Context, _ command.Request) (command.Result, error) {
				descriptors := registry.List()
				var builder strings.Builder
				builder.WriteString(fmt.Sprintf("共 %d 条命令：", len(descriptors)))
				for _, item := range descriptors {
					builder.WriteString("\n· /" + item.Name)
					if item.Title != "" {
						builder.WriteString("　" + item.Title)
					}
					if item.AdminOnly {
						builder.WriteString("（仅管理员）")
					}
					if item.Description != "" {
						builder.WriteString("：" + item.Description)
					}
				}
				return command.Result{
					Text:    builder.String(),
					Details: map[string]any{"commands": descriptors},
				}, nil
			},
		},
		command.Command{
			Name: "doctor", Title: "自检",
			Description: "运行服务自检，汇总依赖/通知/对账/数据源结论", Category: "系统",
			Handler: func(ctx context.Context, _ command.Request) (command.Result, error) {
				return runDoctorCommand(ctx, d.Doctor)
			},
		},
		command.Command{
			Name: "subscribes", Aliases: []string{"subs"}, Title: "订阅列表",
			Description: "列出当前用户的订阅及状态统计", Category: "订阅",
			Handler: d.runSubscribeCommand,
		},
		command.Command{
			Name: "sites", Title: "站点列表",
			Description: "列出已配置站点与启用情况", Category: "站点",
			Handler: func(ctx context.Context, _ command.Request) (command.Result, error) {
				return d.runSitesCommand(ctx)
			},
		},
		command.Command{
			Name: "downloads", Aliases: []string{"dl"}, Title: "下载任务",
			Description: "统计当前正在下载的任务数", Category: "下载",
			Handler: d.runDownloadsCommand,
		},
	)
	if err != nil {
		panic("装配预设命令失败: " + err.Error())
	}
	// help 依赖注册表自身的条目，因此在构建完成后再赋值；
	// 命令只在请求期执行，早于赋值不可能发生。
	registry = built
	return registry
}

func runDoctorCommand(ctx context.Context, runner *doctor.Runner) (command.Result, error) {
	if runner == nil {
		return command.Result{}, errors.New("自检能力未接入")
	}
	report := runner.Run(ctx)
	var builder strings.Builder
	switch {
	case !report.OK:
		builder.WriteString("自检未通过：存在 fail 级问题")
	case report.Counts[doctor.StatusOK] == 0 && report.Counts[doctor.StatusSkipped] > 0:
		// 全部跳过同样满足「没有 fail」，但那不叫通过：一项都没真正验证过。
		// 说成「通过」会让没配任何依赖的部署误以为一切正常。
		builder.WriteString("自检未完成：没有任何一项真正验证（全部因依赖未接入被跳过）")
	default:
		builder.WriteString("自检通过：没有 fail 级问题")
	}
	for _, check := range report.Checks {
		builder.WriteString(fmt.Sprintf("\n· [%s] %s：%s", check.Status, check.Title, check.Message))
	}
	return command.Result{Text: builder.String(), Details: map[string]any{"report": report}}, nil
}

func (d Dependencies) runSubscribeCommand(ctx context.Context, req command.Request) (command.Result, error) {
	if d.Store == nil {
		return command.Result{}, errors.New("订阅存储未初始化")
	}
	items, err := d.Store.ListSubscriptions(ctx, req.Username, req.Admin)
	if err != nil {
		return command.Result{}, fmt.Errorf("查询订阅失败: %w", err)
	}
	text, details := formatSubscriptionSummary(items)
	return command.Result{Text: text, Details: details}, nil
}

// formatSubscriptionSummary 生成订阅命令的文本与结构化摘要。
// 抽成纯函数便于单测：这里决定用户看到的计数、状态顺序与截断提示。
func formatSubscriptionSummary(items []database.Subscription) (string, map[string]any) {
	counts := make(map[string]int, 4)
	lines := make([]string, 0, len(items))
	for _, item := range items {
		state := string(item.InternalState)
		counts[state]++
		if len(lines) < 10 {
			line := "· " + item.Name
			if item.MediaType != "" {
				line += "（" + item.MediaType + "）"
			}
			line += "　" + state
			lines = append(lines, line)
		}
	}
	text := fmt.Sprintf("共 %d 个订阅", len(items))
	for _, state := range sortedIntKeys(counts) {
		text += fmt.Sprintf("，%s %d", state, counts[state])
	}
	if len(lines) > 0 {
		text += "\n" + strings.Join(lines, "\n")
	}
	if len(items) > len(lines) {
		text += fmt.Sprintf("\n…还有 %d 个未显示", len(items)-len(lines))
	}
	return text, map[string]any{"total": len(items), "states": counts}
}

func (d Dependencies) runSitesCommand(ctx context.Context) (command.Result, error) {
	store := d.siteStore()
	if store == nil {
		return command.Result{}, errors.New("站点存储未初始化")
	}
	sites, err := store.ListSites(ctx)
	if err != nil {
		return command.Result{}, fmt.Errorf("查询站点失败: %w", err)
	}
	enabled := 0
	disabled := make([]string, 0, len(sites))
	lines := make([]string, 0, len(sites))
	for _, site := range sites {
		if site.Enabled {
			enabled++
		} else {
			disabled = append(disabled, site.Name)
		}
		if len(lines) < 10 {
			lines = append(lines, "· "+site.Name)
		}
	}
	text := fmt.Sprintf("共 %d 个站点，启用 %d 个", len(sites), enabled)
	if len(disabled) > 0 {
		text += "；未启用：" + strings.Join(disabled, "、")
	}
	if len(lines) > 0 {
		text += "\n" + strings.Join(lines, "\n")
	}
	return command.Result{
		Text:    text,
		Details: map[string]any{"total": len(sites), "enabled": enabled, "disabled": disabled},
	}, nil
}

// runDownloadsCommand 只统计数量与下载器分布：任务明细仍走 /download/ 端点，
// 避免把命令层变成第二套列表接口。
func (d Dependencies) runDownloadsCommand(ctx context.Context, req command.Request) (command.Result, error) {
	if d.DownloadTasks == nil {
		return command.Result{}, errors.New("下载任务源未初始化")
	}
	tasks, err := d.DownloadTasks.ListDownloading(ctx, req.Username)
	if err != nil {
		return command.Result{}, fmt.Errorf("查询下载任务失败: %w", err)
	}
	byDownloader := make(map[string]int, 4)
	for _, task := range tasks {
		byDownloader[task.Downloader]++
	}
	text := fmt.Sprintf("正在下载 %d 个任务", len(tasks))
	for _, name := range sortedIntKeys(byDownloader) {
		text += fmt.Sprintf("，%s %d", name, byDownloader[name])
	}
	return command.Result{
		Text:    text,
		Details: map[string]any{"total": len(tasks), "downloaders": byDownloader},
	}, nil
}

func sortedIntKeys(values map[string]int) []string {
	keys := make([]string, 0, len(values))
	for key := range values {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}

// listCommands 返回全部预设命令，供前端/外部集成展示。
func (d Dependencies) listCommands(registry *command.Registry) http.HandlerFunc {
	return func(w http.ResponseWriter, _ *http.Request) {
		if registry == nil {
			writeError(w, http.StatusServiceUnavailable, "命令注册表未初始化")
			return
		}
		writeSuccess(w, registry.List())
	}
}

// executeCommand 接受聊天文本（`/doctor`、`/sites@bot`）并执行。
func (d Dependencies) executeCommand(registry *command.Registry) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if registry == nil {
			writeError(w, http.StatusServiceUnavailable, "命令注册表未初始化")
			return
		}
		var payload struct {
			Command string   `json:"command"`
			Args    []string `json:"args"`
		}
		if err := json.NewDecoder(io.LimitReader(r.Body, d.Config.MaxRequestBody)).Decode(&payload); err != nil {
			writeError(w, http.StatusBadRequest, "命令参数无效")
			return
		}
		text := payload.Command
		if len(payload.Args) > 0 {
			text = strings.TrimSpace(text + " " + strings.Join(payload.Args, " "))
		}
		result, err := registry.Execute(r.Context(), text, requestFromClaims(r))
		d.dispatchCommand(w, result, err)
	}
}

// executeCommandName 按路径里的命令名执行，参数走请求体。
func (d Dependencies) executeCommandName(registry *command.Registry) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if registry == nil {
			writeError(w, http.StatusServiceUnavailable, "命令注册表未初始化")
			return
		}
		var payload struct {
			Args []string `json:"args"`
		}
		if err := json.NewDecoder(io.LimitReader(r.Body, d.Config.MaxRequestBody)).Decode(&payload); err != nil && !errors.Is(err, io.EOF) {
			writeError(w, http.StatusBadRequest, "命令参数无效")
			return
		}
		req := requestFromClaims(r)
		req.Args = payload.Args
		result, err := registry.ExecuteName(r.Context(), chi.URLParam(r, "name"), req)
		d.dispatchCommand(w, result, err)
	}
}

func (d Dependencies) dispatchCommand(w http.ResponseWriter, result command.Result, err error) {
	switch {
	case err == nil:
		writeSuccess(w, result)
	case errors.Is(err, command.ErrEmpty):
		writeError(w, http.StatusBadRequest, "命令为空")
	case errors.Is(err, command.ErrUnknown):
		writeError(w, http.StatusNotFound, "未知命令")
	case errors.Is(err, command.ErrForbidden):
		writeError(w, http.StatusForbidden, "没有权限执行该命令")
	default:
		writeError(w, http.StatusInternalServerError, "执行命令失败: "+err.Error())
	}
}

// requestFromClaims 把调用者身份转成命令执行上下文。
func requestFromClaims(r *http.Request) command.Request {
	claims, _ := claimsFromContext(r.Context())
	return command.Request{Username: claims.Username, Admin: claims.SuperUser}
}
