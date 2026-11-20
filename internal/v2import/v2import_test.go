package v2import

import (
	"database/sql"
	"path/filepath"
	"testing"

	"github.com/moviepilot-go/moviepilot-go/internal/database"
)

// newV2Fixture 造一个"够用"的 V2 库：只建迁移会读的表，字段挑真实存在的。
func newV2Fixture(t *testing.T) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "moviepilot.db")
	handle, err := sql.Open("sqlite", "file:"+filepath.ToSlash(path))
	if err != nil {
		t.Fatalf("创建测试库: %v", err)
	}
	defer handle.Close()

	statements := []string{
		`CREATE TABLE user (
			id INTEGER PRIMARY KEY, name TEXT, email TEXT, hashed_password TEXT,
			is_active BOOLEAN, is_superuser BOOLEAN, avatar TEXT,
			is_otp BOOLEAN, otp_secret TEXT, permissions JSON, settings JSON)`,
		`CREATE TABLE systemconfig (id INTEGER PRIMARY KEY, key TEXT, value JSON)`,
		`CREATE TABLE subscribe (
			id INTEGER PRIMARY KEY, name TEXT, year TEXT, type TEXT, keyword TEXT,
			tmdbid INTEGER, doubanid TEXT, bangumiid INTEGER, anilistid INTEGER,
			media_source TEXT, media_id TEXT, season INTEGER, state TEXT,
			total_episode INTEGER, lack_episode INTEGER, best_version INTEGER,
			current_priority INTEGER, filter TEXT, sites JSON, save_path TEXT, username TEXT)`,
		`CREATE TABLE siteuserdata (
			id INTEGER PRIMARY KEY, domain TEXT, name TEXT, username TEXT, userid TEXT,
			user_level TEXT, join_at TEXT, bonus FLOAT, upload FLOAT, download FLOAT,
			ratio FLOAT, seeding FLOAT, leeching FLOAT, seeding_size FLOAT,
			leeching_size FLOAT, seeding_info JSON, message_unread INTEGER,
			message_unread_contents JSON, err_msg TEXT, updated_day TEXT, updated_time TEXT)`,
		`CREATE TABLE sitestatistic (
			id INTEGER PRIMARY KEY, domain TEXT, success INTEGER, fail INTEGER,
			seconds INTEGER, lst_state INTEGER, lst_mod_date TEXT, note JSON)`,
		`CREATE TABLE site (id INTEGER PRIMARY KEY, name TEXT, domain TEXT, url TEXT, pri INTEGER)`,
		`INSERT INTO user (name, hashed_password, is_superuser, avatar, permissions)
			VALUES ('admin', '$2b$12$abcdefghijklmnopqrstuv', 1, NULL, '{"pris":{}}')`,
		`INSERT INTO user (name, hashed_password, is_superuser) VALUES ('', '', 0)`,
		`INSERT INTO systemconfig (key, value) VALUES ('proxy', '{"enable":true}')`,
		`INSERT INTO systemconfig (key, value) VALUES ('', '{}')`,
		`INSERT INTO subscribe (name, year, type, tmdbid, season, state, total_episode, filter, username)
			VALUES ('沙丘', '2021', '电影', 438631, 0, 'R', 1, '{"rules":[]}', 'admin')`,
		`INSERT INTO subscribe (name, year, type, doubanid, season, state, username)
			VALUES ('漫长的季节', '2023', '电视剧', '35465232', 1, 'P', 'admin')`,
		`INSERT INTO subscribe (name, type, username) VALUES ('', '电影', 'admin')`,
		`INSERT INTO siteuserdata (domain, name, upload, download, ratio, seeding_info, message_unread_contents)
			VALUES ('haidan.cc', '海胆', 1.5, 0.5, 3.0, '{"x":1}', '[]')`,
		`INSERT INTO sitestatistic (domain, success, fail, seconds, lst_state, note)
			VALUES ('haidan.cc', 10, 2, 30, 1, '{"n":1}')`,
		`INSERT INTO site (name, domain, url, pri) VALUES ('海胆', 'haidan.cc', 'https://haidan.cc', 1)`,
		`INSERT INTO site (name, domain, url, pri) VALUES ('憨憨', 'hhanclub.net', 'https://hhanclub.net', 2)`,
	}
	for _, statement := range statements {
		if _, err := handle.Exec(statement); err != nil {
			t.Fatalf("初始化测试库失败（%s）: %v", statement, err)
		}
	}
	return path
}

// TestRunDryRunMapsV2Data 验证试运行只读源库、按预期统计映射结果。
func TestRunDryRunMapsV2Data(t *testing.T) {
	path := newV2Fixture(t)
	report, err := Run(t.Context(), Options{SQLitePath: path, DryRun: true})
	if err != nil {
		t.Fatalf("试运行失败: %v", err)
	}
	if !report.DryRun {
		t.Fatal("报告应标记为试运行")
	}
	if report.BackupPath != "" {
		t.Fatal("试运行不应生成备份")
	}

	byTarget := map[string]TableReport{}
	for _, table := range report.Tables {
		byTarget[table.Target] = table
	}
	cases := []struct {
		target  string
		read    int
		written int
		skipped int
	}{
		{"users", 2, 1, 1},         // 第二行缺用户名/密码被跳过
		{"settings", 2, 1, 1},      // 空 key 被跳过
		{"subscriptions", 3, 2, 1}, // 空标题被跳过
		{"site_userdata", 1, 1, 0},
		{"site_statistics", 1, 1, 0},
	}
	for _, want := range cases {
		got, ok := byTarget[want.target]
		if !ok {
			t.Fatalf("报告里缺少 %s", want.target)
		}
		if got.Read != want.read || got.Written != want.written || got.Skipped != want.skipped {
			t.Fatalf("%s = 读取 %d/写入 %d/跳过 %d，期望 %d/%d/%d",
				want.target, got.Read, got.Written, got.Skipped, want.read, want.written, want.skipped)
		}
	}

	if len(report.SitePreview) != 2 {
		t.Fatalf("站点提示应列出 2 个，实际 %v", report.SitePreview)
	}
	if report.SitePreview[0] != "憨憨 (hhanclub.net)" {
		t.Fatalf("站点提示排序/格式 = %v", report.SitePreview)
	}
}

// TestRunRejectsMissingFile 验证路径不存在的明确报错。
func TestRunRejectsMissingFile(t *testing.T) {
	if _, err := Run(t.Context(), Options{SQLitePath: filepath.Join(t.TempDir(), "nope.db"), DryRun: true}); err == nil {
		t.Fatal("文件不存在应报错")
	}
	if _, err := Run(t.Context(), Options{DryRun: true}); err == nil {
		t.Fatal("空路径应报错")
	}
}

// TestRunRequiresDatabaseWhenWriting 验证非试运行必须给数据库连接。
func TestRunRequiresDatabaseWhenWriting(t *testing.T) {
	path := newV2Fixture(t)
	var target *database.DB
	if _, err := Run(t.Context(), Options{SQLitePath: path, DB: target}); err == nil {
		t.Fatal("缺少数据库连接时应报错")
	}
}

// TestMediaTypeMapping 覆盖 V2 中文类型到本项目 media_type 的映射。
func TestMediaTypeMapping(t *testing.T) {
	cases := map[string]string{"电影": "movie", "电视剧": "tv", "系列": "", "": "", "unknown": ""}
	for input, want := range cases {
		if got := mediaTypeFromV2(input); got != want {
			t.Fatalf("mediaTypeFromV2(%q) = %q, want %q", input, got, want)
		}
	}
}

// TestMediaIdentityPrefersUnifiedFields 验证新字段优先、老列兜底。
func TestMediaIdentityPrefersUnifiedFields(t *testing.T) {
	cases := []struct {
		row    map[string]any
		source string
		id     string
	}{
		{map[string]any{"media_source": "themoviedb", "media_id": "603", "tmdbid": int64(1)}, "tmdb", "603"},
		{map[string]any{"tmdbid": int64(438631)}, "tmdb", "438631"},
		{map[string]any{"doubanid": "35465232"}, "douban", "35465232"},
		{map[string]any{"bangumiid": int64(253)}, "bangumi", "253"},
		{map[string]any{"anilistid": int64(21)}, "anilist", "21"},
		{map[string]any{}, "", ""},
	}
	for index, testCase := range cases {
		source, id := mediaIdentity(testCase.row)
		if source != testCase.source || id != testCase.id {
			t.Fatalf("第 %d 例 = (%q, %q)，期望 (%q, %q)",
				index+1, source, id, testCase.source, testCase.id)
		}
	}
}

// TestSubscriptionImportedPausedByDefault 验证默认暂停、--resume 才恢复搜索。
func TestSubscriptionImportedPausedByDefault(t *testing.T) {
	row := map[string]any{
		"name": "沙丘", "type": "电影", "tmdbid": int64(438631), "state": "R",
		"total_episode": int64(1), "filter": `{"rules":[]}`,
	}
	paused, err := buildSubscriptionRow(row, false)
	if err != nil {
		t.Fatalf("映射订阅失败: %v", err)
	}
	if paused["internal_state"] != "paused" {
		t.Fatalf("默认应导入为 paused，实际 %v", paused["internal_state"])
	}
	if paused["v2_state"] != "R" {
		t.Fatalf("应保留 V2 原始状态，实际 %v", paused["v2_state"])
	}
	config, ok := paused["config"].(map[string]any)
	if !ok {
		t.Fatalf("config 类型 = %T", paused["config"])
	}
	if config["total_episode"] != int64(1) {
		t.Fatalf("config.total_episode = %v", config["total_episode"])
	}
	legacy, ok := config["v2"].(map[string]any)
	if !ok || legacy["filter"] == nil {
		t.Fatalf("V2 原始筛选规则应保留在 config.v2，实际 %v", config["v2"])
	}

	resumed, err := buildSubscriptionRow(row, true)
	if err != nil {
		t.Fatalf("映射订阅失败: %v", err)
	}
	if resumed["internal_state"] != "pending_search" {
		t.Fatalf("--resume 应为 pending_search，实际 %v", resumed["internal_state"])
	}
}
