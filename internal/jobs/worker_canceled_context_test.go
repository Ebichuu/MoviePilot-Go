package jobs

import (
	"context"
	"errors"
	"testing"

	"github.com/google/uuid"
	"github.com/riverqueue/river"
)

// TestSideEffectWorkersCanceledContext 覆盖抓取/通知 worker 在上下文已取消
// 与 handler 执行过程中被取消时优先返回上下文错误。
func TestSideEffectWorkersCanceledContext(t *testing.T) {
	canceled, cancel := context.WithCancel(context.Background())
	cancel()

	t.Run("抓取", func(t *testing.T) {
		worker := &scrapeWorker{handler: scrapeJobHandlerFunc(func(context.Context, ScrapeArgs) error {
			return nil
		})}
		if err := worker.Work(canceled, nil); !errors.Is(err, context.Canceled) {
			t.Fatalf("已取消上下文 error = %v", err)
		}

		ctx, cancelLater := context.WithCancel(context.Background())
		defer cancelLater()
		failing := &scrapeWorker{handler: scrapeJobHandlerFunc(func(context.Context, ScrapeArgs) error {
			cancelLater()
			return errors.New("抓取失败")
		})}
		job := &river.Job[ScrapeArgs]{Args: ScrapeArgs{RequestID: uuid.New()}}
		if err := failing.Work(ctx, job); !errors.Is(err, context.Canceled) {
			t.Fatalf("取消时的 handler 失败 error = %v", err)
		}
	})

	t.Run("通知", func(t *testing.T) {
		worker := &notificationWorker{handler: notificationJobHandlerFunc(func(context.Context, NotificationArgs) error {
			return nil
		})}
		if err := worker.Work(canceled, nil); !errors.Is(err, context.Canceled) {
			t.Fatalf("已取消上下文 error = %v", err)
		}

		ctx, cancelLater := context.WithCancel(context.Background())
		defer cancelLater()
		failing := &notificationWorker{handler: notificationJobHandlerFunc(func(context.Context, NotificationArgs) error {
			cancelLater()
			return errors.New("通知失败")
		})}
		job := &river.Job[NotificationArgs]{Args: NotificationArgs{RequestID: uuid.New()}}
		if err := failing.Work(ctx, job); !errors.Is(err, context.Canceled) {
			t.Fatalf("取消时的 handler 失败 error = %v", err)
		}
	})
}
