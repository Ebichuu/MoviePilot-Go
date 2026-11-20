package jobs

import (
	"context"
	"errors"
	"slices"
	"testing"
	"time"

	"github.com/riverqueue/river"
	"github.com/riverqueue/river/rivertype"
)

func TestMediaIndexSweepContract(t *testing.T) {
	t.Parallel()
	valid := MediaIndexSweepArgs{Key: mediaIndexSweepKey}
	if err := valid.Validate(); err != nil {
		t.Fatal(err)
	}
	if err := (MediaIndexSweepArgs{}).Validate(); !errors.Is(err, ErrInvalidMediaIndexSweepArgs) {
		t.Fatalf("Validate() error = %v", err)
	}
	opts := valid.InsertOpts()
	if valid.Kind() != mediaIndexSweepKind || opts.Queue != queueMediaIndexSweep || opts.MaxAttempts != 5 || !opts.UniqueOpts.ByArgs {
		t.Fatalf("contract = %q %#v", valid.Kind(), opts)
	}
	for _, state := range []rivertype.JobState{
		rivertype.JobStateAvailable,
		rivertype.JobStatePending,
		rivertype.JobStateRetryable,
		rivertype.JobStateRunning,
		rivertype.JobStateScheduled,
	} {
		if !slices.Contains(opts.UniqueOpts.ByState, state) {
			t.Fatalf("active state %s missing", state)
		}
	}
}

func TestMediaIndexSweepWorkerAndRegistration(t *testing.T) {
	t.Parallel()
	handler := mediaIndexSweepHandlerFunc(func(context.Context) error { return nil })
	if enabled, err := registerMediaIndexSweepWorker(river.NewWorkers(), nil); err != nil || enabled {
		t.Fatalf("disabled registration = %v, %v", enabled, err)
	}
	if enabled, err := registerMediaIndexSweepWorker(river.NewWorkers(), handler); err != nil || !enabled {
		t.Fatalf("enabled registration = %v, %v", enabled, err)
	}
	worker := &mediaIndexSweepWorker{handler: handler}
	if err := worker.Work(t.Context(), &river.Job[MediaIndexSweepArgs]{Args: MediaIndexSweepArgs{Key: mediaIndexSweepKey}}); err != nil {
		t.Fatalf("Work() error = %v", err)
	}
	if err := worker.Work(t.Context(), nil); !errors.Is(err, ErrInvalidMediaIndexSweepArgs) {
		t.Fatalf("nil job error = %v", err)
	}
	if got := worker.Timeout(nil); got != 2*time.Minute {
		t.Fatalf("Timeout() = %s", got)
	}
	args, opts := mediaIndexSweepPeriodicArgs()
	sweep, ok := args.(MediaIndexSweepArgs)
	if !ok || sweep.Key != mediaIndexSweepKey || opts == nil || opts.Queue != queueMediaIndexSweep || mediaIndexSweepPeriodicJob() == nil {
		t.Fatalf("periodic = %#v %#v", args, opts)
	}
}

func TestManagerEnqueueMediaIndexSweepValidatesWithoutDatabase(t *testing.T) {
	t.Parallel()
	manager := &Manager{}
	if err := manager.EnqueueMediaIndexSweep(nil); !errors.Is(err, ErrMediaIndexSweepExecution) {
		t.Fatalf("nil context error = %v", err)
	}
	if err := manager.EnqueueMediaIndexSweep(cancelledOrganizationContext()); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled error = %v", err)
	}
	if err := manager.EnqueueMediaIndexSweep(context.Background()); !errors.Is(err, ErrMediaIndexSweepHandlerUnavailable) {
		t.Fatalf("disabled error = %v", err)
	}
}

type mediaIndexSweepHandlerFunc func(context.Context) error

func (f mediaIndexSweepHandlerFunc) HandleMediaIndexSweep(ctx context.Context) error {
	return f(ctx)
}
