//go:build integration

package database

import (
	"testing"

	"github.com/moviepilot-go/moviepilot-go/internal/subscription"
)

func TestSubscriptionStorePostgresYearRoundTrip(t *testing.T) {
	store := newSiteStatisticsIntegrationStore(t)
	insertSubscriptionIntegrationUsers(t, store, "year-owner")

	var yearDataType string
	if err := store.db.Pool.QueryRow(t.Context(), `
		SELECT data_type
		FROM information_schema.columns
		WHERE table_schema=current_schema()
		  AND table_name='subscriptions'
		  AND column_name='year'`).Scan(&yearDataType); err != nil {
		t.Fatalf("读取 subscriptions.year 类型: %v", err)
	}
	if yearDataType != "text" {
		t.Fatalf("migration 005 后 subscriptions.year 类型 = %q, want text", yearDataType)
	}

	tests := []struct {
		name    string
		year    string
		mediaID string
	}{
		{name: "空年份", year: "", mediaID: "year-empty"},
		{name: "数字字符串年份", year: "2026", mediaID: "year-numeric"},
		{name: "V2 非数字字符串年份", year: "TBA", mediaID: "year-tba"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			id, err := store.CreateSubscription(t.Context(), NewSubscription{
				Username:    "year-owner",
				Name:        "Year " + tt.mediaID,
				MediaType:   "movie",
				Year:        tt.year,
				MediaSource: "tmdb",
				MediaID:     tt.mediaID,
				State:       subscription.PendingSearch,
				Config:      map[string]any{"fixture": tt.mediaID},
			})
			if err != nil {
				t.Fatalf("创建年份为 %q 的订阅: %v", tt.year, err)
			}

			items, err := store.ListSubscriptions(t.Context(), "year-owner", false)
			if err != nil {
				t.Fatalf("列出年份为 %q 的订阅: %v", tt.year, err)
			}
			item, found := findSubscriptionByID(items, id)
			if !found {
				t.Fatalf("未找到刚创建的订阅 id=%d", id)
			}
			if item.Year != tt.year {
				t.Fatalf("ListSubscriptions 年份 = %q, want %q", item.Year, tt.year)
			}
			// 空年份按 NULL 存储（insertSubscription 的 NULLIF($6,'')），
			// V2() 因此输出 null；前端对 null 与 "" 等价处理，这里固定该契约。
			if tt.year == "" {
				if v2Year := item.V2()["year"]; v2Year != nil {
					t.Fatalf("空年份 V2 年份 = %#v, want nil", v2Year)
				}
				return
			}
			v2Year, ok := item.V2()["year"].(string)
			if !ok || v2Year != tt.year {
				t.Fatalf("V2 年份 = %#v, want string(%q)", item.V2()["year"], tt.year)
			}
		})
	}
}

func TestSubscriptionStorePostgresDuplicateIdentityUpsert(t *testing.T) {
	store := newSiteStatisticsIntegrationStore(t)
	insertSubscriptionIntegrationUsers(t, store, "duplicate-owner")

	season := 1
	firstID, err := store.CreateSubscription(t.Context(), NewSubscription{
		Username:    "duplicate-owner",
		Name:        "Original title",
		MediaType:   "tv",
		Year:        "2026",
		Season:      &season,
		MediaSource: "tmdb",
		MediaID:     "duplicate-identity",
		State:       subscription.PendingIdentification,
		Config:      map[string]any{"attempt": "first"},
	})
	if err != nil {
		t.Fatalf("首次创建重复身份订阅: %v", err)
	}
	secondID, err := store.CreateSubscription(t.Context(), NewSubscription{
		Username:    "duplicate-owner",
		Name:        "Second title",
		MediaType:   "tv",
		Year:        "TBA",
		Season:      &season,
		MediaSource: "tmdb",
		MediaID:     "duplicate-identity",
		State:       subscription.PendingSearch,
		Config:      map[string]any{"attempt": "second"},
	})
	if err != nil {
		t.Fatalf("重复创建相同身份订阅: %v", err)
	}
	if secondID != firstID {
		t.Fatalf("重复身份返回 id = %d, want first id %d", secondID, firstID)
	}

	items, err := store.ListSubscriptions(t.Context(), "duplicate-owner", false)
	if err != nil {
		t.Fatalf("列出重复身份订阅: %v", err)
	}
	if len(items) != 1 {
		t.Fatalf("重复身份订阅数 = %d, want 1", len(items))
	}
	if items[0].ID != firstID {
		t.Fatalf("唯一订阅 id = %d, want %d", items[0].ID, firstID)
	}
}

func TestSubscriptionStorePostgresUnidentifiedIdentity(t *testing.T) {
	store := newSiteStatisticsIntegrationStore(t)
	insertSubscriptionIntegrationUsers(t, store, "unidentified-owner", "unidentified-other")

	first := NewSubscription{
		Username:    "unidentified-owner",
		Name:        "Unidentified Movie A",
		MediaType:   "movie",
		Year:        "2026",
		Season:      nil,
		MediaSource: "",
		MediaID:     "",
		State:       subscription.PendingIdentification,
		Config:      map[string]any{"fixture": "unidentified-first"},
	}
	firstID, err := store.CreateSubscription(t.Context(), first)
	if err != nil {
		t.Fatalf("创建第一条未识别订阅: %v", err)
	}
	retryID, err := store.CreateSubscription(t.Context(), first)
	if err != nil {
		t.Fatalf("重试完全相同的未识别订阅: %v", err)
	}
	if retryID != firstID {
		t.Fatalf("完全相同 title/year/season 重试返回 id = %d, want %d", retryID, firstID)
	}

	second := first
	second.Name = "Unidentified Movie B"
	second.Year = "TBA"
	second.Config = map[string]any{"fixture": "unidentified-second"}
	secondID, err := store.CreateSubscription(t.Context(), second)
	if err != nil {
		t.Fatalf("创建标题和年份不同的未识别订阅: %v", err)
	}
	if secondID == firstID {
		t.Errorf("标题和年份不同的未识别订阅复用了 id=%d, want different IDs", firstID)
	}

	otherUser := first
	otherUser.Username = "unidentified-other"
	otherUser.Config = map[string]any{"fixture": "unidentified-other-user"}
	otherUserID, err := store.CreateSubscription(t.Context(), otherUser)
	if err != nil {
		t.Fatalf("其他用户创建同标题未识别订阅: %v", err)
	}
	if otherUserID == firstID {
		t.Errorf("不同用户的同标题订阅复用了 id=%d, want different IDs", firstID)
	}
	otherItems, err := store.ListSubscriptions(t.Context(), "unidentified-other", false)
	if err != nil {
		t.Fatalf("列出其他用户的未识别订阅: %v", err)
	}
	assertSubscriptionIDs(t, otherItems, []int64{otherUserID})

	ownerItems, err := store.ListSubscriptions(t.Context(), "unidentified-owner", false)
	if err != nil {
		t.Fatalf("列出同一用户的未识别订阅: %v", err)
	}
	assertSubscriptionIDs(t, ownerItems, []int64{firstID, secondID})

	firstItem, found := findSubscriptionByID(ownerItems, firstID)
	if !found {
		t.Fatalf("列表缺少第一条未识别订阅 id=%d", firstID)
	}
	if firstItem.Name != first.Name || firstItem.Year != first.Year || firstItem.Season != first.Season {
		t.Fatalf("第一条未识别订阅 = %#v, want title=%q year=%q season=%v",
			firstItem, first.Name, first.Year, first.Season)
	}
	secondItem, found := findSubscriptionByID(ownerItems, secondID)
	if !found {
		t.Fatalf("列表缺少第二条未识别订阅 id=%d", secondID)
	}
	if secondItem.Name != second.Name || secondItem.Year != second.Year || secondItem.Season != second.Season {
		t.Fatalf("第二条未识别订阅 = %#v, want title=%q year=%q season=%v",
			secondItem, second.Name, second.Year, second.Season)
	}
}

func TestSubscriptionStorePostgresListVisibility(t *testing.T) {
	store := newSiteStatisticsIntegrationStore(t)
	insertSubscriptionIntegrationUsers(t, store, "alice", "bob")

	aliceFirst := createSubscriptionIntegrationFixture(t, store, "alice", "alice-first")
	aliceSecond := createSubscriptionIntegrationFixture(t, store, "alice", "alice-second")
	bobOnly := createSubscriptionIntegrationFixture(t, store, "bob", "bob-only")

	tests := []struct {
		name     string
		username string
		admin    bool
		wantIDs  []int64
	}{
		{
			name:     "普通用户只见自己的记录",
			username: "alice",
			wantIDs:  []int64{aliceFirst, aliceSecond},
		},
		{
			name:     "另一普通用户只见自己的记录",
			username: "bob",
			wantIDs:  []int64{bobOnly},
		},
		{
			name:     "管理员可见全部记录",
			username: "ignored-for-admin",
			admin:    true,
			wantIDs:  []int64{aliceFirst, aliceSecond, bobOnly},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			items, err := store.ListSubscriptions(t.Context(), tt.username, tt.admin)
			if err != nil {
				t.Fatalf("列出订阅: %v", err)
			}
			assertSubscriptionIDs(t, items, tt.wantIDs)
			if !tt.admin {
				for _, item := range items {
					if item.Username != tt.username {
						t.Fatalf("普通用户 %q 看到了用户 %q 的订阅 id=%d", tt.username, item.Username, item.ID)
					}
				}
			}
		})
	}
}

func insertSubscriptionIntegrationUsers(t *testing.T, store *Store, usernames ...string) {
	t.Helper()
	for _, username := range usernames {
		if _, err := store.db.Pool.Exec(t.Context(), `
			INSERT INTO users (username, password_hash)
			VALUES ($1, 'integration-test-password-hash')`, username); err != nil {
			t.Fatalf("创建订阅测试用户 %q: %v", username, err)
		}
	}
}

func createSubscriptionIntegrationFixture(t *testing.T, store *Store, username, mediaID string) int64 {
	t.Helper()
	id, err := store.CreateSubscription(t.Context(), NewSubscription{
		Username:    username,
		Name:        mediaID,
		MediaType:   "movie",
		Year:        "2026",
		MediaSource: "tmdb",
		MediaID:     mediaID,
		State:       subscription.PendingSearch,
		Config:      map[string]any{"fixture": mediaID},
	})
	if err != nil {
		t.Fatalf("创建订阅 fixture %q/%q: %v", username, mediaID, err)
	}
	return id
}

func findSubscriptionByID(items []Subscription, id int64) (Subscription, bool) {
	for _, item := range items {
		if item.ID == id {
			return item, true
		}
	}
	return Subscription{}, false
}

func assertSubscriptionIDs(t *testing.T, items []Subscription, wantIDs []int64) {
	t.Helper()
	if len(items) != len(wantIDs) {
		t.Fatalf("可见订阅数 = %d, want %d; items=%#v", len(items), len(wantIDs), items)
	}
	want := make(map[int64]struct{}, len(wantIDs))
	for _, id := range wantIDs {
		want[id] = struct{}{}
	}
	for _, item := range items {
		if _, exists := want[item.ID]; !exists {
			t.Fatalf("出现非预期订阅 id=%d; want ids=%v", item.ID, wantIDs)
		}
		delete(want, item.ID)
	}
	if len(want) != 0 {
		t.Fatalf("缺少订阅 ids=%v", want)
	}
}
