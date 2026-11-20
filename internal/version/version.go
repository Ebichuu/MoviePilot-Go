// Package version 保存构建期注入的版本信息。
//
// 版本号不再写死在源码里：发布流水线通过
//
//	go build -ldflags "-X github.com/moviepilot-go/moviepilot-go/internal/version.Version=v1.2.3 \
//	  -X github.com/moviepilot-go/moviepilot-go/internal/version.Commit=$(git rev-parse --short HEAD) \
//	  -X github.com/moviepilot-go/moviepilot-go/internal/version.BuildDate=$(date -u +%Y-%m-%dT%H:%M:%SZ)"
//
// 注入实际值，未注入时是 "dev"。备份包的 manifest 也用它填 app_version，
// 否则导出的备份无法回答"这是哪个版本的数据"。
package version

import (
	"runtime/debug"
	"strings"
)

var (
	// Version 是发布版本号，例如 v1.2.3 或 v1.2.3-dev。
	Version = "dev"
	// Commit 是构建所用的提交（短哈希）。
	Commit = "unknown"
	// BuildDate 是构建时间（UTC，RFC3339）。
	BuildDate = "unknown"
)

// String 返回人类可读的完整版本串，例如 "v1.2.3 (a1b2c3d, 2026-11-20T14:00:00Z)"。
func String() string {
	version := normalize(Version, "dev")
	var extras []string
	if commit := normalize(Commit, "unknown"); commit != "unknown" {
		extras = append(extras, commit)
	}
	if buildDate := normalize(BuildDate, "unknown"); buildDate != "unknown" {
		extras = append(extras, buildDate)
	}
	if len(extras) == 0 {
		return version
	}
	return version + " (" + strings.Join(extras, ", ") + ")"
}

// Module 返回嵌入在二进制里的主模块版本（`go install` 安装时有值），
// 用于在既没有 ldflags 也没有版本文件时给日志一个可追溯的线索。
func Module() string {
	info, ok := debug.ReadBuildInfo()
	if !ok {
		return ""
	}
	if info.Main.Version == "" || info.Main.Version == "(devel)" {
		return ""
	}
	return info.Main.Version
}

// normalize 把空值和占位符统一成 fallback。
func normalize(value, fallback string) string {
	trimmed := strings.TrimSpace(value)
	if trimmed == "" {
		return fallback
	}
	return trimmed
}
