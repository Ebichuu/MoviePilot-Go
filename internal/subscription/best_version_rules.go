package subscription

import (
	"strings"
	"time"
)

// DefaultBestVersionRuleGroupName 是内置多档位洗版优先级规则组名称。
const DefaultBestVersionRuleGroupName = "内置洗版"

// DefaultBestVersionRuleString 是对齐 MoviePilot V2 常见洗版梯度的内置优先级规则：
// 第 1 档（PriOrder=100）命中即完成洗版并收敛订阅；未达 100 时持续保留订阅并仅接受更高档位升级。
const DefaultBestVersionRuleString = "4K & BLU & CNSUB > " +
	"4K & REMUX & CNSUB > " +
	"4K & (BLURAY | UHD) & CNSUB > " +
	"4K & WEBDL & CNSUB > " +
	"4K & (BLU | REMUX | BLURAY | UHD | WEBDL) > " +
	"1080P & (BLU | REMUX) & CNSUB > " +
	"1080P & BLURAY & CNSUB > " +
	"1080P & WEBDL & CNSUB > " +
	"1080P & (BLU | REMUX | BLURAY | WEBDL) > " +
	"720P | CNSUB | H265 | H264 | AV1"

// DefaultBestVersionRuleGroup 返回内置洗版优先级规则组定义。
func DefaultBestVersionRuleGroup() V2FilterRuleGroup {
	return V2FilterRuleGroup{
		Name:       DefaultBestVersionRuleGroupName,
		RuleString: DefaultBestVersionRuleString,
	}
}

// ScoreDefaultBestVersionCandidates 为未分配优先级（PriOrder == 0）的洗版候选按内置梯度打分（100..91）。
// 已有明确 PriOrder（!= 0）的候选保持原值不变；未命中任何梯度的候选仍保留原顺序并给予保底分 1（允许后续升级）。
func ScoreDefaultBestVersionCandidates(candidates []Candidate, mediaFacts V2FilterMedia, now time.Time) []Candidate {
	if len(candidates) == 0 {
		return nil
	}
	group := DefaultBestVersionRuleGroup()
	groups := []V2FilterRuleGroup{group}
	requested := []string{group.Name}

	result := make([]Candidate, len(candidates))
	for i, candidate := range candidates {
		if candidate.Resource.PriOrder != 0 {
			result[i] = candidate
			continue
		}
		scored, err := FilterV2Candidates([]Candidate{candidate}, requested, groups, nil, mediaFacts, now)
		if err == nil && len(scored) == 1 && scored[0].Resource.PriOrder > 0 {
			result[i] = scored[0]
			continue
		}
		candidate.Resource.PriOrder = 1
		result[i] = candidate
	}
	return result
}

// BestVersionUpgradeEvaluation 描述单个候选在指定规则组下的洗版判定结果。
type BestVersionUpgradeEvaluation struct {
	Matched         bool   `json:"matched"`
	PriOrder        int    `json:"pri_order"`
	Rank            int    `json:"rank"`
	CurrentPriority int    `json:"current_priority"`
	UpgradeAllowed  bool   `json:"upgrade_allowed"`
	WillComplete    bool   `json:"will_complete"`
	MatchedLevel    string `json:"matched_level,omitempty"`
}

// EvaluateBestVersionUpgrade 按给定规则组（为空时自动使用内置洗版规则组）评估候选的洗版档位与升级资格。
func EvaluateBestVersionUpgrade(
	candidate Candidate,
	currentPriority *int,
	groups []V2FilterRuleGroup,
	requestedGroups []string,
	customRules []V2CustomFilterRule,
	mediaFacts V2FilterMedia,
	now time.Time,
) (BestVersionUpgradeEvaluation, error) {
	if len(requestedGroups) == 0 {
		def := DefaultBestVersionRuleGroup()
		groups = []V2FilterRuleGroup{def}
		requestedGroups = []string{def.Name}
	}
	eval := BestVersionUpgradeEvaluation{}
	if currentPriority != nil {
		eval.CurrentPriority = *currentPriority
	}
	filtered, err := FilterV2Candidates([]Candidate{candidate}, requestedGroups, groups, customRules, mediaFacts, now)
	if err != nil {
		return BestVersionUpgradeEvaluation{}, err
	}
	if len(filtered) == 0 {
		return eval, nil
	}
	pri := filtered[0].Resource.PriOrder
	eval.Matched = true
	eval.PriOrder = pri
	if pri > 0 && pri <= 100 {
		eval.Rank = 100 - pri + 1
	}
	eval.UpgradeAllowed = MovieUpgradeAllowed(true, currentPriority, pri)
	eval.WillComplete = pri == 100

	// 记录命中的具体规则档位表达式，便于界面规则测试与排障。
	selected := selectV2FilterGroups(groups, requestedGroups, mediaFacts)
	if len(selected) > 0 && eval.Rank > 0 {
		last := selected[len(selected)-1]
		levels := strings.Split(last.RuleString, ">")
		if eval.Rank <= len(levels) {
			eval.MatchedLevel = strings.TrimSpace(levels[eval.Rank-1])
		}
	}
	return eval, nil
}
