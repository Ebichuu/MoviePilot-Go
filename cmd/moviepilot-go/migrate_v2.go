package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/moviepilot-go/moviepilot-go/internal/app"
	"github.com/moviepilot-go/moviepilot-go/internal/config"
	"github.com/moviepilot-go/moviepilot-go/internal/database"
	"github.com/moviepilot-go/moviepilot-go/internal/v2import"
)

// runMigrateV2 处理 `migrate-v2` 子命令：把 V2 的 SQLite 数据导入当前 PostgreSQL。
//
// 用法：
//
//	moviepilot-go migrate-v2 --db /path/to/moviepilot.db [--dry-run] [--resume]
//
// 写库前会先给目标库做一份整库备份（--skip-backup 可跳过），失败不会留下半截数据。
func runMigrateV2(ctx context.Context, cfg config.Config, logger *slog.Logger, args []string) int {
	flags := flag.NewFlagSet("migrate-v2", flag.ContinueOnError)
	dbPath := flags.String("db", "", "V2 的 moviepilot.db 路径（必填）")
	dryRun := flags.Bool("dry-run", false, "只读源库并输出映射结果，不写目标库")
	resume := flags.Bool("resume", false, "导入的订阅立即参与搜索（默认导入成暂停，避免迁移后立刻批量下载）")
	skipBackup := flags.Bool("skip-backup", false, "跳过写库前的整库备份（已手工备份时使用）")
	backupDir := flags.String("backup-dir", "", "导入前备份的落盘目录，默认当前目录")
	asJSON := flags.Bool("json", false, "以 JSON 输出迁移报告")
	if err := flags.Parse(args); err != nil {
		return 2
	}
	if strings.TrimSpace(*dbPath) == "" {
		logger.Error("缺少 --db 参数", "hint", "migrate-v2 --db /path/to/moviepilot.db")
		return 2
	}

	var target *database.DB
	if !*dryRun {
		// 目标库先迁移到最新结构，导入工具不负责建表。
		if err := app.Migrate(ctx, cfg); err != nil {
			logger.Error("数据库迁移失败", "error", err)
			return 1
		}
		opened, err := database.Open(ctx, cfg)
		if err != nil {
			logger.Error("连接目标数据库失败", "error", err)
			return 1
		}
		defer opened.Close()
		target = opened
	}

	report, err := v2import.Run(ctx, v2import.Options{
		SQLitePath: strings.TrimSpace(*dbPath),
		DB:         target,
		DryRun:     *dryRun,
		Resume:     *resume,
		SkipBackup: *skipBackup,
		BackupDir:  *backupDir,
	})
	if err != nil {
		logger.Error("V2 数据迁移失败", "error", err)
		return 1
	}
	if *asJSON {
		payload, marshalErr := json.MarshalIndent(report, "", "  ")
		if marshalErr != nil {
			logger.Error("编码迁移报告失败", "error", marshalErr)
			return 1
		}
		fmt.Println(string(payload))
		return 0
	}
	printMigrateV2Report(report)
	return 0
}

// printMigrateV2Report 把迁移报告打成人类可读的表格。
func printMigrateV2Report(report v2import.Report) {
	mode := "写库"
	if report.DryRun {
		mode = "试运行（未写库）"
	}
	fmt.Printf("V2 数据迁移：%s，用时 %s\n", mode, report.FinishedAt.Sub(report.StartedAt).Round(time.Millisecond))
	if report.BackupPath != "" {
		fmt.Printf("导入前备份：%s\n", report.BackupPath)
	}
	fmt.Printf("\n%-16s %-18s %6s %8s %8s\n", "V2 表", "目标表", "读取", "写入", "跳过")
	for _, table := range report.Tables {
		fmt.Printf("%-16s %-18s %6d %8d %8d\n",
			table.Source, table.Target, table.Read, table.Written, table.Skipped)
		if table.Note != "" {
			fmt.Printf("  说明：%s\n", table.Note)
		}
	}
	fmt.Printf("\n共写入 %d 行\n", report.Written())
	if len(report.SitePreview) > 0 {
		fmt.Printf("\nV2 站点 %d 个（不会自动导入，请按适配器在站点管理里重建）：\n", len(report.SitePreview))
		for _, site := range report.SitePreview {
			fmt.Printf("  - %s\n", site)
		}
	}
	if len(report.Warnings) > 0 {
		fmt.Printf("\n警告 %d 条：\n", len(report.Warnings))
		for _, warning := range report.Warnings {
			fmt.Printf("  - %s\n", warning)
		}
	}
}
