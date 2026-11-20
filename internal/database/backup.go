package database

import (
	"archive/tar"
	"compress/gzip"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
)

// 备份/恢复：把全部业务表导出成 tar.gz（每张表一个 CSV，另加 manifest.json）。
//
// 为什么不用 pg_dump：这是一个要跑在容器里的产品，镜像里不该再塞一套 PostgreSQL
// 客户端；而且恢复必须和本程序自己的迁移版本绑定，交给外部工具反而更难保证一致。
// 所以这里只用 pgx 的 COPY TO / COPY FROM：
//   - 导出：每张表 COPY (SELECT * FROM t) TO STDOUT WITH (FORMAT csv, HEADER)
//   - 恢复：按外键依赖顺序清空，再逐表 COPY FROM，最后修正自增序列
//
// 已知边界：
//   - 只备份数据，不备份表结构。恢复前目标库必须已经由本版本迁移好——结构归 goose
//     迁移管，备份里再存一份结构只会在升级后互相打架。
//   - 大表先落临时文件再进 tar：内存占用与表大小无关，但需要磁盘余量。
//   - 恢复在单个事务里完成，中途失败整体回滚，不会留下"清空了却没写回去"的库。

const backupManifestName = "manifest.json"

// 表名只允许小写字母、数字和下划线：拼 SQL 前先过滤，避免标识符注入。
var backupIdentPattern = regexp.MustCompile(`^[a-z0-9_]+$`)

// BackupManifest 是备份包的元信息。
type BackupManifest struct {
	Kind          string    `json:"kind"`
	CreatedAt     time.Time `json:"created_at"`
	SchemaVersion int64     `json:"schema_version"`
	AppVersion    string    `json:"app_version"`
	Tables        []string  `json:"tables"`
}

// BackupTo 把数据库导出成 tar.gz 写入 w，返回写入的清单。
func BackupTo(ctx context.Context, db *DB, w io.Writer, appVersion string) (BackupManifest, error) {
	manifest := BackupManifest{
		Kind:       "moviepilot-go-backup",
		CreatedAt:  time.Now().UTC(),
		AppVersion: appVersion,
	}
	if db == nil || db.Pool == nil {
		return manifest, errors.New("数据库未初始化")
	}

	tables, err := listBackupTables(ctx, db)
	if err != nil {
		return manifest, err
	}
	if len(tables) == 0 {
		return manifest, errors.New("数据库里没有可备份的表")
	}
	// 临时目录先探测好：一旦开始写 gzip 流就没法再改状态码了，
	// 目录不可写必须在发响应头之前就失败。
	tempDir, err := BackupTempDir()
	if err != nil {
		return manifest, err
	}
	manifest.Tables = tables
	manifest.SchemaVersion, _ = currentSchemaVersion(ctx, db)

	gzipWriter := gzip.NewWriter(w)
	tarWriter := tar.NewWriter(gzipWriter)

	manifestBytes, err := json.MarshalIndent(manifest, "", "  ")
	if err != nil {
		return manifest, fmt.Errorf("编码备份清单: %w", err)
	}
	if err := writeTarBytes(tarWriter, backupManifestName, manifestBytes); err != nil {
		return manifest, err
	}

	conn, err := db.Pool.Acquire(ctx)
	if err != nil {
		return manifest, fmt.Errorf("获取备份连接: %w", err)
	}
	defer conn.Release()

	for _, table := range tables {
		if err := copyTableToTar(ctx, conn.Conn(), tarWriter, table, tempDir); err != nil {
			return manifest, err
		}
	}
	if err := tarWriter.Close(); err != nil {
		return manifest, fmt.Errorf("收尾 tar: %w", err)
	}
	if err := gzipWriter.Close(); err != nil {
		return manifest, fmt.Errorf("收尾 gzip: %w", err)
	}
	return manifest, nil
}

// RestoreFrom 从 tar.gz 恢复数据，返回包内清单。
func RestoreFrom(ctx context.Context, db *DB, r io.Reader) (BackupManifest, error) {
	var manifest BackupManifest
	if db == nil || db.Pool == nil {
		return manifest, errors.New("数据库未初始化")
	}

	archive, err := readBackupArchive(r)
	if err != nil {
		return manifest, err
	}
	defer archive.cleanup()
	if len(archive.files) == 0 {
		return manifest, errors.New("备份包里没有数据表")
	}
	rawManifest, err := os.ReadFile(archive.manifestPath)
	if err != nil {
		return manifest, fmt.Errorf("读取备份清单: %w", err)
	}
	if err := json.Unmarshal(rawManifest, &manifest); err != nil {
		return manifest, fmt.Errorf("备份清单损坏: %w", err)
	}

	// 只恢复"包里也有、当前库里也存在"的表，避免被篡改的包写进任意表。
	existing, err := listBackupTables(ctx, db)
	if err != nil {
		return manifest, err
	}
	existingSet := make(map[string]struct{}, len(existing))
	for _, name := range existing {
		existingSet[name] = struct{}{}
	}
	restoreTables := make([]string, 0, len(archive.tables))
	for _, name := range archive.tables {
		if _, ok := existingSet[name]; ok {
			restoreTables = append(restoreTables, name)
		}
	}
	if len(restoreTables) == 0 {
		return manifest, errors.New("备份包里的表与当前库结构不匹配，请确认版本")
	}
	ordered, err := orderTablesByDependency(ctx, db, restoreTables)
	if err != nil {
		return manifest, err
	}

	tx, err := db.Pool.Begin(ctx)
	if err != nil {
		return manifest, fmt.Errorf("开启恢复事务: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	quoted := make([]string, len(restoreTables))
	for i, table := range restoreTables {
		quoted[i] = quoteBackupIdent(table)
	}
	if _, err := tx.Exec(ctx, "TRUNCATE TABLE "+strings.Join(quoted, ", ")+" RESTART IDENTITY CASCADE"); err != nil {
		return manifest, fmt.Errorf("清空目标表: %w", err)
	}

	for _, table := range ordered {
		file, err := os.Open(archive.files[table])
		if err != nil {
			return manifest, fmt.Errorf("打开表 %s 的备份数据: %w", table, err)
		}
		sql := "COPY " + quoteBackupIdent(table) + " FROM STDIN WITH (FORMAT csv, HEADER)"
		_, copyErr := tx.Conn().PgConn().CopyFrom(ctx, file, sql)
		file.Close()
		if copyErr != nil {
			return manifest, fmt.Errorf("恢复表 %s: %w", table, copyErr)
		}
	}
	if err := resetSerialSequences(ctx, tx); err != nil {
		return manifest, err
	}
	if err := tx.Commit(ctx); err != nil {
		return manifest, fmt.Errorf("提交恢复事务: %w", err)
	}
	return manifest, nil
}

// listBackupTables 返回 public 下需要备份的普通表（按名字排序，包内容稳定）。
func listBackupTables(ctx context.Context, db *DB) ([]string, error) {
	const query = `
		SELECT table_name FROM information_schema.tables
		WHERE table_schema = 'public' AND table_type = 'BASE TABLE'
		ORDER BY table_name`
	rows, err := db.Pool.Query(ctx, query)
	if err != nil {
		return nil, fmt.Errorf("列出数据表: %w", err)
	}
	defer rows.Close()
	var tables []string
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			return nil, fmt.Errorf("读取表名: %w", err)
		}
		if backupIdentPattern.MatchString(name) && !isRuntimeQueueTable(name) {
			tables = append(tables, name)
		}
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("遍历数据表: %w", err)
	}
	return tables, nil
}

// currentSchemaVersion 读 goose 的迁移版本，标记"这份备份来自哪个结构版本"。
func currentSchemaVersion(ctx context.Context, db *DB) (int64, error) {
	var version int64
	err := db.Pool.QueryRow(ctx, `SELECT COALESCE(MAX(version_id), 0) FROM goose_db_version`).Scan(&version)
	if err != nil {
		return 0, err
	}
	return version, nil
}

// copyTableToTar 把一张表 COPY 到临时文件，再带长度写进 tar。
//
// tar 头必须提前知道条目大小，所以不能把 COPY 的流直接塞进 tar；先落盘再拷贝，
// 内存占用与表大小无关。
func copyTableToTar(ctx context.Context, conn *pgx.Conn, tarWriter *tar.Writer, table, tempDir string) error {
	tempFile, err := os.CreateTemp(tempDir, "moviepilot-backup-*.csv")
	if err != nil {
		return fmt.Errorf("创建备份临时文件: %w", err)
	}
	defer func() {
		tempFile.Close()
		os.Remove(tempFile.Name())
	}()

	sql := "COPY (SELECT * FROM " + quoteBackupIdent(table) + ") TO STDOUT WITH (FORMAT csv, HEADER)"
	if _, err := conn.PgConn().CopyTo(ctx, tempFile, sql); err != nil {
		return fmt.Errorf("导出表 %s: %w", table, err)
	}
	info, err := tempFile.Stat()
	if err != nil {
		return fmt.Errorf("读取表 %s 的导出大小: %w", table, err)
	}
	if _, err := tempFile.Seek(0, io.SeekStart); err != nil {
		return fmt.Errorf("回绕表 %s 的导出文件: %w", table, err)
	}
	header := &tar.Header{
		Name:    table + ".csv",
		Mode:    0o600,
		Size:    info.Size(),
		ModTime: time.Now().UTC(),
	}
	if err := tarWriter.WriteHeader(header); err != nil {
		return fmt.Errorf("写入表 %s 的归档头: %w", table, err)
	}
	if _, err := io.Copy(tarWriter, tempFile); err != nil {
		return fmt.Errorf("写入表 %s 的归档内容: %w", table, err)
	}
	return nil
}

// writeTarBytes 写入一个内存中的 tar 条目。
func writeTarBytes(tarWriter *tar.Writer, name string, payload []byte) error {
	header := &tar.Header{
		Name:    path.Base(name),
		Mode:    0o600,
		Size:    int64(len(payload)),
		ModTime: time.Now().UTC(),
	}
	if err := tarWriter.WriteHeader(header); err != nil {
		return fmt.Errorf("写入 %s 头: %w", name, err)
	}
	if _, err := tarWriter.Write(payload); err != nil {
		return fmt.Errorf("写入 %s 内容: %w", name, err)
	}
	return nil
}

// backupArchive 是解包后的中间状态：清单落在临时文件，各表 CSV 落在临时文件。
type backupArchive struct {
	manifestPath string
	tables       []string
	files        map[string]string
	cleanup      func()
}

// readBackupArchive 解 tar.gz 到临时目录，返回清单路径与各表文件路径。
func readBackupArchive(r io.Reader) (*backupArchive, error) {
	// 和导出用同一套可写目录探测：Windows 上 %TEMP% 可能被 ACL 拒绝。
	baseDir, err := BackupTempDir()
	if err != nil {
		return nil, err
	}
	tempDir, err := os.MkdirTemp(baseDir, "moviepilot-restore-*")
	if err != nil {
		return nil, fmt.Errorf("创建恢复临时目录: %w", err)
	}
	archive := &backupArchive{
		files: make(map[string]string),
		cleanup: func() {
			os.RemoveAll(tempDir)
		},
	}

	gzipReader, err := gzip.NewReader(r)
	if err != nil {
		archive.cleanup()
		return nil, fmt.Errorf("备份包不是有效的 gzip: %w", err)
	}
	defer gzipReader.Close()
	tarReader := tar.NewReader(gzipReader)
	for {
		header, err := tarReader.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			archive.cleanup()
			return nil, fmt.Errorf("读取备份包: %w", err)
		}
		name := path.Base(header.Name)
		if header.Typeflag != tar.TypeReg || name == "." || name == "/" {
			continue
		}
		target := path.Join(tempDir, name)
		file, err := os.Create(target)
		if err != nil {
			archive.cleanup()
			return nil, fmt.Errorf("写出备份条目 %s: %w", name, err)
		}
		if _, err := io.Copy(file, io.LimitReader(tarReader, 1<<28)); err != nil {
			file.Close()
			archive.cleanup()
			return nil, fmt.Errorf("写出备份条目 %s: %w", name, err)
		}
		file.Close()

		if name == backupManifestName {
			archive.manifestPath = target
			continue
		}
		if !strings.HasSuffix(name, ".csv") {
			continue
		}
		table := strings.TrimSuffix(name, ".csv")
		if !backupIdentPattern.MatchString(table) {
			continue
		}
		archive.tables = append(archive.tables, table)
		archive.files[table] = target
	}
	if archive.manifestPath == "" {
		archive.cleanup()
		return nil, errors.New("备份包缺少 manifest.json")
	}
	return archive, nil
}

// orderTablesByDependency 按外键依赖排序：被引用的表先恢复，避免 COPY 时外键报错。
// 有环（互相引用）时把剩余表按名字追加，交给 TRUNCATE CASCADE + 事务兜底。
func orderTablesByDependency(ctx context.Context, db *DB, tables []string) ([]string, error) {
	inSet := make(map[string]bool, len(tables))
	for _, name := range tables {
		inSet[name] = true
	}
	const query = `
		SELECT conrelid::regclass::text AS child, confrelid::regclass::text AS parent
		FROM pg_constraint
		WHERE contype = 'f' AND connamespace = 'public'::regnamespace`
	rows, err := db.Pool.Query(ctx, query)
	if err != nil {
		return nil, fmt.Errorf("读取外键依赖: %w", err)
	}
	defer rows.Close()

	parents := make(map[string][]string, len(tables)) // child -> 依赖的父表
	for rows.Next() {
		var child, parent string
		if err := rows.Scan(&child, &parent); err != nil {
			return nil, fmt.Errorf("读取外键依赖: %w", err)
		}
		child = strings.Trim(splitRegclass(child), `"`)
		parent = strings.Trim(splitRegclass(parent), `"`)
		if inSet[child] && inSet[parent] && child != parent {
			parents[child] = append(parents[child], parent)
		}
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("遍历外键依赖: %w", err)
	}

	pending := make(map[string][]string, len(parents))
	indegree := make(map[string]int, len(tables))
	for _, name := range tables {
		indegree[name] = 0
	}
	for child, deps := range parents {
		pending[child] = append([]string(nil), deps...)
		indegree[child] = len(deps)
	}

	remaining := append([]string(nil), tables...)
	sort.Strings(remaining)
	ordered := make([]string, 0, len(tables))
	for len(remaining) > 0 {
		progress := false
		next := make([]string, 0, len(remaining))
		for _, name := range remaining {
			if indegree[name] == 0 {
				ordered = append(ordered, name)
				progress = true
				for child, deps := range pending {
					for _, dep := range deps {
						if dep == name {
							indegree[child]--
						}
					}
				}
				continue
			}
			next = append(next, name)
		}
		remaining = next
		if !progress {
			// 存在环：剩余表按名字追加，交给事务与外键检查兜底。
			ordered = append(ordered, remaining...)
			break
		}
	}
	return ordered, nil
}

// splitRegclass 去掉 regclass 文本里的 schema 前缀。
func splitRegclass(value string) string {
	value = strings.TrimSpace(value)
	if index := strings.LastIndex(value, "."); index >= 0 {
		return value[index+1:]
	}
	return value
}

// resetSerialSequences 修正自增序列，避免恢复后新插入与已有 id 撞车。
func resetSerialSequences(ctx context.Context, tx pgx.Tx) error {
	const query = `
		SELECT table_name, column_name FROM information_schema.columns
		WHERE table_schema = 'public' AND column_default LIKE 'nextval(%'`
	rows, err := tx.Query(ctx, query)
	if err != nil {
		return fmt.Errorf("读取自增列: %w", err)
	}
	type serialColumn struct{ table, column string }
	var columns []serialColumn
	for rows.Next() {
		var table, column string
		if err := rows.Scan(&table, &column); err != nil {
			rows.Close()
			return fmt.Errorf("读取自增列: %w", err)
		}
		if backupIdentPattern.MatchString(table) && backupIdentPattern.MatchString(column) {
			columns = append(columns, serialColumn{table: table, column: column})
		}
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return fmt.Errorf("遍历自增列: %w", err)
	}

	for _, entry := range columns {
		sql := fmt.Sprintf(
			"SELECT setval(pg_get_serial_sequence('%s', '%s'), COALESCE((SELECT MAX(%s) FROM %s), 0) + 1, false)",
			"public."+quoteBackupIdent(entry.table), entry.column,
			quoteBackupIdent(entry.column), quoteBackupIdent(entry.table),
		)
		if _, err := tx.Exec(ctx, sql); err != nil {
			// 没有序列（比如视图或无归属的自增列）不应让整个恢复失败。
			continue
		}
	}
	return nil
}

// quoteBackupIdent 给标识符加双引号（名字已通过白名单校验）。
func quoteBackupIdent(name string) string {
	return `"` + name + `"`
}

// isRuntimeQueueTable 判断是否是任务队列的运行时表。
//
// river_* 存的是"当前正在排队/正在跑"的任务状态，不属于业务数据：备份它没有意义，
// 恢复它反而可能把几天前的旧任务重新放出来跑一遍。所以导出时跳过。
func isRuntimeQueueTable(name string) bool {
	return strings.HasPrefix(name, "river_")
}

// backupTempEnv 指定备份临时文件目录；容器里 /tmp 可能被挂成只读或 tmpfs 很小，
// 这时用这个环境变量指到有空间的地方。
const backupTempEnv = "MOVIEPILOT_BACKUP_TEMP_DIR"

// BackupTempDir 探测一个可写的备份临时目录。
//
// 顺序：MOVIEPILOT_BACKUP_TEMP_DIR → os.TempDir() → 工作目录下 .backup-tmp。
// 每个候选都真写一个探针文件再删掉，因为"目录存在"不等于"进程能写"——
// 实测 Windows 上 %TEMP% 存在但被 ACL 拒绝，只看 Stat 会漏掉。
func BackupTempDir() (string, error) {
	candidates := make([]string, 0, 3)
	if dir := strings.TrimSpace(os.Getenv(backupTempEnv)); dir != "" {
		candidates = append(candidates, dir)
	}
	if dir := os.TempDir(); dir != "" {
		candidates = append(candidates, dir)
	}
	if workingDir, err := os.Getwd(); err == nil {
		candidates = append(candidates, filepath.Join(workingDir, ".backup-tmp"))
	}

	var failures []string
	for _, dir := range candidates {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			failures = append(failures, dir+"："+err.Error())
			continue
		}
		probe, err := os.CreateTemp(dir, "moviepilot-backup-probe-*")
		if err != nil {
			failures = append(failures, dir+"："+err.Error())
			continue
		}
		probe.Close()
		os.Remove(probe.Name())
		return dir, nil
	}
	return "", fmt.Errorf("没有可写的备份临时目录（%s）", strings.Join(failures, "；"))
}
