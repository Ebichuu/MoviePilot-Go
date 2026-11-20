package httpapi

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"reflect"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/moviepilot-go/moviepilot-go/internal/database"
	"github.com/moviepilot-go/moviepilot-go/internal/indexer"
)

const siteProbeTestWaitTimeout = time.Second

type recordedSiteStatistic struct {
	domain     string
	success    bool
	elapsed    time.Duration
	contextErr error
}

type fakeSiteStatisticStore struct {
	mu                 sync.Mutex
	records            []recordedSiteStatistic
	statistics         []database.SiteStatistic
	statisticsByDomain map[string]database.SiteStatistic
	queriedDomains     []string
	recordErr          error
	listErr            error
	lookupErr          error
}

func (s *fakeSiteStatisticStore) RecordSiteTest(ctx context.Context, domain string, success bool, elapsed time.Duration) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.records = append(s.records, recordedSiteStatistic{
		domain: domain, success: success, elapsed: elapsed, contextErr: ctx.Err(),
	})
	return s.recordErr
}

func (s *fakeSiteStatisticStore) ListSiteStatistics(context.Context) ([]database.SiteStatistic, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.listErr != nil {
		return nil, s.listErr
	}
	return append([]database.SiteStatistic(nil), s.statistics...), nil
}

func (s *fakeSiteStatisticStore) SiteStatisticByDomain(_ context.Context, domain string) (database.SiteStatistic, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.queriedDomains = append(s.queriedDomains, domain)
	if s.lookupErr != nil {
		return database.SiteStatistic{}, false, s.lookupErr
	}
	statistic, found := s.statisticsByDomain[domain]
	if !found {
		return database.SiteStatistic{Domain: domain, Note: map[string]int{}}, false, nil
	}
	return statistic, true, nil
}

func (s *fakeSiteStatisticStore) recorded() []recordedSiteStatistic {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]recordedSiteStatistic(nil), s.records...)
}

func (s *fakeSiteStatisticStore) queried() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.queriedDomains...)
}

func TestSiteProbeRecordsStatisticsWithoutChangingProbeResult(t *testing.T) {
	tests := []struct {
		name        string
		site        database.Site
		probeErr    error
		recordErr   error
		wantSuccess bool
		wantMessage string
		wantDomain  string
	}{
		{
			name: "成功使用持久化域名",
			site: database.Site{
				ID: 7, Name: "Fixture", Domain: " Fixture.EXAMPLE. ", URL: "https://ignored.example", Adapter: "fixture", Enabled: true,
			},
			wantSuccess: true,
			wantMessage: "连接成功",
			wantDomain:  "fixture.example",
		},
		{
			name: "失败从站点地址推导域名",
			site: database.Site{
				ID: 7, Name: "Fixture", URL: "https://Fallback.EXAMPLE./path", Adapter: "fixture", Enabled: true,
			},
			probeErr:    errors.New("upstream cookie=session-secret"),
			wantMessage: "站点连接失败",
			wantDomain:  "fallback.example",
		},
		{
			name: "统计写入失败不覆盖成功响应",
			site: database.Site{
				ID: 7, Name: "Fixture", Domain: "fixture.example", URL: "https://fixture.example", Adapter: "fixture", Enabled: true,
			},
			recordErr:   errors.New("database unavailable"),
			wantSuccess: true,
			wantMessage: "连接成功",
			wantDomain:  "fixture.example",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			coordinator, err := indexer.NewCoordinator(1)
			if err != nil {
				t.Fatal(err)
			}
			defer coordinator.Close()

			statistics := &fakeSiteStatisticStore{recordErr: tt.recordErr}
			deps := Dependencies{
				Sites:          &fakeSiteStore{sites: map[int]database.Site{tt.site.ID: tt.site}},
				SiteStatistics: statistics,
				Search:         coordinator,
				SearchAdapters: []indexer.Adapter{siteProbeTestAdapter{id: tt.site.ID, name: tt.site.Name, probe: func(context.Context) error {
					return tt.probeErr
				}}},
			}
			router := chi.NewRouter()
			deps.registerSiteCompatibility(router)

			response := httptest.NewRecorder()
			router.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/site/test/7", nil))

			var result Response
			if err := json.Unmarshal(response.Body.Bytes(), &result); err != nil {
				t.Fatal(err)
			}
			if response.Code != http.StatusOK || result.Success != tt.wantSuccess || result.Message != tt.wantMessage {
				t.Fatalf("探测响应不匹配: status=%d result=%#v", response.Code, result)
			}
			records := statistics.recorded()
			if len(records) != 1 {
				t.Fatalf("统计记录数=%d, want 1", len(records))
			}
			if records[0].domain != tt.wantDomain || records[0].success != tt.wantSuccess || records[0].contextErr != nil {
				t.Fatalf("统计记录不匹配: %#v", records[0])
			}
			if records[0].elapsed < 0 {
				t.Fatalf("探测耗时为负数: %s", records[0].elapsed)
			}
		})
	}
}

func TestSiteProbeRequestCancellationDoesNotRecordSiteFailure(t *testing.T) {
	coordinator, err := indexer.NewCoordinator(1)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(coordinator.Close)

	probeStarted := make(chan struct{})
	adapter := siteProbeTestAdapter{id: 8, name: "Cancelled", probe: func(ctx context.Context) error {
		close(probeStarted)
		<-ctx.Done()
		return ctx.Err()
	}}
	statistics := &fakeSiteStatisticStore{}
	deps := Dependencies{
		Sites: &fakeSiteStore{sites: map[int]database.Site{8: {
			ID: 8, Name: "Cancelled", Domain: "cancelled.example", URL: "https://cancelled.example", Adapter: "fixture", Enabled: true,
		}}},
		SiteStatistics: statistics,
		Search:         coordinator,
		SearchAdapters: []indexer.Adapter{adapter},
	}
	router := chi.NewRouter()
	deps.registerSiteCompatibility(router)

	requestContext, cancelRequest := context.WithCancel(t.Context())
	t.Cleanup(cancelRequest)
	request := httptest.NewRequest(http.MethodGet, "/site/test/8", nil).WithContext(requestContext)
	cancelDone := make(chan struct{})
	var cancelErr error
	go func() {
		defer close(cancelDone)
		timer := time.NewTimer(siteProbeTestWaitTimeout)
		defer timer.Stop()
		select {
		case <-probeStarted:
		case <-timer.C:
			cancelErr = errors.New("站点探测未在超时内开始")
		case <-t.Context().Done():
			cancelErr = t.Context().Err()
		}
		cancelRequest()
	}()
	response := httptest.NewRecorder()
	router.ServeHTTP(response, request)
	waitForSiteProbeTestSignal(t, cancelDone, "取消请求的 goroutine 未结束")
	if cancelErr != nil {
		t.Fatal(cancelErr)
	}

	var result Response
	if err := json.Unmarshal(response.Body.Bytes(), &result); err != nil {
		t.Fatal(err)
	}
	if response.Code != http.StatusOK || result.Success || result.Message != "连接测试已取消" {
		t.Fatalf("取消响应不匹配: status=%d result=%#v", response.Code, result)
	}
	if records := statistics.recorded(); len(records) != 0 {
		t.Fatalf("请求取消被错误记录为站点失败: %#v", records)
	}
}

func TestSiteProbeStatisticWriteUsesDetachedContext(t *testing.T) {
	requestContext, cancelRequest := context.WithCancel(context.Background())
	cancelRequest()
	statistics := &fakeSiteStatisticStore{}
	deps := Dependencies{SiteStatistics: statistics}
	request := httptest.NewRequest(http.MethodGet, "/site/test/10", nil).WithContext(requestContext)
	deps.recordSiteTestStatistic(request, database.Site{
		ID: 10, Domain: "detached.example", URL: "https://detached.example",
	}, false, time.Second)

	records := statistics.recorded()
	if len(records) != 1 || records[0].success || records[0].contextErr != nil {
		t.Fatalf("统计写入未脱离已取消的请求上下文: %#v", records)
	}
}

func TestSiteProbeDoesNotRecordLocalBackpressureAsSiteFailure(t *testing.T) {
	coordinator, err := indexer.NewCoordinator(2)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(coordinator.Close)

	probeStarted := make(chan struct{})
	releaseProbe := make(chan struct{})
	var releaseProbeOnce sync.Once
	finishProbe := func() {
		releaseProbeOnce.Do(func() { close(releaseProbe) })
	}
	t.Cleanup(finishProbe)
	adapter := siteProbeTestAdapter{id: 9, name: "Busy", probe: func(ctx context.Context) error {
		close(probeStarted)
		select {
		case <-releaseProbe:
			return nil
		case <-ctx.Done():
			return ctx.Err()
		}
	}}
	statistics := &fakeSiteStatisticStore{}
	deps := Dependencies{
		Sites: &fakeSiteStore{sites: map[int]database.Site{9: {
			ID: 9, Name: "Busy", Domain: "busy.example", URL: "https://busy.example", Adapter: "fixture", Enabled: true,
		}}},
		SiteStatistics: statistics,
		Search:         coordinator,
		SearchAdapters: []indexer.Adapter{adapter},
	}
	router := chi.NewRouter()
	deps.registerSiteCompatibility(router)

	firstResponse := httptest.NewRecorder()
	firstDone := make(chan struct{})
	go func() {
		defer close(firstDone)
		request := httptest.NewRequest(http.MethodGet, "/site/test/9", nil).WithContext(t.Context())
		router.ServeHTTP(firstResponse, request)
	}()
	waitForSiteProbeTestSignal(t, probeStarted, "首个站点探测未开始")

	secondResponse := httptest.NewRecorder()
	router.ServeHTTP(secondResponse, httptest.NewRequest(http.MethodGet, "/site/test/9", nil))
	var secondResult Response
	if err := json.Unmarshal(secondResponse.Body.Bytes(), &secondResult); err != nil {
		t.Fatal(err)
	}
	if secondResponse.Code != http.StatusOK || secondResult.Success || secondResult.Message != "站点正在执行其他请求" {
		t.Fatalf("背压响应不匹配: status=%d result=%#v", secondResponse.Code, secondResult)
	}
	if records := statistics.recorded(); len(records) != 0 {
		t.Fatalf("本地背压被错误记录为站点失败: %#v", records)
	}

	finishProbe()
	waitForSiteProbeTestSignal(t, firstDone, "首个站点探测未结束")
	if records := statistics.recorded(); len(records) != 1 || !records[0].success {
		t.Fatalf("真实探测统计不匹配: %#v", records)
	}
}

func TestSiteProbeBuildsAdapterFromCurrentDatabaseSnapshot(t *testing.T) {
	coordinator, err := indexer.NewCoordinator(1)
	if err != nil {
		t.Fatal(err)
	}
	defer coordinator.Close()

	var staleCalls atomic.Int32
	var currentCalls atomic.Int32
	statistics := &fakeSiteStatisticStore{}
	site := database.Site{
		ID: 11, Name: "Current", Domain: "current.example", URL: "https://current.example", Adapter: "fixture", Enabled: true,
	}
	deps := Dependencies{
		Sites:          &fakeSiteStore{sites: map[int]database.Site{site.ID: site}},
		SiteStatistics: statistics,
		Search:         coordinator,
		SearchAdapters: []indexer.Adapter{siteProbeTestAdapter{id: site.ID, name: "Stale", probe: func(context.Context) error {
			staleCalls.Add(1)
			return nil
		}}},
		BuildSiteAdapter: func(got database.Site) (indexer.Adapter, error) {
			if got.Domain != site.Domain || got.URL != site.URL {
				t.Fatalf("构建器未收到当前数据库快照: %#v", got)
			}
			return siteProbeTestAdapter{id: got.ID, name: got.Name, probe: func(context.Context) error {
				currentCalls.Add(1)
				return nil
			}}, nil
		},
	}
	router := chi.NewRouter()
	deps.registerSiteCompatibility(router)
	response := httptest.NewRecorder()
	router.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/site/test/11", nil))

	if response.Code != http.StatusOK || staleCalls.Load() != 0 || currentCalls.Load() != 1 {
		t.Fatalf("探测未使用当前数据库适配器: status=%d stale=%d current=%d body=%s",
			response.Code, staleCalls.Load(), currentCalls.Load(), response.Body.String())
	}
	records := statistics.recorded()
	if len(records) != 1 || records[0].domain != "current.example" || !records[0].success {
		t.Fatalf("当前数据库快照统计不匹配: %#v", records)
	}
}

func TestSiteStatisticHandlers(t *testing.T) {
	want := database.SiteStatistic{
		Domain: "fixture.example", Success: 3, Fail: 1, Seconds: 2,
		LastState: 0, LastModified: "2026-08-11 12:00:00",
		Note: map[string]int{"2026-08-11 12:00:00": 2},
	}
	statistics := &fakeSiteStatisticStore{
		statistics:         []database.SiteStatistic{want},
		statisticsByDomain: map[string]database.SiteStatistic{"fixture.example": want},
	}
	deps := Dependencies{SiteStatistics: statistics}
	router := chi.NewRouter()
	deps.registerSiteCompatibility(router)

	listResponse := httptest.NewRecorder()
	router.ServeHTTP(listResponse, httptest.NewRequest(http.MethodGet, "/site/statistic", nil))
	var list []database.SiteStatistic
	if err := json.Unmarshal(listResponse.Body.Bytes(), &list); err != nil {
		t.Fatal(err)
	}
	if listResponse.Code != http.StatusOK || !reflect.DeepEqual(list, []database.SiteStatistic{want}) {
		t.Fatalf("统计列表不匹配: status=%d list=%#v", listResponse.Code, list)
	}

	detailResponse := httptest.NewRecorder()
	router.ServeHTTP(detailResponse, httptest.NewRequest(http.MethodGet, "/site/statistic/Fixture.EXAMPLE.", nil))
	var detail database.SiteStatistic
	if err := json.Unmarshal(detailResponse.Body.Bytes(), &detail); err != nil {
		t.Fatal(err)
	}
	if detailResponse.Code != http.StatusOK || !reflect.DeepEqual(detail, want) {
		t.Fatalf("单站统计不匹配: status=%d detail=%#v", detailResponse.Code, detail)
	}
	if got := statistics.queried(); !reflect.DeepEqual(got, []string{"fixture.example"}) {
		t.Fatalf("查询域名未规范化: %#v", got)
	}

	missingResponse := httptest.NewRecorder()
	router.ServeHTTP(missingResponse, httptest.NewRequest(http.MethodGet, "/site/statistic/Missing.EXAMPLE.", nil))
	var missing map[string]json.RawMessage
	if err := json.Unmarshal(missingResponse.Body.Bytes(), &missing); err != nil {
		t.Fatal(err)
	}
	if missingResponse.Code != http.StatusOK || string(missing["domain"]) != `"missing.example"` ||
		string(missing["success"]) != "0" || string(missing["fail"]) != "0" ||
		string(missing["lst_mod_date"]) != "null" || string(missing["note"]) != "null" {
		t.Fatalf("缺失统计的兼容响应不匹配: status=%d detail=%s", missingResponse.Code, missingResponse.Body.String())
	}
}

func TestSiteStatisticHandlersReportStoreErrors(t *testing.T) {
	tests := []struct {
		name  string
		path  string
		store *fakeSiteStatisticStore
	}{
		{name: "列表查询失败", path: "/site/statistic", store: &fakeSiteStatisticStore{listErr: errors.New("database unavailable")}},
		{name: "单站查询失败", path: "/site/statistic/fixture.example", store: &fakeSiteStatisticStore{lookupErr: errors.New("database unavailable")}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			deps := Dependencies{SiteStatistics: tt.store}
			router := chi.NewRouter()
			deps.registerSiteCompatibility(router)
			response := httptest.NewRecorder()
			router.ServeHTTP(response, httptest.NewRequest(http.MethodGet, tt.path, nil))
			if response.Code != http.StatusInternalServerError {
				t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
			}
		})
	}
}

func TestSiteStatisticDomainParam(t *testing.T) {
	tests := []struct {
		name string
		raw  string
		want string
	}{
		{name: "普通域名", raw: " Example.COM. ", want: "example.com"},
		{name: "完整地址", raw: "https://Example.COM./path?q=1", want: "example.com"},
		{name: "路径转义地址", raw: "https%3A%2F%2FExample.COM.%2Fpath", want: "example.com"},
		{name: "空值", raw: " ", want: ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if got := siteStatisticDomainParam(tt.raw); got != tt.want {
				t.Fatalf("siteStatisticDomainParam(%q)=%q, want %q", tt.raw, got, tt.want)
			}
		})
	}
}

func waitForSiteProbeTestSignal(t *testing.T, signal <-chan struct{}, timeoutMessage string) {
	t.Helper()
	timer := time.NewTimer(siteProbeTestWaitTimeout)
	defer timer.Stop()
	select {
	case <-signal:
	case <-timer.C:
		t.Fatal(timeoutMessage)
	}
}
