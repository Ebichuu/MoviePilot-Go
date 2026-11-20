package database

import (
	"context"
	"errors"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/moviepilot-go/moviepilot-go/internal/downloader"
)

// intentCreateFakeRower 按顺序返回预设错误，用尽后返回成功行。
type intentCreateFakeRower struct {
	failures []error
	intent   downloader.Intent
	calls    int
}

func (f *intentCreateFakeRower) QueryRow(context.Context, string, ...any) pgx.Row {
	f.calls++
	if f.calls <= len(f.failures) {
		return intentScanErrorRow{err: f.failures[f.calls-1]}
	}
	return intentScanSuccessRow{intent: f.intent}
}

type intentScanErrorRow struct{ err error }

func (r intentScanErrorRow) Scan(...any) error { return r.err }

type intentScanSuccessRow struct{ intent downloader.Intent }

func (r intentScanSuccessRow) Scan(dest ...any) error {
	*(dest[0].(*uuid.UUID)) = r.intent.ID
	*(dest[1].(*string)) = r.intent.CandidateKey
	*(dest[2].(*string)) = r.intent.Label
	*(dest[3].(*downloader.IntentState)) = r.intent.State
	*(dest[4].(*string)) = r.intent.TorrentHash
	return nil
}

// TestCreateDownloadIntentRetriesConcurrencyErrors 覆盖幂等创建对并发 upsert
// 可重试错误（40001 序列化失败、40P01 死锁）的有界重试，以及不可重试错误的直通。
func TestCreateDownloadIntentRetriesConcurrencyErrors(t *testing.T) {
	intent := mustDatabaseDownloadIntent(t, "retry-owner", "retry-key")
	ctx := context.Background()

	rower := &intentCreateFakeRower{
		failures: []error{
			&pgconn.PgError{Code: "40P01"},
			&pgconn.PgError{Code: "40001"},
		},
		intent: intent,
	}
	stored, err := createDownloadIntent(ctx, rower, "sql", nil)
	if err != nil {
		t.Fatalf("重试后创建 error = %v", err)
	}
	if rower.calls != 3 || stored != intent {
		t.Fatalf("调用次数/结果 = (%d, %#v), want (3, %#v)", rower.calls, stored, intent)
	}

	// 可重试错误持续出现时按上限放弃，并映射为持久化错误。
	exhausted := &intentCreateFakeRower{
		failures: []error{
			&pgconn.PgError{Code: "40P01"}, &pgconn.PgError{Code: "40P01"},
			&pgconn.PgError{Code: "40P01"}, &pgconn.PgError{Code: "40P01"},
			&pgconn.PgError{Code: "40P01"},
		},
		intent: intent,
	}
	if _, err := createDownloadIntent(ctx, exhausted, "sql", nil); !errors.Is(err, ErrDownloadIntentPersistence) {
		t.Fatalf("重试耗尽 error = %v, want ErrDownloadIntentPersistence", err)
	}
	if exhausted.calls != downloadIntentCreateAttempts {
		t.Fatalf("重试次数 = %d, want %d", exhausted.calls, downloadIntentCreateAttempts)
	}

	// 非并发错误不重试。
	broken := &intentCreateFakeRower{failures: []error{errors.New("连接被重置")}, intent: intent}
	if _, err := createDownloadIntent(ctx, broken, "sql", nil); !errors.Is(err, ErrDownloadIntentPersistence) {
		t.Fatalf("不可重试错误 error = %v, want ErrDownloadIntentPersistence", err)
	}
	if broken.calls != 1 {
		t.Fatalf("不可重试错误调用次数 = %d, want 1", broken.calls)
	}

	// 重试等待期间上下文取消立即返回。
	canceled, cancel := context.WithCancel(ctx)
	cancel()
	unfinished := &intentCreateFakeRower{
		failures: []error{&pgconn.PgError{Code: "40P01"}, &pgconn.PgError{Code: "40P01"}},
		intent:   intent,
	}
	if _, err := createDownloadIntent(canceled, unfinished, "sql", nil); !errors.Is(err, context.Canceled) {
		t.Fatalf("取消上下文 error = %v, want context.Canceled", err)
	}
}
