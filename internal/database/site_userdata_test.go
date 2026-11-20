package database

import (
	"strings"
	"testing"
)

func TestSiteUserDataFieldsMigrationContract(t *testing.T) {
	content, err := migrationFS.ReadFile("migrations/034_site_userdata_fields.sql")
	if err != nil {
		t.Fatalf("读取站点用户数据迁移: %v", err)
	}
	migration := strings.Join(strings.Fields(string(content)), " ")
	for _, fragment := range []string{
		"ALTER TABLE site_userdata",
		"ADD COLUMN IF NOT EXISTS username TEXT",
		"ADD COLUMN IF NOT EXISTS userid TEXT",
		"ADD COLUMN IF NOT EXISTS user_level TEXT",
		"ADD COLUMN IF NOT EXISTS join_at TEXT",
		"ADD COLUMN IF NOT EXISTS bonus DOUBLE PRECISION",
		"ADD COLUMN IF NOT EXISTS download DOUBLE PRECISION",
		"ADD COLUMN IF NOT EXISTS ratio DOUBLE PRECISION",
		"ADD COLUMN IF NOT EXISTS seeding DOUBLE PRECISION",
		"ADD COLUMN IF NOT EXISTS leeching DOUBLE PRECISION",
		"ADD COLUMN IF NOT EXISTS seeding_size DOUBLE PRECISION",
		"ADD COLUMN IF NOT EXISTS leeching_size DOUBLE PRECISION",
		"ADD COLUMN IF NOT EXISTS seeding_info JSONB",
		"ADD COLUMN IF NOT EXISTS message_unread INTEGER",
		"ADD COLUMN IF NOT EXISTS message_unread_contents JSONB",
		"DROP COLUMN IF EXISTS message_unread_contents",
		"DROP COLUMN IF EXISTS username",
	} {
		if !strings.Contains(migration, fragment) {
			t.Fatalf("站点用户数据迁移缺少 %q: %s", fragment, migration)
		}
	}
	if strings.Contains(strings.ToUpper(migration), "UNIQUE") {
		t.Fatalf("站点用户数据迁移不应新增 V2 没有的唯一约束: %s", migration)
	}
}
