package httpapi

import (
	"net/http"
	"strings"
	"time"

	"github.com/moviepilot-go/moviepilot-go/internal/indexer"
	"github.com/moviepilot-go/moviepilot-go/internal/media"
	"github.com/moviepilot-go/moviepilot-go/internal/subscription"
)

const (
	// ruleTestGroupsSetting 是用户过滤规则组的持久化键，与订阅流程共用同一份数据。
	ruleTestGroupsSetting = "UserFilterRuleGroups"
)

// systemRuleTest 对应 V2 `GET /system/ruletest?title=&rulegroup_name=&subtitle=`：
// 用真实过滤引擎验证某个规则组是否会命中给定标题。
func (d Dependencies) systemRuleTest(w http.ResponseWriter, r *http.Request) {
	query := r.URL.Query()
	title := strings.TrimSpace(query.Get("title"))
	subtitle := strings.TrimSpace(query.Get("subtitle"))
	groupName := strings.TrimSpace(query.Get("rulegroup_name"))
	if title == "" {
		writeJSON(w, http.StatusOK, Response{Success: false, Message: "标题不能为空"})
		return
	}

	groups := []subscription.V2FilterRuleGroup{}
	if d.Store != nil {
		if _, err := d.Store.GetSetting(r.Context(), ruleTestGroupsSetting, &groups); err != nil {
			if d.Logger != nil {
				d.Logger.Warn("读取过滤规则组失败", "request_id", requestID(r.Context()), "error", err)
			}
		}
	}

	result := map[string]any{
		"title": title, "subtitle": subtitle, "rulegroup_name": groupName,
		"rulegroup": nil, "meta_info": ruleTestMetaInfo(title, subtitle),
		"media_info": nil, "priority": nil, "matched": false,
	}
	var matched *subscription.V2FilterRuleGroup
	for index := range groups {
		if groups[index].Name == groupName {
			matched = &groups[index]
			break
		}
	}
	if matched == nil {
		writeJSON(w, http.StatusOK, Response{
			Success: false, Message: "过滤规则组 " + groupName + " 不存在！", Data: result,
		})
		return
	}
	result["rulegroup"] = map[string]any{
		"name": matched.Name, "rule_string": matched.RuleString, "category": matched.Category,
	}

	parsed := media.Parse(title+" "+subtitle, media.Options{})
	candidate := subscription.Candidate{
		Key:      "ruletest",
		Resource: indexer.Resource{Title: title, Description: subtitle},
		Parsed:   parsed,
	}
	filtered, err := subscription.FilterV2Candidates(
		[]subscription.Candidate{candidate}, []string{groupName}, groups, nil,
		subscription.V2FilterMedia{}, time.Now(),
	)
	if err != nil {
		writeJSON(w, http.StatusOK, Response{
			Success: false, Message: "规则执行失败：" + err.Error(), Data: result,
		})
		return
	}
	result["matched"] = len(filtered) == 1
	writeJSON(w, http.StatusOK, Response{Success: true, Data: result})
}

// ruleTestMetaInfo 复刻 V2 `MetaInfo(title, subtitle).to_dict()` 的关键字段。
func ruleTestMetaInfo(title, subtitle string) map[string]any {
	parsed := media.Parse(title+" "+subtitle, media.Options{})
	return map[string]any{
		"title": strings.TrimSpace(parsed.Name()), "en_title": strings.TrimSpace(parsed.Name()),
		"year": strings.TrimSpace(parsed.Year), "season": strings.TrimSpace(parsed.Season()),
		"episode": strings.TrimSpace(parsed.Episode()), "type": string(parsed.Type),
		"original_name": strings.TrimSpace(parsed.OriginalName),
	}
}

// systemUpgrade 对应 V2 `POST /system/upgrade`：本项目以容器镜像交付，
// 不在进程内自改镜像，明确要求由编排侧更新。
func systemUpgrade(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, Response{Success: false, Message: "请通过容器编排更新镜像"})
}

// systemSyncPluginWiki 对应 V2 `POST /system/setting/PLUGIN_MARKET/sync-wiki`：
// 需要从 GitHub 拉取插件市场 Wiki，本实现不内置该能力。
func systemSyncPluginWiki(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, Response{Success: false, Message: "当前版本不支持插件市场 Wiki 同步"})
}
