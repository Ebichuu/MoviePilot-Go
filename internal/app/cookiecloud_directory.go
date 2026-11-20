package app

import (
	"github.com/moviepilot-go/moviepilot-go/internal/database"
	"github.com/moviepilot-go/moviepilot-go/internal/httpapi"
)

// newCookieCloudSiteDirectory 构造站点分类目录端口；store 未初始化时返回 nil，
// 让 `/site/category/{site_id}` 回退到 V2 的空分类语义。
func newCookieCloudSiteDirectory(store *database.Store) httpapi.SiteCategoryCatalog {
	if store == nil {
		return nil
	}
	return &cookieCloudSiteDirectory{store: store}
}
