//go:build integration

package database

import (
	"context"
	"errors"
	"os"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/jackc/pgx/v5/stdlib"
	"golang.org/x/sync/errgroup"

	appconfig "github.com/moviepilot-go/moviepilot-go/internal/config"
)

const (
	siteStatisticsTestDatabaseEnv = "MOVIEPILOT_TEST_DATABASE_URL"
	siteStatisticsSchemaPrefix    = "moviepilot_site_statistics_it_"
)

func TestRecordSiteTestPostgresConcurrentSameDomain(t *testing.T) {
	store := newSiteStatisticsIntegrationStore(t)

	const (
		total        = 64
		successCount = 40
	)
	domains := []string{
		" concurrent.example ",
		"Concurrent.Example",
		"\tCONCURRENT.EXAMPLE\n",
	}

	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()
	start := make(chan struct{})
	var group errgroup.Group
	for index := range total {
		index := index
		group.Go(func() error {
			<-start
			success := index < successCount
			return store.RecordSiteTest(ctx, domains[index%len(domains)], success, 2*time.Second)
		})
	}
	close(start)
	if err := group.Wait(); err != nil {
		t.Fatalf("并发记录同域站点测试: %v", err)
	}

	statistic, found, err := store.SiteStatisticByDomain(t.Context(), " CONCURRENT.example ")
	if err != nil {
		t.Fatalf("读取并发统计: %v", err)
	}
	if !found {
		t.Fatal("并发统计记录不存在")
	}
	if statistic.Domain != "concurrent.example" {
		t.Fatalf("规范化域名 = %q, want %q", statistic.Domain, "concurrent.example")
	}
	if statistic.Success != successCount || statistic.Fail != total-successCount {
		t.Fatalf("并发计数 = success %d, fail %d; want success %d, fail %d",
			statistic.Success, statistic.Fail, successCount, total-successCount)
	}
	if statistic.Seconds != 2 {
		t.Fatalf("并发成功样本平均耗时 = %d, want 2", statistic.Seconds)
	}
	if len(statistic.Note) != maxSiteTimingSamples {
		t.Fatalf("并发成功样本数 = %d, want %d", len(statistic.Note), maxSiteTimingSamples)
	}
	for key, seconds := range statistic.Note {
		if seconds != 2 {
			t.Fatalf("并发成功样本 %q 耗时 = %d, want 2", key, seconds)
		}
	}

	statistics, err := store.ListSiteStatistics(t.Context())
	if err != nil {
		t.Fatalf("列出并发统计: %v", err)
	}
	if len(statistics) != 1 {
		t.Fatalf("规范化后的统计行数 = %d, want 1", len(statistics))
	}
}

func TestRecordSiteTestPostgresResultState(t *testing.T) {
	store := newSiteStatisticsIntegrationStore(t)
	const domain = "state.example"

	if err := store.RecordSiteTest(t.Context(), domain, true, 4*time.Second); err != nil {
		t.Fatalf("记录成功结果: %v", err)
	}
	afterSuccess := mustSiteStatistic(t, store, domain)
	if afterSuccess.Success != 1 || afterSuccess.Fail != 0 || afterSuccess.LastState != 0 {
		t.Fatalf("成功状态 = %#v", afterSuccess)
	}
	if afterSuccess.Seconds != 4 || len(afterSuccess.Note) != 1 {
		t.Fatalf("成功耗时 = seconds %d, note %#v; want seconds 4 and one sample",
			afterSuccess.Seconds, afterSuccess.Note)
	}
	if afterSuccess.LastModified == "" {
		t.Fatal("成功结果未更新最后修改时间")
	}

	if err := store.RecordSiteTest(t.Context(), domain, false, 99*time.Second); err != nil {
		t.Fatalf("记录失败结果: %v", err)
	}
	afterFailure := mustSiteStatistic(t, store, domain)
	if afterFailure.Success != 1 || afterFailure.Fail != 1 || afterFailure.LastState != 1 {
		t.Fatalf("失败状态 = %#v", afterFailure)
	}
	if afterFailure.Seconds != afterSuccess.Seconds {
		t.Fatalf("失败后平均耗时 = %d, want unchanged %d", afterFailure.Seconds, afterSuccess.Seconds)
	}
	if !reflect.DeepEqual(afterFailure.Note, afterSuccess.Note) {
		t.Fatalf("失败修改了成功耗时样本: got %#v, want %#v", afterFailure.Note, afterSuccess.Note)
	}

	if err := store.RecordSiteTest(t.Context(), domain, true, 2*time.Second); err != nil {
		t.Fatalf("再次记录成功结果: %v", err)
	}
	afterRecovery := mustSiteStatistic(t, store, domain)
	if afterRecovery.Success != 2 || afterRecovery.Fail != 1 || afterRecovery.LastState != 0 {
		t.Fatalf("恢复成功状态 = %#v", afterRecovery)
	}
	if afterRecovery.Seconds != 3 || len(afterRecovery.Note) != 2 {
		t.Fatalf("恢复后耗时 = seconds %d, note %#v; want seconds 3 and two samples",
			afterRecovery.Seconds, afterRecovery.Note)
	}
}

func TestSiteStatisticsPostgresNormalizationSortingAndMissing(t *testing.T) {
	store := newSiteStatisticsIntegrationStore(t)

	inputs := []string{
		" Zeta.Example ",
		"alpha.example",
		"  Beta.EXAMPLE  ",
	}
	for _, domain := range inputs {
		if err := store.RecordSiteTest(t.Context(), domain, true, time.Second); err != nil {
			t.Fatalf("记录域名 %q: %v", domain, err)
		}
	}

	statistics, err := store.ListSiteStatistics(t.Context())
	if err != nil {
		t.Fatalf("列出站点统计: %v", err)
	}
	wantDomains := []string{"alpha.example", "beta.example", "zeta.example"}
	if len(statistics) != len(wantDomains) {
		t.Fatalf("统计行数 = %d, want %d", len(statistics), len(wantDomains))
	}
	for index, want := range wantDomains {
		if statistics[index].Domain != want {
			t.Fatalf("排序后的域名[%d] = %q, want %q", index, statistics[index].Domain, want)
		}
	}

	statistic, found, err := store.SiteStatisticByDomain(t.Context(), "\tBETA.example ")
	if err != nil {
		t.Fatalf("按规范化域名查询: %v", err)
	}
	if !found || statistic.Domain != "beta.example" {
		t.Fatalf("规范化查询 = found %v, statistic %#v", found, statistic)
	}

	missing, found, err := store.SiteStatisticByDomain(t.Context(), " Missing.EXAMPLE ")
	if err != nil {
		t.Fatalf("查询不存在域名: %v", err)
	}
	if found {
		t.Fatalf("不存在域名意外返回记录: %#v", missing)
	}
	if missing.Domain != "missing.example" || missing.Success != 0 || missing.Fail != 0 || missing.Seconds != 0 {
		t.Fatalf("不存在域名返回值 = %#v", missing)
	}
}

func TestRecordSiteTestPostgresPreCanceledContext(t *testing.T) {
	store := newSiteStatisticsIntegrationStore(t)
	const baselineDomain = "baseline.example"

	if err := store.RecordSiteTest(t.Context(), baselineDomain, true, 3*time.Second); err != nil {
		t.Fatalf("写入基线统计: %v", err)
	}
	baseline := mustSiteStatistic(t, store, baselineDomain)

	canceledCtx, cancel := context.WithCancel(t.Context())
	cancel()
	if err := store.RecordSiteTest(canceledCtx, baselineDomain, false, 50*time.Second); !errors.Is(err, context.Canceled) {
		t.Fatalf("预取消上下文更新错误 = %v, want context.Canceled", err)
	}
	if err := store.RecordSiteTest(canceledCtx, "new-after-cancel.example", true, time.Second); !errors.Is(err, context.Canceled) {
		t.Fatalf("预取消上下文插入错误 = %v, want context.Canceled", err)
	}

	afterCanceled := mustSiteStatistic(t, store, baselineDomain)
	if !reflect.DeepEqual(afterCanceled, baseline) {
		t.Fatalf("预取消上下文留下部分更新: got %#v, want %#v", afterCanceled, baseline)
	}
	missing, found, err := store.SiteStatisticByDomain(t.Context(), "new-after-cancel.example")
	if err != nil {
		t.Fatalf("查询预取消插入结果: %v", err)
	}
	if found {
		t.Fatalf("预取消上下文产生了新记录: %#v", missing)
	}

	if err := store.RecordSiteTest(t.Context(), baselineDomain, false, time.Second); err != nil {
		t.Fatalf("预取消操作后再次写入: %v", err)
	}
	afterValidWrite := mustSiteStatistic(t, store, baselineDomain)
	if afterValidWrite.Success != 1 || afterValidWrite.Fail != 1 || afterValidWrite.LastState != 1 {
		t.Fatalf("预取消操作后事务状态异常: %#v", afterValidWrite)
	}
	if afterValidWrite.Seconds != baseline.Seconds || !reflect.DeepEqual(afterValidWrite.Note, baseline.Note) {
		t.Fatalf("有效失败写入修改了耗时: got %#v, baseline %#v", afterValidWrite, baseline)
	}
}

func newSiteStatisticsIntegrationStore(t *testing.T) *Store {
	t.Helper()

	databaseURL := strings.TrimSpace(os.Getenv(siteStatisticsTestDatabaseEnv))
	if databaseURL == "" {
		t.Skipf("未设置 %s，跳过 PostgreSQL 集成测试", siteStatisticsTestDatabaseEnv)
	}

	setupCtx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()

	adminConfig, err := pgxpool.ParseConfig(databaseURL)
	if err != nil {
		t.Fatalf("解析 %s: %v", siteStatisticsTestDatabaseEnv, err)
	}
	adminConfig.MaxConns = 2
	adminConfig.MinConns = 0
	adminConfig.ConnConfig.RuntimeParams["application_name"] = "moviepilot-site-statistics-integration-admin"
	adminPool, err := pgxpool.NewWithConfig(setupCtx, adminConfig)
	if err != nil {
		t.Fatalf("创建 PostgreSQL 管理连接池: %v", err)
	}
	t.Cleanup(adminPool.Close)
	if err := adminPool.Ping(setupCtx); err != nil {
		t.Fatalf("连接 PostgreSQL 集成测试数据库: %v", err)
	}

	schema := siteStatisticsSchemaPrefix + strings.ReplaceAll(uuid.NewString(), "-", "")
	quotedSchema := pgx.Identifier{schema}.Sanitize()
	if _, err := adminPool.Exec(setupCtx, "CREATE SCHEMA "+quotedSchema); err != nil {
		t.Fatalf("创建隔离 schema %q: %v", schema, err)
	}

	var storePool *pgxpool.Pool
	t.Cleanup(func() {
		if storePool != nil {
			storePool.Close()
		}
		cleanupCtx, cleanupCancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cleanupCancel()
		if _, err := adminPool.Exec(cleanupCtx, "DROP SCHEMA "+quotedSchema+" CASCADE"); err != nil {
			t.Errorf("清理隔离 schema %q: %v", schema, err)
		}
	})

	migrationConfig, err := pgx.ParseConfig(databaseURL)
	if err != nil {
		t.Fatalf("解析迁移连接: %v", err)
	}
	migrationConfig.RuntimeParams["search_path"] = schema
	migrationConfig.RuntimeParams["application_name"] = "moviepilot-site-statistics-integration-migrate"
	migrationConnectionString := stdlib.RegisterConnConfig(migrationConfig)
	t.Cleanup(func() {
		stdlib.UnregisterConnConfig(migrationConnectionString)
	})
	if err := Migrate(setupCtx, appconfig.Config{DatabaseURL: migrationConnectionString}); err != nil {
		t.Fatalf("在隔离 schema %q 执行迁移: %v", schema, err)
	}

	poolConfig, err := pgxpool.ParseConfig(databaseURL)
	if err != nil {
		t.Fatalf("解析业务连接池配置: %v", err)
	}
	poolConfig.MaxConns = 16
	poolConfig.MinConns = 0
	poolConfig.ConnConfig.RuntimeParams["search_path"] = schema
	poolConfig.ConnConfig.RuntimeParams["application_name"] = "moviepilot-site-statistics-integration-store"
	storePool, err = pgxpool.NewWithConfig(setupCtx, poolConfig)
	if err != nil {
		t.Fatalf("创建隔离业务连接池: %v", err)
	}
	if err := storePool.Ping(setupCtx); err != nil {
		t.Fatalf("连接隔离 schema %q: %v", schema, err)
	}

	var currentSchema string
	if err := storePool.QueryRow(setupCtx, "SELECT current_schema()").Scan(&currentSchema); err != nil {
		t.Fatalf("校验当前 schema: %v", err)
	}
	if currentSchema != schema {
		t.Fatalf("业务连接当前 schema = %q, want %q", currentSchema, schema)
	}

	return NewStore(&DB{Pool: storePool}, nil)
}

func mustSiteStatistic(t *testing.T, store *Store, domain string) SiteStatistic {
	t.Helper()
	statistic, found, err := store.SiteStatisticByDomain(t.Context(), domain)
	if err != nil {
		t.Fatalf("查询站点统计 %q: %v", domain, err)
	}
	if !found {
		t.Fatalf("站点统计 %q 不存在", domain)
	}
	return statistic
}
