package subscriptionflow

import (
	"sort"
	"strings"

	"github.com/moviepilot-go/moviepilot-go/internal/indexer"
	"github.com/moviepilot-go/moviepilot-go/internal/subscription"
)

type rssSource struct {
	siteID    int
	cursorKey string
	adapter   indexer.Adapter
	fetcher   indexer.RSSCursorSource
	noCache   bool
}

func rssSources(adapters []indexer.Adapter) map[int]rssSource {
	sources := make(map[int]rssSource)
	for _, adapter := range adapters {
		identified, idOK := adapter.(indexer.SiteIDAdapter)
		fetcher, fetchOK := adapter.(indexer.RSSCursorSource)
		resolver, resolverOK := adapter.(indexer.VersionedDownloadResolver)
		if !idOK || !fetchOK || !resolverOK || identified.SiteID() <= 0 || strings.TrimSpace(fetcher.RSSCursorKey()) == "" {
			continue
		}
		if strings.TrimSpace(resolver.ResolverKind()) == "" || resolver.ResolverVersion() <= 0 {
			continue
		}
		if _, exists := sources[identified.SiteID()]; exists {
			continue
		}
		noCache := false
		if policy, policyOK := adapter.(indexer.RSSNoCacheSource); policyOK {
			noCache = policy.RSSNoCache()
		}
		sources[identified.SiteID()] = rssSource{
			siteID:    identified.SiteID(),
			cursorKey: fetcher.RSSCursorKey(),
			adapter:   adapter,
			fetcher:   fetcher,
			noCache:   noCache,
		}
	}
	return sources
}

func orderedRSSSources(sources map[int]rssSource) []rssSource {
	ids := make([]int, 0, len(sources))
	for id := range sources {
		ids = append(ids, id)
	}
	sort.Ints(ids)
	result := make([]rssSource, 0, len(ids))
	for _, id := range ids {
		result = append(result, sources[id])
	}
	return result
}

func selectedRSSSources(
	sources map[int]rssSource,
	selectedSiteIDs []int,
	selectAll bool,
) map[int]rssSource {
	selected := make(map[int]rssSource)
	if selectAll {
		for siteID, source := range sources {
			selected[siteID] = source
		}
		return selected
	}
	for _, siteID := range selectedSiteIDs {
		if source, ok := sources[siteID]; ok {
			selected[siteID] = source
		}
	}
	return selected
}

func rssSourceIDs(sources map[int]rssSource) []int {
	result := make([]int, 0, len(sources))
	for siteID := range sources {
		result = append(result, siteID)
	}
	sort.Ints(result)
	if result == nil {
		return []int{}
	}
	return result
}

func rssSiteIDIntersection(userSiteIDs, defaultSiteIDs []int, target map[int]struct{}) bool {
	defaults := make(map[int]struct{}, len(defaultSiteIDs))
	for _, siteID := range defaultSiteIDs {
		defaults[siteID] = struct{}{}
	}
	found := false
	for _, siteID := range userSiteIDs {
		if _, ok := defaults[siteID]; !ok {
			continue
		}
		target[siteID] = struct{}{}
		found = true
	}
	return found
}

func normalizedRSSSiteIDs(siteIDs []int) []int {
	unique := make(map[int]struct{}, len(siteIDs))
	for _, siteID := range siteIDs {
		if siteID > 0 {
			unique[siteID] = struct{}{}
		}
	}
	result := make([]int, 0, len(unique))
	for siteID := range unique {
		result = append(result, siteID)
	}
	sort.Ints(result)
	return result
}

func validExpectedRSSSiteIDs(siteIDs []int) bool {
	for index, siteID := range siteIDs {
		if siteID <= 0 || (index > 0 && siteIDs[index-1] >= siteID) {
			return false
		}
	}
	return true
}

func activeExpectedRSSSiteIDs(siteIDs []int, sources map[int]rssSource) []int {
	active := make([]int, 0, len(siteIDs))
	for _, siteID := range siteIDs {
		if _, ok := sources[siteID]; ok {
			active = append(active, siteID)
		}
	}
	return active
}

func rssSiteIDs64(siteIDs []int) []int64 {
	result := make([]int64, len(siteIDs))
	for index, siteID := range siteIDs {
		result[index] = int64(siteID)
	}
	return result
}

func rssQueryAllowsSite(query subscription.Query, siteID int) bool {
	if len(query.SiteIDs) == 0 {
		return true
	}
	for _, allowed := range query.SiteIDs {
		if allowed == siteID {
			return true
		}
	}
	return false
}
