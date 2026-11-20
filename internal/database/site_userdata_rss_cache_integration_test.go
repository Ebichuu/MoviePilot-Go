//go:build integration

package database

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/moviepilot-go/moviepilot-go/internal/auth"
)

func siteUserDataText(value string) *string { return &value }

// TestSiteUserDataStoreIntegration 覆盖站点用户数据的首次写入、按域名更新与读取。
func TestSiteUserDataStoreIntegration(t *testing.T) {
	base := newSiteStatisticsIntegrationStore(t)
	box, err := auth.NewSecretBox("moviepilot-site-userdata-integration-key")
	if err != nil {
		t.Fatalf("创建敏感配置加密器: %v", err)
	}
	store := NewStore(base.db, box)
	ctx := t.Context()

	siteID, err := store.UpsertSite(ctx, Site{
		Name: "用户数据站点", Domain: "ud.example.com", URL: "https://ud.example.com",
		Adapter: "mteam", Enabled: true, Config: map[string]any{"pri": 1},
	})
	if err != nil {
		t.Fatalf("UpsertSite() error = %v", err)
	}

	bonus, upload := 100.5, 2048.0
	payload := SiteUserData{
		Username:  siteUserDataText("ud-user"),
		UserID:    siteUserDataText("42"),
		UserLevel: siteUserDataText("Power User"),
		Bonus:     &bonus,
		Upload:    &upload,
	}
	if err := store.SaveSiteUserData(ctx, "ud.example.com", "用户数据站点", payload); err != nil {
		t.Fatalf("SaveSiteUserData() error = %v", err)
	}

	latest, err := store.LatestSiteUserData(ctx)
	if err != nil {
		t.Fatalf("LatestSiteUserData() error = %v", err)
	}
	if len(latest) != 1 || latest[0].Domain != "ud.example.com" || latest[0].Username == nil || *latest[0].Username != "ud-user" {
		t.Fatalf("LatestSiteUserData() = %#v", latest)
	}
	byDomain, err := store.SiteUserDataByDomain(ctx, "ud.example.com", "")
	if err != nil || len(byDomain) != 1 || byDomain[0].Bonus == nil || *byDomain[0].Bonus != bonus {
		t.Fatalf("SiteUserDataByDomain() = (%#v, %v)", byDomain, err)
	}

	// 同域名重复保存走更新分支，不新增行。
	bonus = 55
	payload.Bonus = &bonus
	if err := store.SaveSiteUserData(ctx, "ud.example.com", "用户数据站点", payload); err != nil {
		t.Fatalf("重复 SaveSiteUserData() error = %v", err)
	}
	byDomain, err = store.SiteUserDataByDomain(ctx, "ud.example.com", "")
	if err != nil || len(byDomain) != 1 || byDomain[0].Bonus == nil || *byDomain[0].Bonus != 55 {
		t.Fatalf("更新后 SiteUserDataByDomain() = (%#v, %v)", byDomain, err)
	}

	// siteID 通过 sites 行间接关联，确认站点仍在（用户数据不删站点）。
	if _, err := store.SiteByID(ctx, siteID); err != nil {
		t.Fatalf("SiteByID() error = %v", err)
	}
	if missing, err := store.SiteUserDataByDomain(ctx, "missing.example.com", ""); err != nil || len(missing) != 0 {
		t.Fatalf("缺失域名 SiteUserDataByDomain() = (%v, %v)", missing, err)
	}
}

// TestRSSCacheSignaturesIntegration 覆盖 RSS 轮询落库后缓存签名的严格拼接积分。
func TestRSSCacheSignaturesIntegration(t *testing.T) {
	base := newSiteStatisticsIntegrationStore(t)
	box, err := auth.NewSecretBox("moviepilot-rss-cache-integration-key")
	if err != nil {
		t.Fatalf("创建敏感配置加密器: %v", err)
	}
	store := NewStore(base.db, box)
	ctx := t.Context()

	siteID, err := store.UpsertSite(ctx, Site{
		Name: "RSS 站点", Domain: "rss.example.com", URL: "https://rss.example.com",
		Adapter: "mteam", Enabled: true, Config: map[string]any{"pri": 1},
	})
	if err != nil {
		t.Fatalf("UpsertSite() error = %v", err)
	}
	endpoint := "https://rss.example.com/rss"
	// EnsureRSSCursor 写入的 next_poll_at 取库内 now()，取租约的 now 需要略晚一点。
	now := time.Now().UTC().Truncate(time.Second).Add(2 * time.Second)
	if _, err := store.EnsureRSSCursor(ctx, int64(siteID), endpoint); err != nil {
		t.Fatalf("EnsureRSSCursor() error = %v", err)
	}
	lease, acquired, err := store.TryAcquireRSSLease(ctx, int64(siteID), endpoint, now, time.Minute)
	if err != nil || !acquired {
		t.Fatalf("TryAcquireRSSLease() = (%#v, %v, %v)", lease, acquired, err)
	}

	result := RSSPollResult{
		ObservedAt: now,
		Items: []RSSItemInput{
			{ItemKey: "rss-item-key-1", Title: "条目一", Resource: json.RawMessage(`{"description":"描述一"}`)},
			{ItemKey: "rss-item-key-2", Title: "条目二", Resource: json.RawMessage(`{"description":"描述二"}`)},
			{ItemKey: "rss-item-key-3", Title: "条目三", Resource: json.RawMessage(`{}`)},
		},
	}
	if err := store.CommitRSSPoll(ctx, int64(siteID), endpoint, lease.LeaseToken, result, now.Add(time.Hour)); err != nil {
		t.Fatalf("CommitRSSPoll() error = %v", err)
	}

	signatures, err := store.ListRSSCacheSignatures(ctx, int64(siteID))
	if err != nil {
		t.Fatalf("ListRSSCacheSignatures() error = %v", err)
	}
	if len(signatures) != 3 {
		t.Fatalf("签名数量 = %d, want 3: %#v", len(signatures), signatures)
	}
	for _, want := range []string{"条目一描述一", "条目二描述二", "条目三"} {
		if _, ok := signatures[want]; !ok {
			t.Fatalf("缺少签名 %q: %#v", want, signatures)
		}
	}

	snapshot, err := store.ListRSSCacheSnapshot(ctx, int64(siteID), now.Add(time.Minute))
	if err != nil || len(snapshot) != 3 {
		t.Fatalf("ListRSSCacheSnapshot() = (%d, %v)", len(snapshot), err)
	}
	ready, err := store.RSSPollRoundReady(ctx, now, []int64{int64(siteID)})
	if err != nil || !ready {
		t.Fatalf("RSSPollRoundReady() = (%v, %v)", ready, err)
	}
	trimmed, err := store.TrimRSSCacheItems(ctx, int64(siteID), 10)
	if err != nil || trimmed != 0 {
		t.Fatalf("TrimRSSCacheItems() = (%d, %v), want 0", trimmed, err)
	}
}
