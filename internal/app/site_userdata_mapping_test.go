package app

import (
	"encoding/json"
	"testing"

	"github.com/moviepilot-go/moviepilot-go/internal/indexer"
)

func TestDatabaseSiteUserDataMapsFullSnapshot(t *testing.T) {
	t.Parallel()

	username := "alice"
	userLevel := "Power User"
	joinAt := "2024-01-02"
	errMsg := "站点返回部分字段"
	updatedTime := "2026-11-19 08:00:00"
	data := indexer.SiteUserData{
		Domain:        "zmpt.cc",
		Username:      &username,
		UserID:        int64(12345),
		UserLevel:     &userLevel,
		JoinAt:        &joinAt,
		Bonus:         100.5,
		Upload:        1 << 40,
		Download:      1 << 30,
		Ratio:         1024,
		Seeding:       12,
		Leeching:      3,
		SeedingSize:   1 << 32,
		LeechingSize:  1 << 20,
		SeedingInfo:   [][]int64{{1, 2}, {3, 4}},
		MessageUnread: 7,
		MessageUnreadContents: [][]any{
			{"标题", "内容"},
		},
		ErrMsg:      &errMsg,
		UpdatedDay:  "2026-11-19",
		UpdatedTime: &updatedTime,
	}

	row := databaseSiteUserData(data)

	if row.Domain != "zmpt.cc" || row.UpdatedDay != "2026-11-19" || row.UpdatedTime != updatedTime {
		t.Fatalf("基础字段 = %+v", row)
	}
	if row.Username == nil || *row.Username != "alice" ||
		row.UserLevel == nil || *row.UserLevel != "Power User" ||
		row.JoinAt == nil || *row.JoinAt != "2024-01-02" {
		t.Fatalf("文本字段 = %+v", row)
	}
	// V2 的 userid 可能是数字也可能是字符串，统一按字符串落库。
	if row.UserID == nil || *row.UserID != "12345" {
		t.Fatalf("UserID = %v", row.UserID)
	}
	if row.Bonus == nil || *row.Bonus != 100.5 ||
		row.Upload == nil || *row.Upload != float64(1<<40) ||
		row.Download == nil || *row.Download != float64(1<<30) ||
		row.Ratio == nil || *row.Ratio != 1024 {
		t.Fatalf("流量字段 = %+v", row)
	}
	if row.Seeding == nil || *row.Seeding != 12 || row.Leeching == nil || *row.Leeching != 3 ||
		row.SeedingSize == nil || *row.SeedingSize != float64(1<<32) ||
		row.LeechingSize == nil || *row.LeechingSize != float64(1<<20) {
		t.Fatalf("做种字段 = %+v", row)
	}
	if row.MessageUnread == nil || *row.MessageUnread != 7 {
		t.Fatalf("未读消息 = %v", row.MessageUnread)
	}
	if row.ErrMsg == nil || *row.ErrMsg != errMsg {
		t.Fatalf("ErrMsg = %v", row.ErrMsg)
	}
	// 两个复合字段按 JSON 原样保存，读取侧仍能解回同样的结构。
	var seeding [][]int64
	if err := json.Unmarshal(row.SeedingInfo, &seeding); err != nil || len(seeding) != 2 || seeding[1][1] != 4 {
		t.Fatalf("SeedingInfo = %s (%v)", row.SeedingInfo, err)
	}
	var contents [][]any
	if err := json.Unmarshal(row.MessageUnreadContents, &contents); err != nil ||
		len(contents) != 1 || contents[0][0] != "标题" {
		t.Fatalf("MessageUnreadContents = %s (%v)", row.MessageUnreadContents, err)
	}
}

func TestDatabaseSiteUserDataKeepsZeroValues(t *testing.T) {
	t.Parallel()

	// 空快照里数值仍写成 0 而不是 NULL：V2 的日快照用 0 表示「抓到了但为 0」。
	row := databaseSiteUserData(indexer.SiteUserData{})
	if row.UserID != nil || row.UpdatedTime != "" || row.Username != nil {
		t.Fatalf("缺省字段 = %+v", row)
	}
	for name, value := range map[string]*float64{
		"bonus": row.Bonus, "upload": row.Upload, "download": row.Download, "ratio": row.Ratio,
		"seeding": row.Seeding, "leeching": row.Leeching,
		"seeding_size": row.SeedingSize, "leeching_size": row.LeechingSize,
	} {
		if value == nil || *value != 0 {
			t.Fatalf("%s = %v", name, value)
		}
	}
	if row.MessageUnread == nil || *row.MessageUnread != 0 {
		t.Fatalf("MessageUnread = %v", row.MessageUnread)
	}
	// nil 切片序列化为 JSON null，读取侧按「没有详情」处理。
	if string(row.SeedingInfo) != "null" || string(row.MessageUnreadContents) != "null" {
		t.Fatalf("复合字段 = %s / %s", row.SeedingInfo, row.MessageUnreadContents)
	}
}

func TestDatabaseSiteUserDataStringUserID(t *testing.T) {
	t.Parallel()

	row := databaseSiteUserData(indexer.SiteUserData{UserID: "uid-abc"})
	if row.UserID == nil || *row.UserID != "uid-abc" {
		t.Fatalf("字符串 UserID = %v", row.UserID)
	}
	// 指针与源数据共享也无可厚非；这里只确认不会把 nil 写成 "<nil>"。
	if got := databaseSiteUserData(indexer.SiteUserData{}).UserID; got != nil {
		t.Fatalf("nil UserID = %v", got)
	}
}
