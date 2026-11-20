package jobs

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/riverqueue/river"
)

type organizationSweepGuardHandler func(context.Context) error

func (f organizationSweepGuardHandler) HandleOrganizationSweep(ctx context.Context) error {
	return f(ctx)
}

// TestSweepWorkerContextGuards 覆盖整理恢复、订阅精准搜索、下载核对三个 worker
// 的 nil/已取消上下文、非法时间参数，以及 handler 失败时上下文错误优先。
func TestSweepWorkerContextGuards(t *testing.T) {
	canceled, cancel := context.WithCancel(context.Background())
	cancel()
	now := time.Now().UTC()

	t.Run("整理恢复", func(t *testing.T) {
		worker := &organizationSweepWorker{handler: organizationSweepGuardHandler(func(context.Context) error {
			return nil
		})}
		if err := worker.Work(nil, nil); !errors.Is(err, ErrOrganizationSweepExecution) {
			t.Fatalf("nil 上下文 error = %v", err)
		}
		if err := worker.Work(canceled, nil); !errors.Is(err, context.Canceled) {
			t.Fatalf("已取消上下文 error = %v", err)
		}

		cancelCtx, cancelLater := context.WithCancel(context.Background())
		defer cancelLater()
		failing := &organizationSweepWorker{handler: organizationSweepGuardHandler(func(context.Context) error {
			cancelLater()
			return errors.New("扫描失败")
		})}
		job := &river.Job[OrganizationSweepArgs]{Args: OrganizationSweepArgs{Key: organizationSweepKey}}
		if err := failing.Work(cancelCtx, job); !errors.Is(err, context.Canceled) {
			t.Fatalf("取消时的 handler 失败 error = %v", err)
		}
	})

	t.Run("订阅精准搜索", func(t *testing.T) {
		worker := &subscriptionSearchWorker{handler: subscriptionSearchHandlerFunc(func(context.Context, time.Time) error {
			return nil
		})}
		if err := worker.Work(nil, nil); !errors.Is(err, ErrSubscriptionSearchExecution) {
			t.Fatalf("nil 上下文 error = %v", err)
		}
		if err := worker.Work(canceled, nil); !errors.Is(err, context.Canceled) {
			t.Fatalf("已取消上下文 error = %v", err)
		}

		future := &river.Job[SubscriptionSearchSweepArgs]{Args: SubscriptionSearchSweepArgs{
			Key: subscriptionSearchKey, Cutoff: now.Add(2 * time.Hour),
		}}
		if err := worker.Work(context.Background(), future); !errors.Is(err, ErrInvalidSubscriptionSearchArgs) {
			t.Fatalf("未来时间参数 error = %v", err)
		}
	})

	t.Run("下载核对", func(t *testing.T) {
		worker := &downloadReconcileWorker{handler: downloadReconcileHandlerFunc(func(context.Context, time.Time) error {
			return nil
		})}
		if err := worker.Work(nil, nil); !errors.Is(err, ErrDownloadReconcileExecution) {
			t.Fatalf("nil 上下文 error = %v", err)
		}
		if err := worker.Work(canceled, nil); !errors.Is(err, context.Canceled) {
			t.Fatalf("已取消上下文 error = %v", err)
		}

		future := &river.Job[DownloadReconcileSweepArgs]{Args: DownloadReconcileSweepArgs{
			Key: downloadReconcileKey, Cutoff: now.Add(2 * time.Hour),
		}}
		if err := worker.Work(context.Background(), future); !errors.Is(err, ErrInvalidDownloadReconcileArgs) {
			t.Fatalf("未来时间参数 error = %v", err)
		}
	})
}
