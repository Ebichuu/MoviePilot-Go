package notificationconfig

import "strings"

// V2 通知开关（`Notifications[].switchs`）由前端保存为英文标识，例如
// `["search","download","subscribe","site","mediaserver","plugin","agent","system"]`。
// 历史数据与单元测试里也可能出现中文显示名，因此匹配时两种写法都要接受。
const (
	SwitchDownload    = "download"
	SwitchOrganize    = "organize"
	SwitchSubscribe   = "subscribe"
	SwitchSite        = "site"
	SwitchMediaServer = "mediaserver"
	SwitchManual      = "manual"
	SwitchPlugin      = "plugin"
	SwitchAgent       = "agent"
	SwitchSystem      = "system"
	SwitchSearch      = "search"
	SwitchOther       = "other"
)

// switchAliases 把中文显示名映射到 V2 英文开关标识。
var switchAliases = map[string]string{
	"资源下载":  SwitchDownload,
	"整理入库":  SwitchOrganize,
	"订阅":    SwitchSubscribe,
	"站点":    SwitchSite,
	"媒体服务器": SwitchMediaServer,
	"手动处理":  SwitchManual,
	"插件":    SwitchPlugin,
	"智能助手":  SwitchAgent,
	"系统":    SwitchSystem,
	"搜索":    SwitchSearch,
	"其他":    SwitchOther,
}

// SwitchKey 归一化一个开关值：去空白、转小写，中文显示名换成英文标识。
func SwitchKey(value string) string {
	trimmed := strings.ToLower(strings.TrimSpace(value))
	if alias, ok := switchAliases[trimmed]; ok {
		return alias
	}
	return trimmed
}

// SwitchMatches 判断开关列表是否允许该消息类型。
// switchs 为空表示该渠道没有勾选任何类型（V2 语义：不发送）。
func SwitchMatches(switchs []string, value string) bool {
	want := SwitchKey(value)
	if want == "" {
		return false
	}
	for _, configured := range switchs {
		if SwitchKey(configured) == want {
			return true
		}
	}
	return false
}
