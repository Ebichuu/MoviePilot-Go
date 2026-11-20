//go:build integration

package database

import (
	"errors"
	"testing"
)

// TestSubscribeShareStoreIntegration 覆盖订阅分享的新建、按主键/键读取、
// 关注幂等与计数、取消关注以及删除权限隔离。
func TestSubscribeShareStoreIntegration(t *testing.T) {
	store := newSiteStatisticsIntegrationStore(t)
	ctx := t.Context()
	insertSubscriptionIntegrationUsers(t, store, "share-owner", "share-fan", "share-other")

	season := 2
	created, err := store.CreateSubscribeShare(ctx, SubscribeShare{
		ShareKey:    "share-key-1",
		Username:    "share-owner",
		Title:       "分享剧集",
		MediaType:   "电视剧",
		Year:        "2026",
		Season:      &season,
		MediaSource: "themoviedb",
		MediaID:     "9001",
		Sites:       []int{1, 3},
		Filter:      "resolution=1080p",
		Description: "分享说明",
		Poster:      "https://img.example.com/p.jpg",
	})
	if err != nil {
		t.Fatalf("CreateSubscribeShare() error = %v", err)
	}
	if created.ID <= 0 || created.FollowCount != 0 || created.Season == nil || *created.Season != season {
		t.Fatalf("CreateSubscribeShare() = %#v", created)
	}
	if len(created.Sites) != 2 || created.Sites[1] != 3 {
		t.Fatalf("分享站点 = %#v", created.Sites)
	}

	byID, err := store.GetSubscribeShare(ctx, created.ID)
	if err != nil || byID.ShareKey != "share-key-1" || byID.Filter != "resolution=1080p" {
		t.Fatalf("GetSubscribeShare() = (%#v, %v)", byID, err)
	}
	byKey, err := store.GetSubscribeShareByKey(ctx, "share-key-1")
	if err != nil || byKey.ID != created.ID {
		t.Fatalf("GetSubscribeShareByKey() = (%#v, %v)", byKey, err)
	}
	if _, err := store.GetSubscribeShare(ctx, 999999); !errors.Is(err, ErrSubscribeShareNotFound) {
		t.Fatalf("缺失分享 error = %v, want ErrSubscribeShareNotFound", err)
	}
	if _, err := store.GetSubscribeShareByKey(ctx, "missing-key"); !errors.Is(err, ErrSubscribeShareNotFound) {
		t.Fatalf("缺失 share_key error = %v, want ErrSubscribeShareNotFound", err)
	}

	// 第二用户不可见别人的分享列表。
	if _, err := store.CreateSubscribeShare(ctx, SubscribeShare{
		ShareKey: "share-key-2", Username: "share-fan", Title: "另一个分享", MediaType: "电影",
	}); err != nil {
		t.Fatalf("CreateSubscribeShare(second) error = %v", err)
	}
	ownerShares, err := store.ListSubscribeShares(ctx, "share-owner")
	if err != nil || len(ownerShares) != 1 || ownerShares[0].ID != created.ID {
		t.Fatalf("ListSubscribeShares(owner) = (%#v, %v)", ownerShares, err)
	}
	if shares, err := store.ListSubscribeShares(ctx, "share-other"); err != nil || len(shares) != 0 {
		t.Fatalf("ListSubscribeShares(other) = (%v, %v)", shares, err)
	}

	// 关注幂等：重复关注不重复计数。
	if followed, err := store.FollowSubscribeShare(ctx, "share-fan", created.ID); err != nil || !followed {
		t.Fatalf("FollowSubscribeShare() = (%v, %v)", followed, err)
	}
	if followed, err := store.FollowSubscribeShare(ctx, "share-fan", created.ID); err != nil || !followed {
		t.Fatalf("重复 FollowSubscribeShare() = (%v, %v)", followed, err)
	}
	if followed, err := store.FollowSubscribeShare(ctx, "share-fan", 999999); !errors.Is(err, ErrSubscribeShareNotFound) || followed {
		t.Fatalf("关注缺失分享 = (%v, %v)", followed, err)
	}
	refreshed, err := store.GetSubscribeShare(ctx, created.ID)
	if err != nil || refreshed.FollowCount != 1 {
		t.Fatalf("关注后 follow_count = (%d, %v)", refreshed.FollowCount, err)
	}
	follows, err := store.ListSubscribeFollows(ctx, "share-fan")
	if err != nil || len(follows) != 1 || follows[0].ID != created.ID {
		t.Fatalf("ListSubscribeFollows() = (%#v, %v)", follows, err)
	}

	if removed, err := store.UnfollowSubscribeShare(ctx, "share-fan", created.ID); err != nil || !removed {
		t.Fatalf("UnfollowSubscribeShare() = (%v, %v)", removed, err)
	}
	if removed, err := store.UnfollowSubscribeShare(ctx, "share-fan", created.ID); err != nil || removed {
		t.Fatalf("重复 UnfollowSubscribeShare() = (%v, %v)", removed, err)
	}
	if follows, err := store.ListSubscribeFollows(ctx, "share-fan"); err != nil || len(follows) != 0 {
		t.Fatalf("取消关注后 ListSubscribeFollows() = (%v, %v)", follows, err)
	}
	refreshed, err = store.GetSubscribeShare(ctx, created.ID)
	if err != nil || refreshed.FollowCount != 0 {
		t.Fatalf("取消关注后 follow_count = (%d, %v)", refreshed.FollowCount, err)
	}

	// 只有创建者能删除自己的分享。
	if deleted, err := store.DeleteSubscribeShare(ctx, "share-fan", created.ID); err != nil || deleted {
		t.Fatalf("他人 DeleteSubscribeShare() = (%v, %v)", deleted, err)
	}
	if deleted, err := store.DeleteSubscribeShare(ctx, "share-owner", created.ID); err != nil || !deleted {
		t.Fatalf("DeleteSubscribeShare() = (%v, %v)", deleted, err)
	}
	if _, err := store.GetSubscribeShare(ctx, created.ID); !errors.Is(err, ErrSubscribeShareNotFound) {
		t.Fatalf("删除后 GetSubscribeShare() error = %v", err)
	}
	if deleted, err := store.DeleteSubscribeShare(ctx, "share-owner", created.ID); err != nil || deleted {
		t.Fatalf("重复 DeleteSubscribeShare() = (%v, %v)", deleted, err)
	}
}
