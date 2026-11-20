// Package doctor 提供服务自检：把「依赖是否连通、通知是否真有投递渠道、
// 下载对账是否收敛、数据源是否可用」这些运维事实汇总成一份可读报告。
//
// 设计原则与 V2 `app/doctor` 一致：只报告观测到的事实，不用「看起来正常」
// 掩盖缺失的配置。每项检查都有明确的 ok / warn / fail / skip 与中文原因。
package doctor

import (
	"context"
	"fmt"
	"sort"
	"time"
)

// Status 是单项检查的结论。
type Status string

const (
	StatusOK      Status = "ok"
	StatusWarning Status = "warn"
	StatusFailed  Status = "fail"
	StatusSkipped Status = "skip"
)

// Check 是一项检查结果。Details 只放非敏感计数与标识，禁止写入凭据或密钥。
type Check struct {
	ID      string         `json:"id"`
	Title   string         `json:"title"`
	Status  Status         `json:"status"`
	Message string         `json:"message"`
	Details map[string]any `json:"details,omitempty"`
}

// Report 是一次自检的完整结果。
type Report struct {
	Version     string         `json:"version"`
	GeneratedAt time.Time      `json:"generated_at"`
	OK          bool           `json:"ok"`
	Counts      map[Status]int `json:"counts"`
	Checks      []Check        `json:"checks"`
}

// NotificationProbe 描述通知渠道的可用情况。
type NotificationProbe struct {
	Configs     int
	Telegrams   int
	Deliverable int
	Reason      string
}

// ReconcileProbe 描述最近一轮下载对账的结果。
type ReconcileProbe struct {
	Ran        bool
	Cutoff     time.Time
	FinishedAt time.Time
	Scanned    int
	Succeeded  int
	Failed     int
	Orphans    int
	Failures   []string
}

// SourceProbe 描述单个推荐/探索数据源的可用情况。
type SourceProbe struct {
	Source string `json:"source"`
	Status Status `json:"status"`
	Items  int    `json:"items"`
	Error  string `json:"error,omitempty"`
}

// ComponentProbe 描述下载器或媒体服务器的连接情况。
type ComponentProbe struct {
	Name  string `json:"name"`
	Type  string `json:"type"`
	OK    bool   `json:"ok"`
	Error string `json:"error,omitempty"`
}

// Probes 由调用方注入具体能力；为 nil 表示该能力在本次运行中未接入。
type Probes struct {
	Environment  string
	Database     func(context.Context) error
	Redis        func(context.Context) error
	Notification func() NotificationProbe
	Reconcile    func() ReconcileProbe
	Metadata     func(context.Context) []SourceProbe
	Downloaders  func() []ComponentProbe
	MediaServers func() []ComponentProbe
}

// Runner 执行固定顺序的检查，保证输出稳定可 diff。
type Runner struct {
	Version string
	Now     func() time.Time
	Probes  Probes
}

// Run 依次执行全部已接入的检查。
func (r Runner) Run(ctx context.Context) Report {
	if ctx == nil {
		ctx = context.Background()
	}
	now := time.Now().UTC()
	if r.Now != nil {
		now = r.Now().UTC()
	}
	report := Report{
		Version:     r.Version,
		GeneratedAt: now,
		Counts:      make(map[Status]int, 4),
		Checks:      make([]Check, 0, 8),
	}
	for _, check := range []Check{
		r.checkEnvironment(),
		r.checkDependency(ctx, "database", "数据库", r.Probes.Database),
		r.checkDependency(ctx, "redis", "缓存/Redis", r.Probes.Redis),
		r.checkNotification(),
		r.checkReconcile(),
		r.checkComponents("downloaders", "下载器", r.Probes.Downloaders),
		r.checkComponents("mediaservers", "媒体服务器", r.Probes.MediaServers),
		r.checkMetadata(ctx),
	} {
		report.Checks = append(report.Checks, check)
		report.Counts[check.Status]++
	}
	report.OK = report.Counts[StatusFailed] == 0
	return report
}

func (r Runner) checkEnvironment() Check {
	check := Check{ID: "environment", Title: "运行环境", Details: map[string]any{}}
	if r.Probes.Environment == "" {
		check.Status = StatusSkipped
		check.Message = "本次运行未提供环境信息"
		return check
	}
	check.Details["environment"] = r.Probes.Environment
	if r.Probes.Environment == "development" {
		check.Status = StatusOK
		check.Message = "开发环境"
		return check
	}
	check.Status = StatusOK
	check.Message = "配置已加载"
	return check
}

func (r Runner) checkDependency(
	ctx context.Context,
	id string,
	title string,
	probe func(context.Context) error,
) Check {
	check := Check{ID: id, Title: title}
	if probe == nil {
		check.Status = StatusSkipped
		check.Message = "本次运行未接入该依赖"
		return check
	}
	runCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	if err := probe(runCtx); err != nil {
		check.Status = StatusFailed
		check.Message = "连接失败"
		check.Details = map[string]any{"error": err.Error()}
		return check
	}
	check.Status = StatusOK
	check.Message = "连接正常"
	return check
}

func (r Runner) checkNotification() Check {
	check := Check{ID: "notification", Title: "通知投递"}
	if r.Probes.Notification == nil {
		check.Status = StatusSkipped
		check.Message = "本次运行未接入通知注册表"
		return check
	}
	probe := r.Probes.Notification()
	check.Details = map[string]any{
		"configs":     probe.Configs,
		"telegrams":   probe.Telegrams,
		"deliverable": probe.Deliverable,
	}
	if probe.Deliverable > 0 {
		check.Status = StatusOK
		check.Message = fmt.Sprintf("%d 个渠道可投递", probe.Deliverable)
		return check
	}
	// 有配置但没有任何类型被勾选，是「outbox 已发布、notifications 0 行」的根因。
	check.Status = StatusFailed
	check.Message = "没有可投递的通知渠道"
	if probe.Reason != "" {
		check.Message = probe.Reason
	}
	return check
}

func (r Runner) checkReconcile() Check {
	check := Check{ID: "download_reconcile", Title: "下载对账"}
	if r.Probes.Reconcile == nil {
		check.Status = StatusSkipped
		check.Message = "本次运行未接入对账处理器"
		return check
	}
	probe := r.Probes.Reconcile()
	if !probe.Ran {
		check.Status = StatusWarning
		check.Message = "服务启动后尚未完成一轮对账"
		return check
	}
	check.Details = map[string]any{
		"cutoff":      probe.Cutoff.Format(time.RFC3339),
		"finished_at": probe.FinishedAt.Format(time.RFC3339),
		"scanned":     probe.Scanned,
		"succeeded":   probe.Succeeded,
		"failed":      probe.Failed,
		"orphans":     probe.Orphans,
	}
	if probe.Failed > 0 {
		check.Status = StatusWarning
		check.Message = fmt.Sprintf("%d 个目标未收敛，下一轮会重试", probe.Failed)
		check.Details["failures"] = append([]string(nil), probe.Failures...)
		return check
	}
	check.Status = StatusOK
	check.Message = fmt.Sprintf("%d 个目标全部收敛", probe.Succeeded)
	return check
}

func (r Runner) checkComponents(
	id string,
	title string,
	probe func() []ComponentProbe,
) Check {
	check := Check{ID: id, Title: title}
	if probe == nil {
		check.Status = StatusSkipped
		check.Message = "本次运行未接入该组件"
		return check
	}
	components := probe()
	check.Details = map[string]any{"total": len(components)}
	if len(components) == 0 {
		check.Status = StatusWarning
		check.Message = "未配置任何" + title
		return check
	}
	failed := make([]map[string]any, 0, len(components))
	for _, component := range components {
		if component.OK {
			continue
		}
		item := map[string]any{"name": component.Name, "type": component.Type}
		if component.Error != "" {
			item["error"] = component.Error
		}
		failed = append(failed, item)
	}
	if len(failed) == 0 {
		check.Status = StatusOK
		check.Message = fmt.Sprintf("%d 个%s连接正常", len(components), title)
		return check
	}
	check.Status = StatusWarning
	check.Message = fmt.Sprintf("%d/%d 个%s连接失败", len(failed), len(components), title)
	check.Details["failed"] = failed
	return check
}

func (r Runner) checkMetadata(ctx context.Context) Check {
	check := Check{ID: "metadata", Title: "推荐/探索数据源"}
	if r.Probes.Metadata == nil {
		check.Status = StatusSkipped
		check.Message = "本次运行未接入元数据门面"
		return check
	}
	runCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	sources := r.Probes.Metadata(runCtx)
	if len(sources) == 0 {
		check.Status = StatusSkipped
		check.Message = "没有可探测的数据源"
		return check
	}
	details := make([]map[string]any, 0, len(sources))
	available, broken := 0, 0
	for _, source := range sources {
		item := map[string]any{
			"source": source.Source,
			"status": string(source.Status),
			"items":  source.Items,
		}
		if source.Error != "" {
			item["error"] = source.Error
		}
		details = append(details, item)
		switch source.Status {
		case StatusOK:
			available++
		case StatusFailed:
			broken++
		}
	}
	sort.Slice(details, func(i, j int) bool {
		return details[i]["source"].(string) < details[j]["source"].(string)
	})
	check.Details = map[string]any{"sources": details}
	switch {
	case broken > 0:
		check.Status = StatusFailed
		check.Message = fmt.Sprintf("%d/%d 个数据源不可用", broken, len(sources))
	case available == 0:
		check.Status = StatusWarning
		check.Message = "没有数据源返回内容"
	default:
		check.Status = StatusOK
		check.Message = fmt.Sprintf("%d 个数据源可用", available)
	}
	return check
}
