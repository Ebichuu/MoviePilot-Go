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

func TestScrapeArgsContract(t *testing.T) {
	t.Parallel()
	requestID := uuid.MustParse("11111111-2222-4333-8444-555555555555")
	args := ScrapeArgs{RequestID: requestID}
	if err := args.Validate(); err != nil {
		t.Fatal(err)
	}
	if err := (ScrapeArgs{}).Validate(); !errors.Is(err, ErrInvalidScrapeArgs) {
		t.Fatalf("Validate() error = %v", err)
	}
	if args.Kind() != scrapeKind {
		t.Fatalf("Kind() = %q, want %q", args.Kind(), scrapeKind)
	}
	encoded, err := json.Marshal(args)
	if err != nil || string(encoded) != `{"request_id":"11111111-2222-4333-8444-555555555555"}` {
		t.Fatalf("JSON = %s, error = %v", encoded, err)
	}
	field, ok := reflect.TypeOf(ScrapeArgs{}).FieldByName("RequestID")
	if !ok || field.Tag.Get("json") != "request_id" || field.Tag.Get("river") != "unique" {
		t.Fatalf("RequestID tags = %q", field.Tag)
	}
	assertActiveUniqueStates(t, args.InsertOpts(), queueScrape, scrapeMaxAttempts)
}

func TestNotificationArgsContract(t *testing.T) {
	t.Parallel()
	requestID := uuid.MustParse("aaaaaaaa-bbbb-4ccc-8ddd-eeeeeeeeeeee")
	args := NotificationArgs{RequestID: requestID}
	if err := args.Validate(); err != nil {
		t.Fatal(err)
	}
	if err := (NotificationArgs{}).Validate(); !errors.Is(err, ErrInvalidNotificationArgs) {
		t.Fatalf("Validate() error = %v", err)
	}
	if args.Kind() != notificationKind {
		t.Fatalf("Kind() = %q, want %q", args.Kind(), notificationKind)
	}
	encoded, err := json.Marshal(args)
	if err != nil || string(encoded) != `{"request_id":"aaaaaaaa-bbbb-4ccc-8ddd-eeeeeeeeeeee"}` {
		t.Fatalf("JSON = %s, error = %v", encoded, err)
	}
	field, ok := reflect.TypeOf(NotificationArgs{}).FieldByName("RequestID")
	if !ok || field.Tag.Get("json") != "request_id" || field.Tag.Get("river") != "unique" {
		t.Fatalf("RequestID tags = %q", field.Tag)
	}
	assertActiveUniqueStates(t, args.InsertOpts(), queueNotification, notificationMaxAttempts)
}

func TestScrapeWorkerValidatesAndSanitizes(t *testing.T) {
	t.Parallel()
	requestID := uuid.New()
	const sensitive = "url=https://secret.example cookie=secret passkey=secret"
	tests := []struct {
		name       string
		worker     *scrapeWorker
		job        *river.Job[ScrapeArgs]
		wantErr    error
		handlerErr error
	}{
		{name: "missing handler", worker: &scrapeWorker{}, job: &river.Job[ScrapeArgs]{Args: ScrapeArgs{RequestID: requestID}}, wantErr: ErrScrapeHandlerUnavailable},
		{name: "nil job", worker: &scrapeWorker{handler: scrapeJobHandlerFunc(func(context.Context, ScrapeArgs) error { return nil })}, wantErr: ErrInvalidScrapeArgs},
		{name: "invalid args", worker: &scrapeWorker{handler: scrapeJobHandlerFunc(func(context.Context, ScrapeArgs) error { return nil })}, job: &river.Job[ScrapeArgs]{}, wantErr: ErrInvalidScrapeArgs},
		{name: "business failure", worker: &scrapeWorker{handler: scrapeJobHandlerFunc(func(context.Context, ScrapeArgs) error { return errors.New(sensitive) })}, job: &river.Job[ScrapeArgs]{Args: ScrapeArgs{RequestID: requestID}}, wantErr: ErrScrapeExecution, handlerErr: errors.New(sensitive)},
		{name: "success", worker: &scrapeWorker{handler: scrapeJobHandlerFunc(func(context.Context, ScrapeArgs) error { return nil })}, job: &river.Job[ScrapeArgs]{Args: ScrapeArgs{RequestID: requestID}}},
	}
	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			err := tt.worker.Work(t.Context(), tt.job)
			if !errors.Is(err, tt.wantErr) {
				t.Fatalf("Work() error = %v, want %v", err, tt.wantErr)
			}
			if err != nil && strings.Contains(err.Error(), "secret.example") {
				t.Fatal("worker leaked handler detail")
			}
		})
	}
	worker := &scrapeWorker{}
	if got := worker.Timeout(nil); got != scrapeJobTimeout {
		t.Fatalf("Timeout() = %s, want %s", got, scrapeJobTimeout)
	}
	if scrapeJobTimeout >= 2*time.Minute {
		t.Fatalf("scrape worker timeout %s must remain below database lease", scrapeJobTimeout)
	}
}

func TestNotificationWorkerValidatesAndSanitizes(t *testing.T) {
	t.Parallel()
	requestID := uuid.New()
	const sensitive = "url=https://secret.example cookie=secret passkey=secret"
	tests := []struct {
		name       string
		worker     *notificationWorker
		job        *river.Job[NotificationArgs]
		wantErr    error
		handlerErr error
	}{
		{name: "missing handler", worker: &notificationWorker{}, job: &river.Job[NotificationArgs]{Args: NotificationArgs{RequestID: requestID}}, wantErr: ErrNotificationHandlerUnavailable},
		{name: "nil job", worker: &notificationWorker{handler: notificationJobHandlerFunc(func(context.Context, NotificationArgs) error { return nil })}, wantErr: ErrInvalidNotificationArgs},
		{name: "invalid args", worker: &notificationWorker{handler: notificationJobHandlerFunc(func(context.Context, NotificationArgs) error { return nil })}, job: &river.Job[NotificationArgs]{}, wantErr: ErrInvalidNotificationArgs},
		{name: "business failure", worker: &notificationWorker{handler: notificationJobHandlerFunc(func(context.Context, NotificationArgs) error { return errors.New(sensitive) })}, job: &river.Job[NotificationArgs]{Args: NotificationArgs{RequestID: requestID}}, wantErr: ErrNotificationExecution, handlerErr: errors.New(sensitive)},
		{name: "success", worker: &notificationWorker{handler: notificationJobHandlerFunc(func(context.Context, NotificationArgs) error { return nil })}, job: &river.Job[NotificationArgs]{Args: NotificationArgs{RequestID: requestID}}},
	}
	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			err := tt.worker.Work(t.Context(), tt.job)
			if !errors.Is(err, tt.wantErr) {
				t.Fatalf("Work() error = %v, want %v", err, tt.wantErr)
			}
			if err != nil && strings.Contains(err.Error(), "secret.example") {
				t.Fatal("worker leaked handler detail")
			}
		})
	}
	worker := &notificationWorker{}
	if got := worker.Timeout(nil); got != notificationJobTimeout {
		t.Fatalf("Timeout() = %s, want %s", got, notificationJobTimeout)
	}
	if notificationJobTimeout >= 2*time.Minute {
		t.Fatalf("notification worker timeout %s must remain below database lease", notificationJobTimeout)
	}
}

func TestSideEffectWorkerCancellationAndManagerValidation(t *testing.T) {
	t.Parallel()
	requestID := uuid.New()
	cases := []struct {
		name string
		call func(context.Context) error
		want error
	}{
		{name: "scrape nil context", call: func(context.Context) error {
			return (&scrapeWorker{handler: scrapeJobHandlerFunc(func(context.Context, ScrapeArgs) error { return nil })}).Work(nil, &river.Job[ScrapeArgs]{Args: ScrapeArgs{RequestID: requestID}})
		}, want: ErrScrapeExecution},
		{name: "notification nil context", call: func(context.Context) error {
			return (&notificationWorker{handler: notificationJobHandlerFunc(func(context.Context, NotificationArgs) error { return nil })}).Work(nil, &river.Job[NotificationArgs]{Args: NotificationArgs{RequestID: requestID}})
		}, want: ErrNotificationExecution},
		{name: "scrape manager disabled", call: func(ctx context.Context) error {
			return (&Manager{}).EnqueueScrape(ctx, requestID)
		}, want: ErrScrapeHandlerUnavailable},
		{name: "notification manager disabled", call: func(ctx context.Context) error {
			return (&Manager{}).EnqueueNotification(ctx, requestID)
		}, want: ErrNotificationHandlerUnavailable},
	}
	for _, tt := range cases {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if err := tt.call(context.Background()); !errors.Is(err, tt.want) {
				t.Fatalf("error = %v, want %v", err, tt.want)
			}
		})
	}
	cancelled, cancel := context.WithCancel(context.Background())
	cancel()
	if err := (&Manager{}).EnqueueScrape(cancelled, requestID); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled scrape enqueue = %v", err)
	}
	if err := (&Manager{}).EnqueueNotification(cancelled, requestID); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled notification enqueue = %v", err)
	}
}

func TestScrapeSweepContract(t *testing.T) {
	t.Parallel()
	valid := ScrapeSweepArgs{Key: scrapeSweepKey}
	if err := valid.Validate(); err != nil {
		t.Fatal(err)
	}
	if err := (ScrapeSweepArgs{}).Validate(); !errors.Is(err, ErrInvalidScrapeSweepArgs) {
		t.Fatalf("Validate() error = %v", err)
	}
	if valid.Kind() != scrapeSweepKind {
		t.Fatalf("Kind() = %q", valid.Kind())
	}
	assertActiveUniqueStates(t, valid.InsertOpts(), queueScrapeSweep, 5)
	if args, opts := scrapeSweepPeriodicArgs(); opts == nil || opts.Queue != queueScrapeSweep || args.(ScrapeSweepArgs).Key != scrapeSweepKey || scrapeSweepPeriodicJob() == nil {
		t.Fatalf("periodic args/options invalid: %#v %#v", args, opts)
	}
}

func TestNotificationSweepContract(t *testing.T) {
	t.Parallel()
	valid := NotificationSweepArgs{Key: notificationSweepKey}
	if err := valid.Validate(); err != nil {
		t.Fatal(err)
	}
	if err := (NotificationSweepArgs{}).Validate(); !errors.Is(err, ErrInvalidNotificationSweepArgs) {
		t.Fatalf("Validate() error = %v", err)
	}
	if valid.Kind() != notificationSweepKind {
		t.Fatalf("Kind() = %q", valid.Kind())
	}
	assertActiveUniqueStates(t, valid.InsertOpts(), queueNotificationSweep, 5)
	if args, opts := notificationSweepPeriodicArgs(); opts == nil || opts.Queue != queueNotificationSweep || args.(NotificationSweepArgs).Key != notificationSweepKey || notificationSweepPeriodicJob() == nil {
		t.Fatalf("periodic args/options invalid: %#v %#v", args, opts)
	}
}

func TestSideEffectSweepWorkers(t *testing.T) {
	t.Parallel()
	if enabled, err := registerScrapeSweepWorker(river.NewWorkers(), nil); err != nil || enabled {
		t.Fatalf("scrape disabled registration = %v, %v", enabled, err)
	}
	if enabled, err := registerNotificationSweepWorker(river.NewWorkers(), nil); err != nil || enabled {
		t.Fatalf("notification disabled registration = %v, %v", enabled, err)
	}
	scrapeWorkerValue := &scrapeSweepWorker{handler: scrapeSweepHandlerFunc(func(context.Context) error { return nil })}
	if err := scrapeWorkerValue.Work(t.Context(), &river.Job[ScrapeSweepArgs]{Args: ScrapeSweepArgs{Key: scrapeSweepKey}}); err != nil {
		t.Fatalf("scrape sweep Work() = %v", err)
	}
	notificationWorkerValue := &notificationSweepWorker{handler: notificationSweepHandlerFunc(func(context.Context) error { return nil })}
	if err := notificationWorkerValue.Work(t.Context(), &river.Job[NotificationSweepArgs]{Args: NotificationSweepArgs{Key: notificationSweepKey}}); err != nil {
		t.Fatalf("notification sweep Work() = %v", err)
	}
	if got := scrapeWorkerValue.Timeout(nil); got != scrapeSweepTimeout || notificationWorkerValue.Timeout(nil) != notificationSweepTimeout {
		t.Fatalf("sweep timeout = %s/%s", got, notificationWorkerValue.Timeout(nil))
	}
	if err := scrapeWorkerValue.Work(t.Context(), nil); !errors.Is(err, ErrInvalidScrapeSweepArgs) {
		t.Fatalf("nil scrape sweep job = %v", err)
	}
	if err := notificationWorkerValue.Work(t.Context(), nil); !errors.Is(err, ErrInvalidNotificationSweepArgs) {
		t.Fatalf("nil notification sweep job = %v", err)
	}
}

func assertActiveUniqueStates(t *testing.T, opts river.InsertOpts, queue string, maxAttempts int) {
	t.Helper()
	if opts.Queue != queue || opts.MaxAttempts != maxAttempts || !opts.UniqueOpts.ByArgs {
		t.Fatalf("InsertOpts() = %#v, want queue=%q max_attempts=%d by_args", opts, queue, maxAttempts)
	}
	active := []rivertype.JobState{
		rivertype.JobStateAvailable,
		rivertype.JobStatePending,
		rivertype.JobStateRetryable,
		rivertype.JobStateRunning,
		rivertype.JobStateScheduled,
	}
	for _, state := range active {
		if !slices.Contains(opts.UniqueOpts.ByState, state) {
			t.Fatalf("active unique state %s missing", state)
		}
	}
	for _, state := range []rivertype.JobState{rivertype.JobStateCompleted, rivertype.JobStateCancelled, rivertype.JobStateDiscarded} {
		if slices.Contains(opts.UniqueOpts.ByState, state) {
			t.Fatalf("historical unique state %s must not be included", state)
		}
	}
}

type scrapeJobHandlerFunc func(context.Context, ScrapeArgs) error

func (f scrapeJobHandlerFunc) HandleScrape(ctx context.Context, args ScrapeArgs) error {
	return f(ctx, args)
}

type notificationJobHandlerFunc func(context.Context, NotificationArgs) error

func (f notificationJobHandlerFunc) HandleNotification(ctx context.Context, args NotificationArgs) error {
	return f(ctx, args)
}

type scrapeSweepHandlerFunc func(context.Context) error

func (f scrapeSweepHandlerFunc) HandleScrapeSweep(ctx context.Context) error {
	return f(ctx)
}

type notificationSweepHandlerFunc func(context.Context) error

func (f notificationSweepHandlerFunc) HandleNotificationSweep(ctx context.Context) error {
	return f(ctx)
}
