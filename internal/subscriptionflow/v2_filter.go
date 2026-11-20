package subscriptionflow

import (
	"context"
	"errors"
	"time"

	"github.com/moviepilot-go/moviepilot-go/internal/metadata"
	"github.com/moviepilot-go/moviepilot-go/internal/subscription"
)

const (
	v2UserFilterRuleGroupsSetting        = "UserFilterRuleGroups"
	v2CustomFilterRulesSetting           = "CustomFilterRules"
	v2SubscribeFilterRuleGroupsSetting   = "SubscribeFilterRuleGroups"
	v2BestVersionFilterRuleGroupsSetting = "BestVersionFilterRuleGroups"
	v2SubscribeDefaultParamsSetting      = "SubscribeDefaultParams"
)

func (h *Handler) withV2DefaultCandidateFilters(
	ctx context.Context,
	config map[string]any,
	query subscription.Query,
) (subscription.Query, error) {
	defaults := make(map[string]any)
	if _, err := h.store.GetSetting(ctx, v2SubscribeDefaultParamsSetting, &defaults); err != nil {
		return subscription.Query{}, err
	}
	value := func(key string) string {
		if configured := configText(config, key); configured != "" {
			return configured
		}
		return configText(defaults, key)
	}
	query.Include = v2AdditionalPattern(value("include"))
	query.Exclude = v2AdditionalPattern(value("exclude"))
	query.Quality = value("quality")
	query.Resolution = value("resolution")
	query.Effect = value("effect")
	return query, nil
}

func v2AdditionalPattern(value string) []string {
	if value == "" {
		return nil
	}
	return []string{value}
}

func (h *Handler) filterV2SubscriptionCandidates(
	ctx context.Context,
	target persistedSubscription,
	candidates []subscription.Candidate,
) ([]subscription.Candidate, error) {
	requested, own, err := v2SubscriptionFilterGroupNames(target.Config)
	if err != nil {
		return nil, err
	}
	if !own {
		setting := v2SubscribeFilterRuleGroupsSetting
		if subscriptionBestVersionEnabled(target.Config) {
			setting = v2BestVersionFilterRuleGroupsSetting
		}
		if _, err := h.store.GetSetting(ctx, setting, &requested); err != nil {
			return nil, err
		}
	}
	if len(requested) == 0 {
		return filterMovieUpgradeCandidates(target, candidates)
	}

	var groups []subscription.V2FilterRuleGroup
	if _, err := h.store.GetSetting(ctx, v2UserFilterRuleGroupsSetting, &groups); err != nil {
		return nil, err
	}
	var custom []subscription.V2CustomFilterRule
	if _, err := h.store.GetSetting(ctx, v2CustomFilterRulesSetting, &custom); err != nil {
		return nil, err
	}
	facts, err := h.v2FilterMediaFacts(ctx, target)
	if err != nil {
		return nil, err
	}
	filtered, err := subscription.FilterV2Candidates(candidates, requested, groups, custom, facts, time.Now())
	if err != nil {
		return nil, err
	}
	return filterMovieUpgradeCandidates(target, filtered)
}

func (h *Handler) v2FilterMediaFacts(
	ctx context.Context,
	target persistedSubscription,
) (subscription.V2FilterMedia, error) {
	facts := subscription.V2FilterMedia{MediaType: target.MediaType}
	if h == nil || h.mediaDetails == nil || target.MediaSource == "" || target.MediaID == "" {
		return facts, nil
	}
	detail, available, err := h.subscriptionMediaFacts(ctx, target)
	if err != nil {
		if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) || ctx.Err() != nil {
			return subscription.V2FilterMedia{}, err
		}
		if !available {
			return facts, nil
		}
	}
	if !available {
		return facts, nil
	}
	facts.OriginalLanguage = detail.OriginalLanguage
	if h.categoryResolver != nil {
		facts.Category = h.categoryResolver.Category(target.MediaType, detail)
	} else {
		facts.Category = metadata.V2Category(target.MediaType, detail)
	}
	return facts, nil
}

func v2SubscriptionFilterGroupNames(config map[string]any) ([]string, bool, error) {
	value, exists := config["filter_groups"]
	if !exists || value == nil {
		return nil, false, nil
	}
	switch groups := value.(type) {
	case []string:
		if len(groups) == 0 {
			return nil, false, nil
		}
		return append([]string(nil), groups...), true, nil
	case []any:
		if len(groups) == 0 {
			return nil, false, nil
		}
		result := make([]string, len(groups))
		for index, item := range groups {
			text, ok := item.(string)
			if !ok {
				return nil, false, subscription.ErrInvalidV2FilterRules
			}
			result[index] = text
		}
		return result, true, nil
	default:
		return nil, false, subscription.ErrInvalidV2FilterRules
	}
}
