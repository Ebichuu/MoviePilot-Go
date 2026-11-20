package app

import (
	"context"
	"errors"
	"sync"

	"github.com/moviepilot-go/moviepilot-go/internal/jobs"
)

var errMediaServerSyncHandlerAlreadyBound = errors.New("媒体服务器条目同步处理器已绑定")

type mediaServerSyncService interface {
	SyncMediaServerItems(context.Context, string) error
}

// mediaServerSyncHandlerBinder breaks the construction cycle between River and
// the media server runtime without exposing River to the runtime.
type mediaServerSyncHandlerBinder struct {
	mu      sync.RWMutex
	handler mediaServerSyncService
}

func (b *mediaServerSyncHandlerBinder) Bind(handler mediaServerSyncService) error {
	if b == nil || handler == nil {
		return jobs.ErrMediaServerSyncHandlerUnavailable
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.handler != nil {
		return errMediaServerSyncHandlerAlreadyBound
	}
	b.handler = handler
	return nil
}

func (b *mediaServerSyncHandlerBinder) HandleMediaServerSync(ctx context.Context, server string) error {
	if b == nil {
		return jobs.ErrMediaServerSyncHandlerUnavailable
	}
	b.mu.RLock()
	handler := b.handler
	b.mu.RUnlock()
	if handler == nil {
		return jobs.ErrMediaServerSyncHandlerUnavailable
	}
	return handler.SyncMediaServerItems(ctx, server)
}

// mediaServerSyncScheduler owns the per-server River schedules. The runtime
// only hands over plain schedules, so River stays inside the jobs package.
type mediaServerSyncScheduler interface {
	SetMediaServerSyncSchedules([]jobs.MediaServerSyncSchedule) error
	EnqueueMediaServerSyncFirstRun(context.Context, string) error
}

// applyMediaServerSyncSchedules pushes the current media server snapshot to the
// scheduler and arms V2's delayed first run for every enabled server. V2 rebuilds
// these schedules whenever the scheduler is re-initialized, so a settings save
// takes effect without a restart.
func applyMediaServerSyncSchedules(
	ctx context.Context,
	runtime *mediaServerRuntime,
	scheduler mediaServerSyncScheduler,
) error {
	if runtime == nil || scheduler == nil {
		return jobs.ErrMediaServerSyncHandlerUnavailable
	}
	if ctx == nil {
		return jobs.ErrMediaServerSyncExecution
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	schedules := runtime.SyncSchedules(jobs.DefaultMediaServerSyncInterval)
	converted := make([]jobs.MediaServerSyncSchedule, 0, len(schedules))
	for _, schedule := range schedules {
		converted = append(converted, jobs.MediaServerSyncSchedule{
			Server: schedule.Server, Interval: schedule.Interval,
		})
	}
	if err := scheduler.SetMediaServerSyncSchedules(converted); err != nil {
		return err
	}
	for _, schedule := range converted {
		if err := scheduler.EnqueueMediaServerSyncFirstRun(ctx, schedule.Server); err != nil {
			return err
		}
	}
	return nil
}

var _ jobs.MediaServerSyncHandler = (*mediaServerSyncHandlerBinder)(nil)
var _ mediaServerSyncService = (*mediaServerRuntime)(nil)
