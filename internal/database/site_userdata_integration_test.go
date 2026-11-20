//go:build integration

package database

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/pressly/goose/v3"
)

func TestSiteUserDataFieldsMigrationPostgresDownUp(t *testing.T) {
	store := newSiteStatisticsIntegrationStore(t)
	ctx := t.Context()
	var schema string
	if err := store.db.Pool.QueryRow(ctx, `SELECT current_schema()`).Scan(&schema); err != nil {
		t.Fatalf("读取隔离 schema: %v", err)
	}
	migrationDB, _ := openSubscriptionV2StateMigrationDB(t, schema)
	goose.SetBaseFS(migrationFS)
	if err := goose.SetDialect("postgres"); err != nil {
		t.Fatalf("设置 goose PostgreSQL 方言: %v", err)
	}
	if _, err := store.db.Pool.Exec(ctx, `
		INSERT INTO site_userdata (domain, name, upload, err_msg, updated_day, updated_time)
		VALUES ('migration.example', 'Migration', 123, '', '2026-08-16', '12:00:00')`); err != nil {
		t.Fatalf("准备 migration 034 旧字段数据: %v", err)
	}
	if count := siteUserDataAddedColumnCount(t, store); count != 14 {
		t.Fatalf("migration 034 Up 字段数 = %d, want 14", count)
	}
	if err := goose.DownToContext(ctx, migrationDB, "migrations", 33); err != nil {
		t.Fatalf("回退 migration 034: %v", err)
	}
	if count := siteUserDataAddedColumnCount(t, store); count != 0 {
		t.Fatalf("migration 034 Down 后字段数 = %d, want 0", count)
	}
	var upload float64
	if err := store.db.Pool.QueryRow(ctx, `
		SELECT upload FROM site_userdata WHERE domain='migration.example'`).Scan(&upload); err != nil {
		t.Fatalf("回退 migration 034 后读取旧字段: %v", err)
	}
	if upload != 123 {
		t.Fatalf("回退 migration 034 后 upload = %v, want 123", upload)
	}
	if err := goose.UpToContext(ctx, migrationDB, "migrations", 34); err != nil {
		t.Fatalf("重新应用 migration 034: %v", err)
	}
	if count := siteUserDataAddedColumnCount(t, store); count != 14 {
		t.Fatalf("migration 034 第二次 Up 字段数 = %d, want 14", count)
	}
}

func TestSaveSiteUserDataPostgresMatchesV2DailyBehavior(t *testing.T) {
	store := newSiteStatisticsIntegrationStore(t)
	ctx := t.Context()
	domain := "tracker.example"
	firstTime := time.Date(2026, 8, 15, 10, 11, 12, 0, time.Local)

	first := completeSiteUserDataFixture()
	first.Domain = "payload-domain.example"
	first.Name = "Payload name"
	if err := store.saveSiteUserDataAt(ctx, domain, "First name", first, firstTime); err != nil {
		t.Fatalf("写入首条成功快照: %v", err)
	}
	rows, err := store.SiteUserDataByDomain(ctx, domain, "2026-08-15")
	if err != nil {
		t.Fatalf("读取首条快照: %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("首日行数 = %d, want 1", len(rows))
	}
	firstID := rows[0].ID
	if rows[0].Domain != domain || rows[0].Name != "First name" || rows[0].UpdatedTime != "10:11:12" {
		t.Fatalf("首条覆盖字段 = %#v", rows[0])
	}

	failureMessage := "request failed"
	failureUpload := 999.0
	failure := SiteUserData{Upload: &failureUpload, ErrMsg: &failureMessage}
	if err := store.saveSiteUserDataAt(ctx, domain, "Ignored failure", failure,
		time.Date(2026, 8, 15, 11, 0, 0, 0, time.Local)); err != nil {
		t.Fatalf("写入同日失败快照: %v", err)
	}
	rows, err = store.SiteUserDataByDomain(ctx, domain, "2026-08-15")
	if err != nil {
		t.Fatalf("读取失败后快照: %v", err)
	}
	if len(rows) != 1 || rows[0].ID != firstID || rows[0].Name != "First name" ||
		rows[0].Upload == nil || *rows[0].Upload == failureUpload || rows[0].UpdatedTime != "10:11:12" {
		t.Fatalf("同日失败修改了既有行: %#v", rows)
	}

	replacement := completeSiteUserDataFixture()
	replacementUpload := 456.5
	replacement.Upload = &replacementUpload
	if err := store.saveSiteUserDataAt(ctx, domain, "Replacement", replacement,
		time.Date(2026, 8, 15, 12, 30, 0, 0, time.Local)); err != nil {
		t.Fatalf("写入同日成功覆盖: %v", err)
	}
	rows, err = store.SiteUserDataByDomain(ctx, domain, "2026-08-15")
	if err != nil {
		t.Fatalf("读取成功覆盖后快照: %v", err)
	}
	if len(rows) != 1 || rows[0].ID != firstID || rows[0].Name != "Replacement" ||
		rows[0].Upload == nil || *rows[0].Upload != replacementUpload || rows[0].UpdatedTime != "12:30:00" ||
		rows[0].ErrMsg == nil || *rows[0].ErrMsg != "" {
		t.Fatalf("同日成功没有原位覆盖: %#v", rows)
	}

	if err := store.saveSiteUserDataAt(ctx, domain, "Next-day failure", failure,
		time.Date(2026, 8, 16, 1, 2, 3, 0, time.Local)); err != nil {
		t.Fatalf("写入跨日失败: %v", err)
	}
	allRows, err := store.SiteUserDataByDomain(ctx, domain, "")
	if err != nil {
		t.Fatalf("读取跨日历史: %v", err)
	}
	if len(allRows) != 2 {
		t.Fatalf("跨日行数 = %d, want 2", len(allRows))
	}
	latest, err := store.LatestSiteUserData(ctx)
	if err != nil {
		t.Fatalf("读取失败新日期后的 latest: %v", err)
	}
	if len(latest) != 1 || latest[0].UpdatedDay != "2026-08-15" {
		t.Fatalf("失败新日期遮住旧成功日期: %#v", latest)
	}
}

func TestLatestAndHistorySiteUserDataPostgresPreserveV2Shapes(t *testing.T) {
	store := newSiteStatisticsIntegrationStore(t)
	ctx := t.Context()
	if _, err := store.db.Pool.Exec(ctx, `
		INSERT INTO site_userdata (
			domain, name, username, userid, user_level, join_at,
			bonus, upload, download, ratio, seeding, leeching, seeding_size, leeching_size,
			seeding_info, message_unread, message_unread_contents, err_msg, updated_day, updated_time
		) VALUES
			('first.example', 'First', 'alice', '42', 'VIP', '2020-01-02',
			 1.5, 100, 50, 2, 4, 1, 400, 10, '[{"seeders":4,"size":400}]'::jsonb,
			 2, '[{"title":"notice"}]'::jsonb, '', '2026-08-15', '10:00:00'),
			('first.example', 'First error', NULL, NULL, NULL, NULL,
			 NULL, NULL, NULL, NULL, NULL, NULL, NULL, NULL, NULL,
			 NULL, NULL, 'same-day failure', '2026-08-15', '08:00:00'),
			('first.example', 'First newer failure', NULL, NULL, NULL, NULL,
			 NULL, 999, NULL, NULL, NULL, NULL, NULL, NULL, NULL,
			 NULL, NULL, 'newer failure', '2026-08-16', '23:00:00'),
			('second.example', 'Second', NULL, NULL, NULL, NULL,
			 NULL, 200, NULL, NULL, NULL, NULL, NULL, NULL, '[]'::jsonb,
			 0, '[]'::jsonb, NULL, '2026-08-15', '11:00:00'),
			('failure-only.example', 'Failure only', NULL, NULL, NULL, NULL,
			 NULL, 300, NULL, NULL, NULL, NULL, NULL, NULL, NULL,
			 NULL, NULL, 'failed', '2026-08-15', '12:00:00')`); err != nil {
		t.Fatalf("准备 V2 站点用户数据: %v", err)
	}

	latest, err := store.LatestSiteUserData(ctx)
	if err != nil {
		t.Fatalf("读取 latest: %v", err)
	}
	if len(latest) != 3 {
		t.Fatalf("latest 行数 = %d, want 3: %#v", len(latest), latest)
	}
	if latest[0].Domain != "second.example" || latest[1].Name != "First" || latest[2].Name != "First error" {
		t.Fatalf("latest 全局时间倒序 = %#v", latest)
	}
	if latest[1].Username == nil || *latest[1].Username != "alice" ||
		latest[1].UserID == nil || *latest[1].UserID != "42" ||
		latest[1].MessageUnread == nil || *latest[1].MessageUnread != 2 {
		t.Fatalf("完整字段未保留: %#v", latest[1])
	}
	if !json.Valid(latest[1].SeedingInfo) || !json.Valid(latest[1].MessageUnreadContents) {
		t.Fatalf("JSON 字段无效: seeding=%s messages=%s", latest[1].SeedingInfo, latest[1].MessageUnreadContents)
	}
	if latest[2].Upload != nil || latest[2].SeedingInfo != nil || latest[2].MessageUnreadContents != nil {
		t.Fatalf("SQL NULL 未保留为 nil: %#v", latest[2])
	}

	rows, err := store.SiteUserDataByDomain(ctx, "first.example", " ")
	if err != nil {
		t.Fatalf("精确空白 workdate 查询: %v", err)
	}
	if len(rows) != 0 {
		t.Fatalf("空白 workdate 被错误 trim: %#v", rows)
	}
	rows, err = store.SiteUserDataByDomain(ctx, "first.example", "invalid-date")
	if err != nil {
		t.Fatalf("非法 workdate 精确查询: %v", err)
	}
	if len(rows) != 0 {
		t.Fatalf("非法 workdate 不应被解析或放宽: %#v", rows)
	}
}

func completeSiteUserDataFixture() SiteUserData {
	username := "alice"
	userID := "42"
	level := "VIP"
	joinAt := "2020-01-02"
	bonus := 1.5
	upload := 100.0
	download := 50.0
	ratio := 2.0
	seeding := 4.0
	leeching := 1.0
	seedingSize := 400.0
	leechingSize := 10.0
	unread := 2
	return SiteUserData{
		Username: &username, UserID: &userID, UserLevel: &level, JoinAt: &joinAt,
		Bonus: &bonus, Upload: &upload, Download: &download, Ratio: &ratio,
		Seeding: &seeding, Leeching: &leeching, SeedingSize: &seedingSize, LeechingSize: &leechingSize,
		SeedingInfo: json.RawMessage(`[{"seeders":4,"size":400}]`), MessageUnread: &unread,
		MessageUnreadContents: json.RawMessage(`[{"title":"notice"}]`),
	}
}

func siteUserDataAddedColumnCount(t *testing.T, store *Store) int {
	t.Helper()
	var count int
	if err := store.db.Pool.QueryRow(t.Context(), `
		SELECT COUNT(*)
		FROM information_schema.columns
		WHERE table_schema = current_schema()
		  AND table_name = 'site_userdata'
		  AND column_name = ANY($1::text[])`, []string{
		"username", "userid", "user_level", "join_at", "bonus", "download", "ratio",
		"seeding", "leeching", "seeding_size", "leeching_size", "seeding_info",
		"message_unread", "message_unread_contents",
	}).Scan(&count); err != nil {
		t.Fatalf("读取 migration 034 字段数: %v", err)
	}
	return count
}
