package subscription

import (
	"errors"
	"fmt"
	"math"
	"strconv"
	"strings"
	"time"

	"github.com/dlclark/regexp2"

	"github.com/moviepilot-go/moviepilot-go/internal/media"
)

var ErrInvalidV2FilterRules = errors.New("invalid V2 filter rules")

// V2FilterRuleGroup is one persisted UserFilterRuleGroups entry.
type V2FilterRuleGroup struct {
	Name       string `json:"name"`
	RuleString string `json:"rule_string"`
	MediaType  string `json:"media_type"`
	Category   string `json:"category"`
}

// V2CustomFilterRule is one persisted CustomFilterRules entry.
type V2CustomFilterRule struct {
	ID          string `json:"id"`
	Name        string `json:"name"`
	Include     string `json:"include"`
	Exclude     string `json:"exclude"`
	SizeRange   string `json:"size_range"`
	Seeders     string `json:"seeders"`
	PublishTime string `json:"publish_time"`
}

// V2FilterMedia contains the media facts used when selecting and evaluating
// V2 rule groups.
type V2FilterMedia struct {
	MediaType        string
	Category         string
	OriginalLanguage string
}

type v2FilterRule struct {
	include              []string
	exclude              []string
	matchFields          []string
	sizeRange            string
	seeders              string
	publishTime          string
	downloadVolumeFactor *float64
	originalLanguages    []string
}

type v2CompiledFilterRule struct {
	definition v2FilterRule
	include    []*regexp2.Regexp
	exclude    []*regexp2.Regexp
}

type v2FilterEvaluator struct {
	rules    map[string]v2FilterRule
	compiled map[string]v2CompiledFilterRule
	media    V2FilterMedia
	now      time.Time
}

// FilterV2Candidates reproduces FilterModule.filter_torrents. Persisted groups
// are applied in stored order, each group filters the previous result, and the
// first matching '>' level assigns priorities 100, 99, and so on.
func FilterV2Candidates(
	candidates []Candidate,
	requestedGroups []string,
	groups []V2FilterRuleGroup,
	customRules []V2CustomFilterRule,
	mediaFacts V2FilterMedia,
	now time.Time,
) ([]Candidate, error) {
	if len(requestedGroups) == 0 || len(candidates) == 0 {
		return append([]Candidate(nil), candidates...), nil
	}
	selectedGroups := selectV2FilterGroups(groups, requestedGroups, mediaFacts)
	if len(selectedGroups) == 0 {
		return append([]Candidate(nil), candidates...), nil
	}
	if now.IsZero() {
		now = time.Now()
	}
	evaluator := v2FilterEvaluator{
		rules: v2FilterRules(customRules), compiled: make(map[string]v2CompiledFilterRule),
		media: mediaFacts, now: now,
	}
	parsedLevels := make(map[string]v2RuleNode)
	result := append([]Candidate(nil), candidates...)
	for _, group := range selectedGroups {
		if group.RuleString == "" {
			continue
		}
		levels := strings.Split(group.RuleString, ">")
		filtered := make([]Candidate, 0, len(result))
		for _, candidate := range result {
			matched := false
			priority := 100
			for _, rawLevel := range levels {
				level := strings.TrimSpace(rawLevel)
				parsed, ok := parsedLevels[level]
				if !ok {
					var err error
					parsed, err = parseV2RuleLevel(level)
					if err != nil {
						return nil, fmt.Errorf("%w: %v", ErrInvalidV2FilterRules, err)
					}
					parsedLevels[level] = parsed
				}
				matches, matchErr := parsed.evaluate(func(ruleID string) (bool, error) {
					return evaluator.match(ruleID, candidate)
				})
				if matchErr != nil {
					return nil, matchErr
				}
				if matches {
					candidate.Resource.PriOrder = priority
					filtered = append(filtered, candidate)
					matched = true
					break
				}
				priority--
			}
			if !matched {
				continue
			}
		}
		result = filtered
		if len(result) == 0 {
			break
		}
	}
	return result, nil
}

func selectV2FilterGroups(
	groups []V2FilterRuleGroup,
	requested []string,
	facts V2FilterMedia,
) []V2FilterRuleGroup {
	names := make(map[string]struct{}, len(requested))
	for _, name := range requested {
		names[name] = struct{}{}
	}
	selected := make([]V2FilterRuleGroup, 0, len(groups))
	for _, group := range groups {
		if _, ok := names[group.Name]; !ok {
			continue
		}
		switch {
		case group.MediaType == "":
			selected = append(selected, group)
		case facts.MediaType != "" && group.Category == "" && group.MediaType == facts.MediaType:
			selected = append(selected, group)
		case facts.MediaType != "" && group.Category == facts.Category:
			selected = append(selected, group)
		}
	}
	return selected
}

func (e *v2FilterEvaluator) match(ruleID string, candidate Candidate) (bool, error) {
	definition, ok := e.rules[ruleID]
	if !ok {
		return false, nil
	}
	if len(definition.originalLanguages) > 0 && containsFold(
		definition.originalLanguages,
		e.media.OriginalLanguage,
	) {
		return true, nil
	}
	compiled, ok := e.compiled[ruleID]
	if !ok {
		var err error
		compiled, err = compileV2FilterRule(definition)
		if err != nil {
			return false, fmt.Errorf("%w: rule %s: %v", ErrInvalidV2FilterRules, ruleID, err)
		}
		e.compiled[ruleID] = compiled
	}
	content := strings.Join([]string{
		candidate.Resource.Title,
		candidate.Resource.Description,
		strings.Join(candidate.Resource.Labels, " "),
	}, " ")
	if len(definition.matchFields) > 0 {
		parts := make([]string, 0, len(definition.matchFields))
		for _, field := range definition.matchFields {
			if field == "labels" {
				parts = append(parts, candidate.Resource.Labels...)
			}
		}
		if len(parts) > 0 {
			content = strings.Join(parts, " ")
		}
	}
	if len(compiled.include) > 0 {
		included := false
		for _, expression := range compiled.include {
			matched, err := expression.MatchString(content)
			if err != nil {
				return false, fmt.Errorf("%w: rule %s include: %v", ErrInvalidV2FilterRules, ruleID, err)
			}
			if matched {
				included = true
				break
			}
		}
		if !included {
			return false, nil
		}
	}
	for _, expression := range compiled.exclude {
		matched, err := expression.MatchString(content)
		if err != nil {
			return false, fmt.Errorf("%w: rule %s exclude: %v", ErrInvalidV2FilterRules, ruleID, err)
		}
		if matched {
			return false, nil
		}
	}
	if definition.sizeRange != "" {
		matched, err := matchV2SizeRange(definition.sizeRange, candidate)
		if err != nil || !matched {
			return false, err
		}
	}
	if definition.seeders != "" {
		minimum, err := strconv.Atoi(strings.TrimSpace(definition.seeders))
		if err != nil {
			return false, fmt.Errorf("%w: rule %s seeders", ErrInvalidV2FilterRules, ruleID)
		}
		if candidate.Resource.Seeders < minimum {
			return false, nil
		}
	}
	if definition.downloadVolumeFactor != nil {
		if candidate.Resource.DownloadVolumeFactor == nil ||
			*candidate.Resource.DownloadVolumeFactor != *definition.downloadVolumeFactor {
			return false, nil
		}
	}
	if definition.publishTime != "" {
		matched, err := matchV2PublishTime(definition.publishTime, candidate.Resource.PublishedAt, e.now)
		if err != nil || !matched {
			return false, err
		}
	}
	return true, nil
}

func compileV2FilterRule(definition v2FilterRule) (v2CompiledFilterRule, error) {
	compiled := v2CompiledFilterRule{definition: definition}
	for _, pattern := range definition.include {
		expression, err := regexp2.Compile(pattern, regexp2.IgnoreCase)
		if err != nil {
			return v2CompiledFilterRule{}, err
		}
		compiled.include = append(compiled.include, expression)
	}
	for _, pattern := range definition.exclude {
		expression, err := regexp2.Compile(pattern, regexp2.IgnoreCase)
		if err != nil {
			return v2CompiledFilterRule{}, err
		}
		compiled.exclude = append(compiled.exclude, expression)
	}
	return compiled, nil
}

func matchV2SizeRange(value string, candidate Candidate) (bool, error) {
	parts := strings.Split(strings.TrimSpace(value), "-")
	parsed := media.Parse(
		strings.TrimSpace(candidate.Resource.Title+" "+candidate.Resource.Description),
		media.Options{},
	)
	episodeCount := 1
	if parsed.BeginEpisode > 0 {
		end := max(parsed.EndEpisode, parsed.BeginEpisode)
		episodeCount = end - parsed.BeginEpisode + 1
	}
	perEpisode := float64(candidate.Resource.SizeBytes) / float64(episodeCount)
	const mebibyte = 1024 * 1024
	if len(parts) == 2 {
		minimum, minErr := strconv.ParseFloat(strings.TrimSpace(parts[0]), 64)
		maximum, maxErr := strconv.ParseFloat(strings.TrimSpace(parts[1]), 64)
		if minErr != nil || maxErr != nil {
			return false, fmt.Errorf("%w: size range", ErrInvalidV2FilterRules)
		}
		return perEpisode >= minimum*mebibyte && perEpisode <= maximum*mebibyte, nil
	}
	if len(parts) != 1 {
		return false, fmt.Errorf("%w: size range", ErrInvalidV2FilterRules)
	}
	trimmed := strings.TrimSpace(value)
	if strings.HasPrefix(trimmed, ">") {
		minimum, err := strconv.ParseFloat(strings.TrimSpace(trimmed[1:]), 64)
		if err != nil {
			return false, fmt.Errorf("%w: size range", ErrInvalidV2FilterRules)
		}
		return perEpisode >= minimum*mebibyte, nil
	}
	if strings.HasPrefix(trimmed, "<") {
		maximum, err := strconv.ParseFloat(strings.TrimSpace(trimmed[1:]), 64)
		if err != nil {
			return false, fmt.Errorf("%w: size range", ErrInvalidV2FilterRules)
		}
		return perEpisode <= maximum*mebibyte, nil
	}
	return false, nil
}

func matchV2PublishTime(value string, publishedAt, now time.Time) (bool, error) {
	parts := strings.Split(value, "-")
	values := make([]float64, 0, len(parts))
	for _, part := range parts {
		parsed, err := strconv.ParseFloat(strings.TrimSpace(part), 64)
		if err != nil {
			return false, fmt.Errorf("%w: publish time", ErrInvalidV2FilterRules)
		}
		values = append(values, parsed)
	}
	minutes := float64(0)
	if !publishedAt.IsZero() {
		minutes = math.Floor(now.Sub(publishedAt).Minutes())
	}
	if len(values) == 1 {
		return minutes >= values[0], nil
	}
	return len(values) > 1 && minutes >= values[0] && minutes <= values[1], nil
}

func containsFold(values []string, target string) bool {
	for _, value := range values {
		if strings.EqualFold(strings.TrimSpace(value), strings.TrimSpace(target)) {
			return true
		}
	}
	return false
}

func v2FilterRules(customRules []V2CustomFilterRule) map[string]v2FilterRule {
	rules := make(map[string]v2FilterRule, len(v2BuiltinFilterRules)+len(customRules))
	for id, rule := range v2BuiltinFilterRules {
		rules[id] = rule
	}
	for _, custom := range customRules {
		id := custom.ID
		if id == "" {
			continue
		}
		rule := v2FilterRule{
			sizeRange: strings.TrimSpace(custom.SizeRange), seeders: strings.TrimSpace(custom.Seeders),
			publishTime: strings.TrimSpace(custom.PublishTime),
		}
		if custom.Include != "" {
			rule.include = []string{custom.Include}
		}
		if custom.Exclude != "" {
			rule.exclude = []string{custom.Exclude}
		}
		rules[id] = rule
	}
	return rules
}

func float64Pointer(value float64) *float64 { return &value }

var v2BuiltinFilterRules = map[string]v2FilterRule{
	"BLU": {
		include: []string{`(?i)(\bBlu-?Ray\b.*\b(?:VC-?1|AVC|MPEG-?2)\b|\b(?:UHD|4K|2160p)\b(?:.*Blu-?Ray)?.*\b(?:HEVC|H\.?265)\b|\bBlu-?Ray\b.*\b(?:UHD|4K|2160p)\b.*\b(?:HEVC|H\.?265)\b|\b(?:COMPLETE|FULL)\b.*\b(?:(?:UHD|4K|2160p)\b.*)?Blu-?Ray\b|\b(BD25|BD50|BD66|BD100|BDMV|MiniBD)\b)`},
		exclude: []string{`(?i)(\b[XH]\.?264\b|\b[XH]\.?265\b|\bWEB-?DL\b|\bWEB-?RIP\b|\bHDTV(?:RIP)?\b|\bREMUX\b|\bBDRip\b|\bBRRip\b|\bHDRip\b|\bENCODE\b|\b(?<!WEB-|HDTV)RIP\b)`},
	},
	"4K":      {include: []string{`4k|2160p|x2160`}},
	"1080P":   {include: []string{`1080[pi]|x1080`}},
	"720P":    {include: []string{`720[pi]|x720`}},
	"CNSUB":   {include: []string{`[中国國繁简](/|\s|\\|\|)?[繁简英粤]|[英简繁](/|\s|\\|\|)?[中繁简]|繁體|简体|[中国國][字配]|国语|國語|中文|中字|简日|繁日|简繁|繁体|([\s,.-\[])(chs|cht)(|[\s,.-\]])|(?<![a-z0-9])(?<!\d\s)(gb|big5)(?![a-z0-9])`}, originalLanguages: []string{"zh", "cn"}},
	"GZ":      {include: []string{`官方`, `官种`, `官组`}, matchFields: []string{"labels"}},
	"SPECSUB": {include: []string{`特效`}},
	"BLURAY":  {include: []string{`Blu-?Ray`}},
	"UHD":     {include: []string{`UHD|UltraHD`}},
	"H265":    {include: []string{`[Hx].?265|HEVC`}},
	"H264":    {include: []string{`[Hx].?264|AVC`}},
	"DOLBY":   {include: []string{`Dolby[\s.]+Vision|DOVI|[\s.]+DV[\s.]+|杜比视界`}},
	"ATMOS":   {include: []string{`Dolby[\s.+]+Atmos|Atmos|杜比全景[声聲]`}},
	"HDR":     {include: []string{`[\s.]+HDR[\s.]+|HDR10|HDR10\+|HDRVivid`}},
	"SDR":     {include: []string{`[\s.]+SDR[\s.]+`}},
	"REMUX":   {include: []string{`REMUX`}},
	"WEBDL":   {include: []string{`WEB-?DL|WEB-?RIP`}},
	"FREE":    {downloadVolumeFactor: float64Pointer(0)},
	"CNVOI":   {include: []string{`[国國][语語]配音|[国國]配|[国國][语語]`}, originalLanguages: []string{"zh"}},
	"HKVOI":   {include: []string{`粤语配音|粤语`}},
	"60FPS":   {include: []string{`60fps|60帧`}},
	"3D":      {include: []string{`3D`}},
}

type v2RuleNode interface {
	evaluate(func(string) (bool, error)) (bool, error)
}

type v2RuleAtom struct{ id string }

func (n v2RuleAtom) evaluate(match func(string) (bool, error)) (bool, error) {
	return match(n.id)
}

type v2RuleNot struct{ child v2RuleNode }

func (n v2RuleNot) evaluate(match func(string) (bool, error)) (bool, error) {
	value, err := n.child.evaluate(match)
	return !value, err
}

type v2RuleBinary struct {
	operator byte
	left     v2RuleNode
	right    v2RuleNode
}

func (n v2RuleBinary) evaluate(match func(string) (bool, error)) (bool, error) {
	left, err := n.left.evaluate(match)
	if err != nil {
		return false, err
	}
	if n.operator == '&' {
		if !left {
			return false, nil
		}
		return n.right.evaluate(match)
	}
	if left {
		return true, nil
	}
	return n.right.evaluate(match)
}

type v2RuleParser struct {
	input string
	index int
}

func parseV2RuleLevel(input string) (v2RuleNode, error) {
	parser := v2RuleParser{input: input}
	node, err := parser.parseOr()
	if err != nil {
		return nil, err
	}
	// RuleParser.parse_string() does not use parse_all, so V2 accepts a valid
	// expression prefix and ignores remaining top-level input.
	return node, nil
}

func (p *v2RuleParser) parseOr() (v2RuleNode, error) {
	left, err := p.parseAnd()
	if err != nil {
		return nil, err
	}
	for p.consume('|') {
		right, rightErr := p.parseAnd()
		if rightErr != nil {
			return nil, rightErr
		}
		left = v2RuleBinary{operator: '|', left: left, right: right}
	}
	return left, nil
}

func (p *v2RuleParser) parseAnd() (v2RuleNode, error) {
	left, err := p.parseUnary()
	if err != nil {
		return nil, err
	}
	for p.consume('&') {
		right, rightErr := p.parseUnary()
		if rightErr != nil {
			return nil, rightErr
		}
		left = v2RuleBinary{operator: '&', left: left, right: right}
	}
	return left, nil
}

func (p *v2RuleParser) parseUnary() (v2RuleNode, error) {
	if p.consume('!') {
		child, err := p.parseUnary()
		if err != nil {
			return nil, err
		}
		return v2RuleNot{child: child}, nil
	}
	return p.parsePrimary()
}

func (p *v2RuleParser) parsePrimary() (v2RuleNode, error) {
	p.skipWhitespace()
	if p.index >= len(p.input) {
		return nil, errors.New("filter expression ended unexpectedly")
	}
	if p.input[p.index] == '(' {
		p.index++
		node, err := p.parseOr()
		if err != nil {
			return nil, err
		}
		if !p.consume(')') {
			return nil, errors.New("filter expression is missing ')'")
		}
		return node, nil
	}
	atom, ok := p.atom()
	if !ok {
		return nil, fmt.Errorf("filter expression has no rule at byte %d", p.index)
	}
	return v2RuleAtom{id: atom}, nil
}

func (p *v2RuleParser) atom() (string, bool) {
	p.skipWhitespace()
	start := p.index
	if start >= len(p.input) {
		return "", false
	}
	if isV2ASCIILetter(p.input[p.index]) {
		p.index++
		for p.index < len(p.input) && isV2ASCIIAlphaNumeric(p.input[p.index]) {
			p.index++
		}
		return p.input[start:p.index], true
	}
	if !isV2ASCIIDigit(p.input[p.index]) {
		return "", false
	}
	for p.index < len(p.input) && isV2ASCIIDigit(p.input[p.index]) {
		p.index++
	}
	if p.index >= len(p.input) || !isV2ASCIILetter(p.input[p.index]) {
		p.index = start
		return "", false
	}
	p.index++
	for p.index < len(p.input) && isV2ASCIIAlphaNumeric(p.input[p.index]) {
		p.index++
	}
	return p.input[start:p.index], true
}

func (p *v2RuleParser) consume(want byte) bool {
	p.skipWhitespace()
	if p.index >= len(p.input) || p.input[p.index] != want {
		return false
	}
	p.index++
	return true
}

func (p *v2RuleParser) skipWhitespace() {
	for p.index < len(p.input) {
		switch p.input[p.index] {
		case ' ', '\t', '\r', '\n':
			p.index++
		default:
			return
		}
	}
}

func isV2ASCIILetter(value byte) bool {
	return value >= 'A' && value <= 'Z' || value >= 'a' && value <= 'z'
}

func isV2ASCIIDigit(value byte) bool { return value >= '0' && value <= '9' }

func isV2ASCIIAlphaNumeric(value byte) bool {
	return isV2ASCIILetter(value) || isV2ASCIIDigit(value)
}
