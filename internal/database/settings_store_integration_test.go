//go:build integration

package database

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"testing"
)

func TestSettingsStorePostgresRoundTripAndOverwrite(t *testing.T) {
	store := newSiteStatisticsIntegrationStore(t)

	type settingFixture struct {
		Name  string   `json:"name"`
		Paths []string `json:"paths"`
		Limit int      `json:"limit"`
	}

	target := settingFixture{Name: "unchanged"}
	found, err := store.GetSetting(t.Context(), "missing-setting", &target)
	if err != nil || found || target.Name != "unchanged" {
		t.Fatalf("GetSetting(missing) found=%v target=%#v error=%v", found, target, err)
	}

	const key = "store setting '$1'"
	initial := settingFixture{Name: "initial", Paths: []string{"/media/a"}, Limit: 2}
	if err := store.SetSetting(t.Context(), key, initial, false); err != nil {
		t.Fatalf("SetSetting(initial) error = %v", err)
	}
	var loaded settingFixture
	found, err = store.GetSetting(t.Context(), key, &loaded)
	if err != nil || !found || !reflect.DeepEqual(loaded, initial) {
		t.Fatalf("GetSetting(initial) found=%v value=%#v error=%v", found, loaded, err)
	}

	replacement := settingFixture{Name: "replacement", Paths: []string{"/media/b", "/media/c"}, Limit: 4}
	if err := store.SetSetting(t.Context(), key, replacement, true); err != nil {
		t.Fatalf("SetSetting(replacement) error = %v", err)
	}
	loaded = settingFixture{}
	found, err = store.GetSetting(t.Context(), key, &loaded)
	if err != nil || !found || !reflect.DeepEqual(loaded, replacement) {
		t.Fatalf("GetSetting(replacement) found=%v value=%#v error=%v", found, loaded, err)
	}

	var (
		storedValue []byte
		encrypted   bool
		rowCount    int
	)
	if err := store.db.Pool.QueryRow(t.Context(), `
		SELECT value, encrypted, count(*) OVER ()
		FROM settings
		WHERE key=$1`, key).Scan(&storedValue, &encrypted, &rowCount); err != nil {
		t.Fatalf("读取覆盖后的设置: %v", err)
	}
	var stored settingFixture
	if err := json.Unmarshal(storedValue, &stored); err != nil {
		t.Fatalf("解析数据库设置: %v", err)
	}
	if !encrypted || rowCount != 1 || !reflect.DeepEqual(stored, replacement) {
		t.Fatalf("stored setting encrypted=%v rows=%d value=%#v", encrypted, rowCount, stored)
	}

	var incompatible string
	if found, err := store.GetSetting(t.Context(), key, &incompatible); err == nil || found || !strings.Contains(err.Error(), "解析设置") {
		t.Fatalf("GetSetting(incompatible target) found=%v error=%v", found, err)
	}
}

func TestSettingsStorePostgresPropagatesCancellation(t *testing.T) {
	store := newSiteStatisticsIntegrationStore(t)
	ctx, cancel := context.WithCancel(t.Context())
	cancel()

	target := map[string]any{"unchanged": true}
	if _, err := store.GetSetting(ctx, "cancelled-setting", &target); !errors.Is(err, context.Canceled) {
		t.Fatalf("GetSetting(cancelled) error = %v, want context.Canceled", err)
	}
	if !reflect.DeepEqual(target, map[string]any{"unchanged": true}) {
		t.Fatalf("GetSetting(cancelled) target = %#v", target)
	}
	if err := store.SetSetting(ctx, "cancelled-setting", map[string]any{}, false); !errors.Is(err, context.Canceled) {
		t.Fatalf("SetSetting(cancelled) error = %v, want context.Canceled", err)
	}
}
