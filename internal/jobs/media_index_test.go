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

func TestMediaIndexArgsContract(t *testing.T) {
	t.Parallel()
	requestID := uuid.MustParse("11111111-2222-4333-8444-555555555555")
	if err := (MediaIndexArgs{RequestID: requestID}).Validate(); err != nil {
		t.Fatal(err)
	}
	if err := (MediaIndexArgs{}).Validate(); !errors.Is(err, ErrInvalidMediaIndexArgs) {
		t.Fatalf("Validate() error = %v", err)
	}
	if got := (MediaIndexArgs{}).Kind(); got != mediaIndexKind {
		t.Fatalf("Kind() = %q", got)
	}
	field, ok := reflect.TypeOf(MediaIndexArgs{}).FieldByName("RequestID")
	if !ok || field.Tag.Get("json") != "request_id" || field.Tag.Get("river") != "unique" {
		t.Fatalf("RequestID tags = %q", field.Tag)
	}
	encoded, err := json.Marshal(MediaIndexArgs{RequestID: requestID})
	if err != nil || string(encoded) != `{"request_id":"11111111-2222-4333-8444-555555555555"}` {
		t.Fatalf("json = %s, error = %v", encoded, err)
	}
	opts := (MediaIndexArgs{RequestID: requestID}).InsertOpts()
	if opts.Queue != queueMediaIndex || opts.MaxAttempts != mediaIndexMaxAttempts || !opts.UniqueOpts.ByArgs {
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
}

func TestRegisterMediaIndexWorkerBoundsConcurrency(t *testing.T) {
	t.Parallel()
	handler := mediaIndexHandlerFunc(func(context.Context, MediaIndexArgs) error { return nil })
	tests := []struct {
		name       string
		config     ManagerConfig
		wantWorker int
		wantErr    bool
	}{
		{name: "disabled"},
		{name: "count without handler", config: ManagerConfig{MediaIndexMaxWorkers: 1}, wantErr: true},
		{name: "default", config: ManagerConfig{MediaIndexHandler: handler}, wantWorker: mediaIndexDefaultMaxWorkers},
		{name: "configured", config: ManagerConfig{MediaIndexHandler: handler, MediaIndexMaxWorkers: 6}, wantWorker: 6},
		{name: "hard cap", config: ManagerConfig{MediaIndexHandler: handler, MediaIndexMaxWorkers: 99}, wantWorker: mediaIndexMaxWorkersLimit},
		{name: "negative", config: ManagerConfig{MediaIndexHandler: handler, MediaIndexMaxWorkers: -1}, wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got, err := registerMediaIndexWorker(river.NewWorkers(), tt.config)
			if tt.wantErr {
				if err == nil {
					t.Fatal("invalid config accepted")
				}
				return
			}
			if err != nil || got != tt.wantWorker {
				t.Fatalf("register = %d, %v; want %d", got, err, tt.wantWorker)
			}
		})
	}
}

func TestMediaIndexWorkerValidatesAndSanitizes(t *testing.T) {
	t.Parallel()
	requestID := uuid.MustParse("aaaaaaaa-bbbb-4ccc-8ddd-eeeeeeeeeeee")
	sensitive := "token=secret path=/private/video.mkv"
	tests := []struct {
		name    string
		worker  *mediaIndexWorker
		job     *river.Job[MediaIndexArgs]
		wantErr error
	}{
		{name: "missing handler", worker: &mediaIndexWorker{}, job: &river.Job[MediaIndexArgs]{Args: MediaIndexArgs{RequestID: requestID}}, wantErr: ErrMediaIndexHandlerUnavailable},
		{name: "nil job", worker: &mediaIndexWorker{handler: mediaIndexHandlerFunc(func(context.Context, MediaIndexArgs) error { return nil })}, wantErr: ErrInvalidMediaIndexArgs},
		{name: "invalid args", worker: &mediaIndexWorker{handler: mediaIndexHandlerFunc(func(context.Context, MediaIndexArgs) error { return nil })}, job: &river.Job[MediaIndexArgs]{}, wantErr: ErrInvalidMediaIndexArgs},
		{name: "stable failure", worker: &mediaIndexWorker{handler: mediaIndexHandlerFunc(func(context.Context, MediaIndexArgs) error { return errors.New(sensitive) })}, job: &river.Job[MediaIndexArgs]{Args: MediaIndexArgs{RequestID: requestID}}, wantErr: ErrMediaIndexExecution},
		{name: "success", worker: &mediaIndexWorker{handler: mediaIndexHandlerFunc(func(context.Context, MediaIndexArgs) error { return nil })}, job: &river.Job[MediaIndexArgs]{Args: MediaIndexArgs{RequestID: requestID}}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			err := tt.worker.Work(t.Context(), tt.job)
			if !errors.Is(err, tt.wantErr) {
				t.Fatalf("Work() error = %v, want %v", err, tt.wantErr)
			}
			if err != nil && strings.Contains(err.Error(), "secret") {
				t.Fatal("worker leaked handler detail")
			}
		})
	}
	worker := &mediaIndexWorker{handler: mediaIndexHandlerFunc(func(context.Context, MediaIndexArgs) error { return nil })}
	if got := worker.Timeout(nil); got != mediaIndexJobTimeout {
		t.Fatalf("Timeout() = %s", got)
	}
	if mediaIndexJobTimeout != 10*time.Minute {
		t.Fatalf("mediaIndexJobTimeout = %s", mediaIndexJobTimeout)
	}
}

func TestManagerEnqueueMediaIndexValidatesWithoutDatabase(t *testing.T) {
	t.Parallel()
	manager := &Manager{}
	valid := MediaIndexArgs{RequestID: uuid.MustParse("aaaaaaaa-bbbb-4ccc-8ddd-eeeeeeeeeeee")}
	tests := []struct {
		name string
		ctx  context.Context
		args MediaIndexArgs
		want error
	}{
		{name: "invalid args first", ctx: context.Background(), want: ErrInvalidMediaIndexArgs},
		{name: "nil context", args: valid, want: ErrMediaIndexEnqueue},
		{name: "cancelled", ctx: cancelledOrganizationContext(), args: valid, want: context.Canceled},
		{name: "disabled", ctx: context.Background(), args: valid, want: ErrMediaIndexHandlerUnavailable},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if err := manager.EnqueueMediaIndex(tt.ctx, tt.args); !errors.Is(err, tt.want) {
				t.Fatalf("EnqueueMediaIndex() error = %v, want %v", err, tt.want)
			}
		})
	}
}

type mediaIndexHandlerFunc func(context.Context, MediaIndexArgs) error

func (f mediaIndexHandlerFunc) HandleMediaIndex(ctx context.Context, args MediaIndexArgs) error {
	return f(ctx, args)
}
