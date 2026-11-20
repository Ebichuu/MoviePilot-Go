//go:build integration

package database

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"sync"
	"testing"

	"github.com/jackc/pgx/v5"
	"golang.org/x/crypto/bcrypt"
)

func TestUserStorePostgresBootstrapAndLookup(t *testing.T) {
	store := newSiteStatisticsIntegrationStore(t)

	if err := store.EnsureBootstrapUser(t.Context(), "blank-password-user", "   "); err != nil {
		t.Fatalf("EnsureBootstrapUser(blank password) error = %v", err)
	}
	if _, err := store.UserByName(t.Context(), "blank-password-user"); !errors.Is(err, pgx.ErrNoRows) {
		t.Fatalf("UserByName(blank password user) error = %v, want pgx.ErrNoRows", err)
	}

	const (
		username            = "bootstrap-admin"
		initialPassword     = "initial-password"
		replacementPassword = "replacement-password"
	)
	if err := store.EnsureBootstrapUser(t.Context(), username, initialPassword); err != nil {
		t.Fatalf("EnsureBootstrapUser(first) error = %v", err)
	}
	if _, err := store.db.Pool.Exec(t.Context(), `
		UPDATE users
		SET avatar=$2, permissions=$3::jsonb
		WHERE username=$1`, username, "avatar.png", `{"library":{"read":true}}`); err != nil {
		t.Fatalf("更新初始用户字段: %v", err)
	}
	if err := store.EnsureBootstrapUser(t.Context(), username, replacementPassword); err != nil {
		t.Fatalf("EnsureBootstrapUser(replay) error = %v", err)
	}

	byName, err := store.UserByName(t.Context(), username)
	if err != nil {
		t.Fatalf("UserByName() error = %v", err)
	}
	if byName.ID <= 0 || byName.Username != username || !byName.SuperUser || byName.Avatar == nil || *byName.Avatar != "avatar.png" {
		t.Fatalf("UserByName() = %#v", byName)
	}
	wantPermissions := map[string]any{"library": map[string]any{"read": true}}
	if !reflect.DeepEqual(byName.Permissions, wantPermissions) {
		t.Fatalf("UserByName().Permissions = %#v, want %#v", byName.Permissions, wantPermissions)
	}
	if err := bcrypt.CompareHashAndPassword([]byte(byName.Password), []byte(initialPassword)); err != nil {
		t.Fatalf("bootstrap password hash does not match initial password: %v", err)
	}
	if err := bcrypt.CompareHashAndPassword([]byte(byName.Password), []byte(replacementPassword)); err == nil {
		t.Fatal("bootstrap replay replaced the existing password")
	}

	byID, err := store.UserByID(t.Context(), byName.ID)
	if err != nil {
		t.Fatalf("UserByID() error = %v", err)
	}
	if !reflect.DeepEqual(byID, byName) {
		t.Fatalf("UserByID() = %#v, want %#v", byID, byName)
	}
	if _, err := store.UserByName(t.Context(), "missing-user"); !errors.Is(err, pgx.ErrNoRows) {
		t.Fatalf("UserByName(missing) error = %v, want pgx.ErrNoRows", err)
	}
	if _, err := store.UserByID(t.Context(), byName.ID+1_000_000); !errors.Is(err, pgx.ErrNoRows) {
		t.Fatalf("UserByID(missing) error = %v, want pgx.ErrNoRows", err)
	}

	if _, err := store.db.Pool.Exec(t.Context(), `UPDATE users SET permissions='[]'::jsonb WHERE id=$1`, byName.ID); err != nil {
		t.Fatalf("写入不兼容权限 JSON: %v", err)
	}
	if _, err := store.UserByID(t.Context(), byName.ID); err == nil || !strings.Contains(err.Error(), "解析用户权限") {
		t.Fatalf("UserByID(incompatible permissions) error = %v", err)
	}
}

func TestUserStorePostgresBootstrapIsConcurrentIdempotent(t *testing.T) {
	store := newSiteStatisticsIntegrationStore(t)

	const workers = 4
	start := make(chan struct{})
	errorsByWorker := make(chan error, workers)
	var wait sync.WaitGroup
	wait.Add(workers)
	for range workers {
		go func() {
			defer wait.Done()
			<-start
			errorsByWorker <- store.EnsureBootstrapUser(t.Context(), "concurrent-admin", "concurrent-password")
		}()
	}
	close(start)
	wait.Wait()
	close(errorsByWorker)
	for err := range errorsByWorker {
		if err != nil {
			t.Fatalf("concurrent EnsureBootstrapUser() error = %v", err)
		}
	}

	var count int
	if err := store.db.Pool.QueryRow(t.Context(), `SELECT count(*) FROM users WHERE username=$1`, "concurrent-admin").Scan(&count); err != nil {
		t.Fatalf("统计并发初始用户: %v", err)
	}
	if count != 1 {
		t.Fatalf("concurrent bootstrap user count = %d, want 1", count)
	}
}

func TestUserStorePostgresPropagatesCancellation(t *testing.T) {
	store := newSiteStatisticsIntegrationStore(t)
	ctx, cancel := context.WithCancel(t.Context())
	cancel()

	if err := store.EnsureBootstrapUser(ctx, "cancelled-admin", "cancelled-password"); !errors.Is(err, context.Canceled) {
		t.Fatalf("EnsureBootstrapUser(cancelled) error = %v, want context.Canceled", err)
	}
	if _, err := store.UserByName(ctx, "cancelled-admin"); !errors.Is(err, context.Canceled) {
		t.Fatalf("UserByName(cancelled) error = %v, want context.Canceled", err)
	}
	if _, err := store.UserByID(ctx, 1); !errors.Is(err, context.Canceled) {
		t.Fatalf("UserByID(cancelled) error = %v, want context.Canceled", err)
	}
}
