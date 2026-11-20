//go:build integration

package jobs

import (
	"context"
	"encoding/json"
	"net/url"
	"os"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/riverqueue/river"
	"github.com/riverqueue/river/riverdriver/riverpgxv5"
	"github.com/riverqueue/river/rivertype"
)

const rssRiverTestDatabaseEnv = "MOVIEPILOT_TEST_DATABASE_URL"

func TestRSSSweepUniqueAcrossActiveRiverStates(t *testing.T) {
	pool := newRSSRiverIntegrationPool(t)
	client := newRSSRiverInsertClient(t, pool)
	cutoff := time.Now().UTC().Add(-time.Minute)

	states := []rivertype.JobState{
		rivertype.JobStateAvailable,
		rivertype.JobStateScheduled,
		rivertype.JobStateRunning,
		rivertype.JobStateRetryable,
	}
	for _, state := range states {
		t.Run(string(state), func(t *testing.T) {
			if _, err := pool.Exec(t.Context(), `TRUNCATE river_job CASCADE`); err != nil {
				t.Fatalf("清空 River job: %v", err)
			}

			first, err := client.Insert(t.Context(), RSSSweepArgs{Key: rssSweepKey, Cutoff: cutoff}, nil)
			if err != nil {
				t.Fatalf("插入首个 sweep: %v", err)
			}
			if first.UniqueSkippedAsDuplicate {
				t.Fatal("首个 sweep 被错误去重")
			}
			if _, err := pool.Exec(t.Context(), `
				UPDATE river_job
				SET state=$2::text::river_job_state, scheduled_at=now(), finalized_at=NULL,
					attempt=CASE WHEN $2::text='running' THEN 1 ELSE attempt END,
					attempted_at=CASE WHEN $2::text='running' THEN now() ELSE attempted_at END,
					attempted_by=CASE WHEN $2::text='running' THEN ARRAY['rss-river-integration'] ELSE attempted_by END
				WHERE id=$1`, first.Job.ID, state); err != nil {
				t.Fatalf("设置 River 状态 %s: %v", state, err)
			}

			for round := 2; round <= 3; round++ {
				duplicate, err := client.Insert(
					t.Context(),
					RSSSweepArgs{Key: rssSweepKey, Cutoff: cutoff.Add(time.Duration(round-1) * time.Minute)},
					nil,
				)
				if err != nil {
					t.Fatalf("插入第 %d 轮重复 sweep: %v", round, err)
				}
				if !duplicate.UniqueSkippedAsDuplicate || duplicate.Job.ID != first.Job.ID {
					t.Fatalf(
						"state %s round %d duplicate = skipped:%t id:%d, want skipped id %d",
						state,
						round,
						duplicate.UniqueSkippedAsDuplicate,
						duplicate.Job.ID,
						first.Job.ID,
					)
				}
			}
		})
	}
}

func TestRSSSweepStateSurvivesRiverWorkerRestart(t *testing.T) {
	pool := newRSSRiverIntegrationPool(t)
	cutoff := time.Now().UTC().Add(-time.Minute)
	wantState := RSSSweepState{Initialized: true, SiteIDs: []int{7, 9}}

	firstCalled := make(chan struct{}, 1)
	first := newRSSRiverWorkerClient(t, pool, rssSweepStateHandlerFunc(
		func(context.Context, time.Time, RSSSweepState) (RSSSweepState, error) {
			firstCalled <- struct{}{}
			return wantState, ErrRSSSweepPending
		},
	))
	if err := first.Start(t.Context()); err != nil {
		t.Fatalf("启动首个 River worker: %v", err)
	}
	firstRunning := true
	defer func() {
		if firstRunning {
			stopRSSRiverClient(t, first)
		}
	}()
	inserted, err := first.Insert(t.Context(), RSSSweepArgs{Key: rssSweepKey, Cutoff: cutoff}, nil)
	if err != nil {
		t.Fatalf("插入恢复测试 sweep: %v", err)
	}
	select {
	case <-firstCalled:
	case <-time.After(5 * time.Second):
		t.Fatal("首个 River worker 未执行")
	}
	row := waitRSSRiverJobState(t, first, inserted.Job.ID, rivertype.JobStateScheduled)
	var persisted RSSSweepState
	if err := json.Unmarshal(row.Output(), &persisted); err != nil {
		t.Fatalf("解析持久 sweep state: %v", err)
	}
	if !reflect.DeepEqual(persisted, wantState) {
		t.Fatalf("持久 sweep state = %+v, want %+v", persisted, wantState)
	}
	stopRSSRiverClient(t, first)
	firstRunning = false

	if _, err := pool.Exec(t.Context(), `
		UPDATE river_job
		SET state='available', scheduled_at=now(), attempted_at=NULL, attempted_by=NULL
		WHERE id=$1 AND state='scheduled'`, inserted.Job.ID); err != nil {
		t.Fatalf("恢复 snooze job: %v", err)
	}

	restoredState := make(chan RSSSweepState, 1)
	second := newRSSRiverWorkerClient(t, pool, rssSweepStateHandlerFunc(
		func(_ context.Context, _ time.Time, state RSSSweepState) (RSSSweepState, error) {
			restoredState <- state
			return state, nil
		},
	))
	if err := second.Start(t.Context()); err != nil {
		t.Fatalf("启动恢复 River worker: %v", err)
	}
	defer stopRSSRiverClient(t, second)
	select {
	case got := <-restoredState:
		if !reflect.DeepEqual(got, wantState) {
			t.Fatalf("恢复 sweep state = %+v, want %+v", got, wantState)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("恢复 River worker 未执行")
	}
	waitRSSRiverJobState(t, second, inserted.Job.ID, rivertype.JobStateCompleted)
}

func newRSSRiverIntegrationPool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	databaseURL := strings.TrimSpace(os.Getenv(rssRiverTestDatabaseEnv))
	if databaseURL == "" {
		t.Skipf("未设置 %s，跳过 River PostgreSQL 集成测试", rssRiverTestDatabaseEnv)
	}

	ctx, cancel := context.WithTimeout(t.Context(), 45*time.Second)
	defer cancel()
	adminConfig, err := pgxpool.ParseConfig(databaseURL)
	if err != nil {
		t.Fatalf("解析 PostgreSQL 集成地址: %v", err)
	}
	adminConfig.MaxConns = 2
	adminConfig.MinConns = 0
	adminPool, err := pgxpool.NewWithConfig(ctx, adminConfig)
	if err != nil {
		t.Fatalf("创建 PostgreSQL 管理连接池: %v", err)
	}
	if err := adminPool.Ping(ctx); err != nil {
		adminPool.Close()
		t.Fatalf("连接 PostgreSQL 集成数据库: %v", err)
	}

	schema := "rssit_" + strings.ReplaceAll(uuid.NewString(), "-", "")[:16]
	quotedSchema := pgx.Identifier{schema}.Sanitize()
	if _, err := adminPool.Exec(ctx, "CREATE SCHEMA "+quotedSchema); err != nil {
		adminPool.Close()
		t.Fatalf("创建 River 隔离 schema: %v", err)
	}

	scopedURL := rssRiverDatabaseURLWithSearchPath(t, databaseURL, schema)
	poolConfig, err := pgxpool.ParseConfig(scopedURL)
	if err != nil {
		adminPool.Close()
		t.Fatalf("解析 River 隔离地址: %v", err)
	}
	poolConfig.MaxConns = 6
	poolConfig.MinConns = 0
	pool, err := pgxpool.NewWithConfig(ctx, poolConfig)
	if err != nil {
		adminPool.Close()
		t.Fatalf("创建 River 隔离连接池: %v", err)
	}
	if err := pool.Ping(ctx); err != nil {
		pool.Close()
		adminPool.Close()
		t.Fatalf("连接 River 隔离 schema: %v", err)
	}
	if err := Migrate(ctx, pool); err != nil {
		pool.Close()
		adminPool.Close()
		t.Fatalf("迁移 River 隔离 schema: %v", err)
	}

	t.Cleanup(func() {
		pool.Close()
		cleanupCtx, cleanupCancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cleanupCancel()
		if _, err := adminPool.Exec(cleanupCtx, "DROP SCHEMA "+quotedSchema+" CASCADE"); err != nil {
			t.Errorf("清理 River 隔离 schema: %v", err)
		}
		adminPool.Close()
	})
	return pool
}

func rssRiverDatabaseURLWithSearchPath(t *testing.T, databaseURL, schema string) string {
	t.Helper()
	parsed, err := url.Parse(databaseURL)
	if err != nil {
		t.Fatalf("解析 PostgreSQL URL: %v", err)
	}
	query := parsed.Query()
	query.Set("search_path", schema)
	query.Set("application_name", "moviepilot-rss-river-integration")
	parsed.RawQuery = query.Encode()
	return parsed.String()
}

func newRSSRiverInsertClient(t *testing.T, pool *pgxpool.Pool) *river.Client[pgx.Tx] {
	t.Helper()
	client, err := river.NewClient(riverpgxv5.New(pool), &river.Config{})
	if err != nil {
		t.Fatalf("创建 River insert client: %v", err)
	}
	return client
}

func newRSSRiverWorkerClient(
	t *testing.T,
	pool *pgxpool.Pool,
	handler RSSSweepHandler,
) *river.Client[pgx.Tx] {
	t.Helper()
	workers := river.NewWorkers()
	if err := river.AddWorkerSafely(workers, &rssSweepWorker{handler: handler}); err != nil {
		t.Fatalf("注册 RSS sweep worker: %v", err)
	}
	client, err := river.NewClient(riverpgxv5.New(pool), &river.Config{
		Queues: map[string]river.QueueConfig{
			queueRSSSweep: {MaxWorkers: 1},
		},
		Workers:           workers,
		MaxAttempts:       rssSweepMaxAttempts,
		JobTimeout:        time.Minute,
		FetchCooldown:     10 * time.Millisecond,
		FetchPollInterval: 50 * time.Millisecond,
	})
	if err != nil {
		t.Fatalf("创建 RSS River worker client: %v", err)
	}
	return client
}

func waitRSSRiverJobState(
	t *testing.T,
	client *river.Client[pgx.Tx],
	jobID int64,
	want rivertype.JobState,
) *rivertype.JobRow {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		row, err := client.JobGet(t.Context(), jobID)
		if err != nil {
			t.Fatalf("读取 River job %d: %v", jobID, err)
		}
		if row.State == want {
			return row
		}
		time.Sleep(25 * time.Millisecond)
	}
	row, err := client.JobGet(t.Context(), jobID)
	if err != nil {
		t.Fatalf("读取超时 River job %d: %v", jobID, err)
	}
	t.Fatalf("River job %d state = %s, want %s", jobID, row.State, want)
	return nil
}

func stopRSSRiverClient(t *testing.T, client *river.Client[pgx.Tx]) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := client.Stop(ctx); err != nil {
		t.Fatalf("停止 River client: %v", err)
	}
}
