package app

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"

	"github.com/moviepilot-go/moviepilot-go/internal/database"
	"github.com/moviepilot-go/moviepilot-go/internal/indexer"
	"github.com/moviepilot-go/moviepilot-go/internal/siteflow"
)

// siteUserDataFetcher 是批量刷新消费的最小抓取契约（与 Search 协调器一致）。
type siteUserDataFetcher interface {
	FetchSiteUserData(context.Context, indexer.Adapter, ...indexer.SiteUserDataOptions) (indexer.SiteUserData, error)
}

// siteUserDataRefresher 复刻 V2 `SiteChain.refresh_userdatas` 的单站步骤：
// 解析适配器 → 抓取用户数据 → 保存日快照 → 执行 V2 副作用。
type siteUserDataRefresher struct {
	store         *database.Store
	search        siteUserDataFetcher
	buildAdapter  func(database.Site) (indexer.Adapter, error)
	effects       *siteflow.Service
	fetchMessages bool
	logger        *slog.Logger
}

func (r *siteUserDataRefresher) ListSitesForRefresh(ctx context.Context) ([]siteflow.BatchSite, error) {
	sites, err := r.store.ListSites(ctx)
	if err != nil {
		return nil, err
	}
	result := make([]siteflow.BatchSite, 0, len(sites))
	for _, site := range sites {
		result = append(result, siteflow.BatchSite{ID: site.ID, Name: site.Name, Active: site.Enabled})
	}
	return result, nil
}

func (r *siteUserDataRefresher) RefreshSiteUserData(
	ctx context.Context,
	site siteflow.BatchSite,
) (indexer.SiteUserData, bool, error) {
	if r == nil || r.store == nil {
		return indexer.SiteUserData{}, false, fmt.Errorf("站点用户数据刷新未配置")
	}
	runtimeSite, err := r.store.RuntimeSiteByID(ctx, site.ID)
	if err != nil {
		return indexer.SiteUserData{}, false, err
	}
	if r.buildAdapter == nil || r.search == nil {
		return indexer.SiteUserData{}, false, nil
	}
	adapter, err := r.buildAdapter(runtimeSite)
	if err != nil || adapter == nil {
		return indexer.SiteUserData{}, false, nil
	}
	if _, ok := adapter.(indexer.SiteUserDataFetcher); !ok {
		return indexer.SiteUserData{}, false, nil
	}
	userData, err := r.search.FetchSiteUserData(ctx, adapter, indexer.SiteUserDataOptions{
		FetchMessages: r.fetchMessages,
	})
	if err != nil {
		return indexer.SiteUserData{}, false, err
	}
	if err := r.store.SaveSiteUserData(ctx, runtimeSite.Domain, runtimeSite.Name, databaseSiteUserData(userData)); err != nil {
		return indexer.SiteUserData{}, false, err
	}
	if r.effects != nil {
		if err := r.effects.AfterSiteUserDataRefresh(
			ctx, runtimeSite.ID, runtimeSite.Name, runtimeSite.URL, userData,
		); err != nil {
			return indexer.SiteUserData{}, false, err
		}
	}
	return userData, true, nil
}

// HandleSiteUserDataRefresh 执行一次 V2 批量刷新。
func (r *siteUserDataRefresher) HandleSiteUserDataRefresh(ctx context.Context) error {
	batch, err := siteflow.NewBatchRefresher(r, r, nil)
	if err != nil {
		return err
	}
	_, err = batch.RefreshAll(ctx, nil)
	return err
}

func databaseSiteUserData(data indexer.SiteUserData) database.SiteUserData {
	bonus, upload, download, ratio := data.Bonus, float64(data.Upload), float64(data.Download), data.Ratio
	seeding, leeching := float64(data.Seeding), float64(data.Leeching)
	seedingSize, leechingSize := float64(data.SeedingSize), float64(data.LeechingSize)
	messageUnread := data.MessageUnread
	seedingInfo, _ := json.Marshal(data.SeedingInfo)
	messageContents, _ := json.Marshal(data.MessageUnreadContents)
	var userID *string
	if data.UserID != nil {
		value := fmt.Sprint(data.UserID)
		userID = &value
	}
	updatedTime := ""
	if data.UpdatedTime != nil {
		updatedTime = *data.UpdatedTime
	}
	return database.SiteUserData{
		Domain: data.Domain, Username: data.Username, UserID: userID, UserLevel: data.UserLevel, JoinAt: data.JoinAt,
		Bonus: &bonus, Upload: &upload, Download: &download, Ratio: &ratio,
		Seeding: &seeding, Leeching: &leeching, SeedingSize: &seedingSize, LeechingSize: &leechingSize,
		SeedingInfo: seedingInfo, MessageUnread: &messageUnread, MessageUnreadContents: messageContents,
		ErrMsg: data.ErrMsg, UpdatedDay: data.UpdatedDay, UpdatedTime: updatedTime,
	}
}
