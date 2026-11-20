package jobs

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/riverqueue/river"
	"github.com/riverqueue/river/rivertype"
)

// RSS 增量扫描是「控制器」：每轮把控制状态写进 job output，靠这个状态续扫下一批站点。
// 这里覆盖 state 的校验、还原与三条失败路径（未初始化、落盘失败、下一批非法）。

func TestValidateInitializedRSSSweepState(t *testing.T) {
	if err := validateInitializedRSSSweepState(RSSSweepState{Initialized: true, SiteIDs: []int{}}); err != nil {
		t.Fatalf("「本轮没有站点」是合法状态: %v", err)
	}
	if err := validateInitializedRSSSweepState(RSSSweepState{Initialized: true, SiteIDs: []int{1, 2, 3}}); err != nil {
		t.Fatalf("递增站点 ID 应合法: %v", err)
	}
	cases := map[string]RSSSweepState{
		"未初始化":      {SiteIDs: []int{1}},
		"站点列表为 nil": {Initialized: true},
		"未排序":       {Initialized: true, SiteIDs: []int{2, 1}},
		"有重复":       {Initialized: true, SiteIDs: []int{1, 1}},
		"含非正 ID":    {Initialized: true, SiteIDs: []int{0}},
		"超过单页上限":    {Initialized: true, SiteIDs: makeRSSIDs(RSSSweepPageSize + 1)},
	}
	for name, state := range cases {
		if err := validateInitializedRSSSweepState(state); !errors.Is(err, ErrInvalidRSSSweepArgs) {
			t.Fatalf("%s 应判为无效状态，实际 %v", name, err)
		}
	}
}

func makeRSSIDs(count int) []int {
	ids := make([]int, count)
	for index := range ids {
		ids[index] = index + 1
	}
	return ids
}

// River 把 job output 存在 JobRow.Metadata 的 output 字段里。
func rssJobWithMetadata(metadata string) *river.Job[RSSSweepArgs] {
	return &river.Job[RSSSweepArgs]{JobRow: &rivertype.JobRow{Metadata: []byte(metadata)}}
}

func TestRestoreRSSSweepState(t *testing.T) {
	// 没有 JobRow、或没有记录 output 时是「首次执行」，不是错误。
	if state, err := restoreRSSSweepState(&river.Job[RSSSweepArgs]{}); err != nil || state.Initialized {
		t.Fatalf("首次执行应返回零值状态: %+v %v", state, err)
	}
	if state, err := restoreRSSSweepState(rssJobWithMetadata("")); err != nil || state.Initialized {
		t.Fatalf("无 output 应返回零值状态: %+v %v", state, err)
	}
	if state, err := restoreRSSSweepState(rssJobWithMetadata(`{"other":1}`)); err != nil || state.Initialized {
		t.Fatalf("没有 output 字段应返回零值状态: %+v %v", state, err)
	}

	state, err := restoreRSSSweepState(rssJobWithMetadata(`{"output":{"initialized":true,"site_ids":[1,2]}}`))
	if err != nil {
		t.Fatalf("合法 output 解析失败: %v", err)
	}
	if !state.Initialized || len(state.SiteIDs) != 2 || state.SiteIDs[1] != 2 {
		t.Fatalf("解析结果不对: %+v", state)
	}

	invalid := map[string]string{
		"输出不是对象": `{"output":[]}`,
		"未知字段":   `{"output":{"initialized":true,"site_ids":[],"extra":1}}`,
		// 注：「尾部多余内容」无法通过 River 的 metadata 构造——`{"output":{...}{}}`
		// 外层 JSON 自己就先非法了，Output() 会当作「没有 output」。该分支属于防御性代码。
		"未初始化":       `{"output":{"initialized":false,"site_ids":[]}}`,
		"站点列表为 null": `{"output":{"initialized":true,"site_ids":null}}`,
		"站点未排序":      `{"output":{"initialized":true,"site_ids":[2,1]}}`,
	}
	for name, metadata := range invalid {
		if _, err := restoreRSSSweepState(rssJobWithMetadata(metadata)); err == nil {
			t.Fatalf("%s 应判为无效输出", name)
		}
	}
}

type fakeRSSSweepHandler struct {
	next RSSSweepState
	err  error
}

func (f fakeRSSSweepHandler) HandleRSSSweep(context.Context, time.Time, RSSSweepState) (RSSSweepState, error) {
	return f.next, f.err
}

func rssSweepTestJob(now time.Time) *river.Job[RSSSweepArgs] {
	return &river.Job[RSSSweepArgs]{Args: RSSSweepArgs{Key: rssSweepKey, Cutoff: now.Add(-time.Minute)}}
}

func TestRSSSweepWorkerRecordsInitializedState(t *testing.T) {
	now := time.Date(2026, 11, 19, 10, 0, 0, 0, time.UTC)
	var recorded []RSSSweepState
	worker := &rssSweepWorker{
		handler: fakeRSSSweepHandler{next: RSSSweepState{Initialized: true, SiteIDs: []int{1, 2}}},
		now:     func() time.Time { return now },
		recordOutput: func(_ context.Context, state RSSSweepState) error {
			recorded = append(recorded, state)
			return nil
		},
	}
	if err := worker.Work(context.Background(), rssSweepTestJob(now)); err != nil {
		t.Fatalf("正常执行不应报错: %v", err)
	}
	if len(recorded) != 1 || len(recorded[0].SiteIDs) != 2 {
		t.Fatalf("应把本轮状态落一次盘，实际 %+v", recorded)
	}
}

func TestRSSSweepWorkerCancelsUninitializedState(t *testing.T) {
	now := time.Date(2026, 11, 19, 10, 0, 0, 0, time.UTC)
	worker := &rssSweepWorker{
		handler:      fakeRSSSweepHandler{},
		now:          func() time.Time { return now },
		recordOutput: func(context.Context, RSSSweepState) error { return nil },
	}
	err := worker.Work(context.Background(), rssSweepTestJob(now))
	var cancelErr *river.JobCancelError
	if !errors.As(err, &cancelErr) {
		t.Fatalf("未初始化且非 pending 应取消任务，实际 %v", err)
	}
}

func TestRSSSweepWorkerSnoozesOnPending(t *testing.T) {
	now := time.Date(2026, 11, 19, 10, 0, 0, 0, time.UTC)
	worker := &rssSweepWorker{
		handler: fakeRSSSweepHandler{
			next: RSSSweepState{Initialized: true, SiteIDs: []int{1}},
			err:  ErrRSSSweepPending,
		},
		now:          func() time.Time { return now },
		recordOutput: func(context.Context, RSSSweepState) error { return nil },
	}
	err := worker.Work(context.Background(), rssSweepTestJob(now))
	var snoozeErr *river.JobSnoozeError
	if !errors.As(err, &snoozeErr) {
		t.Fatalf("pending 应 snooze 以便继续下一批，实际 %v", err)
	}
	if snoozeErr.Duration != rssSweepPendingSnooze {
		t.Fatalf("snooze 时长 = %v，期望 %v", snoozeErr.Duration, rssSweepPendingSnooze)
	}
}

func TestRSSSweepWorkerRejectsInvalidNextState(t *testing.T) {
	now := time.Date(2026, 11, 19, 10, 0, 0, 0, time.UTC)
	worker := &rssSweepWorker{
		handler:      fakeRSSSweepHandler{next: RSSSweepState{Initialized: true, SiteIDs: []int{2, 1}}},
		now:          func() time.Time { return now },
		recordOutput: func(context.Context, RSSSweepState) error { return nil },
	}
	err := worker.Work(context.Background(), rssSweepTestJob(now))
	var cancelErr *river.JobCancelError
	if !errors.As(err, &cancelErr) {
		t.Fatalf("未排序的下一批应被拒绝，实际 %v", err)
	}
}

func TestRSSSweepWorkerFailsWhenStateCannotBeRecorded(t *testing.T) {
	now := time.Date(2026, 11, 19, 10, 0, 0, 0, time.UTC)
	worker := &rssSweepWorker{
		handler:      fakeRSSSweepHandler{next: RSSSweepState{Initialized: true, SiteIDs: []int{1}}},
		now:          func() time.Time { return now },
		recordOutput: func(context.Context, RSSSweepState) error { return errors.New("db down") },
	}
	err := worker.Work(context.Background(), rssSweepTestJob(now))
	if !errors.Is(err, ErrRSSSweepExecution) {
		t.Fatalf("状态落盘失败应让任务重试，实际 %v", err)
	}
}
