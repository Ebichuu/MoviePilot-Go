package metadata

import (
	"strconv"
	"strings"
)

type v2CategoryCondition struct {
	attribute string
	value     string
}

type v2CategoryRule struct {
	name       string
	conditions []v2CategoryCondition
}

// V2Category returns the first category selected by MoviePilot V2's default
// category.yaml. Rules retain file order because first match wins.
func V2Category(mediaType string, item Media) string {
	switch strings.ToLower(mediaType) {
	case "movie", "电影":
		return evaluateV2Category(v2MovieCategoryRules, item)
	case "tv", "电视剧":
		return evaluateV2Category(v2TVCategoryRules, item)
	default:
		return ""
	}
}

func evaluateV2Category(rules []v2CategoryRule, item Media) string {
	for _, rule := range rules {
		if len(rule.conditions) == 0 {
			return rule.name
		}
		matched := true
		for _, condition := range rule.conditions {
			if condition.value == "" {
				continue
			}
			facts, exists := v2CategoryFacts(item, condition.attribute)
			if !exists {
				matched = false
				continue
			}
			matcher := newV2CategoryMatcher(condition.value)
			if matcher.hasPositive() && !matcher.matchesPositive(facts) {
				matched = false
			}
			if matcher.hasInverted() && matcher.matchesInverted(facts) {
				matched = false
			}
		}
		if matched {
			return rule.name
		}
	}
	return ""
}

func v2CategoryFacts(item Media, attribute string) ([]string, bool) {
	var values []string
	switch attribute {
	case "genre_ids":
		values = make([]string, 0, len(item.GenreIDs))
		for _, id := range item.GenreIDs {
			values = append(values, strconv.Itoa(id))
		}
	case "original_language":
		if item.OriginalLanguage != "" {
			values = []string{item.OriginalLanguage}
		}
	case "origin_country":
		values = item.OriginCountries
	case "production_countries":
		values = item.ProductionCountries
	case "release_year":
		if item.Year != 0 {
			values = []string{strconv.Itoa(item.Year)}
		}
	default:
		return nil, false
	}
	if len(values) == 0 {
		return nil, false
	}
	result := make([]string, len(values))
	for index, value := range values {
		result[index] = strings.ToUpper(value)
	}
	return result, true
}

type v2NumericRange struct {
	begin string
	end   string
}

type v2CategoryMatcher struct {
	positive       []string
	inverted       []string
	positiveRanges []v2NumericRange
	invertedRanges []v2NumericRange
}

func newV2CategoryMatcher(raw string) v2CategoryMatcher {
	var matcher v2CategoryMatcher
	for _, value := range strings.Split(raw, ",") {
		if value == "" {
			continue
		}
		if !strings.Contains(value, "-") {
			matcher.addValue(value)
			continue
		}
		parts := strings.SplitN(value, "-", 2)
		begin, end := parts[0], parts[1]
		inverted := false
		if strings.HasPrefix(begin, "!") {
			inverted = true
			begin = begin[1:]
		}
		beginNumber, beginOK := canonicalV2Decimal(begin)
		endNumber, endOK := canonicalV2Decimal(end)
		if beginOK && endOK {
			// Python range(begin, end+1) is empty for reversed ranges.
			if compareV2Decimal(beginNumber, endNumber) <= 0 {
				numericRange := v2NumericRange{begin: beginNumber, end: endNumber}
				if inverted {
					matcher.invertedRanges = append(matcher.invertedRanges, numericRange)
				} else {
					matcher.positiveRanges = append(matcher.positiveRanges, numericRange)
				}
			}
			continue
		}
		prefix := ""
		if inverted {
			prefix = "!"
		}
		matcher.addValue(prefix + begin)
		matcher.addValue(prefix + end)
	}
	return matcher
}

func (m *v2CategoryMatcher) addValue(value string) {
	value = strings.ToUpper(value)
	if strings.HasPrefix(value, "!") {
		m.inverted = append(m.inverted, value[1:])
	} else {
		m.positive = append(m.positive, value)
	}
}

func (m v2CategoryMatcher) hasPositive() bool {
	return len(m.positive) > 0 || len(m.positiveRanges) > 0
}

func (m v2CategoryMatcher) hasInverted() bool {
	return len(m.inverted) > 0 || len(m.invertedRanges) > 0
}

func (m v2CategoryMatcher) matchesPositive(facts []string) bool {
	return v2CategoryMatches(m.positive, m.positiveRanges, facts)
}

func (m v2CategoryMatcher) matchesInverted(facts []string) bool {
	return v2CategoryMatches(m.inverted, m.invertedRanges, facts)
}

func v2CategoryMatches(values []string, ranges []v2NumericRange, facts []string) bool {
	for _, fact := range facts {
		for _, value := range values {
			if value == fact {
				return true
			}
		}
		canonical, ok := canonicalV2Decimal(fact)
		if !ok || canonical != fact {
			continue
		}
		for _, numericRange := range ranges {
			if compareV2Decimal(numericRange.begin, canonical) <= 0 && compareV2Decimal(canonical, numericRange.end) <= 0 {
				return true
			}
		}
	}
	return false
}

func canonicalV2Decimal(value string) (string, bool) {
	if value == "" {
		return "", false
	}
	for _, character := range value {
		if character < '0' || character > '9' {
			return "", false
		}
	}
	canonical := strings.TrimLeft(value, "0")
	if canonical == "" {
		canonical = "0"
	}
	return canonical, true
}

func compareV2Decimal(left, right string) int {
	if len(left) < len(right) {
		return -1
	}
	if len(left) > len(right) {
		return 1
	}
	return strings.Compare(left, right)
}

var v2MovieCategoryRules = []v2CategoryRule{
	{name: "动画电影", conditions: []v2CategoryCondition{{attribute: "genre_ids", value: "16"}}},
	{name: "华语电影", conditions: []v2CategoryCondition{{attribute: "original_language", value: "zh,cn,bo,za"}}},
	{name: "外语电影"},
}

var v2TVCategoryRules = []v2CategoryRule{
	{name: "国漫", conditions: []v2CategoryCondition{
		{attribute: "genre_ids", value: "16"},
		{attribute: "origin_country", value: "CN,TW,HK"},
	}},
	{name: "日番", conditions: []v2CategoryCondition{
		{attribute: "genre_ids", value: "16"},
		{attribute: "origin_country", value: "JP"},
	}},
	{name: "纪录片", conditions: []v2CategoryCondition{{attribute: "genre_ids", value: "99"}}},
	{name: "儿童", conditions: []v2CategoryCondition{{attribute: "genre_ids", value: "10762"}}},
	{name: "综艺", conditions: []v2CategoryCondition{{attribute: "genre_ids", value: "10764,10767"}}},
	{name: "国产剧", conditions: []v2CategoryCondition{{attribute: "origin_country", value: "CN,TW,HK"}}},
	{name: "欧美剧", conditions: []v2CategoryCondition{{attribute: "origin_country", value: "US,FR,GB,DE,ES,IT,NL,PT,RU,UK"}}},
	{name: "日韩剧", conditions: []v2CategoryCondition{{attribute: "origin_country", value: "JP,KP,KR,TH,IN,SG"}}},
	{name: "未分类"},
}
