package jobs

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/riverqueue/river"
	"github.com/riverqueue/river/rivertype"
)

func TestOrganizationArgsContract(t *testing.T) {
	t.Parallel()

	batchID := uuid.MustParse("11111111-2222-4333-8444-555555555555")
	tests := []struct {
		name    string
		args    OrganizationArgs
		wantErr error
	}{
		{name: "valid batch", args: OrganizationArgs{BatchID: batchID}},
		{name: "nil batch", args: OrganizationArgs{BatchID: uuid.Nil}, wantErr: ErrInvalidOrganizationArgs},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if err := tt.args.Validate(); !errors.Is(err, tt.wantErr) {
				t.Fatalf("Validate() error = %v, want %v", err, tt.wantErr)
			}
		})
	}
	if got := (OrganizationArgs{}).Kind(); got != organizationKind {
		t.Fatalf("Kind() = %q, want %q", got, organizationKind)
	}
	field, ok := reflect.TypeOf(OrganizationArgs{}).FieldByName("BatchID")
	if !ok || field.Tag.Get("json") != "batch_id" || field.Tag.Get("river") != "unique" {
		t.Fatalf("BatchID tags = %q, want json=batch_id and river=unique", field.Tag)
	}
	encoded, err := json.Marshal(OrganizationArgs{BatchID: batchID})
	if err != nil {
		t.Fatalf("json.Marshal() error = %v", err)
	}
	if got, want := string(encoded), `{"batch_id":"11111111-2222-4333-8444-555555555555"}`; got != want {
		t.Fatalf("JSON = %s, want %s", got, want)
	}
	var decoded OrganizationArgs
	if err := json.Unmarshal(encoded, &decoded); err != nil {
		t.Fatalf("json.Unmarshal() error = %v", err)
	}
	if decoded.BatchID != batchID {
		t.Fatalf("decoded BatchID = %s, want %s", decoded.BatchID, batchID)
	}

	opts := (OrganizationArgs{}).InsertOpts()
	if opts.Queue != queueOrganization || opts.MaxAttempts != organizationMaxAttempts || !opts.UniqueOpts.ByArgs {
		t.Fatalf("InsertOpts() = %#v, want bounded unique organization options", opts)
	}
	if opts.MaxAttempts != 1 {
		t.Fatalf("MaxAttempts = %d, want 1 (批次重试由数据库 next_attempt_at 驱动)", opts.MaxAttempts)
	}
	activeStates := []rivertype.JobState{
		rivertype.JobStateAvailable,
		rivertype.JobStatePending,
		rivertype.JobStateRetryable,
		rivertype.JobStateRunning,
		rivertype.JobStateScheduled,
	}
	for _, state := range activeStates {
		if !slices.Contains(opts.UniqueOpts.ByState, state) {
			t.Fatalf("active unique states missing %s", state)
		}
	}
	for _, state := range []rivertype.JobState{
		rivertype.JobStateCompleted,
		rivertype.JobStateCancelled,
		rivertype.JobStateDiscarded,
	} {
		if slices.Contains(opts.UniqueOpts.ByState, state) {
			t.Fatalf("historical state %s must not block a retry", state)
		}
	}
	if !reflect.DeepEqual(opts, (OrganizationArgs{BatchID: batchID}).InsertOpts()) {
		t.Fatal("InsertOpts() is not deterministic")
	}
}

func TestNormalizeOrganizationMaxWorkers(t *testing.T) {
	t.Parallel()
	if organizationDefaultMaxWorkers != 3 || organizationMaxWorkersLimit != 8 {
		t.Fatalf("worker bounds = %d/%d, want 3/8", organizationDefaultMaxWorkers, organizationMaxWorkersLimit)
	}

	tests := []struct {
		name    string
		input   int
		want    int
		wantErr bool
	}{
		{name: "default", want: organizationDefaultMaxWorkers},
		{name: "configured", input: 6, want: 6},
		{name: "hard cap", input: organizationMaxWorkersLimit + 4, want: organizationMaxWorkersLimit},
		{name: "negative", input: -1, wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got, err := normalizeOrganizationMaxWorkers(tt.input)
			if tt.wantErr {
				if err == nil {
					t.Fatal("negative worker count was accepted")
				}
				return
			}
			if err != nil || got != tt.want {
				t.Fatalf("normalizeOrganizationMaxWorkers(%d) = %d, %v; want %d", tt.input, got, err, tt.want)
			}
		})
	}
}

func TestRegisterOrganizationWorkerBoundsConcurrency(t *testing.T) {
	t.Parallel()

	handler := organizationHandlerFunc(func(context.Context, OrganizationArgs) error { return nil })
	tests := []struct {
		name       string
		config     ManagerConfig
		wantWorker int
		wantErr    bool
	}{
		{name: "disabled by default"},
		{name: "worker count without handler", config: ManagerConfig{OrganizationMaxWorkers: 1}, wantErr: true},
		{name: "default concurrency", config: ManagerConfig{OrganizationHandler: handler}, wantWorker: organizationDefaultMaxWorkers},
		{name: "configured concurrency", config: ManagerConfig{OrganizationHandler: handler, OrganizationMaxWorkers: 6}, wantWorker: 6},
		{name: "hard cap", config: ManagerConfig{OrganizationHandler: handler, OrganizationMaxWorkers: 100}, wantWorker: organizationMaxWorkersLimit},
		{name: "negative count", config: ManagerConfig{OrganizationHandler: handler, OrganizationMaxWorkers: -1}, wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got, err := registerOrganizationWorker(river.NewWorkers(), tt.config)
			if tt.wantErr {
				if err == nil {
					t.Fatal("invalid organization worker config was accepted")
				}
				return
			}
			if err != nil {
				t.Fatalf("registerOrganizationWorker() error = %v", err)
			}
			if got != tt.wantWorker {
				t.Fatalf("MaxWorkers = %d, want %d", got, tt.wantWorker)
			}
		})
	}
}

func TestOrganizationWorkerForwardsSafeArgsAndContext(t *testing.T) {
	t.Parallel()

	type contextKey struct{}
	batchID := uuid.MustParse("aaaaaaaa-bbbb-4ccc-8ddd-eeeeeeeeeeee")
	ctx := context.WithValue(context.Background(), contextKey{}, "fixture")
	var receivedArgs OrganizationArgs
	var receivedContextValue any
	worker := &organizationWorker{handler: organizationHandlerFunc(func(handlerCtx context.Context, args OrganizationArgs) error {
		receivedArgs = args
		receivedContextValue = handlerCtx.Value(contextKey{})
		return nil
	})}

	job := &river.Job[OrganizationArgs]{Args: OrganizationArgs{BatchID: batchID}}
	if err := worker.Work(ctx, job); err != nil {
		t.Fatalf("Work() error = %v", err)
	}
	if receivedArgs.BatchID != batchID || receivedContextValue != "fixture" {
		t.Fatalf("handler received args/context = %#v/%v", receivedArgs, receivedContextValue)
	}
	if got := worker.Timeout(job); got != organizationJobTimeout || got <= 0 {
		t.Fatalf("Timeout() = %s, want %s", got, organizationJobTimeout)
	}
	if got := worker.Timeout(job); got != 30*time.Minute {
		t.Fatalf("Timeout() = %s, want 30m", got)
	}
}

func TestOrganizationWorkerRejectsInvalidInputBeforeHandler(t *testing.T) {
	t.Parallel()

	batchID := uuid.MustParse("aaaaaaaa-bbbb-4ccc-8ddd-eeeeeeeeeeee")
	tests := []struct {
		name string
		ctx  context.Context
		job  *river.Job[OrganizationArgs]
		want error
	}{
		{name: "nil context", job: &river.Job[OrganizationArgs]{Args: OrganizationArgs{BatchID: batchID}}, want: ErrOrganizationExecution},
		{name: "cancelled context", ctx: cancelledOrganizationContext(), job: &river.Job[OrganizationArgs]{Args: OrganizationArgs{BatchID: batchID}}, want: context.Canceled},
		{name: "missing handler", ctx: context.Background(), job: &river.Job[OrganizationArgs]{Args: OrganizationArgs{BatchID: batchID}}, want: ErrOrganizationHandlerUnavailable},
		{name: "nil job", ctx: context.Background(), want: ErrInvalidOrganizationArgs},
		{name: "nil batch", ctx: context.Background(), job: &river.Job[OrganizationArgs]{}, want: ErrInvalidOrganizationArgs},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			called := false
			worker := &organizationWorker{handler: organizationHandlerFunc(func(context.Context, OrganizationArgs) error {
				called = true
				return nil
			})}
			if tt.name == "missing handler" {
				worker.handler = nil
			}
			if err := worker.Work(tt.ctx, tt.job); !errors.Is(err, tt.want) {
				t.Fatalf("Work() error = %v, want %v", err, tt.want)
			}
			if called {
				t.Fatal("invalid job reached the handler")
			}
		})
	}
}

func TestOrganizationWorkerSanitizesFailuresAndCancellation(t *testing.T) {
	t.Parallel()

	batchID := uuid.MustParse("aaaaaaaa-bbbb-4ccc-8ddd-eeeeeeeeeeee")
	const sensitive = "url=https://secret.example cookie=secret passkey=secret"
	tests := []struct {
		name       string
		handlerErr error
		wantErr    error
	}{
		{name: "business failure", handlerErr: errors.New(sensitive), wantErr: ErrOrganizationExecution},
		{name: "cancelled", handlerErr: errors.Join(errors.New(sensitive), context.Canceled), wantErr: context.Canceled},
		{name: "deadline", handlerErr: errors.Join(errors.New(sensitive), context.DeadlineExceeded), wantErr: context.DeadlineExceeded},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			worker := &organizationWorker{handler: organizationHandlerFunc(func(context.Context, OrganizationArgs) error {
				return tt.handlerErr
			})}
			err := worker.Work(context.Background(), &river.Job[OrganizationArgs]{Args: OrganizationArgs{BatchID: batchID}})
			if !errors.Is(err, tt.wantErr) {
				t.Fatalf("Work() error = %v, want %v", err, tt.wantErr)
			}
			if strings.Contains(err.Error(), "secret.example") || strings.Contains(err.Error(), "passkey") {
				t.Fatal("handler error leaked sensitive details")
			}
		})
	}
}

func TestOrganizationWorkerPropagatesCancellationToHandler(t *testing.T) {
	batchID := uuid.MustParse("aaaaaaaa-bbbb-4ccc-8ddd-eeeeeeeeeeee")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	started := make(chan struct{})
	worker := &organizationWorker{handler: organizationHandlerFunc(func(handlerCtx context.Context, _ OrganizationArgs) error {
		close(started)
		<-handlerCtx.Done()
		return errors.Join(errors.New("upstream secret"), handlerCtx.Err())
	})}
	result := make(chan error, 1)
	go func() {
		result <- worker.Work(ctx, &river.Job[OrganizationArgs]{Args: OrganizationArgs{BatchID: batchID}})
	}()
	select {
	case <-started:
		cancel()
	case <-time.After(time.Second):
		t.Fatal("organization handler did not start")
	}
	select {
	case err := <-result:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("Work() error = %v, want context.Canceled", err)
		}
		if strings.Contains(err.Error(), "upstream secret") {
			t.Fatal("cancellation error leaked handler details")
		}
	case <-time.After(time.Second):
		t.Fatal("organization worker did not stop after cancellation")
	}
}

func TestManagerEnqueueOrganizationValidatesWithoutDatabase(t *testing.T) {
	t.Parallel()

	manager := &Manager{}
	validArgs := OrganizationArgs{BatchID: uuid.MustParse("aaaaaaaa-bbbb-4ccc-8ddd-eeeeeeeeeeee")}
	tests := []struct {
		name string
		ctx  context.Context
		args OrganizationArgs
		want error
	}{
		{name: "invalid args first", ctx: context.Background(), want: ErrInvalidOrganizationArgs},
		{name: "nil context", args: validArgs, want: ErrOrganizationEnqueue},
		{name: "cancelled context", ctx: cancelledOrganizationContext(), args: validArgs, want: context.Canceled},
		{name: "missing handler", ctx: context.Background(), args: validArgs, want: ErrOrganizationHandlerUnavailable},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if err := manager.EnqueueOrganization(tt.ctx, tt.args); !errors.Is(err, tt.want) {
				t.Fatalf("EnqueueOrganization() error = %v, want %v", err, tt.want)
			}
		})
	}
}

type organizationHandlerFunc func(context.Context, OrganizationArgs) error

func (f organizationHandlerFunc) HandleOrganization(ctx context.Context, args OrganizationArgs) error {
	return f(ctx, args)
}

func cancelledOrganizationContext() context.Context {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	return ctx
}
