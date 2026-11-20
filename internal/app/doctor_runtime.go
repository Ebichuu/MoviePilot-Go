package app

import (
	"context"
	"fmt"
	"strings"

	"github.com/moviepilot-go/moviepilot-go/internal/cache"
	"github.com/moviepilot-go/moviepilot-go/internal/config"
	"github.com/moviepilot-go/moviepilot-go/internal/database"
	"github.com/moviepilot-go/moviepilot-go/internal/doctor"
	"github.com/moviepilot-go/moviepilot-go/internal/downloadflow"
	"github.com/moviepilot-go/moviepilot-go/internal/metadata"
	"github.com/moviepilot-go/moviepilot-go/internal/notification"
)

// newDoctorRunner 装配在线自检。
//
// 没有接入的能力保持 nil：报告里会显示「本次运行未接入」，而不是伪装成健康。
// 这样 /system/doctor 的结论与运维真实可观测面一致。
func newDoctorRunner(
	cfg config.Config,
	db *database.DB,
	redis *cache.Redis,
	registry *notification.Registry,
	reconcile *downloadflow.ReconciliationHandler,
	discover *metadata.Discover,
) *doctor.Runner {
	probes := doctor.Probes{Environment: cfg.Environment}
	if db != nil {
		probes.Database = db.Ping
	}
	if redis != nil {
		probes.Redis = redis.Ping
	}
	if registry != nil {
		probes.Notification = func() doctor.NotificationProbe {
			return notificationDoctorProbe(registry)
		}
	}
	if reconcile != nil {
		probes.Reconcile = func() doctor.ReconcileProbe {
			return reconcileDoctorProbe(reconcile)
		}
	}
	if discover != nil {
		probes.Metadata = func(ctx context.Context) []doctor.SourceProbe {
			return metadataDoctorProbes(ctx, discover)
		}
	}
	return &doctor.Runner{Probes: probes}
}

// notificationDoctorProbe 统计「配置了几个渠道」与「真的能被业务通知选中几个渠道」。
func notificationDoctorProbe(registry *notification.Registry) doctor.NotificationProbe {
	configured := registry.Targets(notification.Message{})
	probe := doctor.NotificationProbe{Configs: len(configured), Telegrams: len(configured)}
	deliverable := make(map[string]struct{})
	for _, mtype := range []notification.Type{
		notification.TypeDownload, notification.TypeOrganize, notification.TypeSubscribe,
		notification.TypeSiteMessage, notification.TypeMediaServer, notification.TypeManual,
		notification.TypePlugin,
	} {
		for _, target := range registry.Targets(notification.Message{Type: mtype}) {
			deliverable[string(target.Channel)+"/"+target.Source] = struct{}{}
		}
	}
	probe.Deliverable = len(deliverable)
	if probe.Deliverable == 0 {
		if probe.Configs == 0 {
			probe.Reason = "尚未配置任何通知渠道"
		} else {
			probe.Reason = "已配置渠道，但没有任何渠道在 switchs 里勾选业务通知类型"
		}
	}
	return probe
}

// reconcileDoctorProbe 暴露最近一轮下载对账的收敛情况与失败明细，
// 让「每轮失败」这类问题不必再靠翻 River 的 errors 数组定位。
func reconcileDoctorProbe(handler *downloadflow.ReconciliationHandler) doctor.ReconcileProbe {
	report := handler.LastSweepReport()
	probe := doctor.ReconcileProbe{
		Ran:        !report.FinishedAt.IsZero(),
		Cutoff:     report.Cutoff,
		FinishedAt: report.FinishedAt,
		Scanned:    report.Scanned,
		Succeeded:  report.Succeeded,
		Failed:     len(report.Failures),
		Orphans:    int(report.Orphans),
	}
	for _, failure := range report.Failures {
		probe.Failures = append(probe.Failures, fmt.Sprintf(
			"intent=%s downloader=%s state=%s stage=%s reason=%s",
			failure.IntentID, failure.DownloaderID, failure.State, failure.Stage, failure.Err,
		))
	}
	return probe
}

// metadataDoctorProbes 逐个探测推荐/探索数据源。未配置的来源记为 skip，
// 与「配置了但请求失败」区分开。
func metadataDoctorProbes(ctx context.Context, discover *metadata.Discover) []doctor.SourceProbe {
	type sourceProbe struct {
		source string
		run    func() ([]metadata.Media, error)
	}
	probes := []sourceProbe{
		{"themoviedb", func() ([]metadata.Media, error) { return discover.TMDBTrending(ctx, 1) }},
		{"douban", func() ([]metadata.Media, error) { return discover.DoubanCollection(ctx, "movie_showing", 1, 5) }},
		{"bangumi", func() ([]metadata.Media, error) { return discover.BangumiCalendar(ctx, 1, 5) }},
		{"anilist", func() ([]metadata.Media, error) { return discover.AniListTrending(ctx, 1) }},
	}
	result := make([]doctor.SourceProbe, 0, len(probes))
	for _, item := range probes {
		items, err := item.run()
		switch {
		case err != nil:
			status := doctor.StatusFailed
			if strings.Contains(err.Error(), "未配置") {
				status = doctor.StatusSkipped
			}
			result = append(result, doctor.SourceProbe{Source: item.source, Status: status, Error: err.Error()})
		case len(items) == 0:
			result = append(result, doctor.SourceProbe{Source: item.source, Status: doctor.StatusWarning})
		default:
			result = append(result, doctor.SourceProbe{Source: item.source, Status: doctor.StatusOK, Items: len(items)})
		}
	}
	return result
}
