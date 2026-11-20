package jobs

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/riverqueue/river"
	"github.com/riverqueue/river/rivertype"
)

func TestRSSSweepWorkerPersistsRoundState(t *testing.T) {
	t.Parallel()

	cutoff := time.Date(2026, 8, 16, 16, 0, 0, 0, time.UTC)
	tests := []struct {
		name       string
		metadata   []byte
		wantState  RSSSweepState
		nextState  RSSSweepState
		handlerErr error
		wantSnooze bool
	}{
		{
			name:      "首次执行",
			wantState: RSSSweepState{},
			nextState: RSSSweepState{Initialized: true, SiteIDs: []int{2, 5}},
		},
		{
			name:       "snooze 后恢复",
			metadata:   rssSweepMetadata(t, RSSSweepState{Initialized: true, SiteIDs: []int{2, 5}}),
			wantState:  RSSSweepState{Initialized: true, SiteIDs: []int{2, 5}},
			nextState:  RSSSweepState{Initialized: true, SiteIDs: []int{2, 5}},
			handlerErr: ErrRSSSweepPending,
			wantSnooze: true,
		},
		{
			name:      "已初始化空站点集合",
			metadata:  rssSweepMetadata(t, RSSSweepState{Initialized: true, SiteIDs: []int{}}),
			wantState: RSSSweepState{Initialized: true, SiteIDs: []int{}},
			nextState: RSSSweepState{Initialized: true, SiteIDs: []int{}},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			var received RSSSweepState
			var recorded RSSSweepState
			worker := &rssSweepWorker{
				handler: rssSweepStateHandlerFunc(func(_ context.Context, gotCutoff time.Time, state RSSSweepState) (RSSSweepState, error) {
					if !gotCutoff.Equal(cutoff) || gotCutoff.Location() != time.UTC {
						t.Fatalf("cutoff = %v, want UTC %v", gotCutoff, cutoff)
					}
					received = state
					return tt.nextState, tt.handlerErr
				}),
				now: func() time.Time { return cutoff },
				recordOutput: func(_ context.Context, state RSSSweepState) error {
					recorded = state
					return nil
				},
			}
			job := &river.Job[RSSSweepArgs]{
				JobRow: &rivertype.JobRow{Metadata: tt.metadata},
				Args:   RSSSweepArgs{Key: rssSweepKey, Cutoff: cutoff},
			}
			err := worker.Work(context.Background(), job)
			if tt.wantSnooze {
				var snoozeErr *river.JobSnoozeError
				if !errors.As(err, &snoozeErr) || snoozeErr.Duration != rssSweepPendingSnooze {
					t.Fatalf("Work() error = %v, want %s snooze", err, rssSweepPendingSnooze)
				}
			} else if err != nil {
				t.Fatalf("Work() error = %v", err)
			}
			assertRSSSweepState(t, received, tt.wantState)
			assertRSSSweepState(t, recorded, tt.nextState)
		})
	}
}

func TestRSSSweepWorkerRejectsInvalidRecordedState(t *testing.T) {
	t.Parallel()

	cutoff := time.Date(2026, 8, 16, 16, 0, 0, 0, time.UTC)
	overLimit := make([]int, RSSSweepPageSize+1)
	for index := range overLimit {
		overLimit[index] = index + 1
	}
	tests := []struct {
		name   string
		output any
	}{
		{name: "非对象", output: "url=https://secret.example?passkey=secret"},
		{name: "未知字段", output: map[string]any{"initialized": true, "site_ids": []int{}, "secret": "passkey"}},
		{name: "未初始化", output: RSSSweepState{Initialized: false, SiteIDs: []int{}}},
		{name: "缺少初始化标记", output: map[string]any{"site_ids": []int{1}}},
		{name: "缺少站点集合", output: map[string]any{"initialized": true}},
		{name: "null 站点集合", output: RSSSweepState{Initialized: true}},
		{name: "越界", output: RSSSweepState{Initialized: true, SiteIDs: overLimit}},
		{name: "站点 ID 非正数", output: RSSSweepState{Initialized: true, SiteIDs: []int{0}}},
		{name: "未排序", output: RSSSweepState{Initialized: true, SiteIDs: []int{2, 1}}},
		{name: "重复", output: RSSSweepState{Initialized: true, SiteIDs: []int{1, 1}}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			called := false
			worker := &rssSweepWorker{
				handler: rssSweepStateHandlerFunc(func(context.Context, time.Time, RSSSweepState) (RSSSweepState, error) {
					called = true
					return RSSSweepState{Initialized: true, SiteIDs: []int{}}, nil
				}),
				now: func() time.Time { return cutoff },
				recordOutput: func(context.Context, RSSSweepState) error {
					t.Fatal("invalid state was recorded")
					return nil
				},
			}
			job := &river.Job[RSSSweepArgs]{
				JobRow: &rivertype.JobRow{Metadata: rssSweepMetadata(t, tt.output)},
				Args:   RSSSweepArgs{Key: rssSweepKey, Cutoff: cutoff},
			}
			err := worker.Work(context.Background(), job)
			var cancelErr *river.JobCancelError
			if !errors.As(err, &cancelErr) || !errors.Is(err, ErrInvalidRSSSweepArgs) {
				t.Fatalf("Work() error = %v, want JobCancel(ErrInvalidRSSSweepArgs)", err)
			}
			if strings.Contains(err.Error(), "secret.example") || strings.Contains(err.Error(), "passkey") {
				t.Fatalf("Work() leaked recorded output: %v", err)
			}
			if called {
				t.Fatal("handler called with invalid recorded state")
			}
		})
	}
}

func TestRSSSweepWorkerValidatesStateBeforeRecording(t *testing.T) {
	t.Parallel()

	cutoff := time.Date(2026, 8, 16, 16, 0, 0, 0, time.UTC)
	tests := []struct {
		name  string
		state RSSSweepState
	}{
		{name: "未初始化", state: RSSSweepState{SiteIDs: []int{}}},
		{name: "null 站点集合", state: RSSSweepState{Initialized: true}},
		{name: "未排序", state: RSSSweepState{Initialized: true, SiteIDs: []int{8, 3}}},
		{name: "重复", state: RSSSweepState{Initialized: true, SiteIDs: []int{3, 3}}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			recorded := false
			worker := &rssSweepWorker{
				handler: rssSweepStateHandlerFunc(func(context.Context, time.Time, RSSSweepState) (RSSSweepState, error) {
					return tt.state, ErrRSSSweepPending
				}),
				now: func() time.Time { return cutoff },
				recordOutput: func(context.Context, RSSSweepState) error {
					recorded = true
					return nil
				},
			}
			err := worker.Work(context.Background(), &river.Job[RSSSweepArgs]{
				Args: RSSSweepArgs{Key: rssSweepKey, Cutoff: cutoff},
			})
			if !errors.Is(err, ErrInvalidRSSSweepArgs) {
				t.Fatalf("Work() error = %v, want ErrInvalidRSSSweepArgs", err)
			}
			if recorded {
				t.Fatal("invalid next state was recorded")
			}
		})
	}
}

func TestRSSSweepWorkerRecordOutputStrategy(t *testing.T) {
	t.Parallel()

	cutoff := time.Date(2026, 8, 16, 16, 0, 0, 0, time.UTC)
	newWorker := func() *rssSweepWorker {
		return &rssSweepWorker{
			handler: rssSweepStateHandlerFunc(func(context.Context, time.Time, RSSSweepState) (RSSSweepState, error) {
				return RSSSweepState{Initialized: true, SiteIDs: []int{}}, nil
			}),
			now: func() time.Time { return cutoff },
		}
	}
	newJob := func() *river.Job[RSSSweepArgs] {
		return &river.Job[RSSSweepArgs]{Args: RSSSweepArgs{Key: rssSweepKey, Cutoff: cutoff}}
	}

	worker := newWorker()
	if err := worker.Work(context.Background(), newJob()); !errors.Is(err, ErrRSSSweepExecution) {
		t.Fatalf("Work() outside River context error = %v, want ErrRSSSweepExecution", err)
	}

	recorded := false
	worker = newWorker()
	worker.recordOutput = func(context.Context, RSSSweepState) error {
		recorded = true
		return nil
	}
	if err := worker.Work(context.Background(), newJob()); err != nil {
		t.Fatalf("Work() with recorder error = %v", err)
	}
	if !recorded {
		t.Fatal("injected recorder was not called")
	}
}

func TestRSSSweepWorkerSanitizesStateHandlerAndRecorderFailures(t *testing.T) {
	t.Parallel()

	cutoff := time.Date(2026, 8, 16, 16, 0, 0, 0, time.UTC)
	validState := RSSSweepState{Initialized: true, SiteIDs: []int{1}}
	tests := []struct {
		name        string
		handlerErr  error
		recorderErr error
	}{
		{name: "handler", handlerErr: errors.New("url=https://secret.example?passkey=secret")},
		{name: "recorder", recorderErr: errors.New("metadata secret=passkey")},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			worker := &rssSweepWorker{
				handler: rssSweepStateHandlerFunc(func(context.Context, time.Time, RSSSweepState) (RSSSweepState, error) {
					return validState, tt.handlerErr
				}),
				now: func() time.Time { return cutoff },
				recordOutput: func(context.Context, RSSSweepState) error {
					return tt.recorderErr
				},
			}
			err := worker.Work(context.Background(), &river.Job[RSSSweepArgs]{
				Args: RSSSweepArgs{Key: rssSweepKey, Cutoff: cutoff},
			})
			if !errors.Is(err, ErrRSSSweepExecution) {
				t.Fatalf("Work() error = %v, want ErrRSSSweepExecution", err)
			}
			if strings.Contains(err.Error(), "secret.example") || strings.Contains(err.Error(), "passkey") {
				t.Fatalf("Work() leaked failure details: %v", err)
			}
		})
	}
}

func TestRSSSweepWorkerPersistsInitializedStateBeforeRetryableFailure(t *testing.T) {
	t.Parallel()

	cutoff := time.Date(2026, 8, 16, 16, 0, 0, 0, time.UTC)
	wantState := RSSSweepState{Initialized: true, SiteIDs: []int{3, 8}}
	var recorded RSSSweepState
	worker := &rssSweepWorker{
		handler: rssSweepStateHandlerFunc(func(context.Context, time.Time, RSSSweepState) (RSSSweepState, error) {
			return wantState, errors.New("temporary database failure")
		}),
		now: func() time.Time { return cutoff },
		recordOutput: func(_ context.Context, state RSSSweepState) error {
			recorded = state
			return nil
		},
	}

	err := worker.Work(context.Background(), &river.Job[RSSSweepArgs]{
		Args: RSSSweepArgs{Key: rssSweepKey, Cutoff: cutoff},
	})
	if !errors.Is(err, ErrRSSSweepExecution) {
		t.Fatalf("Work() error = %v, want ErrRSSSweepExecution", err)
	}
	assertRSSSweepState(t, recorded, wantState)
}

func rssSweepMetadata(t *testing.T, output any) []byte {
	t.Helper()
	metadata, err := json.Marshal(map[string]any{"output": output})
	if err != nil {
		t.Fatalf("marshal RSS sweep metadata: %v", err)
	}
	return metadata
}

func assertRSSSweepState(t *testing.T, got, want RSSSweepState) {
	t.Helper()
	if got.Initialized != want.Initialized {
		t.Fatalf("state initialized = %t, want %t", got.Initialized, want.Initialized)
	}
	if len(got.SiteIDs) != len(want.SiteIDs) {
		t.Fatalf("state site IDs = %v, want %v", got.SiteIDs, want.SiteIDs)
	}
	for index := range want.SiteIDs {
		if got.SiteIDs[index] != want.SiteIDs[index] {
			t.Fatalf("state site IDs = %v, want %v", got.SiteIDs, want.SiteIDs)
		}
	}
	if got.Initialized && (got.SiteIDs == nil) != (want.SiteIDs == nil) {
		t.Fatalf("state site IDs nil = %t, want %t", got.SiteIDs == nil, want.SiteIDs == nil)
	}
}

type rssSweepStateHandlerFunc func(context.Context, time.Time, RSSSweepState) (RSSSweepState, error)

func (f rssSweepStateHandlerFunc) HandleRSSSweep(ctx context.Context, cutoff time.Time, state RSSSweepState) (RSSSweepState, error) {
	return f(ctx, cutoff, state)
}
