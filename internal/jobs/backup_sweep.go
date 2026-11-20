package jobs

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/riverqueue/river"
)

const (
	// backupSweepKind 是定期配置备份，对应 V2 `scheduler.py` 的备份类任务。
	backupSweepKind = "backup_sweep"
	// queueBackupSweep 单独一条队列，避免备份影响业务队列。
	queueBackupSweep = "backup_sweep"
	// backupSweepInterval：每天一次。
	backupSweepInterval = 24 * time.Hour
	// backupSweepPeriodicID：River 周期任务 ID。
	backupSweepPeriodicID = "backup_sweep_periodic"
	// backupFilePrefix：备份文件名前缀。
	backupFilePrefix = "settings-"
)

// BackupSweepArgs 是备份任务的入参：带上目标目录便于重放与排障。
type BackupSweepArgs struct {
	// Dir 为空时读 MOVIEPILOT_BACKUP_DIR；都为空则不备份（不伪造成功）。
	Dir string `json:"dir,omitempty"`
	// Keep 是要保留的份数；<=0 时退回 MOVIEPILOT_BACKUP_KEEP（默认 7）。
	// 插件配置里的 keep 通过这里生效，避免「改了配置但被打回环境变量」。
	Keep int `json:"keep,omitempty"`
}

// Kind 实现 river.JobArgs。
func (BackupSweepArgs) Kind() string { return backupSweepKind }

// BackupSweepResult 描述一次备份的产物。
type BackupSweepResult struct {
	Path           string
	ExportedRows   int
	SkippedEncrypt int
}

type backupSweepWorker struct {
	river.WorkerDefaults[BackupSweepArgs]

	db     *pgxpool.Pool
	logger *slog.Logger
}

// Work 导出「未加密」的配置项到 JSON 文件，并按保留份数清理旧文件。
//
// 为什么只导出未加密项：`settings.value` 里的加密列是被 MASTER_KEY 保护的密文，
// 把它写到磁盘等于把密文散落在备份目录里，收益很低、风险很高。加密项的条数会
// 计入日志与返回结果，保证「漏了什么」是可数的。
func (w *backupSweepWorker) Work(ctx context.Context, job *river.Job[BackupSweepArgs]) error {
	dir := strings.TrimSpace(job.Args.Dir)
	if dir == "" {
		dir = strings.TrimSpace(os.Getenv("MOVIEPILOT_BACKUP_DIR"))
	}
	if dir == "" {
		w.log().Info("备份任务跳过：未配置 MOVIEPILOT_BACKUP_DIR")
		return nil
	}
	if w.db == nil {
		return nil
	}
	keep := job.Args.Keep
	if keep <= 0 {
		keep = backupKeep(os.Getenv("MOVIEPILOT_BACKUP_KEEP"))
	}
	result, err := BackupSettings(ctx, w.db, dir, keep)
	if err != nil {
		w.log().Warn("备份任务失败", "dir", dir, "err", err)
		return err
	}
	w.log().Info("备份任务完成",
		"path", result.Path, "rows", result.ExportedRows, "skipped_encrypted", result.SkippedEncrypt)
	return nil
}

func (w *backupSweepWorker) log() *slog.Logger {
	if w.logger != nil {
		return w.logger
	}
	return slog.Default()
}

// BackupSettings 把 settings 表里未加密的配置导出成 JSON 文件。
// 抽成普通函数便于单测直接调用，不依赖 River 运行时。
func BackupSettings(ctx context.Context, db *pgxpool.Pool, dir string, keep int) (BackupSweepResult, error) {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return BackupSweepResult{}, err
	}

	rows, err := db.Query(ctx, `
		SELECT key, value, COALESCE(encrypted, false), updated_at
		FROM settings
		ORDER BY key`)
	if err != nil {
		return BackupSweepResult{}, err
	}
	defer rows.Close()

	type entry struct {
		Key       string    `json:"key"`
		Value     string    `json:"value"`
		UpdatedAt time.Time `json:"updated_at"`
	}
	exported := make([]entry, 0, 32)
	result := BackupSweepResult{}
	for rows.Next() {
		var (
			key       string
			value     string
			encrypted bool
			updatedAt time.Time
		)
		if err := rows.Scan(&key, &value, &encrypted, &updatedAt); err != nil {
			return BackupSweepResult{}, err
		}
		if encrypted {
			result.SkippedEncrypt++
			continue
		}
		exported = append(exported, entry{Key: key, Value: value, UpdatedAt: updatedAt})
	}
	if err := rows.Err(); err != nil {
		return BackupSweepResult{}, err
	}

	payload := map[string]any{
		"generated_at":      time.Now().UTC(),
		"schema":            "moviepilot-go/settings-backup/1",
		"settings":          exported,
		"skipped_encrypted": result.SkippedEncrypt,
	}
	encoded, err := json.MarshalIndent(payload, "", "  ")
	if err != nil {
		return BackupSweepResult{}, err
	}

	path := filepath.Join(dir, backupFilePrefix+time.Now().UTC().Format("20060102-150405")+".json")
	if err := os.WriteFile(path, encoded, 0o600); err != nil {
		return BackupSweepResult{}, err
	}
	result.Path = path
	result.ExportedRows = len(exported)

	pruneBackups(dir, keep)
	return result, nil
}

// pruneBackups 只保留最近 keep 份备份；keep<=0 表示不清理。
func pruneBackups(dir string, keep int) {
	if keep <= 0 {
		return
	}
	matches, err := filepath.Glob(filepath.Join(dir, backupFilePrefix+"*.json"))
	if err != nil || len(matches) <= keep {
		return
	}
	sort.Strings(matches)
	for _, stale := range matches[:len(matches)-keep] {
		_ = os.Remove(stale)
	}
}

func backupKeep(value string) int {
	parsed, err := strconv.Atoi(strings.TrimSpace(value))
	if err != nil || parsed <= 0 {
		return 7
	}
	return parsed
}

// registerBackupSweepWorker 注册备份 worker。返回 false 表示本进程不启用。
func registerBackupSweepWorker(workers *river.Workers, db *pgxpool.Pool, logger *slog.Logger) (bool, error) {
	if db == nil {
		return false, nil
	}
	// river.AddWorker 在重复注册时 panic：kind 唯一性由本文件常量保证。
	river.AddWorker(workers, &backupSweepWorker{db: db, logger: logger})
	return true, nil
}

// backupSweepPeriodicJob 每天插入一次备份任务。
func backupSweepPeriodicJob() *river.PeriodicJob {
	return river.NewPeriodicJob(
		river.PeriodicInterval(backupSweepInterval),
		func() (river.JobArgs, *river.InsertOpts) {
			return BackupSweepArgs{}, nil
		},
		&river.PeriodicJobOpts{ID: backupSweepPeriodicID},
	)
}

// 备份的「未启用」与「入队失败」分开报，理由同签到：一个是配置，一个是队列。
var (
	ErrBackupSweepUnavailable = errors.New("备份 worker 未启用")
	ErrBackupSweepExecution   = errors.New("执行备份任务失败")
)

// backupSweepMinInterval 是插件配置允许的最短备份周期：备份是写盘操作，
// 比每小时更密只会把磁盘写满。
const backupSweepMinInterval = time.Hour

// backupRuntimePeriodicJob 用于热更新，ID 留空由 River 生成（详见签到侧注释）。
func backupRuntimePeriodicJob(interval time.Duration) *river.PeriodicJob {
	return river.NewPeriodicJob(
		river.PeriodicInterval(normalizeBackupSweepInterval(interval)),
		func() (river.JobArgs, *river.InsertOpts) {
			return BackupSweepArgs{}, nil
		},
		&river.PeriodicJobOpts{},
	)
}

func normalizeBackupSweepInterval(configured time.Duration) time.Duration {
	if configured < backupSweepMinInterval {
		return backupSweepMinInterval
	}
	return configured
}

// SetBackupSweepInterval 换一个备份周期：不重启 River 客户端，也不立即备份一次。
func (m *Manager) SetBackupSweepInterval(interval time.Duration) error {
	if m == nil || !m.backupSweepEnabled || m.backupPeriodicJobs == nil {
		return ErrBackupSweepUnavailable
	}
	m.backupPeriodicMu.Lock()
	defer m.backupPeriodicMu.Unlock()
	newHandle, err := m.backupPeriodicJobs.AddSafely(backupRuntimePeriodicJob(interval))
	if err != nil {
		return fmt.Errorf("更新备份周期任务: %w", err)
	}
	m.backupPeriodicJobs.Remove(m.backupPeriodicHandle)
	m.backupPeriodicHandle = newHandle
	m.backupPeriodicInterval = normalizeBackupSweepInterval(interval)
	return nil
}

// EnqueueBackupSweep 立即插入一轮配置备份。dir 为空时由 worker 回退到
// MOVIEPILOT_BACKUP_DIR；keep<=0 时回退到 MOVIEPILOT_BACKUP_KEEP。
func (m *Manager) EnqueueBackupSweep(ctx context.Context, dir string, keep int) error {
	if ctx == nil {
		return ErrBackupSweepExecution
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if m == nil || m.client == nil || !m.backupSweepEnabled {
		return ErrBackupSweepUnavailable
	}
	args := BackupSweepArgs{Dir: strings.TrimSpace(dir), Keep: keep}
	opts := river.InsertOpts{Queue: queueBackupSweep, MaxAttempts: 3}
	if _, err := m.client.Insert(ctx, args, &opts); err != nil {
		if contextErr := ctx.Err(); contextErr != nil {
			return contextErr
		}
		return ErrBackupSweepExecution
	}
	return nil
}
