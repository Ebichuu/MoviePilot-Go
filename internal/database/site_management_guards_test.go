package database

import (
	"context"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/moviepilot-go/moviepilot-go/internal/auth"
)

// 本文件覆盖站点管理的错误与配置编码分支：
// 连接不可用时的查询/执行/事务失败、加密器缺失、公开与敏感配置编码失败。

func siteManagementUnitStore(t *testing.T) *Store {
	t.Helper()
	pool, err := pgxpool.New(context.Background(), "postgres://invalid:invalid@127.0.0.1:1/none?sslmode=disable")
	if err != nil {
		t.Fatalf("创建不可用连接池: %v", err)
	}
	t.Cleanup(pool.Close)
	return &Store{db: &DB{Pool: pool}}
}

func siteManagementSecretStore(t *testing.T) *Store {
	t.Helper()
	box, err := auth.NewSecretBox("site-management-guards-secret-key")
	if err != nil {
		t.Fatalf("创建加密器: %v", err)
	}
	return NewStore(siteManagementUnitStore(t).db, box)
}

func TestSiteManagementReportsUnavailablePool(t *testing.T) {
	store := siteManagementUnitStore(t)
	ctx := context.Background()

	if _, err := store.ListSites(ctx); err == nil || !strings.Contains(err.Error(), "查询站点") {
		t.Fatalf("ListSites error = %v", err)
	}
	if _, err := store.SiteByID(ctx, 1); err == nil {
		t.Fatal("SiteByID 应返回错误")
	}
	if _, err := store.RuntimeSiteByID(ctx, 1); err == nil {
		t.Fatal("RuntimeSiteByID 应返回错误")
	}
	if _, err := store.DeleteSite(ctx, 1); err == nil || !strings.Contains(err.Error(), "删除站点") {
		t.Fatalf("DeleteSite error = %v", err)
	}
	if _, err := store.DeleteAllSites(ctx); err == nil || !strings.Contains(err.Error(), "重置站点") {
		t.Fatalf("DeleteAllSites error = %v", err)
	}
	if err := store.UpdateSitePriorities(ctx, []SitePriority{{ID: 1, Priority: 10}}); err == nil ||
		!strings.Contains(err.Error(), "开始更新站点优先级") {
		t.Fatalf("UpdateSitePriorities error = %v", err)
	}
}

func TestSiteManagementValidatesUpdateInput(t *testing.T) {
	ctx := context.Background()

	t.Run("站点 ID 无效", func(t *testing.T) {
		if err := siteManagementUnitStore(t).UpdateSite(ctx, Site{}); err == nil ||
			!strings.Contains(err.Error(), "站点 ID 无效") {
			t.Fatalf("error = %v", err)
		}
	})

	t.Run("加密器未初始化", func(t *testing.T) {
		err := siteManagementUnitStore(t).UpdateSite(ctx, Site{ID: 3, Config: map[string]any{}})
		if err == nil || !strings.Contains(err.Error(), "加密器未初始化") {
			t.Fatalf("error = %v", err)
		}
	})

	t.Run("公开配置无法编码", func(t *testing.T) {
		err := siteManagementSecretStore(t).UpdateSite(ctx, Site{
			ID:     3,
			Config: map[string]any{"pri": make(chan int)},
		})
		if err == nil || !strings.Contains(err.Error(), "编码站点公开配置失败") {
			t.Fatalf("error = %v", err)
		}
	})

	t.Run("敏感配置无法编码", func(t *testing.T) {
		err := siteManagementSecretStore(t).UpdateSite(ctx, Site{
			ID:     3,
			Config: map[string]any{"apikey": make(chan int)},
		})
		if err == nil || !strings.Contains(err.Error(), "编码站点敏感配置失败") {
			t.Fatalf("error = %v", err)
		}
	})

	t.Run("连接不可用时更新失败", func(t *testing.T) {
		err := siteManagementSecretStore(t).UpdateSite(ctx, Site{
			ID:     3,
			Name:   "演示站点",
			Domain: "demo.example",
			URL:    "https://demo.example",
			Config: map[string]any{"pri": 10},
		})
		if err == nil || !strings.Contains(err.Error(), "更新站点") {
			t.Fatalf("error = %v", err)
		}
	})
}
