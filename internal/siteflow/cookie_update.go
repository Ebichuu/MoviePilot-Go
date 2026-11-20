package siteflow

import (
	"context"
	"errors"
	"fmt"
	"net/http"

	"github.com/moviepilot-go/moviepilot-go/internal/database"
	"github.com/moviepilot-go/moviepilot-go/internal/indexer"
)

// SiteCookieUpdateStore 是站点 Cookie 更新所需的最小持久化能力。
type SiteCookieUpdateStore interface {
	RuntimeSiteByID(ctx context.Context, id int) (database.Site, error)
	UpdateSite(ctx context.Context, site database.Site) error
}

// SiteCookieAdapterBuilder 按站点配置构建索引器适配器。
type SiteCookieAdapterBuilder func(site database.Site) (indexer.Adapter, error)

type siteSigner interface {
	SignIn(ctx context.Context, username, password string) (string, error)
}

// CookieUpdater 实现 V2 `SiteChain.update_cookie`：用调用方给出的用户名密码登录站点，
// 把站点下发的 Cookie 写回站点配置，供后续搜索/取种使用。
type CookieUpdater struct {
	store SiteCookieUpdateStore
	build SiteCookieAdapterBuilder
}

// NewCookieUpdater 构造站点 Cookie 更新器；任一依赖缺失时返回 nil，端点会如实拒绝。
func NewCookieUpdater(store SiteCookieUpdateStore, build SiteCookieAdapterBuilder) *CookieUpdater {
	if store == nil || build == nil {
		return nil
	}
	return &CookieUpdater{store: store, build: build}
}

// UpdateSiteCookie 登录站点并持久化新的 Cookie。
func (u *CookieUpdater) UpdateSiteCookie(
	ctx context.Context,
	siteID int,
	username, password, code string,
) (bool, string, error) {
	if u == nil || u.store == nil || u.build == nil {
		return false, "站点 Cookie 更新能力尚未接入", nil
	}
	site, err := u.store.RuntimeSiteByID(ctx, siteID)
	if err != nil {
		return false, "", err
	}
	adapter, err := u.build(site)
	if err != nil {
		return false, err.Error(), nil
	}
	signer, ok := adapter.(siteSigner)
	if !ok {
		return false, "该站点适配器不支持登录，请手动更新 Cookie", nil
	}
	cookie, err := signer.SignIn(ctx, username, password)
	if err != nil {
		return false, siteSignInMessage(err), nil
	}
	if site.Config == nil {
		site.Config = make(map[string]any)
	}
	site.Config["cookie"] = cookie
	if err := u.store.UpdateSite(ctx, site); err != nil {
		return false, "", err
	}
	return true, "站点 Cookie 更新成功", nil
}

// siteSignInMessage 把底层错误收敛成可直接展示的中文原因，不外泄内部细节。
func siteSignInMessage(err error) string {
	var statusErr *indexer.HTTPStatusError
	if errors.As(err, &statusErr) {
		if statusErr.StatusCode == http.StatusUnauthorized || statusErr.StatusCode == http.StatusForbidden {
			return "站点登录被拒绝：用户名或密码错误"
		}
		return fmt.Sprintf("站点登录失败：站点返回 HTTP %d", statusErr.StatusCode)
	}
	switch {
	case errors.Is(err, context.Canceled):
		return "站点登录已取消"
	case errors.Is(err, context.DeadlineExceeded):
		return "站点登录超时"
	}
	return err.Error()
}
