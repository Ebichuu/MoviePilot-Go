package cookiecloud

import (
	"context"
	"fmt"
	"strings"
)

// Site 是同步所需的最小站点字段。
type Site struct {
	ID      int
	Name    string
	URL     string
	Domain  string
	Adapter string
	Config  map[string]any
	Cookie  string
	RSS     string
	Public  bool
	Active  bool
	UA      string
	Proxy   bool
	Timeout int
}

// Indexer 是已知索引器在 CookieCloud 同步时需要的最小字段。
type Indexer struct {
	Name       string
	Domain     string
	Adapter    string
	ExtDomains []string
	Public     bool
	Config     map[string]any
}

// SiteStore 是同步所需的站点读写端口（V2 SiteOper）。
type SiteStore interface {
	SiteByDomain(ctx context.Context, domain string) (Site, bool, error)
	UpdateCookie(ctx context.Context, domain, cookie string) error
	UpdateRSS(ctx context.Context, domain, rss string) error
	AddSite(ctx context.Context, site Site) error
}

// ConnectivityTester 检查站点连通性（V2 `self.test(domain)`）。
type ConnectivityTester interface {
	Test(ctx context.Context, site Site) (bool, string, error)
}

// IndexerResolver 按域名解析索引器信息（V2 SitesHelper.get_indexer）。
type IndexerResolver interface {
	IndexerByDomain(ctx context.Context, domain string) (Indexer, bool, error)
}

// RSSResolver 生成站点 RSS 地址（V2 RssHelper.get_rss_link）。
type RSSResolver interface {
	RSSLink(ctx context.Context, site Site, cookie string) (string, string)
}

// SiteUpdatedNotifier 在站点更新后发出 V2 `SiteUpdated` 事件。
type SiteUpdatedNotifier interface {
	SiteUpdated(ctx context.Context, domain string) error
}

// SyncOptions 复刻 V2 `remote_sync` 的输入。
type SyncOptions struct {
	Blacklist []string
	Manual    bool
}

// SyncResult 复刻 V2 的返回计数与消息。
type SyncResult struct {
	Updated int
	Added   int
	Failed  int
	Message string
}

// SyncService 复刻 V2 `SiteChain.remote_sync`（app/chain/site.py:415–588）的编排：
// 已存在且连通 → 不同步；已存在但不通 → 更新 Cookie；未知但已知索引器 → 新增
// 站点；黑名单跳过；每次处理发出 SiteUpdated。
type SyncService struct {
	helper   *Helper
	store    SiteStore
	tester   ConnectivityTester
	indexers IndexerResolver
	rss      RSSResolver
	events   SiteUpdatedNotifier
}

// NewSyncService 创建 CookieCloud 同步服务。
func NewSyncService(
	helper *Helper,
	store SiteStore,
	tester ConnectivityTester,
	indexers IndexerResolver,
	rss RSSResolver,
	events SiteUpdatedNotifier,
) (*SyncService, error) {
	if helper == nil || store == nil {
		return nil, ErrConfig
	}
	return &SyncService{helper: helper, store: store, tester: tester, indexers: indexers, rss: rss, events: events}, nil
}

// Sync 执行一次同步。
func (s *SyncService) Sync(ctx context.Context, options SyncOptions) (SyncResult, error) {
	cookies, err := s.helper.Download(ctx)
	if err != nil {
		return SyncResult{}, err
	}
	var result SyncResult
	for domain, cookie := range cookies {
		if err := ctx.Err(); err != nil {
			return result, err
		}
		site, found, err := s.store.SiteByDomain(ctx, domain)
		if err != nil {
			return result, err
		}
		if found && site.Active {
			reachable, _, testErr := s.siteReachable(ctx, site)
			if testErr != nil && ctx.Err() != nil {
				return result, ctx.Err()
			}
			if reachable {
				if !site.Public && strings.TrimSpace(site.RSS) == "" && s.rss != nil {
					if rssURL, _ := s.rss.RSSLink(ctx, site, cookie); strings.TrimSpace(rssURL) != "" {
						if err := s.store.UpdateRSS(ctx, domain, rssURL); err != nil {
							return result, err
						}
					}
				}
				s.notify(ctx, domain)
				continue
			}
			if err := s.store.UpdateCookie(ctx, domain, cookie); err != nil {
				return result, err
			}
			result.Updated++
			s.notify(ctx, domain)
			continue
		}
		indexer, known := Indexer{}, false
		if s.indexers != nil {
			var lookupErr error
			indexer, known, lookupErr = s.indexers.IndexerByDomain(ctx, domain)
			if lookupErr != nil {
				return result, lookupErr
			}
		}
		if !known || blacklisted(options.Blacklist, indexer, domain) {
			continue
		}
		resolved := resolveIndexerDomain(indexer, domain)
		newSite := Site{
			Name: indexer.Name, Domain: domain, URL: siteBaseURL(resolved), Adapter: indexer.Adapter,
			Config: cloneSiteConfig(indexer.Config), Cookie: cookie, Public: indexer.Public,
		}
		if strings.TrimSpace(newSite.Adapter) == "" {
			result.Failed++
			continue
		}
		if reachable, _, _ := s.siteReachable(ctx, newSite); !reachable {
			if contextErr := ctx.Err(); contextErr != nil {
				return result, contextErr
			}
			result.Failed++
			continue
		}
		if s.rss != nil && !indexer.Public {
			if rssURL, _ := s.rss.RSSLink(ctx, newSite, cookie); strings.TrimSpace(rssURL) != "" {
				newSite.RSS = rssURL
			}
		}
		if err := s.store.AddSite(ctx, newSite); err != nil {
			result.Failed++
			continue
		}
		result.Added++
		s.notify(ctx, domain)
	}
	result.Message = fmt.Sprintf("更新了%d个站点，新增了%d个站点", result.Updated, result.Added)
	if result.Failed > 0 {
		result.Message += fmt.Sprintf("，%d个站点添加失败，下次同步时将重试，也可以手动添加", result.Failed)
	}
	return result, nil
}

func (s *SyncService) siteReachable(ctx context.Context, site Site) (bool, string, error) {
	if s.tester == nil {
		return false, "", nil
	}
	return s.tester.Test(ctx, site)
}

func (s *SyncService) notify(ctx context.Context, domain string) {
	if s.events != nil {
		_ = s.events.SiteUpdated(ctx, domain)
	}
}

func blacklisted(blacklist []string, indexer Indexer, domain string) bool {
	if len(blacklist) == 0 {
		return false
	}
	candidates := append([]string{domain, indexer.Domain}, indexer.ExtDomains...)
	for _, candidate := range candidates {
		candidate = strings.TrimSpace(candidate)
		if candidate == "" {
			continue
		}
		for _, entry := range blacklist {
			if strings.TrimSpace(entry) == candidate {
				return true
			}
		}
	}
	return false
}

// resolveIndexerDomain 复刻 V2 `__indexer_domain`：域名归组一致时用索引器域名，
// 否则回退到 CookieCloud 归组域名。
func resolveIndexerDomain(indexer Indexer, domain string) string {
	if URLDomain(indexer.Domain) == domain {
		return indexer.Domain
	}
	for _, extDomain := range indexer.ExtDomains {
		if URLDomain(extDomain) == domain {
			return extDomain
		}
	}
	return domain
}

func siteBaseURL(raw string) string {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return ""
	}
	if strings.Contains(raw, "://") {
		return raw
	}
	return "https://" + strings.TrimLeft(raw, ".")
}

func cloneSiteConfig(source map[string]any) map[string]any {
	clone := make(map[string]any, len(source))
	for key, value := range source {
		clone[key] = value
	}
	return clone
}
