//go:build integration

package database

import (
	"reflect"
	"testing"
)

func TestLoadV2CandidateSortInputsMatchesV2LatestUserDataDictionary(t *testing.T) {
	store := newSiteStatisticsIntegrationStore(t)
	ctx := t.Context()

	if _, err := store.db.Pool.Exec(ctx, `
		INSERT INTO settings (key, value)
		VALUES ('TorrentsPriority', '["site", "upload", "unknown"]'::jsonb)`); err != nil {
		t.Fatalf("准备种子排序设置: %v", err)
	}

	var firstSiteID, secondSiteID, missingSiteID, nullSiteID, zeroSiteID, negativeSiteID int
	if err := store.db.Pool.QueryRow(ctx, `
		INSERT INTO sites (name, domain, url, adapter, config)
		VALUES ('First', 'first.example', 'https://first.example', 'fixture', '{"pri":7}'::jsonb)
		RETURNING id`).Scan(&firstSiteID); err != nil {
		t.Fatalf("准备第一站点: %v", err)
	}
	if err := store.db.Pool.QueryRow(ctx, `
		INSERT INTO sites (name, domain, url, adapter, config)
		VALUES ('Second', 'second.example', 'https://second.example', 'fixture', '{}'::jsonb)
		RETURNING id`).Scan(&secondSiteID); err != nil {
		t.Fatalf("准备第二站点: %v", err)
	}
	if err := store.db.Pool.QueryRow(ctx, `
		INSERT INTO sites (name, domain, url, adapter, config)
		VALUES ('Missing', 'missing.example', 'https://missing.example', 'fixture', '{"pri":"invalid"}'::jsonb)
		RETURNING id`).Scan(&missingSiteID); err != nil {
		t.Fatalf("准备缺失用户数据站点: %v", err)
	}
	if err := store.db.Pool.QueryRow(ctx, `
		INSERT INTO sites (name, domain, url, adapter, config)
		VALUES ('Null', 'null.example', 'https://null.example', 'fixture', '{"pri":null}'::jsonb)
		RETURNING id`).Scan(&nullSiteID); err != nil {
		t.Fatalf("准备空优先级站点: %v", err)
	}
	if err := store.db.Pool.QueryRow(ctx, `
		INSERT INTO sites (name, domain, url, adapter, config)
		VALUES ('Zero', 'zero-priority.example', 'https://zero-priority.example', 'fixture', '{"pri":0}'::jsonb)
		RETURNING id`).Scan(&zeroSiteID); err != nil {
		t.Fatalf("准备零优先级站点: %v", err)
	}
	if err := store.db.Pool.QueryRow(ctx, `
		INSERT INTO sites (name, domain, url, adapter, config)
		VALUES ('Negative', 'negative.example', 'https://negative.example', 'fixture', '{"pri":-2}'::jsonb)
		RETURNING id`).Scan(&negativeSiteID); err != nil {
		t.Fatalf("准备负优先级站点: %v", err)
	}

	if _, err := store.db.Pool.Exec(ctx, `
		INSERT INTO site_userdata (domain, name, upload, err_msg, updated_day, updated_time)
		VALUES
			('first.example', 'First', 100, '', '2026-08-14', '23:59:59'),
			('first.example', 'First', 200, NULL, '2026-08-15', '10:00:00'),
			('first.example', 'First', 300, 'same-day request failed', '2026-08-15', '08:00:00'),
			('first.example', 'First', 999, 'newer-day request failed', '2026-08-16', '01:00:00'),
			('a-earlier.example', 'Shared', 600, '', '2026-08-15', '06:00:00'),
			('z-later.example', 'Shared', 500, '', '2026-08-15', '11:00:00'),
			('failure-only.example', 'FailureOnly', 700, 'request failed', '2026-08-15', '05:00:00'),
			('second.example', 'Second', 456.5, NULL, '2026-08-15', '09:00:00'),
			('nullable.example', 'Nullable', NULL, '', '2026-08-15', '04:00:00'),
			('zero.example', 'Zero', 0, '', '2026-08-15', '03:00:00')`); err != nil {
		t.Fatalf("准备站点用户数据历史: %v", err)
	}

	rules, uploads, siteOrders, err := store.LoadV2CandidateSortInputs(ctx)
	if err != nil {
		t.Fatalf("LoadV2CandidateSortInputs() error = %v", err)
	}
	if want := []string{"site", "upload", "unknown"}; !reflect.DeepEqual(rules, want) {
		t.Fatalf("priority rules = %v, want %v", rules, want)
	}
	uploadTests := []struct {
		name   string
		key    string
		want   float64
		exists bool
	}{
		{
			name:   "same-day error row overwrites after global descending sort",
			key:    "First",
			want:   300,
			exists: true,
		},
		{
			name:   "shared name is overwritten by globally earlier row",
			key:    "Shared",
			want:   600,
			exists: true,
		},
		{name: "second site", key: "Second", want: 456.5, exists: true},
		{name: "nullable upload", key: "Nullable", want: 0, exists: true},
		{name: "zero upload", key: "Zero", want: 0, exists: true},
		{name: "domain without successful day", key: "FailureOnly", exists: false},
		{name: "site without user data", key: "Missing", exists: false},
	}
	for _, tt := range uploadTests {
		t.Run(tt.name, func(t *testing.T) {
			upload, exists := uploads[tt.key]
			if exists != tt.exists || upload != tt.want {
				t.Fatalf("uploads[%q] = (%v, %t), want (%v, %t)", tt.key, upload, exists, tt.want, tt.exists)
			}
		})
	}
	if siteOrders[firstSiteID] != 7 || siteOrders[secondSiteID] != 0 || siteOrders[missingSiteID] != 0 ||
		siteOrders[nullSiteID] != 0 || siteOrders[zeroSiteID] != 0 || siteOrders[negativeSiteID] != -2 {
		t.Fatalf(
			"site orders = first:%d second:%d missing:%d null:%d zero:%d negative:%d, want 7,0,0,0,0,-2",
			siteOrders[firstSiteID], siteOrders[secondSiteID], siteOrders[missingSiteID],
			siteOrders[nullSiteID], siteOrders[zeroSiteID], siteOrders[negativeSiteID],
		)
	}
}
