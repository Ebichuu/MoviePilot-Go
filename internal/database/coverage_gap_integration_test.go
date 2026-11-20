//go:build integration

package database

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/moviepilot-go/moviepilot-go/internal/scrapeflow"
)

func containsUUID(ids []uuid.UUID, want uuid.UUID) bool {
	for _, id := range ids {
		if id == want {
			return true
		}
	}
	return false
}

// TestListDueScrapesIntegration 覆盖到期抓取请求的可见性、认领后的隐藏与重排后的重新到期。
func TestListDueScrapesIntegration(t *testing.T) {
	store := newSiteStatisticsIntegrationStore(t)
	ctx := t.Context()
	fixture := newSideEffectRequestFixtureIntegration(t, store, "list-due-scrapes")
	now := time.Now().UTC()

	ids, err := store.ListDueScrapes(ctx, now, 10)
	if err != nil {
		t.Fatalf("ListDueScrapes() error = %v", err)
	}
	if !containsUUID(ids, fixture.scrapeID) {
		t.Fatalf("新建请求 %s 未出现在到期列表 %v", fixture.scrapeID, ids)
	}

	if _, err := store.ListDueScrapes(ctx, time.Time{}, 10); !errors.Is(err, scrapeflow.ErrInvalidPlan) {
		t.Fatalf("空时间 error = %v", err)
	}
	if _, err := store.ListDueScrapes(ctx, now, 0); !errors.Is(err, scrapeflow.ErrInvalidPlan) {
		t.Fatalf("零上限 error = %v", err)
	}
	if _, err := store.ListDueScrapes(ctx, now, scrapeflow.SweepPageSize+1); !errors.Is(err, scrapeflow.ErrInvalidPlan) {
		t.Fatalf("超量上限 error = %v", err)
	}
	if _, err := store.ListDueScrapes(nil, now, 10); !errors.Is(err, scrapeflow.ErrPersistence) {
		t.Fatalf("nil 上下文 error = %v", err)
	}

	lease, claimed, err := store.ClaimScrape(ctx, fixture.scrapeID, now.Add(time.Second), time.Minute)
	if err != nil || !claimed {
		t.Fatalf("ClaimScrape() = (%#v, %v, %v)", lease, claimed, err)
	}
	ids, err = store.ListDueScrapes(ctx, now.Add(2*time.Second), 10)
	if err != nil {
		t.Fatalf("认领后 ListDueScrapes() error = %v", err)
	}
	if containsUUID(ids, fixture.scrapeID) {
		t.Fatalf("执行中的请求不应再到期: %v", ids)
	}

	retryAt := now.Add(time.Hour)
	if err := store.RescheduleScrape(ctx, fixture.scrapeID, lease.Token, retryAt, "稍后重试"); err != nil {
		t.Fatalf("RescheduleScrape() error = %v", err)
	}
	ids, err = store.ListDueScrapes(ctx, now.Add(time.Minute), 10)
	if err != nil {
		t.Fatalf("重排后 ListDueScrapes() error = %v", err)
	}
	if containsUUID(ids, fixture.scrapeID) {
		t.Fatalf("未到重试时间的请求不应到期: %v", ids)
	}
	ids, err = store.ListDueScrapes(ctx, retryAt.Add(time.Second), 10)
	if err != nil {
		t.Fatalf("到期后 ListDueScrapes() error = %v", err)
	}
	if !containsUUID(ids, fixture.scrapeID) {
		t.Fatalf("到达重试时间的请求应重新到期: %v", ids)
	}
}

// TestListDownloadIntentOwnersIntegration 覆盖意图标签到 V2 用户的批量投影。
func TestListDownloadIntentOwnersIntegration(t *testing.T) {
	store := newSiteStatisticsIntegrationStore(t)
	ctx := t.Context()
	const username = "owner-lookup-user"
	insertDownloadIntentIntegrationUsers(t, store, username)

	label := "mp-" + strings.Repeat("a", 32)
	otherLabel := "mp-" + strings.Repeat("b", 32)
	if _, err := store.db.Pool.Exec(ctx, `
		INSERT INTO download_intents (id, username, candidate_key, label, state)
		VALUES ($1, $2, 'owner-lookup-candidate', $3, 'pending')`,
		uuid.New(), username, label); err != nil {
		t.Fatalf("插入下载意图: %v", err)
	}

	owners, err := store.ListDownloadIntentOwners(ctx, []string{" " + label + " ", label, otherLabel})
	if err != nil {
		t.Fatalf("ListDownloadIntentOwners() error = %v", err)
	}
	owner, ok := owners[label]
	if !ok || owner.Username != username || owner.UserID <= 0 {
		t.Fatalf("owner = %#v (存在=%v), want username=%s", owner, ok, username)
	}
	if _, exists := owners[otherLabel]; exists {
		t.Fatalf("缺失标签不应出现在结果中: %#v", owners)
	}

	if _, err := store.ListDownloadIntentOwners(ctx, []string{"not-a-label"}); !errors.Is(err, ErrInvalidDownloadIntentOwnerLookup) {
		t.Fatalf("非法标签 error = %v", err)
	}
	if _, err := store.ListDownloadIntentOwners(nil, []string{label}); !errors.Is(err, ErrDownloadIntentOwnerLookup) {
		t.Fatalf("nil 上下文 error = %v", err)
	}
}
