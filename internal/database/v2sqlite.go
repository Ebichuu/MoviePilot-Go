package database

import (
	"context"
	"database/sql"
	"fmt"
	"regexp"
	"strings"
	"time"

	_ "modernc.org/sqlite" // 纯 Go 的 SQLite 驱动：不引 cgo，容器里也能跑
)

// V2SQLite 是 V2（MoviePilot 原版）SQLite 数据库的只读句柄。
//
// 放在 database 包里是因为本项目的分层规则要求 `database/sql` 只能出现在
// `internal/database`（见 internal/architecture）；迁移逻辑本身在 internal/v2import。
//
// 一律以 `mode=ro&immutable=1` 打开：迁移无论成功失败都不能改动 V2 的原库。
type V2SQLite struct {
	handle *sql.DB
}

// OpenV2SQLite 以只读方式打开 V2 的 `moviepilot.db`。
func OpenV2SQLite(path string) (*V2SQLite, error) {
	trimmed := strings.TrimSpace(path)
	if trimmed == "" {
		return nil, fmt.Errorf("V2 数据库路径为空")
	}
	dsn := "file:" + strings.ReplaceAll(trimmed, `\`, "/") + "?mode=ro&immutable=1"
	handle, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("打开 V2 数据库: %w", err)
	}
	handle.SetMaxOpenConns(1)
	if err := handle.Ping(); err != nil {
		handle.Close()
		return nil, fmt.Errorf("读取 V2 数据库: %w", err)
	}
	return &V2SQLite{handle: handle}, nil
}

// Close 释放文件句柄。
func (v *V2SQLite) Close() error {
	if v == nil || v.handle == nil {
		return nil
	}
	return v.handle.Close()
}

// HasTable 判断 V2 库里有没有这张表：不同 V2 版本的表集合并不完全一致。
func (v *V2SQLite) HasTable(ctx context.Context, table string) (bool, error) {
	var count int
	err := v.handle.QueryRowContext(ctx,
		`SELECT count(*) FROM sqlite_master WHERE type = 'table' AND name = ?`, table).Scan(&count)
	if err != nil {
		return false, fmt.Errorf("查询 V2 表 %s: %w", table, err)
	}
	return count > 0, nil
}

// Rows 把整张表读成 []map，值统一成 string/int64/float64/bool/nil。
func (v *V2SQLite) Rows(ctx context.Context, table string) ([]map[string]any, error) {
	if !v2TableNamePattern.MatchString(table) {
		return nil, fmt.Errorf("非法的 V2 表名 %q", table)
	}
	rows, err := v.handle.QueryContext(ctx, `SELECT * FROM "`+table+`"`)
	if err != nil {
		return nil, fmt.Errorf("读取 V2 表 %s: %w", table, err)
	}
	defer rows.Close()
	columns, err := rows.Columns()
	if err != nil {
		return nil, fmt.Errorf("读取 V2 表 %s 的列: %w", table, err)
	}
	var result []map[string]any
	for rows.Next() {
		values := make([]any, len(columns))
		pointers := make([]any, len(columns))
		for index := range values {
			pointers[index] = &values[index]
		}
		if err := rows.Scan(pointers...); err != nil {
			return nil, fmt.Errorf("读取 V2 表 %s 的行: %w", table, err)
		}
		row := make(map[string]any, len(columns))
		for index, name := range columns {
			row[name] = normalizeV2SQLiteValue(values[index])
		}
		result = append(result, row)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("遍历 V2 表 %s: %w", table, err)
	}
	return result, nil
}

// v2TableNamePattern 限制读表名的字符集，避免把奇怪名字拼进 SQL。
var v2TableNamePattern = regexp.MustCompile(`^[a-z0-9_]+$`)

// normalizeV2SQLiteValue 把驱动返回值统一成可移植的类型。
func normalizeV2SQLiteValue(value any) any {
	switch typed := value.(type) {
	case []byte:
		return string(typed)
	case time.Time:
		return typed.Format(time.RFC3339)
	case int64:
		return typed
	case float64:
		return typed
	case bool:
		return typed
	case nil:
		return nil
	default:
		return fmt.Sprint(typed)
	}
}
