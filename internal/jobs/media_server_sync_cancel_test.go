package jobs

import (
	"context"
	"errors"
	"testing"
)

// TestMediaServerSyncWorkerCanceledContext 覆盖同步 worker 在上下文已取消时
// 直接返回上下文错误。
func TestMediaServerSyncWorkerCanceledContext(t *testing.T) {
	canceled, cancel := context.WithCancel(context.Background())
	cancel()

	worker := &mediaServerSyncWorker{handler: mediaServerSyncHandlerFunc(func(context.Context, string) error {
		return nil
	})}
	if err := worker.Work(canceled, nil); !errors.Is(err, context.Canceled) {
		t.Fatalf("已取消上下文 error = %v", err)
	}
}
