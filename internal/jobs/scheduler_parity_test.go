package jobs

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/riverqueue/river"
)

type signinHandlerFake struct {
	report SiteSigninReport
	err    error
	calls  int
}

func (f *signinHandlerFake) SigninEnabledSites(context.Context) (SiteSigninReport, error) {
	f.calls++
	return f.report, f.err
}

func newJobsTestLogger() (*slog.Logger, *bytes.Buffer) {
	buffer := &bytes.Buffer{}
	return slog.New(slog.NewTextHandler(buffer, &slog.HandlerOptions{Level: slog.LevelDebug})), buffer
}

func TestNormalizeQueueMaxWorkers(t *testing.T) {
	notificationCases := []struct {
		requested int
		want      int
		wantErr   bool
	}{
		{0, notificationDefaultMaxWorkers, false},
		{1, 1, false},
		{notificationMaxWorkersLimit, notificationMaxWorkersLimit, false},
		// 超过上限收敛到上限，不能把上游打爆。
		{notificationMaxWorkersLimit + 5, notificationMaxWorkersLimit, false},
		{-1, 0, true},
	}
	for _, testCase := range notificationCases {
		got, err := normalizeNotificationMaxWorkers(testCase.requested)
		if testCase.wantErr {
			if err == nil {
				t.Fatalf("requested=%d 应报错", testCase.requested)
			}
			continue
		}
		if err != nil || got != testCase.want {
			t.Fatalf("requested=%d → %d %v，期望 %d", testCase.requested, got, err, testCase.want)
		}
	}

	scrapeCases := []struct {
		requested int
		want      int
		wantErr   bool
	}{
		{0, scrapeDefaultMaxWorkers, false},
		{2, 2, false},
		{scrapeMaxWorkersLimit + 1, scrapeMaxWorkersLimit, false},
		{-3, 0, true},
	}
	for _, testCase := range scrapeCases {
		got, err := normalizeScrapeMaxWorkers(testCase.requested)
		if testCase.wantErr {
			if err == nil {
				t.Fatalf("requested=%d 应报错", testCase.requested)
			}
			continue
		}
		if err != nil || got != testCase.want {
			t.Fatalf("requested=%d → %d %v，期望 %d", testCase.requested, got, err, testCase.want)
		}
	}
}

func TestRegisterWorkersWithoutHandler(t *testing.T) {
	// 没有处理器时该队列不启用：返回 0 且不报错（nil workers 也不应被触碰）。
	if workers, err := registerNotificationWorker(nil, ManagerConfig{}); workers != 0 || err != nil {
		t.Fatalf("无处理器 = %d %v", workers, err)
	}
	if workers, err := registerScrapeWorker(nil, ManagerConfig{}); workers != 0 || err != nil {
		t.Fatalf("无处理器 = %d %v", workers, err)
	}
	// 配了并发却没有处理器是配置错误，必须显式报错而不是静默忽略。
	if _, err := registerNotificationWorker(nil, ManagerConfig{NotificationMaxWorkers: 2}); err == nil {
		t.Fatal("缺少通知处理器却配置并发应报错")
	}
	if _, err := registerScrapeWorker(nil, ManagerConfig{ScrapeMaxWorkers: 2}); err == nil {
		t.Fatal("缺少 NFO 处理器却配置并发应报错")
	}
}

func TestSanitizedDownloadError(t *testing.T) {
	// 上下文取消/超时必须保持可识别，方便上层区分「被取消」与「真失败」。
	if got := sanitizedDownloadError(context.Background(), errors.Join(errors.New("包装"), context.Canceled), errors.New("兜底")); !errors.Is(got, context.Canceled) {
		t.Fatalf("取消 = %v", got)
	}
	if got := sanitizedDownloadError(context.Background(), errors.Join(errors.New("包装"), context.DeadlineExceeded), errors.New("兜底")); !errors.Is(got, context.DeadlineExceeded) {
		t.Fatalf("超时 = %v", got)
	}
	// 其它错误一律折叠成兜底文案，避免下游把原始错误（可能含凭据）抛给用户。
	fallback := errors.New("下载失败")
	if got := sanitizedDownloadError(context.Background(), errors.New("unexpected token in URL http://user:pass@host"), fallback); !errors.Is(got, fallback) {
		t.Fatalf("兜底 = %v", got)
	}
	if got := sanitizedDownloadError(nil, nil, fallback); !errors.Is(got, fallback) {
		t.Fatalf("nil 上下文 = %v", got)
	}
	// 上下文自身已取消时优先返回上下文错误。
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if got := sanitizedDownloadError(ctx, errors.New("无关错误"), fallback); !errors.Is(got, context.Canceled) {
		t.Fatalf("已取消上下文 = %v", got)
	}
}

func TestSiteSweepArgsAndPeriodicJobs(t *testing.T) {
	if got := (SiteSigninSweepArgs{}).Kind(); got != siteSigninSweepKind {
		t.Fatalf("签到 Kind = %q", got)
	}
	if got := (SiteUserDataRefreshArgs{}).Kind(); got != siteUserDataRefreshKind {
		t.Fatalf("用户数据刷新 Kind = %q", got)
	}
	if siteSigninSweepInterval != 24*time.Hour {
		t.Fatalf("签到间隔 = %v", siteSigninSweepInterval)
	}
	if siteSigninSweepPeriodicJob() == nil {
		t.Fatal("签到周期任务不应为空")
	}
	if downloadReconcilePeriodicJob() == nil {
		t.Fatal("下载对账周期任务不应为空")
	}
	args, opts := downloadReconcilePeriodicArgs()
	if args.Kind() != (DownloadReconcileSweepArgs{}).Kind() {
		t.Fatalf("对账周期参数 Kind = %q", args.Kind())
	}
	if opts == nil || opts.Queue != queueDownloadReconcile {
		t.Fatalf("对账周期队列 = %+v", opts)
	}
	// 超时必须是正数，否则 River 会让任务立即超时。
	if (&siteUserDataRefreshWorker{}).Timeout(nil) <= 0 {
		t.Fatal("用户数据刷新超时必须为正")
	}
}

func TestEnqueueSweepsGuards(t *testing.T) {
	var nilManager *Manager
	if err := nilManager.EnqueueNotificationSweep(nil); !errors.Is(err, ErrNotificationSweepExecution) {
		t.Fatalf("nil 上下文 = %v", err)
	}
	if err := nilManager.EnqueueNotificationSweep(context.Background()); !errors.Is(err, ErrNotificationSweepUnavailable) {
		t.Fatalf("未启用 = %v", err)
	}
	cancelled, cancel := context.WithCancel(context.Background())
	cancel()
	if err := nilManager.EnqueueNotificationSweep(cancelled); !errors.Is(err, context.Canceled) {
		t.Fatalf("已取消 = %v", err)
	}

	if err := nilManager.EnqueueScrapeSweep(nil); !errors.Is(err, ErrScrapeSweepExecution) {
		t.Fatalf("nil 上下文 = %v", err)
	}
	if err := nilManager.EnqueueScrapeSweep(context.Background()); !errors.Is(err, ErrScrapeSweepHandlerUnavailable) {
		t.Fatalf("未启用 = %v", err)
	}
	if err := nilManager.EnqueueScrapeSweep(cancelled); !errors.Is(err, context.Canceled) {
		t.Fatalf("已取消 = %v", err)
	}
	// 有 Manager 但没有数据库客户端时同样视为不可用（不能 panic）。
	if err := (&Manager{}).EnqueueNotificationSweep(context.Background()); !errors.Is(err, ErrNotificationSweepUnavailable) {
		t.Fatalf("无客户端 = %v", err)
	}
	if err := (&Manager{}).EnqueueScrapeSweep(context.Background()); !errors.Is(err, ErrScrapeSweepHandlerUnavailable) {
		t.Fatalf("无客户端 = %v", err)
	}
}

func TestSiteSigninSweepWorkerLogsReport(t *testing.T) {
	logger, buffer := newJobsTestLogger()
	handler := &signinHandlerFake{report: SiteSigninReport{
		Signed: 2, Skipped: 1, Failed: 1,
		Items: []SiteSigninResult{
			{SiteID: 1, Site: "A站", Status: "signed"},
			{SiteID: 2, Site: "B站", Status: "skipped", Reason: "已签到"},
			{SiteID: 3, Site: "C站", Status: "failed", Reason: "登录失败"},
		},
	}}
	worker := &siteSigninSweepWorker{handler: handler, logger: logger}
	if err := worker.Work(context.Background(), nil); err != nil {
		t.Fatalf("执行失败: %v", err)
	}
	if handler.calls != 1 {
		t.Fatalf("调用次数 = %d", handler.calls)
	}
	output := buffer.String()
	if !strings.Contains(output, "站点签到完成") {
		t.Fatalf("缺少完成日志: %s", output)
	}
	// 只有失败项要单独 WARN，成功/跳过不要刷日志。
	if !strings.Contains(output, "站点签到未成功") || !strings.Contains(output, "C站") {
		t.Fatalf("缺少失败明细: %s", output)
	}
	if strings.Contains(output, "A站") || strings.Contains(output, "B站") {
		t.Fatalf("成功/跳过项不应逐条记录: %s", output)
	}
}

func TestSiteSigninSweepWorkerHandlesHandlerErrors(t *testing.T) {
	logger, buffer := newJobsTestLogger()
	handler := &signinHandlerFake{err: errors.New("数据库不可用")}
	worker := &siteSigninSweepWorker{handler: handler, logger: logger}
	// 通道级错误必须向上返回，交给 River 重试。
	if err := worker.Work(context.Background(), nil); err == nil {
		t.Fatal("处理器错误应向上返回")
	}
	if !strings.Contains(buffer.String(), "站点签到失败") {
		t.Fatalf("缺少失败日志: %s", buffer.String())
	}

	// 没有处理器时静默返回（该任务未被启用）。
	idle := &siteSigninSweepWorker{logger: logger}
	if err := idle.Work(context.Background(), nil); err != nil {
		t.Fatalf("无处理器 = %v", err)
	}
	// 没有 logger 时回落到默认 logger，不能 panic。
	plain := &siteSigninSweepWorker{handler: &signinHandlerFake{}}
	if err := plain.Work(context.Background(), nil); err != nil {
		t.Fatalf("默认 logger = %v", err)
	}
	if plain.log() == nil {
		t.Fatal("logger 不应为空")
	}
}

func TestSchedulerJobArgsValidate(t *testing.T) {
	// 周期任务参数必须能被 River 序列化并校验通过，否则调度会在启动时报错。
	args := []river.JobArgs{
		SiteSigninSweepArgs{Key: "daily"},
		SiteUserDataRefreshArgs{Key: siteUserDataRefreshKey},
		DownloadReconcileSweepArgs{Key: downloadReconcileKey, Cutoff: time.Now().UTC()},
		NotificationSweepArgs{Key: notificationSweepKey},
		ScrapeSweepArgs{Key: scrapeSweepKey},
	}
	for _, arg := range args {
		validatable, ok := arg.(interface{ Validate() error })
		if !ok {
			continue
		}
		if err := validatable.Validate(); err != nil {
			t.Fatalf("%s 校验失败: %v", arg.Kind(), err)
		}
		if arg.Kind() == "" {
			t.Fatal("Kind 不应为空")
		}
	}
	// 固定 Key 的任务只接受约定的取值，写错时应在插入前就失败。
	if err := (SiteUserDataRefreshArgs{Key: "daily"}).Validate(); err == nil {
		t.Fatal("非法用户数据刷新 Key 应被拒绝")
	}
	if err := (DownloadReconcileSweepArgs{Key: "other", Cutoff: time.Now().UTC()}).Validate(time.Now().UTC()); err == nil {
		t.Fatal("非法对账 Key 应被拒绝")
	}
	// 没有截止时间的对账会把全量任务当目标，必须拒绝。
	if err := (DownloadReconcileSweepArgs{Key: downloadReconcileKey}).Validate(time.Now().UTC()); err == nil {
		t.Fatal("缺少 Cutoff 应被拒绝")
	}
}
