//go:build integration

package httpapi

import "testing"

// TestSiteUserDataStoreFallbacksIntegration 覆盖「没有能力化的 Sites 时回落到数据库存储」：
// 真实 `*database.Store` 同时实现读取与写入契约。
func TestSiteUserDataStoreFallbacksIntegration(t *testing.T) {
	deps := newHTTPAPIIntegrationDeps(t)
	if deps.siteUserDataStore() == nil {
		t.Fatal("数据库存储应提供站点用户数据读取能力")
	}
	if deps.siteUserDataWriter() == nil {
		t.Fatal("数据库存储应提供站点用户数据写入能力")
	}

	// 站点存储自身实现能力时优先使用它。
	capable := &siteUserDataFake{}
	preferred := deps
	preferred.Sites = capable
	if preferred.siteUserDataStore() != SiteUserDataStore(capable) {
		t.Fatal("能力化的站点存储应优先被选中")
	}
	if preferred.siteUserDataWriter() != SiteUserDataWriter(capable) {
		t.Fatal("能力化的站点存储应优先承担写入")
	}
}
