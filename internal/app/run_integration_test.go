//go:build integration

package app

import (
	"context"
	"net"
	"net/http"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/moviepilot-go/moviepilot-go/internal/config"
)

// freeTCPAddr 预留一个空闲端口再释放，供 Run 的 HTTPAddr 使用：
// 固定端口会和 compose 里的 moviepilot-go 容器抢占。
func freeTCPAddr(t *testing.T) string {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("预留端口: %v", err)
	}
	addr := listener.Addr().String()
	if err := listener.Close(); err != nil {
		t.Fatalf("释放端口: %v", err)
	}
	return addr
}

// waitForRunReady 轮询服务自身的就绪接口，等到 Run 真正开始监听再触发关停。
func waitForRunReady(t *testing.T, addr string, runErrors <-chan error) {
	t.Helper()
	client := &http.Client{Timeout: time.Second}
	paths := []string{"/readyz", "/api/v1/system/readyz"}
	deadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) {
		select {
		case err := <-runErrors:
			t.Fatalf("Run 在监听前退出: %v", err)
		default:
		}
		for _, path := range paths {
			response, err := client.Get("http://" + addr + path)
			if err == nil {
				_ = response.Body.Close()
				if response.StatusCode == http.StatusOK {
					return
				}
			}
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("等待 %s 就绪超时", addr)
}

type runIntegrationFixture struct {
	cfg    config.Config
	admin  *pgxpool.Pool
	schema string
}

// newRunIntegrationFixture 在独立 schema 上准备一份可启动的配置；
// mutate 用于按用例打开可选装配分支（TMDB、存储、站点消息等）。
func newRunIntegrationFixture(t *testing.T, mutate func(*config.Config)) runIntegrationFixture {
	t.Helper()

	databaseURL := strings.TrimSpace(os.Getenv(migrateTestDatabaseEnv))
	if databaseURL == "" {
		t.Skipf("未设置 %s，跳过 PostgreSQL 集成测试", migrateTestDatabaseEnv)
	}

	setupCtx, cancelSetup := context.WithTimeout(t.Context(), 60*time.Second)
	defer cancelSetup()

	adminConfig, err := pgxpool.ParseConfig(databaseURL)
	if err != nil {
		t.Fatalf("解析 %s: %v", migrateTestDatabaseEnv, err)
	}
	adminConfig.MaxConns = 2
	adminPool, err := pgxpool.NewWithConfig(setupCtx, adminConfig)
	if err != nil {
		t.Fatalf("创建 PostgreSQL 管理连接池: %v", err)
	}
	t.Cleanup(adminPool.Close)

	// River 的 insert 通知通道名是 "river_insert_<schema>_<queue>"，
	// schema 超过 ~30 字符就会触发 PostgreSQL 的 channel name too long，
	// 所以这里用短 schema（生产用默认 public 不受影响）。
	schema := "app_run_it_" + strings.ReplaceAll(uuid.NewString(), "-", "")[:10]
	quotedSchema := pgx.Identifier{schema}.Sanitize()
	if _, err := adminPool.Exec(setupCtx, "CREATE SCHEMA "+quotedSchema); err != nil {
		t.Fatalf("创建隔离 schema %q: %v", schema, err)
	}
	t.Cleanup(func() {
		cleanupCtx, cleanupCancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cleanupCancel()
		if _, err := adminPool.Exec(cleanupCtx, "DROP SCHEMA "+quotedSchema+" CASCADE"); err != nil {
			t.Errorf("清理隔离 schema %q: %v", schema, err)
		}
	})

	cfg, err := config.Load()
	if err != nil {
		t.Fatalf("加载配置默认值: %v", err)
	}
	cfg.DatabaseURL = databaseURLWithSearchPath(t, databaseURL, schema)
	cfg.ConfigDir = t.TempDir()
	cfg.MasterKey = "moviepilot-run-integration-master-key"
	cfg.JWTSecret = "moviepilot-run-integration-jwt-secret"
	cfg.ResourceSecret = "moviepilot-run-integration-resource-secret"
	cfg.HTTPAddr = freeTCPAddr(t)
	cfg.ShutdownTimeout = 20 * time.Second
	cfg.BootstrapUsername = "run-integration-admin"
	cfg.BootstrapPassword = "run-integration-password"
	// 不触达外部元数据/通知；Redis 缺失时应以无缓存模式启动。
	cfg.TMDBToken = ""
	cfg.DoubanAPIKey = ""
	cfg.DoubanSecretKey = ""
	cfg.BangumiToken = ""
	cfg.RedisURL = ""
	if mutate != nil {
		mutate(&cfg)
	}
	return runIntegrationFixture{cfg: cfg, admin: adminPool, schema: schema}
}

// start 启动 Run，等就绪后取消上下文，断言优雅退出与启动期落库。
func (f runIntegrationFixture) start(t *testing.T) {
	t.Helper()
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	runErrors := make(chan error, 1)
	go func() {
		runErrors <- Run(ctx, f.cfg, appIntegrationLogger())
	}()

	waitForRunReady(t, f.cfg.HTTPAddr, runErrors)
	cancel()

	select {
	case err := <-runErrors:
		if err != nil {
			t.Fatalf("Run() = %v, want nil", err)
		}
	case <-time.After(60 * time.Second):
		t.Fatal("上下文取消后 Run 未退出")
	}

	quotedSchema := pgx.Identifier{f.schema}.Sanitize()
	var planted int
	if err := f.admin.QueryRow(t.Context(),
		"SELECT count(*) FROM "+quotedSchema+".users WHERE username=$1",
		f.cfg.BootstrapUsername).Scan(&planted); err != nil {
		t.Fatalf("读取隔离 schema 引导用户: %v", err)
	}
	if planted != 1 {
		t.Fatalf("隔离 schema 引导用户数 = %d, want 1", planted)
	}
	var riverTables int
	if err := f.admin.QueryRow(t.Context(),
		"SELECT count(*) FROM information_schema.tables WHERE table_schema=$1 AND table_name='river_job'",
		f.schema).Scan(&riverTables); err != nil {
		t.Fatalf("读取 River 表: %v", err)
	}
	if riverTables != 1 {
		t.Fatalf("隔离 schema river_job 表数 = %d, want 1", riverTables)
	}
}

// TestRunIntegrationServesUntilContextCancel 覆盖进程级 Run 的最小装配：
// 迁移 → 依赖装配 → River 启动 → HTTP 监听 → 上下文取消后优雅关停。
func TestRunIntegrationServesUntilContextCancel(t *testing.T) {
	newRunIntegrationFixture(t, nil).start(t)
}

// TestRunIntegrationStorageProviders 覆盖本地/rclone/alist 存储注册表与 CookieCloud 同步器装配。
func TestRunIntegrationStorageProviders(t *testing.T) {
	newRunIntegrationFixture(t, func(cfg *config.Config) {
		cfg.StorageLocalPath = t.TempDir()
		cfg.StorageRcloneURL = "http://127.0.0.1:9"
		cfg.StorageRcloneFS = "gdrive"
		cfg.StorageRcloneUsername = "run-integration"
		cfg.StorageRclonePassword = "run-integration"
		cfg.StorageAlistURL = "http://127.0.0.1:9"
		cfg.StorageAlistRoot = "/"
		cfg.CookieCloudHost = "http://127.0.0.1:9"
		cfg.CookieCloudKey = "run-integration-key"
		cfg.CookieCloudPassword = "run-integration-password"
		cfg.CookieCloudInterval = time.Hour
	}).start(t)
}

// TestRunIntegrationOptionalProviders 覆盖 Run 里由配置驱动的可选装配分支：
// TMDB 抓削/元数据补充、本地存储提供者、站点消息抓取开关。
func TestRunIntegrationOptionalProviders(t *testing.T) {
	newRunIntegrationFixture(t, func(cfg *config.Config) {
		cfg.TMDBToken = "run-integration-tmdb-token"
		cfg.StorageLocalPath = t.TempDir()
		cfg.SiteMessage = true
	}).start(t)
}
