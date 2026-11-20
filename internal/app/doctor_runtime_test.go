package app

import (
	"context"
	"strings"
	"testing"

	"github.com/moviepilot-go/moviepilot-go/internal/config"
	"github.com/moviepilot-go/moviepilot-go/internal/doctor"
)

// 缺组件时体检必须**如实跳过**，而不是报 ok——这是 doctor 存在的意义。
func TestNewDoctorRunnerSkipsMissingComponents(t *testing.T) {
	runner := newDoctorRunner(config.Config{Environment: "test"}, nil, nil, nil, nil, nil)
	report := runner.Run(context.Background())

	if report.Counts[doctor.StatusFailed] != 0 {
		t.Fatalf("没有组件时不应出现失败项: %+v", report.Counts)
	}
	if report.Counts[doctor.StatusOK] < 1 {
		t.Fatalf("环境信息应至少产生一项 ok: %+v", report.Counts)
	}
	if report.Counts[doctor.StatusSkipped] < 5 {
		t.Fatalf("数据库/缓存/通知/对账/元数据都应记为 skip，实际 %+v", report.Counts)
	}

	skipped := make(map[string]bool, len(report.Checks))
	for _, check := range report.Checks {
		if check.Status != doctor.StatusSkipped {
			continue
		}
		skipped[strings.ToLower(check.ID)] = true
		if strings.TrimSpace(check.Message) == "" {
			t.Fatalf("跳过项 %q 必须说明原因，不能只留空消息", check.ID)
		}
	}
	for _, wanted := range []string{"database", "redis"} {
		found := false
		for id := range skipped {
			if strings.Contains(id, wanted) {
				found = true
				break
			}
		}
		if !found {
			t.Fatalf("%q 应以 skip 出现，实际跳过项: %v", wanted, skipped)
		}
	}
}

// 探针函数必须容忍 nil 依赖：启动早期或组件未接线时不能 panic。
func TestDoctorProbesAreNilSafe(t *testing.T) {
	if probe := notificationDoctorProbe(nil); probe.Deliverable != 0 || probe.Configs != 0 {
		t.Fatalf("没有通知注册表时不应宣称有可投递渠道: %+v", probe)
	}
	if probe := reconcileDoctorProbe(nil); probe.Ran {
		t.Fatalf("没有对账处理器时不应宣称跑过对账: %+v", probe)
	}
	// 没有元数据协调器时，doctor 仍会列出四个已知数据源并逐个说明为什么跳过，
	// 而不是返回空列表——「哪些源没配上」本身就是需要被看到的信息。
	probes := metadataDoctorProbes(context.Background(), nil)
	if len(probes) != 4 {
		t.Fatalf("应列出 4 个数据源，实际 %+v", probes)
	}
	seen := make(map[string]bool, len(probes))
	for _, probe := range probes {
		if probe.Status != doctor.StatusSkipped {
			t.Fatalf("数据源 %q 应为 skip，实际 %q", probe.Source, probe.Status)
		}
		if strings.TrimSpace(probe.Error) == "" {
			t.Fatalf("数据源 %q 必须说明跳过原因", probe.Source)
		}
		seen[probe.Source] = true
	}
	for _, wanted := range []string{"themoviedb", "douban", "bangumi", "anilist"} {
		if !seen[wanted] {
			t.Fatalf("缺少数据源 %q，实际 %v", wanted, seen)
		}
	}
}
