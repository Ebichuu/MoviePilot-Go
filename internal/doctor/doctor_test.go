package doctor

import (
	"context"
	"errors"
	"testing"
	"time"
)

func TestRunnerReportsFactsAndFailsOnBrokenNotification(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 11, 19, 4, 0, 0, 0, time.UTC)
	runner := Runner{
		Version: "test",
		Now:     func() time.Time { return now },
		Probes: Probes{
			Environment: "production",
			Database:    func(context.Context) error { return nil },
			Redis:       func(context.Context) error { return errors.New("redis down") },
			Notification: func() NotificationProbe {
				return NotificationProbe{Configs: 1, Telegrams: 1, Deliverable: 0, Reason: "没有启用的渠道勾选该类型"}
			},
			Reconcile: func() ReconcileProbe {
				return ReconcileProbe{
					Ran: true, Cutoff: now.Add(-time.Minute), FinishedAt: now,
					Scanned: 3, Succeeded: 2, Failed: 1,
					Failures: []string{"intent=a stage=acceptance reason=x"},
				}
			},
			Downloaders: func() []ComponentProbe {
				return []ComponentProbe{{Name: "qb", Type: "qbittorrent", OK: true}}
			},
			MediaServers: func() []ComponentProbe { return nil },
			Metadata: func(context.Context) []SourceProbe {
				return []SourceProbe{
					{Source: "themoviedb", Status: StatusOK, Items: 20},
					{Source: "douban", Status: StatusFailed, Error: "HTTP 401"},
				}
			},
		},
	}

	report := runner.Run(context.Background())
	if report.OK {
		t.Fatal("存在 fail 级检查时报告不应判定为健康")
	}
	if !report.GeneratedAt.Equal(now) || report.Version != "test" {
		t.Fatalf("report 头信息错误: %+v", report)
	}
	byID := make(map[string]Check, len(report.Checks))
	for _, check := range report.Checks {
		byID[check.ID] = check
	}
	if got := byID["database"].Status; got != StatusOK {
		t.Fatalf("database = %s", got)
	}
	if got := byID["redis"].Status; got != StatusFailed {
		t.Fatalf("redis = %s, want fail", got)
	}
	if got := byID["notification"]; got.Status != StatusFailed || got.Message != "没有启用的渠道勾选该类型" {
		t.Fatalf("notification = %+v", byID["notification"])
	}
	if got := byID["download_reconcile"]; got.Status != StatusWarning {
		t.Fatalf("download_reconcile = %+v, want warn", got)
	} else if failures, ok := got.Details["failures"].([]string); !ok || len(failures) != 1 {
		t.Fatalf("download_reconcile 未带上失败明细: %+v", got.Details)
	}
	if got := byID["mediaservers"].Status; got != StatusWarning {
		t.Fatalf("mediaservers = %+v, want warn", got)
	}
	if got := byID["metadata"].Status; got != StatusFailed {
		t.Fatalf("metadata = %+v, want fail", got)
	}
	if report.Counts[StatusFailed] != 3 {
		t.Fatalf("fail 计数 = %d, want 3", report.Counts[StatusFailed])
	}
}

func TestRunnerMarksUnwiredProbesAsSkipped(t *testing.T) {
	t.Parallel()

	report := Runner{Version: "test"}.Run(context.Background())
	if !report.OK {
		t.Fatalf("未接入任何能力时不应判定为失败: %+v", report)
	}
	for _, check := range report.Checks {
		if check.Status != StatusSkipped {
			t.Fatalf("%s = %s, want skip", check.ID, check.Status)
		}
	}
	if report.Counts[StatusSkipped] != len(report.Checks) {
		t.Fatalf("skip 计数 = %d", report.Counts[StatusSkipped])
	}
}
