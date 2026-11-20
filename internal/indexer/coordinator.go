package indexer

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strconv"
	"sync"
	"time"
)

// Query 是跨站点搜索的规范化输入；Page 与 V2 一致，从 0 开始。
type Query struct {
	Keyword string
	Page    int
	Limit   int
}

// Resource 是站点结果的统一模型。
type Resource struct {
	SiteID               int       `json:"site_id,omitempty"`
	Site                 string    `json:"site"`
	ItemKey              string    `json:"-"`
	Title                string    `json:"title"`
	Description          string    `json:"description,omitempty"`
	DetailURL            string    `json:"detail_url"`
	DownloadURL          string    `json:"download_url"`
	SizeBytes            int64     `json:"size_bytes"`
	Seeders              int       `json:"seeders"`
	PriOrder             int       `json:"pri_order,omitempty"`
	SiteOrder            int       `json:"site_order,omitempty"`
	Peers                int       `json:"peers"`
	Grabs                int       `json:"grabs"`
	PublishedAt          time.Time `json:"published_at"`
	IMDBID               string    `json:"imdbid,omitempty"`
	Labels               []string  `json:"labels,omitempty"`
	Category             string    `json:"category,omitempty"`
	FreeDate             time.Time `json:"freedate,omitempty"`
	DownloadVolumeFactor *float64  `json:"downloadvolumefactor,omitempty"`
	UploadVolumeFactor   *float64  `json:"uploadvolumefactor,omitempty"`
	HitAndRun            bool      `json:"hit_and_run,omitempty"`
	TorrentID            string    `json:"-"`
	DownloadID           string    `json:"-"`
	// RecognizedMediaTitle is the compact V2 RSS context fact used only by the
	// subscription matcher. It is intentionally excluded from public resource
	// JSON; RSS persistence adds it under its private cache key.
	RecognizedMediaTitle string `json:"-"`
}

// ResolvedDownload 是二阶段下载解析后的有界种子文件。
type ResolvedDownload struct {
	Body        []byte
	ContentType string
	Filename    string
}

// DownloadResolver 由需要临时令牌或专用鉴权的站点实现。
type DownloadResolver interface {
	ResolveDownload(ctx context.Context, resourceID string) (ResolvedDownload, error)
}

// VersionedDownloadResolver 为持久任务提供稳定的解析器种类和数据版本。
// 版本变化时旧执行计划必须显式迁移，不能把历史密文交给新语义解析。
type VersionedDownloadResolver interface {
	DownloadResolver
	ResolverKind() string
	ResolverVersion() int16
}

const downloadResolverVersionV1 int16 = 1

// Adapter 是声明式解析器和特殊站点适配器共同实现的接口。
type Adapter interface {
	Name() string
	Search(ctx context.Context, query Query) ([]Resource, error)
}

// SiteIDAdapter 是需要按 V2 站点 ID 筛选时由适配器选择实现的接口。
type SiteIDAdapter interface {
	SiteID() int
}

// Event 是流式搜索产生的有界事件。
type Event struct {
	Site      string     `json:"site"`
	Resources []Resource `json:"resources,omitempty"`
	Error     string     `json:"error,omitempty"`
}

// Coordinator 对全局并发、单站点并发和失败退避统一施加硬限制。
type Coordinator struct {
	workers    int
	mu         sync.Mutex
	gates      map[string]chan struct{}
	failures   map[string]failure
	jobs       chan searchJob
	probeSlots chan struct{}
	ctx        context.Context
	cancel     context.CancelFunc
	workerWG   sync.WaitGroup
	closeOnce  sync.Once

	lastMu      sync.RWMutex
	lastResults []Resource
	lastQuery   Query
}

type searchJob struct {
	ctx     context.Context
	adapter Adapter
	query   Query
	result  chan<- Event
}

type failure struct {
	count int
	next  time.Time
}

// NewCoordinator 创建全局受控的搜索协调器。
func NewCoordinator(workers int) (*Coordinator, error) {
	if workers < 1 || workers > 128 {
		return nil, errors.New("搜索 worker 数必须在 1 到 128 之间")
	}
	ctx, cancel := context.WithCancel(context.Background())
	coordinator := &Coordinator{
		workers: workers, gates: make(map[string]chan struct{}), failures: make(map[string]failure),
		jobs: make(chan searchJob, workers), probeSlots: make(chan struct{}, workers), ctx: ctx, cancel: cancel,
	}
	coordinator.workerWG.Add(workers)
	for range workers {
		go coordinator.runWorker()
	}
	return coordinator, nil
}

// Close 停止全局搜索 worker；调用后不再接受新任务。
func (c *Coordinator) Close() {
	if c == nil {
		return
	}
	c.closeOnce.Do(func() {
		c.cancel()
		c.workerWG.Wait()
	})
}

func (c *Coordinator) runWorker() {
	defer c.workerWG.Done()
	for {
		select {
		case job := <-c.jobs:
			event := c.searchOne(job.ctx, job.adapter, job.query)
			select {
			case job.result <- event:
			case <-job.ctx.Done():
			case <-c.ctx.Done():
				return
			}
		case <-c.ctx.Done():
			return
		}
	}
}

// Search 搜索所有站点并按站点完成顺序回调，回调在单一 goroutine 中执行。
func (c *Coordinator) Search(ctx context.Context, adapters []Adapter, query Query, onEvent func(Event)) ([]Resource, error) {
	if len(adapters) == 0 {
		return []Resource{}, nil
	}
	if query.Page < 0 {
		query.Page = 0
	}
	if query.Limit < 1 || query.Limit > 200 {
		query.Limit = 100
	}

	results := make(chan Event, min(c.workers, len(adapters)))
	all := make([]Resource, 0, len(adapters)*4)
	submitted, completed := 0, 0
	for completed < len(adapters) {
		var jobs chan searchJob
		var job searchJob
		if submitted < len(adapters) {
			jobs = c.jobs
			job = searchJob{ctx: ctx, adapter: adapters[submitted], query: query, result: results}
		}
		select {
		case jobs <- job:
			submitted++
		case event := <-results:
			completed++
			if onEvent != nil {
				onEvent(event)
			}
			all = append(all, event.Resources...)
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-c.ctx.Done():
			return nil, errors.New("搜索协调器已关闭")
		}
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	all = deduplicate(all)
	sort.SliceStable(all, func(i, j int) bool {
		if all[i].Seeders == all[j].Seeders {
			return all[i].PublishedAt.After(all[j].PublishedAt)
		}
		return all[i].Seeders > all[j].Seeders
	})
	c.lastMu.Lock()
	c.lastResults = append([]Resource(nil), all...)
	c.lastQuery = query
	c.lastMu.Unlock()
	return all, nil
}

// LastSearchResults 返回最近一次成功搜索的结果副本（V2 `/search/last`）。
func (c *Coordinator) LastSearchResults() []Resource {
	if c == nil {
		return nil
	}
	c.lastMu.RLock()
	defer c.lastMu.RUnlock()
	return append([]Resource(nil), c.lastResults...)
}

// LastSearchQuery 返回最近一次成功搜索的查询参数（V2 `/search/last/context`）。
func (c *Coordinator) LastSearchQuery() Query {
	if c == nil {
		return Query{}
	}
	c.lastMu.RLock()
	defer c.lastMu.RUnlock()
	return c.lastQuery
}

func (c *Coordinator) searchOne(ctx context.Context, adapter Adapter, query Query) Event {
	name := adapter.Name()
	key := adapterGateKey(adapter)
	gate := c.siteGate(key)
	select {
	case gate <- struct{}{}:
		defer func() { <-gate }()
	case <-ctx.Done():
		return Event{Site: name, Error: ctx.Err().Error()}
	}
	if wait := c.backoff(name); wait > 0 {
		return Event{Site: name, Error: fmt.Sprintf("站点退避中，剩余 %s", wait.Round(time.Second))}
	}
	resources, err := adapter.Search(ctx, query)
	if err != nil {
		if !errors.Is(err, context.Canceled) && !errors.Is(err, context.DeadlineExceeded) {
			c.recordFailure(name)
		}
		return Event{Site: name, Error: err.Error()}
	}
	c.clearFailure(name)
	siteID := 0
	if identified, ok := adapter.(SiteIDAdapter); ok {
		siteID = identified.SiteID()
	}
	for i := range resources {
		resources[i].Site = name
		if resources[i].SiteID == 0 {
			resources[i].SiteID = siteID
		}
	}
	resources = deduplicate(resources)
	if len(resources) > query.Limit {
		resources = resources[:query.Limit]
	}
	return Event{Site: name, Resources: resources}
}

func (c *Coordinator) siteGate(name string) chan struct{} {
	c.mu.Lock()
	defer c.mu.Unlock()
	gate := c.gates[name]
	if gate == nil {
		gate = make(chan struct{}, 1)
		c.gates[name] = gate
	}
	return gate
}

func (c *Coordinator) backoff(name string) time.Duration {
	c.mu.Lock()
	defer c.mu.Unlock()
	remaining := time.Until(c.failures[name].next)
	if remaining < 0 {
		return 0
	}
	return remaining
}

func (c *Coordinator) recordFailure(name string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	state := c.failures[name]
	state.count++
	delay := time.Second * time.Duration(1<<min(state.count-1, 8))
	if delay > 5*time.Minute {
		delay = 5 * time.Minute
	}
	state.next = time.Now().Add(delay)
	c.failures[name] = state
}

func (c *Coordinator) clearFailure(name string) {
	c.mu.Lock()
	delete(c.failures, name)
	c.mu.Unlock()
}

func deduplicate(resources []Resource) []Resource {
	seen := make(map[string]struct{}, len(resources))
	result := make([]Resource, 0, len(resources))
	for _, resource := range resources {
		key := resource.Site + "\x00" + resource.DownloadURL
		if resource.ItemKey != "" {
			key = strconv.Itoa(resource.SiteID) + "\x00" + resource.ItemKey
		}
		if resource.ItemKey == "" && resource.DownloadURL == "" {
			key = resource.Site + "\x00" + resource.DetailURL + "\x00" + resource.Title
		}
		if _, exists := seen[key]; exists {
			continue
		}
		seen[key] = struct{}{}
		result = append(result, resource)
	}
	return result
}
