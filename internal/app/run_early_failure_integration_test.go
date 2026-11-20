//go:build integration

package app

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/moviepilot-go/moviepilot-go/internal/config"
)

// runExpectStartupError 用可启动夹具跑 Run，并断言它在依赖装配阶段就返回错误。
// 这些用例只覆盖「配置本身就非法」的早退分支，不需要等到 HTTP 监听。
func runExpectStartupError(t *testing.T, mutate func(*config.Config)) error {
	t.Helper()
	fixture := newRunIntegrationFixture(t, mutate)
	ctx, cancel := context.WithTimeout(t.Context(), 60*time.Second)
	defer cancel()
	errCh := make(chan error, 1)
	go func() {
		errCh <- Run(ctx, fixture.cfg, appIntegrationLogger())
	}()
	select {
	case err := <-errCh:
		return err
	case <-ctx.Done():
		t.Fatal("Run 在装配失败后未返回")
		return nil
	}
}

// TestRunIntegrationRejectsInvalidDatabaseURL 覆盖迁移阶段的数据库连接失败。
func TestRunIntegrationRejectsInvalidDatabaseURL(t *testing.T) {
	err := runExpectStartupError(t, func(cfg *config.Config) {
		cfg.DatabaseURL = "://bad-url"
	})
	if err == nil {
		t.Fatal("非法数据库连接串应让 Run 返回错误")
	}
}

// TestRunIntegrationRejectsShortMasterKey 覆盖敏感配置加密初始化失败。
func TestRunIntegrationRejectsShortMasterKey(t *testing.T) {
	err := runExpectStartupError(t, func(cfg *config.Config) {
		cfg.MasterKey = "short"
	})
	if err == nil || !strings.Contains(err.Error(), "初始化敏感配置加密") {
		t.Fatalf("Run() = %v，期望包含「初始化敏感配置加密」", err)
	}
}

// TestRunIntegrationRejectsInvalidSearchWorkers 覆盖搜索协调器构造失败。
func TestRunIntegrationRejectsInvalidSearchWorkers(t *testing.T) {
	err := runExpectStartupError(t, func(cfg *config.Config) {
		cfg.SearchWorkers = 0
	})
	if err == nil || !strings.Contains(err.Error(), "初始化搜索协调器") {
		t.Fatalf("Run() = %v，期望包含「初始化搜索协调器」", err)
	}
}

// TestRunIntegrationRejectsInvalidSearchHTTPParams 覆盖站点 HTTP 客户端参数校验。
func TestRunIntegrationRejectsInvalidSearchHTTPParams(t *testing.T) {
	cases := map[string]func(*config.Config){
		"请求超时为零":  func(cfg *config.Config) { cfg.RequestTimeout = 0 },
		"响应体上限为零": func(cfg *config.Config) { cfg.MaxResponseBody = 0 },
		"响应体上限过大": func(cfg *config.Config) { cfg.MaxResponseBody = 64 << 20 },
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			err := runExpectStartupError(t, mutate)
			if err == nil || !strings.Contains(err.Error(), "站点 HTTP 客户端参数无效") {
				t.Fatalf("Run() = %v，期望包含「站点 HTTP 客户端参数无效」", err)
			}
		})
	}
}

// TestRunIntegrationRejectsInvalidMetadataHTTPParams 覆盖元数据 HTTP 客户端参数校验。
func TestRunIntegrationRejectsInvalidMetadataHTTPParams(t *testing.T) {
	cases := map[string]func(*config.Config){
		"元数据 worker 为零": func(cfg *config.Config) { cfg.MetadataWorkers = 0 },
		"元数据超时为零":       func(cfg *config.Config) { cfg.MetadataTimeout = 0 },
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			err := runExpectStartupError(t, mutate)
			if err == nil || !strings.Contains(err.Error(), "初始化元数据 HTTP 客户端") {
				t.Fatalf("Run() = %v，期望包含「初始化元数据 HTTP 客户端」", err)
			}
		})
	}
}

// TestRunIntegrationRejectsInvalidOrganizationWorkers 覆盖 River 管理器构造时
// 对整理队列并发配置的校验（此时除数据库外的依赖都已装配完成）。
func TestRunIntegrationRejectsInvalidOrganizationWorkers(t *testing.T) {
	err := runExpectStartupError(t, func(cfg *config.Config) {
		cfg.OrganizeWorkers = -1
	})
	if err == nil || !strings.Contains(err.Error(), "整理队列并发配置无效") {
		t.Fatalf("Run() = %v，期望包含「整理队列并发配置无效」", err)
	}
}

// TestRunIntegrationReportsHTTPListenFailure 覆盖监听失败分支：装配全部成功后，
// ListenAndServe 立即返回错误，Run 必须把它包成「运行 HTTP 服务」。
func TestRunIntegrationReportsHTTPListenFailure(t *testing.T) {
	err := runExpectStartupError(t, func(cfg *config.Config) {
		cfg.HTTPAddr = "127.0.0.1:99999"
	})
	if err == nil || !strings.Contains(err.Error(), "运行 HTTP 服务") {
		t.Fatalf("Run() = %v，期望包含「运行 HTTP 服务」", err)
	}
}
