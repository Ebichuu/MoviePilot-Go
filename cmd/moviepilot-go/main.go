package main

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"syscall"

	"github.com/moviepilot-go/moviepilot-go/internal/app"
	"github.com/moviepilot-go/moviepilot-go/internal/config"
	"github.com/moviepilot-go/moviepilot-go/internal/version"
)

func main() {
	logger := slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelInfo}))
	// 包内兜底日志（slog.Default()）与应用日志保持同一格式与输出。
	slog.SetDefault(logger)
	// `version` 不依赖配置：配置写错了也应该能查版本，所以放在加载配置之前。
	if len(os.Args) > 1 && (os.Args[1] == "version" || os.Args[1] == "--version") {
		fmt.Println(version.String())
		return
	}
	cfg, err := config.Load()
	if err != nil {
		logger.Error("配置无效", "error", err)
		os.Exit(1)
	}
	if cfg.Environment == "development" && os.Getenv("MOVIEPILOT_JWT_SECRET") == "" {
		logger.Warn("开发环境正在使用进程级临时密钥，重启后现有令牌会失效")
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	if len(os.Args) > 1 {
		switch os.Args[1] {
		case "migrate":
			if err := app.Migrate(ctx, cfg); err != nil {
				logger.Error("数据库迁移失败", "error", err)
				os.Exit(1)
			}
			logger.Info("数据库迁移完成")
			return
		case "migrate-v2":
			// 从 V2（SQLite）迁移数据：写库前自动备份，订阅默认导入成暂停。
			os.Exit(runMigrateV2(ctx, cfg, logger, os.Args[2:]))
		default:
			logger.Error("未知命令", "command", os.Args[1])
			os.Exit(2)
		}
	}

	logger.Info("启动 MoviePilot Go", "version", version.String())
	if err := app.Run(ctx, cfg, logger); err != nil {
		logger.Error("服务退出", "error", err)
		os.Exit(1)
	}
}
