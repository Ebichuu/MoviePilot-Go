package app

import (
	"log/slog"
	"os"
	"strings"
)

// brushEnabledEnv 是刷流的配置开关。默认关闭。
//
// 为什么只有一个开关、没有实现：刷流在 V2 里是「挑免费种 + 限体积/做种人数 +
// 按目标上传量分批加种 + 达标删种」的一整套策略，硬塞进现有订阅下载链
// （`downloadflow.SubmissionService.Submit` 要求 Intent + DownloadIntentPlan +
// SuccessInput 三件套）会把下载意图表写满无媒体信息的垃圾行。
// 正确做法是单开一条轻量下种路径与独立的删种判据，那是一次独立的产品改造。
//
// 所以这里的开关只做两件事：让「想开刷流的人」知道自己开的是个占位开关，
// 并且**不注册任何周期任务**——宁可没有条目，也不要一个每天空跑的 job。
func brushEnabled() bool {
	switch strings.ToLower(strings.TrimSpace(os.Getenv("MOVIEPILOT_BRUSH_ENABLED"))) {
	case "1", "true", "yes", "on":
		return true
	default:
		return false
	}
}

// logBrushSwitch 在启动时如实说明刷流开关的状态。
func logBrushSwitch(logger *slog.Logger) {
	if logger == nil {
		return
	}
	if brushEnabled() {
		logger.Warn("刷流已开启，但刷流能力尚未实现：不会注册任何刷流周期任务，" +
			"也不会创建任何刷流下载；请勿据此认为刷流已生效")
		return
	}
	logger.Debug("刷流未启用（MOVIEPILOT_BRUSH_ENABLED 未设置）")
}
