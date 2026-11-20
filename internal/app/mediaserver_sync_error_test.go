package app

import (
	"context"
	"errors"
	"testing"
	"time"
)

func TestMediaServerSyncErrorStableMessages(t *testing.T) {
	tests := []struct {
		name string
		ctx  context.Context
		err  error
		want error
	}{
		{
			name: "取消的上下文优先于普通错误",
			ctx:  cancelledContext(),
			err:  errors.New("上游超时"),
			want: context.Canceled,
		},
		{
			name: "超时的上下文优先",
			ctx:  expiredDeadlineContext(),
			err:  errors.New("上游超时"),
			want: context.DeadlineExceeded,
		},
		{
			name: "错误本身是取消也算取消",
			ctx:  context.Background(),
			err:  context.Canceled,
			want: context.Canceled,
		},
		{
			name: "包装后的超时同样识别",
			ctx:  context.Background(),
			err:  &wrappedError{inner: context.DeadlineExceeded},
			want: context.DeadlineExceeded,
		},
		{
			name: "普通错误统一收敛为同步失败",
			ctx:  context.Background(),
			err:  errors.New("解析条目失败"),
			want: errMediaServerItemSyncFailed,
		},
		{
			name: "nil 上下文也不会 panic",
			ctx:  nil,
			err:  errors.New("解析条目失败"),
			want: errMediaServerItemSyncFailed,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got := mediaServerSyncError(test.ctx, test.err)
			if !errors.Is(got, test.want) {
				t.Fatalf("mediaServerSyncError() = %v, want %v", got, test.want)
			}
			// 稳定文案：调用方按固定错误归类，不能把上游原文透出。
			if test.want == errMediaServerItemSyncFailed && got != errMediaServerItemSyncFailed {
				t.Fatalf("普通错误被包装: %v", got)
			}
			if test.want == context.Canceled && !errors.Is(got, context.Canceled) {
				t.Fatalf("取消失败: %v", got)
			}
		})
	}
}

// cancelledContext 返回一个已取消的上下文。
func cancelledContext() context.Context {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	return ctx
}

// expiredDeadlineContext 返回一个已经超过截止时间的上下文。
func expiredDeadlineContext() context.Context {
	ctx, cancel := context.WithDeadline(context.Background(), time.Now().Add(-time.Second))
	_ = cancel
	return ctx
}

type wrappedError struct{ inner error }

func (e *wrappedError) Error() string { return "上游请求失败: " + e.inner.Error() }
func (e *wrappedError) Unwrap() error { return e.inner }
