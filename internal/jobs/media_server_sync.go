package jobs

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/riverqueue/river"
	"github.com/riverqueue/river/rivertype"
)

const (
	queueMediaServerSync = "media_server_sync"
	mediaServerSyncKind  = "media_server_item_sync"
	// mediaServerSyncPeriodicPrefix mirrors V2's deterministic per-server job ID
	// `mediaserver_sync_<sha256(name)[:12]>`.
	mediaServerSyncPeriodicPrefix = "media-server-item-sync-v1-"
	mediaServerSyncKey            = "run"
	// DefaultMediaServerSyncInterval is V2's `MEDIASERVER_SYNC_INTERVAL` default.
	DefaultMediaServerSyncInterval = 6 * time.Hour
	// MediaServerSyncFirstRunDelay is V2's `next_run_time = now + 10 minutes`.
	MediaServerSyncFirstRunDelay = 10 * time.Minute
	mediaServerSyncTimeout       = 15 * time.Minute
	mediaServerSyncMaxNameBytes  = 255
)

var (
	ErrInvalidMediaServerSyncArgs        = errors.New("媒体服务器条目同步任务参数无效")
	ErrMediaServerSyncHandlerUnavailable = errors.New("媒体服务器条目同步任务处理器未配置")
	ErrMediaServerSyncExecution          = errors.New("执行媒体服务器条目同步任务失败")
)

// MediaServerSyncArgs wakes one cache refresh. An empty Server means "all
// enabled servers", which is V2's manual `mediaserver_sync` job; a named server
// mirrors V2's per-server schedule kwargs.
type MediaServerSyncArgs struct {
	Key    string `json:"key" river:"unique"`
	Server string `json:"server" river:"unique"`
}

func (MediaServerSyncArgs) Kind() string { return mediaServerSyncKind }

// Normalize trims the fields used for uniqueness so one server never gets two
// distinct durable jobs that differ only by surrounding whitespace.
func (a MediaServerSyncArgs) Normalize() MediaServerSyncArgs {
	a.Key = strings.TrimSpace(a.Key)
	a.Server = strings.TrimSpace(a.Server)
	return a
}

func (a MediaServerSyncArgs) InsertOpts() river.InsertOpts {
	return river.InsertOpts{
		Queue:       queueMediaServerSync,
		MaxAttempts: 5,
		UniqueOpts: river.UniqueOpts{
			ByArgs: true,
			ByState: []rivertype.JobState{
				rivertype.JobStateAvailable,
				rivertype.JobStatePending,
				rivertype.JobStateRetryable,
				rivertype.JobStateRunning,
				rivertype.JobStateScheduled,
			},
		},
	}
}

func (a MediaServerSyncArgs) Validate() error {
	normalized := a.Normalize()
	if normalized.Key != mediaServerSyncKey ||
		len(normalized.Server) > mediaServerSyncMaxNameBytes ||
		strings.ContainsAny(normalized.Server, "\x00\r\n") {
		return ErrInvalidMediaServerSyncArgs
	}
	return nil
}

// MediaServerSyncHandler syncs the named server, or every enabled server when
// the selector is empty.
type MediaServerSyncHandler interface {
	HandleMediaServerSync(context.Context, string) error
}

// MediaServerSyncSchedule is one enabled server and its effective interval.
type MediaServerSyncSchedule struct {
	Server   string
	Interval time.Duration
}

type mediaServerSyncWorker struct {
	river.WorkerDefaults[MediaServerSyncArgs]
	handler MediaServerSyncHandler
}

func (w *mediaServerSyncWorker) Work(ctx context.Context, job *river.Job[MediaServerSyncArgs]) error {
	if ctx == nil {
		return ErrMediaServerSyncExecution
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if w == nil || w.handler == nil {
		return ErrMediaServerSyncHandlerUnavailable
	}
	if job == nil {
		return river.JobCancel(ErrInvalidMediaServerSyncArgs)
	}
	args := job.Args.Normalize()
	if err := args.Validate(); err != nil {
		return river.JobCancel(err)
	}
	if err := w.handler.HandleMediaServerSync(ctx, args.Server); err != nil {
		if contextErr := sanitizedMediaServerSyncContextError(ctx, err); contextErr != nil {
			return contextErr
		}
		return ErrMediaServerSyncExecution
	}
	return nil
}

func (*mediaServerSyncWorker) Timeout(*river.Job[MediaServerSyncArgs]) time.Duration {
	return mediaServerSyncTimeout
}

func registerMediaServerSyncWorker(workers *river.Workers, handler MediaServerSyncHandler) (bool, error) {
	if handler == nil {
		return false, nil
	}
	if err := river.AddWorkerSafely(workers, &mediaServerSyncWorker{handler: handler}); err != nil {
		return false, errors.New("注册媒体服务器条目同步 worker 失败")
	}
	return true, nil
}

// mediaServerSyncPeriodicJob registers one server interval schedule. V2 starts
// the first run 10 minutes after the scheduler is built and then repeats on the
// configured interval, so the periodic job itself does not run on start; the
// delayed first run is enqueued separately.
func mediaServerSyncPeriodicJob(schedule MediaServerSyncSchedule, generation uint64) *river.PeriodicJob {
	args := MediaServerSyncArgs{Key: mediaServerSyncKey, Server: schedule.Server}.Normalize()
	return river.NewPeriodicJob(
		river.PeriodicInterval(schedule.Interval),
		func() (river.JobArgs, *river.InsertOpts) {
			opts := args.InsertOpts()
			return args, &opts
		},
		&river.PeriodicJobOpts{ID: mediaServerSyncPeriodicJobID(schedule.Server, generation)},
	)
}

// mediaServerSyncPeriodicJobID 由服务器名与注册代数共同决定。River v0.24 的
// PeriodicJobBundle.Remove 只注销任务本体，不会释放 ID 的唯一性集合，因此同名
// 服务器重建时必须使用新 ID，否则必定报「同一 ID 已注册」（保存媒体服务器设置
// 的真实故障）。gen 单调递增，旧的墓碑 ID 由 River 的定期回收清理。
func mediaServerSyncPeriodicJobID(server string, generation uint64) string {
	sum := sha256.Sum256([]byte(strings.TrimSpace(server)))
	return mediaServerSyncPeriodicPrefix + hex.EncodeToString(sum[:])[:12] + "-" + strconv.FormatUint(generation, 10)
}

// SetMediaServerSyncSchedules replaces every per-server periodic sync job with
// the given schedules. V2 registers one interval schedule per enabled media
// server using that server's `sync_interval` or the 6 hour default.
//
// 周期任务 ID 由服务器名决定，因此同名服务器的间隔或配置变更必须先注销旧任务，
// 否则 River 会以「同一 ID 已注册」拒绝重建（V2 保存媒体服务器设置时必然触发）。
// 重建失败时按上一次成功注册的计划回滚，保证旧计划不丢。
func (m *Manager) SetMediaServerSyncSchedules(schedules []MediaServerSyncSchedule) error {
	if m == nil || !m.mediaServerSyncEnabled || m.mediaServerPeriodicJobs == nil {
		return ErrMediaServerSyncHandlerUnavailable
	}
	normalized, err := normalizeMediaServerSyncSchedules(schedules)
	if err != nil {
		return err
	}

	m.mediaServerPeriodicMu.Lock()
	defer m.mediaServerPeriodicMu.Unlock()

	previous := append([]MediaServerSyncSchedule(nil), m.mediaServerPeriodicSchedules...)
	m.mediaServerPeriodicGeneration++
	generation := m.mediaServerPeriodicGeneration
	for _, handle := range m.mediaServerPeriodicHandles {
		m.mediaServerPeriodicJobs.Remove(handle)
	}
	m.mediaServerPeriodicHandles = nil

	next := make([]rivertype.PeriodicJobHandle, 0, len(normalized))
	for _, schedule := range normalized {
		handle, addErr := m.mediaServerPeriodicJobs.AddSafely(mediaServerSyncPeriodicJob(schedule, generation))
		if addErr != nil {
			for _, added := range next {
				m.mediaServerPeriodicJobs.Remove(added)
			}
			m.mediaServerPeriodicHandles = m.restoreMediaServerSyncSchedules(previous, generation)
			return fmt.Errorf("更新媒体服务器同步周期任务: %w", addErr)
		}
		next = append(next, handle)
	}
	m.mediaServerPeriodicHandles = next
	m.mediaServerPeriodicSchedules = append([]MediaServerSyncSchedule(nil), normalized...)
	return nil
}

// restoreMediaServerSyncSchedules 重新注册上一次成功应用的计划；单条失败时保留其余，
// 不再继续放大错误。
func (m *Manager) restoreMediaServerSyncSchedules(schedules []MediaServerSyncSchedule, generation uint64) []rivertype.PeriodicJobHandle {
	restored := make([]rivertype.PeriodicJobHandle, 0, len(schedules))
	for _, schedule := range schedules {
		handle, err := m.mediaServerPeriodicJobs.AddSafely(mediaServerSyncPeriodicJob(schedule, generation))
		if err != nil {
			continue
		}
		restored = append(restored, handle)
	}
	return restored
}

func normalizeMediaServerSyncSchedules(schedules []MediaServerSyncSchedule) ([]MediaServerSyncSchedule, error) {
	normalized := make([]MediaServerSyncSchedule, 0, len(schedules))
	seen := make(map[string]struct{}, len(schedules))
	for _, schedule := range schedules {
		server := strings.TrimSpace(schedule.Server)
		if server == "" || len(server) > mediaServerSyncMaxNameBytes ||
			strings.ContainsAny(server, "\x00\r\n") || schedule.Interval <= 0 {
			return nil, ErrInvalidMediaServerSyncArgs
		}
		if _, ok := seen[server]; ok {
			continue
		}
		seen[server] = struct{}{}
		normalized = append(normalized, MediaServerSyncSchedule{Server: server, Interval: schedule.Interval})
	}
	return normalized, nil
}

// EnqueueMediaServerSync requests an immediate bounded cache refresh for one
// server, or for every enabled server when the selector is empty.
func (m *Manager) EnqueueMediaServerSync(ctx context.Context, server string) error {
	return m.enqueueMediaServerSync(ctx, server, time.Time{})
}

// EnqueueMediaServerSyncFirstRun schedules the delayed first sync V2 performs 10
// minutes after it registers a server schedule.
func (m *Manager) EnqueueMediaServerSyncFirstRun(ctx context.Context, server string) error {
	return m.enqueueMediaServerSync(ctx, server, time.Now().Add(MediaServerSyncFirstRunDelay))
}

func (m *Manager) enqueueMediaServerSync(ctx context.Context, server string, scheduledAt time.Time) error {
	if ctx == nil {
		return ErrMediaServerSyncExecution
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	args := MediaServerSyncArgs{Key: mediaServerSyncKey, Server: server}
	if err := args.Validate(); err != nil {
		return err
	}
	if m == nil || m.client == nil || !m.mediaServerSyncEnabled {
		return ErrMediaServerSyncHandlerUnavailable
	}
	args = args.Normalize()
	opts := args.InsertOpts()
	if !scheduledAt.IsZero() {
		opts.ScheduledAt = scheduledAt
	}
	if _, err := m.client.Insert(ctx, args, &opts); err != nil {
		if contextErr := sanitizedMediaServerSyncContextError(ctx, err); contextErr != nil {
			return contextErr
		}
		return ErrMediaServerSyncExecution
	}
	return nil
}

func sanitizedMediaServerSyncContextError(ctx context.Context, err error) error {
	if ctx != nil {
		if contextErr := ctx.Err(); contextErr != nil {
			return contextErr
		}
	}
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return err
	}
	return nil
}

var _ river.JobArgsWithInsertOpts = MediaServerSyncArgs{}
var _ river.Worker[MediaServerSyncArgs] = (*mediaServerSyncWorker)(nil)
