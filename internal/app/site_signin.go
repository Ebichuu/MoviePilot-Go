package app

import (
	"context"
	"log/slog"
	"strings"

	"github.com/moviepilot-go/moviepilot-go/internal/database"
	"github.com/moviepilot-go/moviepilot-go/internal/jobs"
	"github.com/moviepilot-go/moviepilot-go/internal/siteflow"
)

// siteSigninStore 是签到需要的只读站点清单。
type siteSigninStore interface {
	ListSites(context.Context) ([]database.Site, error)
}

// siteSigninSweep 实现 V2 `scheduler.py` 的站点签到：
// 对每个启用站点做一次「登录保活」，把新 Cookie 写回站点配置。
//
// 语义边界（不伪造成功）：
//   - 站点未启用 → skipped「站点未启用」；
//   - 站点配置里没有用户名/密码 → skipped「未配置登录凭据」；
//   - 适配器不支持登录 → 由 CookieUpdater 返回原因，记为 skipped；
//   - 登录被拒或失败 → CookieUpdater 同样用 (false, 原因, nil) 上报，本轮记为 skipped
//     并保留原因；它不区分「这个站点做不到」与「登录失败」，这里不臆造 failed；
//   - 站点读取/写回错误（err != nil）→ failed，带上站点与原因，不影响其他站点。
type siteSigninSweep struct {
	store   siteSigninStore
	updater *siteflow.CookieUpdater
	logger  *slog.Logger
}

func newSiteSigninSweep(
	store siteSigninStore,
	updater *siteflow.CookieUpdater,
	logger *slog.Logger,
) *siteSigninSweep {
	if store == nil || updater == nil {
		// 依赖缺失时返回 nil：jobs 层会因此不注册周期任务，
		// 而不是注册一个每天空跑的任务。
		return nil
	}
	return &siteSigninSweep{store: store, updater: updater, logger: logger}
}

// SigninEnabledSites 逐站点执行登录保活并汇总结果。
func (s *siteSigninSweep) SigninEnabledSites(ctx context.Context) (jobs.SiteSigninReport, error) {
	sites, err := s.store.ListSites(ctx)
	if err != nil {
		return jobs.SiteSigninReport{}, err
	}
	report := jobs.SiteSigninReport{Items: make([]jobs.SiteSigninResult, 0, len(sites))}
	for _, site := range sites {
		result := s.signinSite(ctx, site)
		switch result.Status {
		case "signed":
			report.Signed++
		case "failed":
			report.Failed++
		default:
			report.Skipped++
		}
		report.Items = append(report.Items, result)
	}
	return report, nil
}

func (s *siteSigninSweep) signinSite(ctx context.Context, site database.Site) jobs.SiteSigninResult {
	result := jobs.SiteSigninResult{SiteID: site.ID, Site: site.Name}
	if !site.Enabled {
		result.Status = "skipped"
		result.Reason = "站点未启用"
		return result
	}
	username, password := siteCredentials(site)
	if username == "" || password == "" {
		result.Status = "skipped"
		result.Reason = "未配置登录凭据"
		return result
	}
	ok, message, err := s.updater.UpdateSiteCookie(ctx, site.ID, username, password, "")
	switch {
	case err != nil:
		result.Status = "failed"
		result.Reason = err.Error()
	case ok:
		result.Status = "signed"
	default:
		// CookieUpdater 用 (false, 原因, nil) 表达「这个站点做不到」，
		// 属于跳过而不是失败——它不该让整轮看起来出错。
		result.Status = "skipped"
		result.Reason = message
	}
	return result
}

// siteCredentials 从站点配置里取登录凭据；缺失时返回空串。
func siteCredentials(site database.Site) (string, string) {
	if site.Config == nil {
		return "", ""
	}
	username, _ := site.Config["username"].(string)
	password, _ := site.Config["password"].(string)
	return strings.TrimSpace(username), strings.TrimSpace(password)
}
