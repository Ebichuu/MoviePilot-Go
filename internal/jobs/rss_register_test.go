package jobs

import (
	"context"
	"testing"
	"time"

	"github.com/riverqueue/river"
)

type fakeRSSPollRegistrar struct{ err error }

func (f fakeRSSPollRegistrar) HandleRSSPoll(context.Context, RSSPollArgs) error { return f.err }

func TestRegisterRSSSweepWorkerRequiresHandler(t *testing.T) {
	if enabled, err := registerRSSSweepWorker(river.NewWorkers(), nil); err != nil || enabled {
		t.Fatalf("没有处理器时不应注册: enabled=%v err=%v", enabled, err)
	}
	enabled, err := registerRSSSweepWorker(river.NewWorkers(), fakeRSSSweepHandler{})
	if err != nil {
		t.Fatalf("注册失败: %v", err)
	}
	if !enabled {
		t.Fatal("有处理器时应注册成功")
	}
}

func TestRegisterRSSPollWorkerRequiresHandler(t *testing.T) {
	if enabled, err := registerRSSPollWorker(river.NewWorkers(), nil); err != nil || enabled {
		t.Fatalf("没有处理器时不应注册: enabled=%v err=%v", enabled, err)
	}
	enabled, err := registerRSSPollWorker(river.NewWorkers(), fakeRSSPollRegistrar{})
	if err != nil {
		t.Fatalf("注册失败: %v", err)
	}
	if !enabled {
		t.Fatal("有处理器时应注册成功")
	}
}

// 周期任务每次触发都要带上「当前时间」作为 cutoff，否则扫描窗口会越跑越旧。
func TestRSSSweepPeriodicArgsUsesCurrentCutoff(t *testing.T) {
	before := time.Now().UTC()
	args, opts := rssSweepPeriodicArgs()
	after := time.Now().UTC()

	typed, ok := args.(RSSSweepArgs)
	if !ok {
		t.Fatalf("周期参数类型不对: %T", args)
	}
	if typed.Key != rssSweepKey {
		t.Fatalf("key = %q，期望 %q", typed.Key, rssSweepKey)
	}
	if typed.Cutoff.Before(before) || typed.Cutoff.After(after) {
		t.Fatalf("cutoff = %v，应落在 [%v, %v] 之间", typed.Cutoff, before, after)
	}
	if opts == nil || opts.Queue != queueRSSSweep {
		t.Fatalf("插入选项不对: %+v", opts)
	}
}

func TestRSSPollWorkerTimeout(t *testing.T) {
	worker := &rssPollWorker{}
	if got := worker.Timeout(nil); got != rssPollJobTimeout {
		t.Fatalf("Timeout() = %v，期望 %v", got, rssPollJobTimeout)
	}
}
