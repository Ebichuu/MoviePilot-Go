package notificationflow

import (
	"errors"
	"log/slog"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/moviepilot-go/moviepilot-go/internal/notification"
	"github.com/moviepilot-go/moviepilot-go/internal/organization"
	"github.com/moviepilot-go/moviepilot-go/internal/organizer"
)

func invalidTargetPlanner(t *testing.T) *Planner {
	t.Helper()
	planner, err := NewPlanner(staticTargetProvider{{Channel: "unsupported-channel"}})
	if err != nil {
		t.Fatal(err)
	}
	return planner
}

func TestPlannerSetLogger(t *testing.T) {
	t.Parallel()
	var nilPlanner *Planner
	nilPlanner.SetLogger(slog.Default())

	planner, err := NewPlanner(staticTargetProvider{})
	if err != nil {
		t.Fatal(err)
	}
	planner.SetLogger(nil)

	var buffer strings.Builder
	planner.SetLogger(slog.New(slog.NewTextHandler(&buffer, nil)))
	if _, err := planner.planWithCount(uuid.New(), "alice", 1); err != nil {
		t.Fatalf("空目标计划应有效: %v", err)
	}
	if !strings.Contains(buffer.String(), "没有可投递的渠道") {
		t.Fatalf("空目标计划未记录日志: %q", buffer.String())
	}
}

func TestPlannerPlanNilReceiver(t *testing.T) {
	t.Parallel()
	var planner *Planner
	if _, err := planner.Plan(uuid.New(), "alice", organizer.Plan{}); !errors.Is(err, ErrInvalidPlan) {
		t.Fatalf("nil planner error = %v", err)
	}
}

func TestPlannerRejectsInvalidTargets(t *testing.T) {
	t.Parallel()
	if _, err := invalidTargetPlanner(t).planWithCount(uuid.New(), "alice", 1); !errors.Is(err, ErrInvalidPlan) {
		t.Fatalf("非法渠道 error = %v", err)
	}

	batchID := uuid.New()
	plan, err := invalidTargetPlanner(t).Plan(batchID, "alice", organizer.Plan{Operations: []organizer.Operation{{}, {}}})
	if err == nil && !plan.Valid() {
		t.Fatal("Plan 返回了无效计划")
	}
}

func TestPlannerPlanSuccessLegacyTargetFailure(t *testing.T) {
	t.Parallel()
	batchID := uuid.New()
	intentID := uuid.New()
	media := organization.MediaContext{TitleYear: "电影 (2026)", MediaType: "电影"}
	groupKey, err := organization.BuildGroupKey(media, 0)
	if err != nil {
		t.Fatal(err)
	}
	legacy := organization.SuccessSnapshot{
		BatchID: batchID, IntentID: intentID, GroupKey: groupKey,
		Legacy: true, Username: "alice", NeedNotify: true, Background: true, Media: media,
	}
	if _, err := invalidTargetPlanner(t).PlanSuccess(batchID, "alice",
		[]organization.SuccessSnapshot{legacy}, ""); !errors.Is(err, ErrInvalidPlan) {
		t.Fatalf("legacy 目标失败 = %v", err)
	}
}

// resultFixtures 生成「文件 0 成功、文件 1 失败」的终止结果与失败列表。
func resultFixtures(t *testing.T) (uuid.UUID, []organization.ResultSnapshot, []organization.FileFailure) {
	t.Helper()
	batchID := uuid.New()
	intentID := uuid.New()
	media := organization.MediaContext{TitleYear: "示例剧 (2026)", MediaType: "电视剧", Category: "国产剧"}
	snapshot := plannerResultSnapshot(t, batchID, intentID, media, 1, []organization.FileFact{
		{Ordinal: 0, Role: "video", SizeBytes: 100, Season: 1, Episodes: []int{1}},
		{Ordinal: 1, Role: "video", SizeBytes: 200, Season: 1, Episodes: []int{2}},
	})
	results, failures, err := organization.ProjectTerminalResults(
		[]organization.SuccessSnapshot{snapshot},
		[]organization.FileOutcome{
			{Ordinal: 0, State: organization.FileOutcomeCompleted},
			{Ordinal: 1, State: organization.FileOutcomeTerminalFailed, Error: "磁盘空间不足"},
		},
	)
	if err != nil {
		t.Fatal(err)
	}
	if len(results) != 1 || len(failures) != 1 {
		t.Fatalf("fixtures = %d results, %d failures", len(results), len(failures))
	}
	return batchID, results, failures
}

func validPlanner(t *testing.T) *Planner {
	t.Helper()
	planner, err := NewPlanner(staticTargetProvider{{Channel: notification.ChannelTelegram, Source: "通知"}})
	if err != nil {
		t.Fatal(err)
	}
	return planner
}

func TestPlanResultsGuards(t *testing.T) {
	t.Parallel()
	batchID, results, failures := resultFixtures(t)

	var nilPlanner *Planner
	if _, err := nilPlanner.PlanResults(batchID, "alice", results, failures, ""); !errors.Is(err, ErrInvalidPlan) {
		t.Fatalf("nil planner = %v", err)
	}
	planner := validPlanner(t)
	if _, err := planner.PlanResults(batchID, "  ", results, failures, ""); !errors.Is(err, ErrInvalidPlan) {
		t.Fatalf("空用户名 = %v", err)
	}
	if _, err := planner.PlanResults(uuid.Nil, "alice", results, failures, ""); !errors.Is(err, ErrInvalidPlan) {
		t.Fatalf("空批次 = %v", err)
	}
	tooMany := make([]organization.ResultSnapshot, MaxEventsPerPlan+1)
	if _, err := planner.PlanResults(batchID, "alice", tooMany, nil, ""); !errors.Is(err, ErrInvalidPlan) {
		t.Fatalf("过多结果 = %v", err)
	}
}

func TestPlanResultsRejectsInconsistentIdentity(t *testing.T) {
	t.Parallel()
	batchID, results, failures := resultFixtures(t)
	planner := validPlanner(t)

	mismatched := results[0].Clone()
	mismatched.Snapshot.BatchID = uuid.New()
	if _, err := planner.PlanResults(batchID, "alice", []organization.ResultSnapshot{mismatched}, nil, ""); !errors.Is(err, ErrInvalidPlan) {
		t.Fatalf("批次不一致 = %v", err)
	}

	duplicated := append(append([]organization.ResultSnapshot(nil), results...), results[0])
	if _, err := planner.PlanResults(batchID, "alice", duplicated, nil, ""); !errors.Is(err, ErrInvalidPlan) {
		t.Fatalf("重复分组 = %v", err)
	}

	badFailure := failures[0].Clone()
	badFailure.Snapshot.BatchID = uuid.New()
	if _, err := planner.PlanResults(batchID, "alice", results, []organization.FileFailure{badFailure}, ""); !errors.Is(err, ErrInvalidPlan) {
		t.Fatalf("失败批次不一致 = %v", err)
	}

	if _, err := planner.PlanResults(batchID, "alice", results,
		[]organization.FileFailure{failures[0], failures[0]}, ""); !errors.Is(err, ErrInvalidPlan) {
		t.Fatalf("重复失败序号 = %v", err)
	}

	// 失败原因非法会让 V2 手动通知无法渲染。
	unrenderable := failures[0].Clone()
	unrenderable.Error = ""
	if _, err := planner.PlanResults(batchID, "alice", results,
		[]organization.FileFailure{unrenderable}, ""); !errors.Is(err, ErrInvalidPlan) {
		t.Fatalf("无法渲染的失败 = %v", err)
	}
}

func TestPlanResultsLegacyGuards(t *testing.T) {
	t.Parallel()
	planner := validPlanner(t)
	batchID := uuid.New()
	intentID := uuid.New()
	media := organization.MediaContext{TitleYear: "电影 (2026)", MediaType: "电影"}
	groupKey, err := organization.BuildGroupKey(media, 0)
	if err != nil {
		t.Fatal(err)
	}
	legacy := organization.ResultSnapshot{Snapshot: organization.SuccessSnapshot{
		BatchID: batchID, IntentID: intentID, GroupKey: groupKey,
		Legacy: true, Username: "alice", NeedNotify: true, Background: true, Media: media,
	}}
	if _, err := planner.PlanResults(batchID, "alice", []organization.ResultSnapshot{legacy}, nil, ""); err != nil {
		t.Fatalf("legacy 结果应成功: %v", err)
	}

	withError := legacy.Clone()
	withError.ErrMsg = "失败"
	if _, err := planner.PlanResults(batchID, "alice", []organization.ResultSnapshot{withError}, nil, ""); !errors.Is(err, ErrInvalidPlan) {
		t.Fatalf("legacy 带错误 = %v", err)
	}

	if _, err := planner.PlanResults(batchID, "alice",
		[]organization.ResultSnapshot{legacy, legacy.Clone()}, nil, ""); !errors.Is(err, ErrInvalidPlan) {
		t.Fatalf("legacy 多结果 = %v", err)
	}
}

func TestPlanResultsSkipsIneligibleAndRendersFailures(t *testing.T) {
	t.Parallel()
	batchID, results, _ := resultFixtures(t)
	planner := validPlanner(t)

	ineligible := results[0].Clone()
	ineligible.Snapshot.NeedNotify = false
	ineligible.Snapshot.Manual = true
	plan, err := planner.PlanResults(batchID, "alice", []organization.ResultSnapshot{ineligible}, nil, "")
	if err != nil || len(plan.Events) != 0 {
		t.Fatalf("不可通知结果应生成空计划: %#v, %v", plan, err)
	}
}

func TestPlanResultsRejectsBrokenTerminalResults(t *testing.T) {
	t.Parallel()
	batchID, results, failures := resultFixtures(t)
	planner := validPlanner(t)

	// TerminalOrdinal 小于组内最大序号时不能渲染成功通知。
	broken := results[0].Clone()
	broken.TerminalOrdinal = -1
	if _, err := planner.PlanResults(batchID, "alice", []organization.ResultSnapshot{broken}, nil, ""); !errors.Is(err, ErrInvalidPlan) {
		t.Fatalf("非法终止序号 = %v", err)
	}

	// 成功事件的目标渠道非法时不能静默产出半成品计划。
	brokenTargets := results[0].Clone()
	if _, err := invalidTargetPlanner(t).PlanResults(batchID, "alice",
		[]organization.ResultSnapshot{brokenTargets}, nil, ""); !errors.Is(err, ErrInvalidPlan) {
		t.Fatalf("非法目标 = %v", err)
	}

	// 同一序号的手动事件与成功事件都非法目标时，手动事件先失败。
	if _, err := invalidTargetPlanner(t).PlanResults(batchID, "alice",
		results, failures, ""); !errors.Is(err, ErrInvalidPlan) {
		t.Fatalf("非法手动目标 = %v", err)
	}
}

func TestPlanResultsRejectsFailureCrossChecks(t *testing.T) {
	t.Parallel()
	batchID, results, failures := resultFixtures(t)
	planner := validPlanner(t)

	// 组末序号失败原因与结果错误文本不一致。
	conflicting := failures[0].Clone()
	conflicting.Error = "另一个原因"
	if _, err := planner.PlanResults(batchID, "alice", results,
		[]organization.FileFailure{conflicting}, ""); !errors.Is(err, ErrInvalidPlan) {
		t.Fatalf("失败原因冲突 = %v", err)
	}

	// 成功组不应出现晚于终止序号的失败文件。
	late := failures[0].Clone()
	late.File.Ordinal = results[0].TerminalOrdinal + 1
	successOnly := results[0].Clone()
	successOnly.ErrMsg = ""
	if _, err := planner.PlanResults(batchID, "alice",
		[]organization.ResultSnapshot{successOnly}, []organization.FileFailure{late}, ""); !errors.Is(err, ErrInvalidPlan) {
		t.Fatalf("超期失败序号 = %v", err)
	}
}

func TestManualEventRejectsInvalidOrdinal(t *testing.T) {
	t.Parallel()
	planner := validPlanner(t)
	if _, err := planner.manualEvent(uuid.New(), -1, organization.V2ManualFailure{}); !errors.Is(err, ErrInvalidPlan) {
		t.Fatalf("负序号 = %v", err)
	}
	if _, err := planner.manualEvent(uuid.New(), 1<<31, organization.V2ManualFailure{}); !errors.Is(err, ErrInvalidPlan) {
		t.Fatalf("超大序号 = %v", err)
	}
}

func TestSuccessEventRejectsOversizedTargets(t *testing.T) {
	t.Parallel()
	targets := make([]notification.Target, MaxRequestsPerEvent+1)
	for index := range targets {
		targets[index] = notification.Target{Channel: notification.ChannelTelegram, Source: "s"}
	}
	planner, err := NewPlanner(staticTargetProvider(targets))
	if err != nil {
		t.Fatal(err)
	}
	batchID, results, _ := resultFixtures(t)
	if _, err := planner.PlanResults(batchID, "alice", results, nil, ""); !errors.Is(err, ErrInvalidPlan) {
		t.Fatalf("超量目标 = %v", err)
	}
}
