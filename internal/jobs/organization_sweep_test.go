package jobs

import (
	"context"
	"errors"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/riverqueue/river"
	"github.com/riverqueue/river/rivertype"
)

func TestOrganizationSweepArgsContract(t *testing.T) {
	t.Parallel()
	valid := OrganizationSweepArgs{Key: organizationSweepKey}
	if err := valid.Validate(); err != nil {
		t.Fatal(err)
	}
	if err := (OrganizationSweepArgs{}).Validate(); !errors.Is(err, ErrInvalidOrganizationSweepArgs) {
		t.Fatalf("Validate() error = %v", err)
	}
	if valid.Kind() != organizationSweepKind {
		t.Fatalf("Kind() = %q", valid.Kind())
	}
	opts := valid.InsertOpts()
	if opts.Queue != queueOrganizationSweep || !opts.UniqueOpts.ByArgs || opts.MaxAttempts != 5 {
		t.Fatalf("InsertOpts() = %#v", opts)
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
	if !reflect.DeepEqual(opts, (OrganizationSweepArgs{Key: organizationSweepKey}).InsertOpts()) {
		t.Fatal("InsertOpts() is not deterministic")
	}
}

func TestOrganizationSweepWorkerValidatesAndSanitizes(t *testing.T) {
	t.Parallel()
	sensitive := "postgres://private"
	tests := []struct {
		name       string
		worker     *organizationSweepWorker
		job        *river.Job[OrganizationSweepArgs]
		handlerErr error
		wantErr    error
	}{
		{name: "missing handler", worker: &organizationSweepWorker{}, job: &river.Job[OrganizationSweepArgs]{Args: OrganizationSweepArgs{Key: organizationSweepKey}}, wantErr: ErrOrganizationSweepHandlerUnavailable},
		{name: "nil job", worker: &organizationSweepWorker{handler: organizationSweepHandlerFunc(func(context.Context) error { return nil })}, wantErr: ErrInvalidOrganizationSweepArgs},
		{name: "invalid args", worker: &organizationSweepWorker{handler: organizationSweepHandlerFunc(func(context.Context) error { return nil })}, job: &river.Job[OrganizationSweepArgs]{}, wantErr: ErrInvalidOrganizationSweepArgs},
		{name: "stable failure", worker: &organizationSweepWorker{handler: organizationSweepHandlerFunc(func(context.Context) error { return errors.New(sensitive) })}, job: &river.Job[OrganizationSweepArgs]{Args: OrganizationSweepArgs{Key: organizationSweepKey}}, wantErr: ErrOrganizationSweepExecution},
		{name: "success", worker: &organizationSweepWorker{handler: organizationSweepHandlerFunc(func(context.Context) error { return nil })}, job: &river.Job[OrganizationSweepArgs]{Args: OrganizationSweepArgs{Key: organizationSweepKey}}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			err := tt.worker.Work(t.Context(), tt.job)
			if !errors.Is(err, tt.wantErr) {
				t.Fatalf("Work() error = %v, want %v", err, tt.wantErr)
			}
			if err != nil && strings.Contains(err.Error(), sensitive) {
				t.Fatal("worker leaked handler detail")
			}
		})
	}
}

func TestRegisterOrganizationSweepWorkerAndPeriodicJob(t *testing.T) {
	t.Parallel()
	if enabled, err := registerOrganizationSweepWorker(river.NewWorkers(), nil); err != nil || enabled {
		t.Fatalf("disabled registration = %v, %v", enabled, err)
	}
	handler := organizationSweepHandlerFunc(func(context.Context) error { return nil })
	if enabled, err := registerOrganizationSweepWorker(river.NewWorkers(), handler); err != nil || !enabled {
		t.Fatalf("enabled registration = %v, %v", enabled, err)
	}
	worker := &organizationSweepWorker{handler: handler}
	if got := worker.Timeout(nil); got != 5*time.Minute {
		t.Fatalf("Timeout() = %s", got)
	}
	args, opts := organizationSweepPeriodicArgs()
	sweep, ok := args.(OrganizationSweepArgs)
	if !ok || sweep.Key != organizationSweepKey || opts == nil || opts.Queue != queueOrganizationSweep {
		t.Fatalf("periodic args = %#v, %#v", args, opts)
	}
	if organizationSweepPeriodicJob() == nil {
		t.Fatal("periodic job = nil")
	}
}

func TestManagerEnqueueOrganizationSweepValidatesWithoutDatabase(t *testing.T) {
	t.Parallel()
	manager := &Manager{}
	tests := []struct {
		name string
		ctx  context.Context
		want error
	}{
		{name: "nil context", want: ErrOrganizationSweepExecution},
		{name: "cancelled context", ctx: cancelledOrganizationContext(), want: context.Canceled},
		{name: "disabled manager", ctx: context.Background(), want: ErrOrganizationSweepHandlerUnavailable},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if err := manager.EnqueueOrganizationSweep(tt.ctx); !errors.Is(err, tt.want) {
				t.Fatalf("EnqueueOrganizationSweep() error = %v, want %v", err, tt.want)
			}
		})
	}
}

type organizationSweepHandlerFunc func(context.Context) error

func (f organizationSweepHandlerFunc) HandleOrganizationSweep(ctx context.Context) error {
	return f(ctx)
}
