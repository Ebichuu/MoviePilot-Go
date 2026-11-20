// Package v2import 把 MoviePilot V2（SQLite + 配置文件）的数据迁移到本项目的 PostgreSQL。
//
// 为什么需要：V2 用的数据库是单文件 `moviepilot.db`，我们换成 PostgreSQL 之后，
// 老用户没有任何"拷个文件"就完成迁移的办法——没有这个工具，等于换不过来。
//
// 设计取舍：
//   - V2 的库按**只读**打开（`mode=ro`），绝不回写；迁移失败也不会破坏原库。
//   - 每张表一个显式映射函数，不用配置驱动。V2 的字段语义（例如 `type` 存的是中文
//     "电影"/"电视剧"）必须逐个对照，配置文件反而藏坑。
//   - 目标表用 `ON CONFLICT DO NOTHING`：重复执行只补缺，不会覆盖用户现有数据。
//   - 订阅默认导入成**暂停**状态：迁移完第一次启动就按订阅批量下载是灾难性的意外。
//     想立刻恢复搜索时用 --resume。
//   - 站点（site）**不自动导入**：V2 的站点是 Python 插件实例，字段是插件自己的；
//     我们这边要求"适配器 + config"，硬映射只会生成一堆跑不起来的站点。
//     工具会列出 V2 站点清单，提示用户按适配器手工重建。
package v2import

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/moviepilot-go/moviepilot-go/internal/database"
)

// Options 是迁移参数。
type Options struct {
	// SQLitePath 是 V2 的 moviepilot.db 路径。
	SQLitePath string
	// DB 是目标 PostgreSQL；DryRun 为 true 时可以为 nil。
	DB *database.DB
	// DryRun 只读源库并输出映射结果，不写目标库，也不需要数据库连接。
	DryRun bool
	// Resume 让导入的订阅立刻参与搜索；默认导入成暂停。
	Resume bool
	// SkipBackup 跳过"导入前先备份目标库"这一步（仅限已经手工备份过的情况）。
	SkipBackup bool
	// BackupDir 是导入前备份的落盘目录，空则用当前工作目录。
	BackupDir string
}

// TableReport 是单张表的迁移结果。
type TableReport struct {
	Source  string `json:"source"`
	Target  string `json:"target"`
	Read    int    `json:"read"`
	Written int    `json:"written"`
	Skipped int    `json:"skipped"`
	Note    string `json:"note,omitempty"`
}

// Report 是整次迁移的结果。
type Report struct {
	StartedAt   time.Time     `json:"started_at"`
	FinishedAt  time.Time     `json:"finished_at"`
	DryRun      bool          `json:"dry_run"`
	Tables      []TableReport `json:"tables"`
	Warnings    []string      `json:"warnings,omitempty"`
	SitePreview []string      `json:"site_preview,omitempty"`
	BackupPath  string        `json:"backup_path,omitempty"`
}

// Written 返回实际写入的总行数。
func (r Report) Written() int {
	total := 0
	for _, table := range r.Tables {
		total += table.Written
	}
	return total
}

// Run 执行迁移。
func Run(ctx context.Context, opts Options) (Report, error) {
	report := Report{StartedAt: time.Now().UTC(), DryRun: opts.DryRun}
	if strings.TrimSpace(opts.SQLitePath) == "" {
		return report, errors.New("必须指定 V2 数据库文件路径")
	}
	if _, err := os.Stat(opts.SQLitePath); err != nil {
		return report, fmt.Errorf("打不开 V2 数据库: %w", err)
	}
	if !opts.DryRun && opts.DB == nil {
		return report, errors.New("写库迁移必须提供目标数据库连接")
	}

	// 只读打开：任何情况下都不改用户的 V2 原库（mode=ro 由 database 包保证）。
	source, err := database.OpenV2SQLite(opts.SQLitePath)
	if err != nil {
		return report, err
	}
	defer source.Close()

	if !opts.DryRun && !opts.SkipBackup {
		backupPath, err := backupBeforeImport(ctx, opts)
		if err != nil {
			return report, err
		}
		report.BackupPath = backupPath
	}

	runner := &runner{opts: opts, source: source, report: &report}
	for _, spec := range tableSpecs(opts.Resume) {
		if err := runner.runTable(ctx, spec); err != nil {
			return report, err
		}
	}
	if err := runner.previewSites(ctx); err != nil {
		report.Warnings = append(report.Warnings, "读取 V2 站点清单失败: "+err.Error())
	}
	report.FinishedAt = time.Now().UTC()
	return report, nil
}

// backupBeforeImport 在写入前先给目标库做一份整库备份。
//
// 迁移是不可逆的覆盖式写入，没有退路等于让用户赌运气；这里复用产品自带的导出。
func backupBeforeImport(ctx context.Context, opts Options) (string, error) {
	dir := strings.TrimSpace(opts.BackupDir)
	if dir == "" {
		workingDir, err := os.Getwd()
		if err != nil {
			return "", fmt.Errorf("确定备份目录: %w", err)
		}
		dir = workingDir
	}
	name := "backup-before-v2-import-" + time.Now().Format("20060102-150405") + ".tar.gz"
	fullPath := filepath.Join(dir, name)
	file, err := os.Create(fullPath)
	if err != nil {
		return "", fmt.Errorf("创建导入前备份: %w", err)
	}
	if _, err := database.BackupTo(ctx, opts.DB, file, ""); err != nil {
		file.Close()
		os.Remove(fullPath)
		return "", fmt.Errorf("导入前备份失败（未写入任何数据）: %w", err)
	}
	if err := file.Close(); err != nil {
		return "", fmt.Errorf("收尾导入前备份: %w", err)
	}
	return fullPath, nil
}

// tableSpec 描述一张表的迁移方式。
type tableSpec struct {
	source  string
	target  string
	jsonCol []string
	// 返回 nil 表示这一行不迁移（例如缺少用户名）。
	mapRow func(row map[string]any) (map[string]any, error)
	note   string
}

type runner struct {
	opts   Options
	source *database.V2SQLite
	report *Report
}

// runTable 读一张 V2 表，逐行映射并写入目标库。
func (r *runner) runTable(ctx context.Context, spec tableSpec) error {
	entry := TableReport{Source: spec.source, Target: spec.target, Note: spec.note}
	exists, err := r.source.HasTable(ctx, spec.source)
	if err != nil {
		return err
	}
	if !exists {
		entry.Note = strings.TrimSpace(entry.Note + " (源库没有这张表)")
		r.report.Tables = append(r.report.Tables, entry)
		return nil
	}
	rows, err := r.source.Rows(ctx, spec.source)
	if err != nil {
		return err
	}
	entry.Read = len(rows)
	for index, row := range rows {
		mapped, err := spec.mapRow(row)
		if err != nil {
			r.report.Warnings = append(r.report.Warnings,
				fmt.Sprintf("%s 第 %d 行映射失败，已跳过: %v", spec.source, index+1, err))
			entry.Skipped++
			continue
		}
		if mapped == nil {
			entry.Skipped++
			continue
		}
		if r.opts.DryRun {
			entry.Written++
			continue
		}
		written, err := insertRow(ctx, r.opts.DB, spec, mapped)
		if err != nil {
			return fmt.Errorf("写入 %s: %w", spec.target, err)
		}
		if written {
			entry.Written++
		} else {
			entry.Skipped++
		}
	}
	r.report.Tables = append(r.report.Tables, entry)
	return nil
}

// previewSites 列出 V2 站点，仅作提示：站点需要用户按适配器手工重建。
func (r *runner) previewSites(ctx context.Context) error {
	exists, err := r.source.HasTable(ctx, "site")
	if err != nil || !exists {
		return err
	}
	rows, err := r.source.Rows(ctx, "site")
	if err != nil {
		return err
	}
	names := make([]string, 0, len(rows))
	for _, row := range rows {
		name := stringValue(row["name"])
		domain := stringValue(row["domain"])
		if name == "" && domain == "" {
			continue
		}
		if domain != "" {
			names = append(names, name+" ("+domain+")")
			continue
		}
		names = append(names, name)
	}
	sort.Strings(names)
	r.report.SitePreview = names
	return nil
}

// tableSpecs 按外键依赖顺序排列：先用户（订阅要引用 username），再设置，再业务数据。
func tableSpecs(resume bool) []tableSpec {
	return []tableSpec{
		{source: "user", target: "users", jsonCol: []string{"permissions"}, mapRow: mapUserRow},
		{source: "systemconfig", target: "settings", jsonCol: []string{"value"}, mapRow: mapSettingRow},
		{source: "subscribe", target: "subscriptions", jsonCol: []string{"config"},
			mapRow: func(row map[string]any) (map[string]any, error) {
				return buildSubscriptionRow(row, resume)
			}},
		{source: "siteuserdata", target: "site_userdata", jsonCol: []string{"seeding_info", "message_unread_contents"}, mapRow: mapSiteUserdataRow},
		{source: "sitestatistic", target: "site_statistics", jsonCol: []string{"note"}, mapRow: mapSiteStatisticRow},
	}
}

// mapUserRow 映射 V2 `user` → `users`。
//
// V2 的密码是 passlib bcrypt 摘要（$2b$ 前缀），与 Go bcrypt 互相兼容，所以直接搬过来，
// 用户迁移后仍然用原来的密码登录。
func mapUserRow(row map[string]any) (map[string]any, error) {
	username := strings.TrimSpace(stringValue(row["name"]))
	hash := strings.TrimSpace(stringValue(row["hashed_password"]))
	if username == "" || hash == "" {
		return nil, nil
	}
	return map[string]any{
		"username":      username,
		"password_hash": hash,
		"is_superuser":  boolValue(row["is_superuser"]),
		"avatar":        nullableString(row["avatar"]),
		"permissions":   jsonValue(row["permissions"], map[string]any{}),
		"created_at":    time.Now().UTC(),
		"updated_at":    time.Now().UTC(),
	}, nil
}

// mapSettingRow 映射 V2 `systemconfig` → `settings`。
func mapSettingRow(row map[string]any) (map[string]any, error) {
	key := strings.TrimSpace(stringValue(row["key"]))
	if key == "" {
		return nil, nil
	}
	return map[string]any{
		"key":        key,
		"value":      jsonValue(row["value"], nil),
		"encrypted":  false,
		"updated_at": time.Now().UTC(),
	}, nil
}

// buildSubscriptionRow 把一行 V2 订阅转成目标行；resume 决定导入后的状态。
func buildSubscriptionRow(row map[string]any, resume bool) (map[string]any, error) {
	title := strings.TrimSpace(stringValue(row["name"]))
	if title == "" {
		return nil, nil
	}
	mediaType := mediaTypeFromV2(stringValue(row["type"]))
	if mediaType == "" {
		return nil, fmt.Errorf("未知媒体类型 %q", stringValue(row["type"]))
	}
	source, mediaID := mediaIdentity(row)

	internalState := "paused"
	if resume {
		internalState = "pending_search"
	}
	season := intValue(row["season"])
	config := buildSubscriptionConfig(row)

	// username / v2_state 在目标表里是 NOT NULL：V2 的空值语义用空字符串表示，
	// 不能写成 NULL，否则整行插不进去。
	return map[string]any{
		"username":       strings.TrimSpace(stringValue(row["username"])),
		"media_type":     mediaType,
		"title":          title,
		"media_source":   nullableString(source),
		"media_id":       nullableString(mediaID),
		"year":           nullableString(row["year"]),
		"season":         nullableInt(season),
		"internal_state": internalState,
		"v2_state":       strings.TrimSpace(stringValue(row["state"])),
		"config":         config,
		"created_at":     time.Now().UTC(),
		"updated_at":     time.Now().UTC(),
	}, nil
}

// buildSubscriptionConfig 保留引擎认识的字段，其余 V2 原始字段收进 v2 子对象。
//
// 这样做的好处：认识的（总集数、缺失集数、洗版、优先级）直接生效；
// 不认识的（筛选规则、站点列表、保存路径）不丢，但也不会被误当成已生效的规则。
func buildSubscriptionConfig(row map[string]any) map[string]any {
	config := map[string]any{}
	if value := intValue(row["total_episode"]); value > 0 {
		config["total_episode"] = value
	}
	if value := intValue(row["lack_episode"]); value > 0 {
		config["lack_episode"] = value
	}
	if value := intValue(row["best_version"]); value > 0 {
		config["best_version"] = value
	}
	if value := intValue(row["current_priority"]); value > 0 {
		config["current_priority"] = value
	}
	legacy := map[string]any{}
	for _, key := range []string{
		"filter", "include", "exclude", "quality", "resolution", "effect",
		"sites", "downloader", "save_path", "episode_group", "custom_words", "note", "date",
		"keyword", "poster", "backdrop", "vote", "description", "episode_priority", "filter_groups",
	} {
		value := row[key]
		if value == nil {
			continue
		}
		if text := stringValue(value); text != "" {
			legacy[key] = text
			continue
		}
		legacy[key] = value
	}
	if len(legacy) > 0 {
		config["v2"] = legacy
	}
	return config
}

// mediaTypeFromV2 把 V2 的中文类型转成本项目的 media_type。
func mediaTypeFromV2(value string) string {
	switch strings.TrimSpace(value) {
	case "电影":
		return "movie"
	case "电视剧":
		return "tv"
	default:
		return ""
	}
}

// mediaIdentity 推断订阅的媒体源与 ID。
//
// V2 有两套标识：早期是 tmdbid/doubanid/bangumiid/anilistid 多列，后来统一成
// media_source + media_id。新字段优先，缺了再按老列推断。
func mediaIdentity(row map[string]any) (string, string) {
	source := strings.ToLower(strings.TrimSpace(stringValue(row["media_source"])))
	mediaID := strings.TrimSpace(stringValue(row["media_id"]))
	if source != "" && mediaID != "" {
		return normalizeMediaSource(source), mediaID
	}
	if value := intValue(row["tmdbid"]); value > 0 {
		return "tmdb", strconv.FormatInt(value, 10)
	}
	if value := strings.TrimSpace(stringValue(row["doubanid"])); value != "" {
		return "douban", value
	}
	if value := intValue(row["bangumiid"]); value > 0 {
		return "bangumi", strconv.FormatInt(value, 10)
	}
	if value := intValue(row["anilistid"]); value > 0 {
		return "anilist", strconv.FormatInt(value, 10)
	}
	return "", ""
}

// normalizeMediaSource 统一媒体源命名（V2 里出现过 tmdb/themoviedb 混用）。
func normalizeMediaSource(value string) string {
	switch value {
	case "themoviedb":
		return "tmdb"
	default:
		return value
	}
}

// mapSiteUserdataRow 映射 V2 `siteuserdata` → `site_userdata`（列名基本一致）。
func mapSiteUserdataRow(row map[string]any) (map[string]any, error) {
	domain := strings.TrimSpace(stringValue(row["domain"]))
	if domain == "" {
		return nil, nil
	}
	name := strings.TrimSpace(stringValue(row["name"]))
	if name == "" {
		// 我们的 name 是 NOT NULL；V2 允许留空，用域名兜底比写 NULL 实在。
		name = domain
	}
	// updated_day / updated_time 也是 NOT NULL：V2 插入时会给默认当天时间，
	// 但老数据里可能是空的，这里补成"现在"，统计页面才不会显示空白。
	updatedDay := strings.TrimSpace(stringValue(row["updated_day"]))
	if updatedDay == "" {
		updatedDay = time.Now().Format("2006-01-02")
	}
	updatedTime := strings.TrimSpace(stringValue(row["updated_time"]))
	if updatedTime == "" {
		updatedTime = time.Now().Format("15:04:05")
	}
	target := map[string]any{
		"domain": domain,
		"name":   name,
		// site_userdata 没有自然主键（domain+时间可以重复），保留 V2 的 id 才能让
		// 重复执行变成"命中主键冲突被跳过"，而不是每次都插一条新记录。
		"username":                nullableString(row["username"]),
		"userid":                  nullableString(row["userid"]),
		"user_level":              nullableString(row["user_level"]),
		"join_at":                 nullableString(row["join_at"]),
		"bonus":                   floatValue(row["bonus"]),
		"upload":                  floatValue(row["upload"]),
		"download":                floatValue(row["download"]),
		"ratio":                   floatValue(row["ratio"]),
		"seeding":                 floatValue(row["seeding"]),
		"leeching":                floatValue(row["leeching"]),
		"seeding_size":            floatValue(row["seeding_size"]),
		"leeching_size":           floatValue(row["leeching_size"]),
		"seeding_info":            jsonValue(row["seeding_info"], map[string]any{}),
		"message_unread":          intValue(row["message_unread"]),
		"message_unread_contents": jsonValue(row["message_unread_contents"], []any{}),
		"err_msg":                 nullableString(row["err_msg"]),
		"updated_day":             updatedDay,
		"updated_time":            updatedTime,
	}
	if id := intValue(row["id"]); id > 0 {
		target["id"] = id
	}
	return target, nil
}

// mapSiteStatisticRow 映射 V2 `sitestatistic` → `site_statistics`。
func mapSiteStatisticRow(row map[string]any) (map[string]any, error) {
	domain := strings.TrimSpace(stringValue(row["domain"]))
	if domain == "" {
		return nil, nil
	}
	lstModDate := strings.TrimSpace(stringValue(row["lst_mod_date"]))
	if lstModDate == "" {
		lstModDate = time.Now().Format("2006-01-02 15:04:05")
	}
	return map[string]any{
		"domain":       domain,
		"success":      intValue(row["success"]),
		"fail":         intValue(row["fail"]),
		"seconds":      intValue(row["seconds"]),
		"lst_state":    intValue(row["lst_state"]),
		"lst_mod_date": lstModDate,
		"note":         jsonValue(row["note"], map[string]any{}),
	}, nil
}

// insertRow 写入一行；返回 false 表示被唯一约束挡下（已存在，跳过）。
func insertRow(ctx context.Context, db *database.DB, spec tableSpec, row map[string]any) (bool, error) {
	columns := make([]string, 0, len(row))
	for column := range row {
		columns = append(columns, column)
	}
	sort.Strings(columns)

	jsonColumns := make(map[string]bool, len(spec.jsonCol))
	for _, column := range spec.jsonCol {
		jsonColumns[column] = true
	}

	placeholders := make([]string, len(columns))
	args := make([]any, len(columns))
	for index, column := range columns {
		placeholders[index] = "$" + strconv.Itoa(index+1)
		if jsonColumns[column] {
			payload, err := json.Marshal(row[column])
			if err != nil {
				return false, fmt.Errorf("编码 %s.%s: %w", spec.target, column, err)
			}
			args[index] = string(payload)
			placeholders[index] += "::jsonb"
			continue
		}
		args[index] = row[column]
	}
	sqlText := fmt.Sprintf(
		"INSERT INTO %s (%s) VALUES (%s) ON CONFLICT DO NOTHING",
		spec.target, strings.Join(columns, ", "), strings.Join(placeholders, ", "),
	)
	tag, err := db.Pool.Exec(ctx, sqlText, args...)
	if err != nil {
		return false, err
	}
	return tag.RowsAffected() > 0, nil
}

func stringValue(value any) string {
	switch typed := value.(type) {
	case nil:
		return ""
	case string:
		return typed
	case []byte:
		return string(typed)
	case int64:
		return strconv.FormatInt(typed, 10)
	case float64:
		return strconv.FormatFloat(typed, 'f', -1, 64)
	case bool:
		if typed {
			return "true"
		}
		return "false"
	default:
		return fmt.Sprint(typed)
	}
}

// nullableString 空字符串转成 NULL，避免把 "" 写进可空的文本列。
func nullableString(value any) any {
	text := strings.TrimSpace(stringValue(value))
	if text == "" {
		return nil
	}
	return text
}

func intValue(value any) int64 {
	switch typed := value.(type) {
	case int64:
		return typed
	case int:
		return int64(typed)
	case float64:
		return int64(typed)
	case bool:
		if typed {
			return 1
		}
		return 0
	case string:
		trimmed := strings.TrimSpace(typed)
		if trimmed == "" {
			return 0
		}
		parsed, err := strconv.ParseInt(trimmed, 10, 64)
		if err != nil {
			parsedFloat, floatErr := strconv.ParseFloat(trimmed, 64)
			if floatErr != nil {
				return 0
			}
			return int64(parsedFloat)
		}
		return parsed
	default:
		return 0
	}
}

func nullableInt(value int64) any {
	if value == 0 {
		return nil
	}
	return value
}

func floatValue(value any) float64 {
	switch typed := value.(type) {
	case float64:
		return typed
	case int64:
		return float64(typed)
	case string:
		parsed, err := strconv.ParseFloat(strings.TrimSpace(typed), 64)
		if err != nil {
			return 0
		}
		return parsed
	case nil:
		return 0
	default:
		return 0
	}
}

func boolValue(value any) bool {
	switch typed := value.(type) {
	case bool:
		return typed
	case int64:
		return typed != 0
	case string:
		trimmed := strings.ToLower(strings.TrimSpace(typed))
		return trimmed == "1" || trimmed == "true" || trimmed == "yes"
	default:
		return false
	}
}

// jsonValue 把 V2 的 JSON 文本解码成可写进 jsonb 的值。
func jsonValue(value any, fallback any) any {
	switch typed := value.(type) {
	case nil:
		return fallback
	case string:
		trimmed := strings.TrimSpace(typed)
		if trimmed == "" {
			return fallback
		}
		var decoded any
		if err := json.Unmarshal([]byte(trimmed), &decoded); err != nil {
			// 不是合法 JSON（V2 里也有存纯文本的情况），按字符串存。
			return map[string]any{"raw": trimmed}
		}
		return decoded
	default:
		return value
	}
}
