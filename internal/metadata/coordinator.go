package metadata

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"
	"unicode"

	"golang.org/x/sync/singleflight"
)

const (
	maxProviders      = 16
	maxQueryLength    = 512
	minimumMatchScore = 60
)

// JSONCache 是元数据层使用的最小可丢弃缓存边界。
type JSONCache interface {
	GetJSON(ctx context.Context, key string, target any) (bool, error)
	SetJSON(ctx context.Context, key string, value any, ttl time.Duration) error
}

// Options 定义元数据协调器的资源边界。
type Options struct {
	Cache               JSONCache
	CacheTTL            time.Duration
	RequestTimeout      time.Duration
	MaxResults          int
	ProviderMinInterval time.Duration
	ProviderMaxBackoff  time.Duration
}

// Query 是跨源搜索与识别的规范化输入。
type Query struct {
	Keyword string
	Type    string
	Year    int
	Source  string
}

// Match 保存候选媒体及其确定性匹配分数。
type Match struct {
	Media Media
	Score int
}

// SearchResult 同时保留可用候选和单源失败诊断。
type SearchResult struct {
	Matches []Match
	Errors  map[string]error
}

// Coordinator 对多个元数据源施加请求合并、缓存、退避和结果上限。
type Coordinator struct {
	// providersMu 保护 providers/order 的发布。运行期允许按系统设置热替换数据源：
	// V2 的 CONFIG_WATCH 在 TMDB_API_KEY / TMDB_API_DOMAIN 变化时会重建 TMDB 客户端，
	// 这里同一条路线，改完设置不用重启。替换走 copy-on-write——整块换新 map，
	// 读侧只取一次引用，绝不修改已发布的 map。
	providersMu sync.RWMutex
	providers   map[string]*providerState
	order       map[string]int
	cache       JSONCache
	cacheTTL    time.Duration
	timeout     time.Duration
	maxResults  int
	minInterval time.Duration
	maxBackoff  time.Duration
	flights     singleflight.Group
}

type providerState struct {
	provider    Provider
	minInterval time.Duration
	maxBackoff  time.Duration

	mu           sync.Mutex
	nextRequest  time.Time
	backoffUntil time.Time
	failures     uint8
}

type providerResponse struct {
	source string
	items  []Media
	err    error
}

// NewCoordinator 创建跨源元数据协调器。供应商数量、响应数量和所有等待均有硬上限。
func NewCoordinator(providers []Provider, options Options) (*Coordinator, error) {
	if len(providers) == 0 {
		return nil, errors.New("至少需要一个元数据供应商")
	}
	if len(providers) > maxProviders {
		return nil, fmt.Errorf("元数据供应商数量不能超过 %d", maxProviders)
	}
	if options.RequestTimeout <= 0 {
		return nil, errors.New("元数据请求超时必须为正数")
	}
	if options.CacheTTL <= 0 {
		return nil, errors.New("元数据缓存 TTL 必须为正数")
	}
	if options.MaxResults < 1 || options.MaxResults > 200 {
		return nil, errors.New("元数据结果上限必须在 1 到 200 之间")
	}
	if options.ProviderMinInterval < 0 || options.ProviderMaxBackoff < 0 {
		return nil, errors.New("元数据流控时间不能为负数")
	}
	if options.ProviderMaxBackoff == 0 {
		options.ProviderMaxBackoff = 30 * time.Second
	}

	coordinator := &Coordinator{
		cache: options.Cache, cacheTTL: options.CacheTTL,
		timeout: options.RequestTimeout, maxResults: options.MaxResults,
		minInterval: options.ProviderMinInterval, maxBackoff: options.ProviderMaxBackoff,
	}
	states, order, err := buildProviderStates(providers, coordinator.minInterval, coordinator.maxBackoff)
	if err != nil {
		return nil, err
	}
	coordinator.providers, coordinator.order = states, order
	return coordinator, nil
}

// buildProviderStates 把供应商列表编译成「名字 → 运行态」与「名字 → 顺序」两张表。
// 新建协调器和运行期热替换（SetProviders）共用同一套校验，避免两条路径行为漂移。
func buildProviderStates(providers []Provider, minInterval, maxBackoff time.Duration) (map[string]*providerState, map[string]int, error) {
	states := make(map[string]*providerState, len(providers))
	order := make(map[string]int, len(providers))
	for index, provider := range providers {
		if provider == nil {
			return nil, nil, errors.New("元数据供应商不能为 nil")
		}
		name := normalizeSource(provider.Name())
		if name == "" {
			return nil, nil, errors.New("元数据供应商名称不能为空")
		}
		if _, exists := states[name]; exists {
			return nil, nil, fmt.Errorf("元数据供应商名称重复: %s", name)
		}
		states[name] = &providerState{
			provider: provider, minInterval: minInterval, maxBackoff: maxBackoff,
		}
		order[name] = index
	}
	return states, order, nil
}

// SetProviders 用新的供应商集合替换现有集合，供「系统设置里改了 TMDB 密钥」这类
// 需要立刻生效的场景使用（V2 的 CONFIG_WATCH 行为）。
//
// 旧集合的流控/退避状态一并丢弃：那是为旧客户端累积的，换客户端后没有意义；
// 若沿用，新客户端会被旧客户端的限流连坐。
func (c *Coordinator) SetProviders(providers []Provider) error {
	if c == nil {
		return errors.New("元数据协调器不能为空")
	}
	states, order, err := buildProviderStates(providers, c.minInterval, c.maxBackoff)
	if err != nil {
		return err
	}
	c.providersMu.Lock()
	c.providers, c.order = states, order
	c.providersMu.Unlock()
	return nil
}

// providerStates 返回当前供应商表的只读快照（copy-on-write，读侧不加锁复制）。
func (c *Coordinator) providerStates() map[string]*providerState {
	c.providersMu.RLock()
	defer c.providersMu.RUnlock()
	return c.providers
}

// providerOrder 返回当前顺序表的只读快照。
func (c *Coordinator) providerOrder() map[string]int {
	c.providersMu.RLock()
	defer c.providersMu.RUnlock()
	return c.order
}

// Search 并行查询指定或全部来源；只在所有来源均失败时返回顶层错误。
// Provider 按数据源名返回底层供应商，供需要供应商专有能力的接口做类型断言。
func (c *Coordinator) Provider(name string) (Provider, bool) {
	state, ok := c.providerStates()[name]
	if !ok || state == nil || state.provider == nil {
		return nil, false
	}
	return state.provider, true
}
func (c *Coordinator) Search(ctx context.Context, query Query) (SearchResult, error) {
	query, states, err := c.prepareQuery(query)
	if err != nil {
		return SearchResult{}, err
	}

	responses := make(chan providerResponse, len(states))
	for source, state := range states {
		go func() {
			items, searchErr := c.searchProvider(ctx, source, state, query.Keyword)
			responses <- providerResponse{source: source, items: items, err: searchErr}
		}()
	}

	result := SearchResult{Errors: make(map[string]error)}
	succeeded := false
	for range states {
		response := <-responses
		if response.err != nil {
			result.Errors[response.source] = response.err
			continue
		}
		succeeded = true
		for _, item := range response.items {
			item.Source = response.source
			if !typeMatches(query.Type, item.Type) {
				continue
			}
			result.Matches = append(result.Matches, Match{Media: item, Score: scoreMedia(query, item)})
		}
	}
	result.Matches = c.sortAndLimit(result.Matches)
	if !succeeded && len(result.Errors) > 0 {
		return result, joinProviderErrors(result.Errors)
	}
	return result, nil
}

// Recognize 返回达到最低可信分数的最佳候选。
func (c *Coordinator) Recognize(ctx context.Context, query Query) (Match, bool, error) {
	result, err := c.Search(ctx, query)
	if err != nil {
		return Match{}, false, err
	}
	if len(result.Matches) == 0 || result.Matches[0].Score < minimumMatchScore {
		return Match{}, false, nil
	}
	return result.Matches[0], true, nil
}

// Detail 按来源原生 ID 查询详情，并对同一请求进行合并和缓存。
func (c *Coordinator) Detail(ctx context.Context, source, id string) (Media, error) {
	return c.detail(ctx, source, id, false)
}

// DetailFresh 跳过旧详情缓存并独立合并刷新请求，成功后更新普通详情缓存。
func (c *Coordinator) DetailFresh(ctx context.Context, source, id string) (Media, error) {
	return c.detail(ctx, source, id, true)
}

func (c *Coordinator) detail(ctx context.Context, source, id string, fresh bool) (Media, error) {
	source = normalizeSource(source)
	id = strings.TrimSpace(id)
	if source == "" || id == "" {
		return Media{}, errors.New("元数据来源和 ID 不能为空")
	}
	state, exists := c.providerStates()[source]
	if !exists {
		return Media{}, fmt.Errorf("未配置元数据来源: %s", source)
	}
	if fresh {
		if err := ctx.Err(); err != nil {
			return Media{}, err
		}
	}
	key := metadataCacheKey("detail", source, id)
	var cached Media
	if !fresh && c.cache != nil {
		if hit, _ := c.cache.GetJSON(ctx, key, &cached); hit {
			return cached, nil
		}
	}

	flightKey := key
	if fresh {
		flightKey = metadataCacheKey("detail-fresh", source, id)
	}
	value, err := c.doFlight(ctx, flightKey, func(callCtx context.Context) (any, error) {
		if !fresh && c.cache != nil {
			if hit, _ := c.cache.GetJSON(callCtx, key, &cached); hit {
				return cached, nil
			}
		}
		if err := state.beforeCall(callCtx); err != nil {
			return nil, err
		}
		item, detailErr := state.provider.Detail(callCtx, id)
		state.afterCall(detailErr)
		if detailErr != nil {
			return nil, detailErr
		}
		item.Source = source
		if !fresh && c.cache != nil {
			_ = c.cache.SetJSON(callCtx, key, item, c.cacheTTL)
		}
		return item, nil
	})
	if err != nil {
		return Media{}, err
	}
	item := value.(Media)
	if fresh {
		if err := ctx.Err(); err != nil {
			return Media{}, err
		}
		if c.cache != nil {
			cacheCtx, cancel := context.WithTimeout(ctx, c.timeout)
			defer cancel()
			_ = c.cache.SetJSON(cacheCtx, key, item, c.cacheTTL)
		}
	}
	return item, nil
}

func (c *Coordinator) prepareQuery(query Query) (Query, map[string]*providerState, error) {
	query.Keyword = strings.TrimSpace(query.Keyword)
	query.Type = normalizeMediaType(query.Type)
	query.Source = normalizeSource(query.Source)
	if query.Keyword == "" {
		return Query{}, nil, errors.New("元数据搜索关键词不能为空")
	}
	if len([]rune(query.Keyword)) > maxQueryLength {
		return Query{}, nil, fmt.Errorf("元数据搜索关键词不能超过 %d 个字符", maxQueryLength)
	}
	if query.Year < 0 || query.Year > 9999 {
		return Query{}, nil, errors.New("元数据年份无效")
	}
	if query.Source != "" {
		state, exists := c.providerStates()[query.Source]
		if !exists {
			return Query{}, nil, fmt.Errorf("未配置元数据来源: %s", query.Source)
		}
		return query, map[string]*providerState{query.Source: state}, nil
	}
	return query, c.providerStates(), nil
}

func (c *Coordinator) searchProvider(ctx context.Context, source string, state *providerState, keyword string) ([]Media, error) {
	key := metadataCacheKey("search", source, normalizeText(keyword))
	var cached []Media
	if c.cache != nil {
		if hit, _ := c.cache.GetJSON(ctx, key, &cached); hit {
			return cached, nil
		}
	}

	value, err := c.doFlight(ctx, key, func(callCtx context.Context) (any, error) {
		if c.cache != nil {
			if hit, _ := c.cache.GetJSON(callCtx, key, &cached); hit {
				return cached, nil
			}
		}
		if err := state.beforeCall(callCtx); err != nil {
			return nil, err
		}
		items, searchErr := state.provider.Search(callCtx, keyword)
		state.afterCall(searchErr)
		if searchErr != nil {
			return nil, searchErr
		}
		items = limitMedia(items, c.maxResults)
		if c.cache != nil {
			_ = c.cache.SetJSON(callCtx, key, items, c.cacheTTL)
		}
		return items, nil
	})
	if err != nil {
		return nil, err
	}
	return append([]Media(nil), value.([]Media)...), nil
}

func (c *Coordinator) doFlight(ctx context.Context, key string, call func(context.Context) (any, error)) (any, error) {
	result := c.flights.DoChan(key, func() (any, error) {
		callCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), c.timeout)
		defer cancel()
		return call(callCtx)
	})
	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	case completed := <-result:
		return completed.Val, completed.Err
	}
}

func (c *Coordinator) sortAndLimit(matches []Match) []Match {
	order := c.providerOrder()
	sort.SliceStable(matches, func(i, j int) bool {
		if matches[i].Score != matches[j].Score {
			return matches[i].Score > matches[j].Score
		}
		left := order[normalizeSource(matches[i].Media.Source)]
		right := order[normalizeSource(matches[j].Media.Source)]
		if left != right {
			return left < right
		}
		return matches[i].Media.ID < matches[j].Media.ID
	})
	seen := make(map[string]struct{}, len(matches))
	output := make([]Match, 0, min(len(matches), c.maxResults))
	for _, match := range matches {
		identity := mediaIdentity(match.Media)
		if _, exists := seen[identity]; exists {
			continue
		}
		seen[identity] = struct{}{}
		output = append(output, match)
		if len(output) == c.maxResults {
			break
		}
	}
	return output
}

func (s *providerState) beforeCall(ctx context.Context) error {
	now := time.Now()
	s.mu.Lock()
	if now.Before(s.backoffUntil) {
		remaining := time.Until(s.backoffUntil).Round(time.Millisecond)
		s.mu.Unlock()
		return fmt.Errorf("元数据来源退避中，剩余 %s", remaining)
	}
	wait := time.Until(s.nextRequest)
	if wait < 0 {
		wait = 0
	}
	s.nextRequest = now.Add(wait + s.minInterval)
	s.mu.Unlock()
	if wait == 0 {
		return nil
	}
	timer := time.NewTimer(wait)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

func (s *providerState) afterCall(err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err == nil {
		s.failures = 0
		s.backoffUntil = time.Time{}
		return
	}
	if s.failures < 10 {
		s.failures++
	}
	delay := time.Second << (s.failures - 1)
	if delay > s.maxBackoff {
		delay = s.maxBackoff
	}
	s.backoffUntil = time.Now().Add(delay)
}

func scoreMedia(query Query, item Media) int {
	keyword := normalizeText(query.Keyword)
	compactKeyword := strings.ReplaceAll(keyword, " ", "")
	score := 0
	for _, candidate := range append([]string{item.Title, item.OriginalTitle}, item.Aliases...) {
		normalized := normalizeText(candidate)
		if normalized == "" {
			continue
		}
		compact := strings.ReplaceAll(normalized, " ", "")
		switch {
		case normalized == keyword || compact == compactKeyword:
			score = max(score, 100)
		case strings.Contains(normalized, keyword) || strings.Contains(keyword, normalized):
			score = max(score, 75)
		case compact != "" && (strings.Contains(compact, compactKeyword) || strings.Contains(compactKeyword, compact)):
			score = max(score, 70)
		}
	}
	if query.Year > 0 {
		if item.Year == query.Year {
			score += 20
		} else if item.Year > 0 {
			score -= 15
		}
	}
	if query.Type != "" && normalizeMediaType(item.Type) == query.Type {
		score += 10
	}
	return score
}

func typeMatches(want, actual string) bool {
	return want == "" || normalizeMediaType(actual) == "" || normalizeMediaType(actual) == want
}

func normalizeMediaType(value string) string {
	switch strings.ToLower(strings.TrimSpace(value)) {
	case "movie", "电影":
		return "movie"
	case "tv", "电视剧", "电视", "剧集", "anime":
		return "tv"
	default:
		return ""
	}
}

func normalizeSource(value string) string {
	switch strings.ToLower(strings.TrimSpace(value)) {
	case "tmdb", "themoviedb":
		return "themoviedb"
	case "douban", "bangumi", "anilist":
		return strings.ToLower(strings.TrimSpace(value))
	default:
		return strings.ToLower(strings.TrimSpace(value))
	}
}

func normalizeText(value string) string {
	var builder strings.Builder
	builder.Grow(len(value))
	space := false
	for _, r := range strings.ToLower(strings.TrimSpace(value)) {
		if unicode.IsLetter(r) || unicode.IsDigit(r) {
			builder.WriteRune(r)
			space = false
		} else if !space && builder.Len() > 0 {
			builder.WriteByte(' ')
			space = true
		}
	}
	return strings.TrimSpace(builder.String())
}

func limitMedia(items []Media, limit int) []Media {
	seen := make(map[string]struct{}, min(len(items), limit))
	output := make([]Media, 0, min(len(items), limit))
	for _, item := range items {
		identity := mediaIdentity(item)
		if _, exists := seen[identity]; exists {
			continue
		}
		seen[identity] = struct{}{}
		output = append(output, item)
		if len(output) == limit {
			break
		}
	}
	return output
}

func metadataCacheKey(operation, source, value string) string {
	digest := sha256.Sum256([]byte(value))
	return fmt.Sprintf("metadata:v1:%s:%s:%x", operation, source, digest[:16])
}

func mediaIdentity(item Media) string {
	if item.ID != "" {
		return normalizeSource(item.Source) + ":" + item.ID
	}
	return normalizeSource(item.Source) + ":" + normalizeText(item.Title) + fmt.Sprintf(":%d", item.Year)
}

func joinProviderErrors(items map[string]error) error {
	names := make([]string, 0, len(items))
	for name := range items {
		names = append(names, name)
	}
	sort.Strings(names)
	errs := make([]error, 0, len(names))
	for _, name := range names {
		errs = append(errs, fmt.Errorf("%s: %w", name, items[name]))
	}
	return errors.Join(errs...)
}
