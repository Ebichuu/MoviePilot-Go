package httpapi

import (
	"context"
	"net/http"
	"runtime"
	"time"
)

// monitorRiverJobStates 是 River 各状态的计数，用于观察积压。
type monitorRiverJobStates map[string]int64

// monitorRuntime 返回进程与队列的实时观测值。
//
// 与 `/system/doctor` 的区别：doctor 回答「系统是否健康、配置是否齐全」，
// monitor 回答「现在有多忙、有没有积压」。两者都不包含凭据。
func (d Dependencies) monitorRuntime(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	var stats runtime.MemStats
	runtime.ReadMemStats(&stats)

	payload := map[string]any{
		"generated_at":   time.Now().UTC(),
		"environment":    d.Config.Environment,
		"goroutines":     runtime.NumGoroutine(),
		"heap_alloc":     stats.HeapAlloc,
		"heap_inuse":     stats.HeapInuse,
		"heap_sys":       stats.HeapSys,
		"gc_cycles":      stats.NumGC,
		"gc_pause_total": stats.PauseTotalNs,
	}

	if d.DB != nil {
		started := time.Now()
		if err := d.DB.Ping(ctx); err != nil {
			payload["database"] = map[string]any{"ok": false, "error": err.Error()}
		} else {
			payload["database"] = map[string]any{
				"ok":      true,
				"latency": time.Since(started).String(),
			}
		}
		if jobs, err := d.riverJobStates(ctx); err == nil {
			payload["river_jobs"] = jobs
		} else {
			payload["river_jobs_error"] = err.Error()
		}
	}

	if d.Cache != nil {
		started := time.Now()
		if err := d.Cache.Ping(ctx); err != nil {
			payload["redis"] = map[string]any{"ok": false, "error": err.Error()}
		} else {
			payload["redis"] = map[string]any{
				"ok":      true,
				"latency": time.Since(started).String(),
			}
		}
	}

	writeSuccess(w, payload)
}

// riverJobStates 统计 River 队列各状态的行数；失败时由调用方决定如何降级。
func (d Dependencies) riverJobStates(ctx context.Context) (monitorRiverJobStates, error) {
	rows, err := d.DB.Pool.Query(ctx, `SELECT state::text, count(*) FROM river_job GROUP BY state`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	result := make(monitorRiverJobStates, 6)
	for rows.Next() {
		var state string
		var count int64
		if err := rows.Scan(&state, &count); err != nil {
			return nil, err
		}
		result[state] = count
	}
	return result, rows.Err()
}
