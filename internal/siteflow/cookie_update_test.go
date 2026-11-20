package siteflow

import (
	"context"
	"errors"
	"testing"

	"github.com/moviepilot-go/moviepilot-go/internal/database"
	"github.com/moviepilot-go/moviepilot-go/internal/indexer"
)

type fakeCookieStore struct {
	site    database.Site
	updated database.Site
	updates int
}

func (f *fakeCookieStore) RuntimeSiteByID(context.Context, int) (database.Site, error) {
	return f.site, nil
}

func (f *fakeCookieStore) UpdateSite(_ context.Context, site database.Site) error {
	f.updated = site
	f.updates++
	return nil
}

type fakeSignInAdapter struct {
	cookie string
	err    error
}

func (fakeSignInAdapter) Name() string { return "fake" }

func (fakeSignInAdapter) Search(context.Context, indexer.Query) ([]indexer.Resource, error) {
	return nil, nil
}

func (a fakeSignInAdapter) SignIn(context.Context, string, string) (string, error) {
	return a.cookie, a.err
}

type plainAdapter struct{}

func (plainAdapter) Name() string { return "plain" }

func (plainAdapter) Search(context.Context, indexer.Query) ([]indexer.Resource, error) {
	return nil, nil
}

func TestCookieUpdaterPersistsSignInCookie(t *testing.T) {
	t.Parallel()

	store := &fakeCookieStore{site: database.Site{ID: 3, Name: "Zmpt", URL: "https://zmpt.cc"}}
	updater := NewCookieUpdater(store, func(database.Site) (indexer.Adapter, error) {
		return fakeSignInAdapter{cookie: "c_secure_uid=NzM; c_secure_pass=abc123"}, nil
	})

	success, message, err := updater.UpdateSiteCookie(t.Context(), 3, "user", "pass", "")
	if err != nil {
		t.Fatalf("UpdateSiteCookie() error = %v", err)
	}
	if !success || message != "站点 Cookie 更新成功" {
		t.Fatalf("UpdateSiteCookie() = (%t, %q), want success", success, message)
	}
	if store.updates != 1 {
		t.Fatalf("UpdateSite 调用次数 = %d, want 1", store.updates)
	}
	if got := store.updated.Config["cookie"]; got != "c_secure_uid=NzM; c_secure_pass=abc123" {
		t.Fatalf("持久化 Cookie = %v", got)
	}
}

func TestCookieUpdaterReportsSignInFailure(t *testing.T) {
	t.Parallel()

	store := &fakeCookieStore{site: database.Site{ID: 3, Name: "Zmpt"}}
	updater := NewCookieUpdater(store, func(database.Site) (indexer.Adapter, error) {
		return fakeSignInAdapter{err: errors.New("站点未返回登录 Cookie")}, nil
	})

	success, message, err := updater.UpdateSiteCookie(t.Context(), 3, "user", "pass", "")
	if err != nil {
		t.Fatalf("UpdateSiteCookie() error = %v", err)
	}
	if success || message != "站点未返回登录 Cookie" {
		t.Fatalf("UpdateSiteCookie() = (%t, %q), want failure message", success, message)
	}
	if store.updates != 0 {
		t.Fatalf("失败时不应写库，调用次数 = %d", store.updates)
	}
}

func TestCookieUpdaterRejectsAdapterWithoutSignIn(t *testing.T) {
	t.Parallel()

	store := &fakeCookieStore{site: database.Site{ID: 3, Name: "Zmpt"}}
	updater := NewCookieUpdater(store, func(database.Site) (indexer.Adapter, error) {
		return plainAdapter{}, nil
	})

	success, message, err := updater.UpdateSiteCookie(t.Context(), 3, "user", "pass", "")
	if err != nil {
		t.Fatalf("UpdateSiteCookie() error = %v", err)
	}
	if success || message != "该站点适配器不支持登录，请手动更新 Cookie" {
		t.Fatalf("UpdateSiteCookie() = (%t, %q)", success, message)
	}
}
