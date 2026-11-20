package siteflow

import (
	"context"
	"errors"
	"fmt"

	"github.com/moviepilot-go/moviepilot-go/internal/indexer"
)

var errInvalidBatchRefresh = errors.New("站点批量刷新参数无效")

// BatchSite 是批量刷新消费的最小站点投影。
type BatchSite struct {
	ID     int
	Name   string
	Active bool
}

// BatchSiteSource 按 V2 `SitesHelper().get_indexers()` 的定义顺序提供站点。
type BatchSiteSource interface {
	ListSitesForRefresh(ctx context.Context) ([]BatchSite, error)
}

// BatchSiteRefresher 刷新单个站点，返回是否产生了新数据（V2 `refresh_userdata`）。
type BatchSiteRefresher interface {
	RefreshSiteUserData(ctx context.Context, site BatchSite) (indexer.SiteUserData, bool, error)
}

// AllSitesRefreshedPublisher 发布 V2 批量 `SiteRefreshed {site_id: "*"}`。
type AllSitesRefreshedPublisher interface {
	PublishSiteRefreshedAll(ctx context.Context)
}

// BatchProgress 对应 V2 progress_callback 的 total/finished/current。
type BatchProgress struct {
	Total    int
	Finished int
	Current  int
}

// BatchRefreshResult 汇总一次批量刷新的计数与数据。
type BatchRefreshResult struct {
	Total      int
	Refreshed  int
	Failed     int
	AnyUpdated bool
	Data       map[string]indexer.SiteUserData
}

// Message 复刻 V2 批量完成后生成的文本。
func (r BatchRefreshResult) Message() string {
	return fmt.Sprintf("刷新了%d个站点，%d个站点失败", r.Refreshed, r.Failed)
}

// BatchRefresher 复刻 V2 `SiteChain.refresh_userdatas`（app/chain/site.py:124–173）：
// 只刷新 `is_active` 站点并按来源顺序处理；系统停止（ctx 取消）立即返回；单站
// 失败计数后继续；全部结束后仅当有任意站点更新时发布一次 `site_id:"*"` 事件。
type BatchRefresher struct {
	sites     BatchSiteSource
	refresher BatchSiteRefresher
	publisher AllSitesRefreshedPublisher
}

// NewBatchRefresher 创建批量刷新编排器。
func NewBatchRefresher(
	sites BatchSiteSource,
	refresher BatchSiteRefresher,
	publisher AllSitesRefreshedPublisher,
) (*BatchRefresher, error) {
	if sites == nil || refresher == nil {
		return nil, errInvalidBatchRefresh
	}
	return &BatchRefresher{sites: sites, refresher: refresher, publisher: publisher}, nil
}

// RefreshAll 执行一次批量刷新。
func (b *BatchRefresher) RefreshAll(
	ctx context.Context,
	progress func(BatchProgress),
) (BatchRefreshResult, error) {
	if ctx == nil {
		return BatchRefreshResult{}, errInvalidBatchRefresh
	}
	sites, err := b.sites.ListSitesForRefresh(ctx)
	if err != nil {
		return BatchRefreshResult{}, errInvalidBatchRefresh
	}
	active := make([]BatchSite, 0, len(sites))
	for _, site := range sites {
		if site.Active && site.ID > 0 {
			active = append(active, site)
		}
	}
	result := BatchRefreshResult{Total: len(active), Data: make(map[string]indexer.SiteUserData, len(active))}
	if progress != nil {
		progress(BatchProgress{Total: result.Total})
	}
	for index, site := range active {
		if err := ctx.Err(); err != nil {
			return result, err
		}
		data, updated, refreshErr := b.refresher.RefreshSiteUserData(ctx, site)
		if refreshErr != nil {
			if contextErr := ctx.Err(); contextErr != nil {
				return result, contextErr
			}
			result.Failed++
		} else if updated {
			result.Refreshed++
			result.AnyUpdated = true
			if site.Name != "" {
				result.Data[site.Name] = data
			}
		}
		if progress != nil {
			progress(BatchProgress{Total: result.Total, Finished: index + 1, Current: site.ID})
		}
	}
	if result.AnyUpdated && b.publisher != nil {
		b.publisher.PublishSiteRefreshedAll(ctx)
	}
	return result, nil
}
