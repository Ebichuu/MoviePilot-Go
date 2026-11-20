//go:build integration

package database

import (
	"context"
	"os"
	"strings"
	"testing"

	neturl "net/url"

	appconfig "github.com/moviepilot-go/moviepilot-go/internal/config"
)

// TestUserStorePostgresCRUDIntegration 覆盖用户增删改查、权限编码与超管计数。
func TestUserStorePostgresCRUDIntegration(t *testing.T) {
	store := newSiteStatisticsIntegrationStore(t)
	ctx := t.Context()

	created, err := store.CreateUser(ctx, "user-a", "hash-a", true, map[string]any{"manage": true})
	if err != nil {
		t.Fatalf("CreateUser() error = %v", err)
	}
	if created.ID <= 0 || created.Username != "user-a" || !created.SuperUser || created.Permissions["manage"] != true {
		t.Fatalf("CreateUser() = %#v", created)
	}
	if _, err := store.CreateUser(ctx, "user-b", "hash-b", false, nil); err != nil {
		t.Fatalf("CreateUser(nil permissions) error = %v", err)
	}
	if _, err := store.CreateUser(ctx, "user-b", "hash-b", false, nil); err == nil {
		t.Fatal("重名 CreateUser() 应报错")
	}
	if _, err := store.CreateUser(ctx, "user-c", "hash-c", false, map[string]any{"bad": make(chan int)}); err == nil {
		t.Fatal("不可编码权限 CreateUser() 应报错")
	}

	users, err := store.ListUsers(ctx)
	if err != nil {
		t.Fatalf("ListUsers() error = %v", err)
	}
	if len(users) != 2 || users[0].Username != "user-a" || users[1].Username != "user-b" {
		t.Fatalf("ListUsers() = %#v", users)
	}
	if users[1].Permissions == nil || len(users[1].Permissions) != 0 {
		t.Fatalf("空权限应编码成空对象: %#v", users[1].Permissions)
	}
	if count, err := store.CountSuperUsers(ctx); err != nil || count != 1 {
		t.Fatalf("CountSuperUsers() = (%d, %v)", count, err)
	}

	updated, err := store.UpdateUser(ctx, created.ID, "user-a2", "", false, map[string]any{"manage": false})
	if err != nil {
		t.Fatalf("UpdateUser() error = %v", err)
	}
	if updated.Username != "user-a2" || updated.SuperUser || updated.Password != "hash-a" {
		t.Fatalf("保持密码的 UpdateUser() = %#v", updated)
	}
	updated, err = store.UpdateUser(ctx, created.ID, "user-a3", "hash-a2", true, nil)
	if err != nil || updated.Password != "hash-a2" || !updated.SuperUser {
		t.Fatalf("改密码的 UpdateUser() = (%#v, %v)", updated, err)
	}
	if _, err := store.UpdateUser(ctx, 999999, "ghost", "", false, nil); err == nil {
		t.Fatal("不存在用户 UpdateUser() 应报错")
	}

	if err := store.UpdateUserAvatar(ctx, created.ID, "data:image/png;base64,AAAA"); err != nil {
		t.Fatalf("UpdateUserAvatar() error = %v", err)
	}
	avatarUser, err := store.UserByID(ctx, created.ID)
	if err != nil || avatarUser.Avatar == nil || *avatarUser.Avatar != "data:image/png;base64,AAAA" {
		t.Fatalf("头像 = (%v, %v)", avatarUser.Avatar, err)
	}

	if err := store.DeleteUser(ctx, created.ID); err != nil {
		t.Fatalf("DeleteUser() error = %v", err)
	}
	if _, err := store.UserByID(ctx, created.ID); err == nil {
		t.Fatal("删除后 UserByID() 应报错")
	}
	if err := store.DeleteUser(ctx, created.ID); err != nil {
		t.Fatalf("重复 DeleteUser() 应幂等: %v", err)
	}
}

// TestWebPushSubscriptionStoreIntegration 覆盖推送订阅的 upsert、稳定排序与删除。
func TestWebPushSubscriptionStoreIntegration(t *testing.T) {
	store := newSiteStatisticsIntegrationStore(t)
	ctx := t.Context()

	if err := store.UpsertWebPushSubscription(ctx, WebPushSubscription{
		Endpoint: "https://push.example.com/b", Username: "owner", P256dh: "key-b", Auth: "auth-b", UserAgent: "UA/1",
	}); err != nil {
		t.Fatalf("UpsertWebPushSubscription() error = %v", err)
	}
	if err := store.UpsertWebPushSubscription(ctx, WebPushSubscription{
		Endpoint: "https://push.example.com/a", Username: "owner", P256dh: "key-a", Auth: "auth-a",
	}); err != nil {
		t.Fatalf("UpsertWebPushSubscription(a) error = %v", err)
	}
	// 同 endpoint 覆盖不新增。
	if err := store.UpsertWebPushSubscription(ctx, WebPushSubscription{
		Endpoint: "https://push.example.com/a", Username: "owner2", P256dh: "key-a2", Auth: "auth-a2", UserAgent: "UA/2",
	}); err != nil {
		t.Fatalf("覆盖 UpsertWebPushSubscription() error = %v", err)
	}
	for _, invalid := range []WebPushSubscription{
		{Endpoint: "https://push.example.com/x", P256dh: "key", Auth: ""},
		{Endpoint: "", P256dh: "key", Auth: "auth"},
		{Endpoint: "https://push.example.com/y", P256dh: strings.Repeat("k", 257), Auth: "auth"},
	} {
		if err := store.UpsertWebPushSubscription(ctx, invalid); err == nil {
			t.Fatalf("非法订阅 %#v 应报错", invalid)
		}
	}

	subscriptions, err := store.ListWebPushSubscriptions(ctx)
	if err != nil {
		t.Fatalf("ListWebPushSubscriptions() error = %v", err)
	}
	if len(subscriptions) != 2 || subscriptions[0].Endpoint != "https://push.example.com/a" {
		t.Fatalf("ListWebPushSubscriptions() = %#v", subscriptions)
	}
	if subscriptions[0].P256dh != "key-a2" || subscriptions[0].Username != "owner2" || subscriptions[0].UserAgent != "UA/2" {
		t.Fatalf("覆盖后的订阅 = %#v", subscriptions[0])
	}

	deleted, err := store.DeleteWebPushSubscription(ctx, " https://push.example.com/a ")
	if err != nil || !deleted {
		t.Fatalf("DeleteWebPushSubscription() = (%v, %v)", deleted, err)
	}
	if deleted, err := store.DeleteWebPushSubscription(ctx, "https://push.example.com/a"); err != nil || deleted {
		t.Fatalf("重复 DeleteWebPushSubscription() = (%v, %v)", deleted, err)
	}
	if deleted, err := store.DeleteWebPushSubscription(ctx, "  "); err == nil || deleted {
		t.Fatalf("空 endpoint DeleteWebPushSubscription() = (%v, %v)", deleted, err)
	}
	if subscriptions, err := store.ListWebPushSubscriptions(ctx); err != nil || len(subscriptions) != 1 {
		t.Fatalf("删除后 ListWebPushSubscriptions() = (%d, %v)", len(subscriptions), err)
	}
}

// TestDatabaseOpenIntegration 覆盖 Open 的连接池装配与 Ping/Close。
func TestDatabaseOpenIntegration(t *testing.T) {
	base := newSiteStatisticsIntegrationStore(t)
	ctx := t.Context()
	var schema string
	if err := base.db.Pool.QueryRow(ctx, "SELECT current_schema()").Scan(&schema); err != nil {
		t.Fatalf("读取当前 schema: %v", err)
	}
	databaseURL := strings.TrimSpace(os.Getenv(siteStatisticsTestDatabaseEnv))
	parsed, err := neturl.Parse(databaseURL)
	if err != nil {
		t.Fatalf("解析数据库地址: %v", err)
	}
	query := parsed.Query()
	query.Set("search_path", schema)
	parsed.RawQuery = query.Encode()

	db, err := Open(context.Background(), appconfig.Config{DatabaseURL: parsed.String(), PostgresMaxConns: 2})
	if err != nil {
		t.Fatalf("Open() error = %v", err)
	}
	if err := db.Ping(t.Context()); err != nil {
		t.Fatalf("Ping() error = %v", err)
	}
	db.Close()
}
