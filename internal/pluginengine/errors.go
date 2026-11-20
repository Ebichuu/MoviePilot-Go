package pluginengine

import "errors"

// 本包只做「原生插件」的清单、安装/启停、配置与动作分派，刻意不内嵌 Python
// 运行时：插件能力由调用方注入，
// 未注入时动作返回 ErrServiceUnavailable，而不是假装执行成功。
var (
	// ErrInvalidManifest 表示插件清单自相矛盾（ID/名称/版本/字段定义不合法）。
	ErrInvalidManifest = errors.New("插件清单不合法")
	// ErrInvalidConfig 表示外部提交的配置与清单不符。
	ErrInvalidConfig = errors.New("插件配置不合法")
	// ErrPluginNotFound 表示注册表里没有该插件。
	ErrPluginNotFound = errors.New("插件不存在")
	// ErrPluginExists 表示同一 ID 被重复注册。
	ErrPluginExists = errors.New("插件已注册")
	// ErrPluginNotInstalled 表示插件尚未安装，不能启用或执行。
	ErrPluginNotInstalled = errors.New("插件未安装")
	// ErrPluginDisabled 表示插件已安装但未启用。
	ErrPluginDisabled = errors.New("插件未启用")
	// ErrUnknownAction 表示插件没有注册该动作。
	ErrUnknownAction = errors.New("插件动作不存在")
	// ErrServiceUnavailable 表示动作已在清单里声明，但后端能力尚未接线。
	ErrServiceUnavailable = errors.New("插件能力未接线")
)
