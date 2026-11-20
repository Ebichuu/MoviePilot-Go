package httpapi

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/moviepilot-go/moviepilot-go/internal/command"
	"github.com/moviepilot-go/moviepilot-go/internal/database"
	"github.com/moviepilot-go/moviepilot-go/internal/subscription"
)

func subscriptionItem(name, mediaType, state string) database.Subscription {
	return database.Subscription{
		Name: name, MediaType: mediaType, InternalState: subscription.State(state),
	}
}

func TestRunSubscribeCommandRequiresStore(t *testing.T) {
	var deps Dependencies
	// 没有订阅存储时必须明确报错，不能返回「共 0 个订阅」这种像是真结果的文本。
	if _, err := deps.runSubscribeCommand(context.Background(), command.Request{}); err == nil {
		t.Fatal("缺少订阅存储应报错")
	} else if !strings.Contains(err.Error(), "订阅存储未初始化") {
		t.Fatalf("错误信息 = %v", err)
	}
}

func TestFormatSubscriptionSummaryEmpty(t *testing.T) {
	text, details := formatSubscriptionSummary(nil)
	if text != "共 0 个订阅" {
		t.Fatalf("文本 = %q", text)
	}
	// 没有订阅时不能出现换行或截断提示。
	if strings.Contains(text, "\n") {
		t.Fatalf("文本不应有明细行: %q", text)
	}
	if details["total"] != 0 {
		t.Fatalf("details = %+v", details)
	}
	states, ok := details["states"].(map[string]int)
	if !ok || len(states) != 0 {
		t.Fatalf("states = %#v", details["states"])
	}
}

func TestFormatSubscriptionSummaryGroupsStates(t *testing.T) {
	items := []database.Subscription{
		subscriptionItem("沙丘", "电影", "DOWNLOADING"),
		subscriptionItem("三体", "电视剧", "DOWNLOADING"),
		subscriptionItem("किताब", "", "RSS"),
	}
	text, details := formatSubscriptionSummary(items)
	// 状态按键名升序排列，计数准确。
	if !strings.HasPrefix(text, "共 3 个订阅，DOWNLOADING 2，RSS 1") {
		t.Fatalf("文本 = %q", text)
	}
	// 明细行形如「· 名字（类型）　状态」；没有类型的条目省略括号。
	lines := strings.Split(text, "\n")
	if len(lines) != 4 {
		t.Fatalf("行数 = %d: %q", len(lines), text)
	}
	if lines[1] != "· 沙丘（电影）　DOWNLOADING" {
		t.Fatalf("明细行 = %q", lines[1])
	}
	if lines[3] != "· किताब　RSS" {
		t.Fatalf("无类型明细行 = %q", lines[3])
	}
	states := details["states"].(map[string]int)
	if states["DOWNLOADING"] != 2 || states["RSS"] != 1 {
		t.Fatalf("states = %v", states)
	}
	if details["total"] != 3 {
		t.Fatalf("total = %v", details["total"])
	}
}

func TestFormatSubscriptionSummaryTruncatesLongLists(t *testing.T) {
	items := make([]database.Subscription, 0, 13)
	for index := 0; index < 13; index++ {
		items = append(items, subscriptionItem(fmt.Sprintf("订阅%02d", index), "电影", "RSS"))
	}
	text, details := formatSubscriptionSummary(items)
	// 只展示前 10 条，其余用一行汇总，避免命令回复被刷屏。
	if got := strings.Count(text, "\n· "); got != 10 {
		t.Fatalf("明细行数 = %d", got)
	}
	if !strings.Contains(text, "…还有 3 个未显示") {
		t.Fatalf("缺少截断提示: %q", text)
	}
	if details["total"] != 13 {
		t.Fatalf("total = %v", details["total"])
	}
	// 计数按全部 13 条统计，而不是按展示的 10 条。
	states := details["states"].(map[string]int)
	if states["RSS"] != 13 {
		t.Fatalf("states = %v", states)
	}
}

func TestFormatSubscriptionSummaryExactlyTenItems(t *testing.T) {
	items := make([]database.Subscription, 0, 10)
	for index := 0; index < 10; index++ {
		items = append(items, subscriptionItem(fmt.Sprintf("订阅%02d", index), "", "RSS"))
	}
	text, _ := formatSubscriptionSummary(items)
	// 正好 10 条时不应出现截断提示。
	if strings.Contains(text, "未显示") {
		t.Fatalf("不应提示截断: %q", text)
	}
	if got := strings.Count(text, "\n· "); got != 10 {
		t.Fatalf("明细行数 = %d", got)
	}
}
