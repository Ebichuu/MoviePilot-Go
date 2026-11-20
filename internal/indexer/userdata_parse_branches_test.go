package indexer

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"testing"
)

// TestParseHDDolbyUserBranches 覆盖 HDDolby 用户信息解析的长度分支。
func TestParseHDDolbyUserBranches(t *testing.T) {
	if err := parseHDDolbyUser(&SiteUserData{}, []byte("not json")); err == nil {
		t.Fatal("非法 JSON 应报错")
	}

	unchanged := SiteUserData{}
	if err := parseHDDolbyUser(&unchanged, []byte(`{"status":1,"data":[{"username":"x"}]}`)); err != nil {
		t.Fatalf("状态非零: %v", err)
	}
	if unchanged.Username != nil {
		t.Fatalf("状态非零不应写入: %+v", unchanged)
	}
	for _, body := range []string{`{"status":0}`, `{"status":0,"data":[]}`, `{"status":0,"data":["x"]}`} {
		target := SiteUserData{}
		if err := parseHDDolbyUser(&target, []byte(body)); err != nil {
			t.Fatalf("%s 解析失败: %v", body, err)
		}
		if target.Username != nil || target.UserID != nil {
			t.Fatalf("%s 不应写入: %+v", body, target)
		}
	}

	// 已知等级映射与全部数值字段。
	known := SiteUserData{}
	body := `{"status":0,"data":[{"id":42,"username":"alice","class":"2","added":"2020-01-01",
		"uploaded":1024,"downloaded":512,"seedbonus":"12.5","unread_messages":3}]}`
	if err := parseHDDolbyUser(&known, []byte(body)); err != nil {
		t.Fatalf("合法用户: %v", err)
	}
	if fmt.Sprint(known.UserID) != "42" || known.Username == nil || *known.Username != "alice" || known.UserLevel == nil ||
		*known.UserLevel != hddolbyUserLevels["2"] {
		t.Fatalf("用户字段 = %+v", known)
	}
	if known.Upload != 1024 || known.Download != 512 || known.Ratio != 2 ||
		known.Bonus != 12.5 || known.MessageUnread != 3 {
		t.Fatalf("数值字段 = %+v", known)
	}

	// 未知等级回落默认档，下载为 0 时不算比率。
	fallback := SiteUserData{Ratio: 9}
	if err := parseHDDolbyUser(&fallback, []byte(`{"status":0,"data":[{"username":"bob","class":"99","downloaded":0}]}`)); err != nil {
		t.Fatalf("回落档位: %v", err)
	}
	if fallback.UserLevel == nil || *fallback.UserLevel != hddolbyUserLevels["1"] {
		t.Fatalf("默认档位 = %+v", fallback.UserLevel)
	}
	if fallback.Ratio != 9 {
		t.Fatalf("下载为 0 不应改动比率: %v", fallback.Ratio)
	}
}

// TestParseHDDolbyPeersBranches 覆盖做种列表解析的跳过与默认值。
func TestParseHDDolbyPeersBranches(t *testing.T) {
	if err := parseHDDolbyPeers(&SiteUserData{}, []byte("not json")); err == nil {
		t.Fatal("非法 JSON 应报错")
	}
	empty := SiteUserData{}
	if err := parseHDDolbyPeers(&empty, []byte(`{"status":1,"data":[{"size":1}]}`)); err != nil {
		t.Fatalf("状态非零: %v", err)
	}
	if empty.Seeding != 0 {
		t.Fatalf("状态非零不应累加: %+v", empty)
	}
	if err := parseHDDolbyPeers(&empty, []byte(`{"status":0,"data":{}}`)); err != nil {
		t.Fatalf("非数组数据: %v", err)
	}
	if empty.Seeding != 0 {
		t.Fatalf("非数组不应累加: %+v", empty)
	}

	result := SiteUserData{}
	body := `{"status":0,"data":[{"size":100,"seeders":0}, "skip", {"size":200,"seeders":5}]}`
	if err := parseHDDolbyPeers(&result, []byte(body)); err != nil {
		t.Fatalf("做种列表: %v", err)
	}
	if result.Seeding != 2 || result.SeedingSize != 300 {
		t.Fatalf("做种统计 = %+v", result)
	}
	if len(result.SeedingInfo) != 2 || result.SeedingInfo[0][0] != 1 || result.SeedingInfo[1][0] != 5 {
		t.Fatalf("做种明细 = %+v", result.SeedingInfo)
	}
}

// TestParseHDDolbyMessageLinksBranches 覆盖消息链接与翻页解析。
func TestParseHDDolbyMessageLinksBranches(t *testing.T) {
	links, next := parseHDDolbyMessageLinks([]byte(`<html><body></body></html>`))
	if len(links) != 0 || next != "" {
		t.Fatalf("空页面 = %+v / %q", links, next)
	}

	body := []byte(`<html><body><table>
		<tr><td><a href="viewmessage.php?id=1">未读</a></td></tr>
		<tr><td><img alt="Read"></td><td><a href="viewmessage.php?id=2">已读</a></td></tr>
		<tr><td><a href="other.php">其它</a></td></tr>
	</table>
	<a href=" /messages.php?page=2 ">下一页</a></body></html>`)
	links, next = parseHDDolbyMessageLinks(body)
	if len(links) != 1 || links[0] != "viewmessage.php?id=1" {
		t.Fatalf("链接 = %+v", links)
	}
	if next != "/messages.php?page=2" {
		t.Fatalf("下一页 = %q", next)
	}

	// 繁体「下一頁」同样识别。
	_, next = parseHDDolbyMessageLinks([]byte(`<html><body><a href="/p2">下一頁</a></body></html>`))
	if next != "/p2" {
		t.Fatalf("繁体下一页 = %q", next)
	}
}

// TestParseHDDolbyMessageBranches 覆盖消息正文的两种页面结构。
func TestParseHDDolbyMessageBranches(t *testing.T) {
	title, date, content := parseHDDolbyMessage([]byte(`<html><body></body></html>`))
	if title != nil || date != nil || content != nil {
		t.Fatalf("空页面 = %v/%v/%v", title, date, content)
	}

	classic := []byte(`<html><body>
		<h1>站内信标题</h1>
		<table><tr><td class="colhead">头</td></tr>
		<tr><td>时间</td><td>2026-01-02 03:04</td></tr>
		<tr><td>正文内容</td></tr></table>
	</body></html>`)
	title, date, content = parseHDDolbyMessage(classic)
	if title != "站内信标题" || date != "2026-01-02 03:04" || content != "正文内容" {
		t.Fatalf("经典结构 = %v/%v/%v", title, date, content)
	}

	layui := []byte(`<html><body>
		<div class="layui-card-header"><span>Layui 标题</span><span><span>发送者</span><span>2026-05-06</span></span></div>
		<div class="layui-card-body">Layui 正文</div>
	</body></html>`)
	title, date, content = parseHDDolbyMessage(layui)
	if title != "Layui 标题" || date != "2026-05-06" || content != "Layui 正文" {
		t.Fatalf("layui 结构 = %v/%v/%v", title, date, content)
	}
}

// TestHDDolbyUserDataPageBranches 覆盖用户页请求的状态码白名单。
func TestHDDolbyUserDataPageBranches(t *testing.T) {
	client := newIndexerHTTPClient(t, 1)
	for _, status := range []int{http.StatusOK, http.StatusForbidden, http.StatusInternalServerError} {
		endpoint := probeServer(t, func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(status)
			_, _ = w.Write([]byte("body"))
		})
		body, err := hddolbyUserDataPage(context.Background(), client, endpoint, http.Header{})
		if err != nil || string(body) != "body" {
			t.Fatalf("状态 %d = %q（%v）", status, body, err)
		}
	}

	endpoint := probeServer(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	})
	body, err := hddolbyUserDataPage(context.Background(), client, endpoint, http.Header{})
	if err != nil || body != nil {
		t.Fatalf("404 = %q（%v）", body, err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := hddolbyUserDataPage(ctx, client, endpoint, http.Header{}); !errors.Is(err, context.Canceled) {
		t.Fatalf("取消上下文 = %v", err)
	}
}

// TestHDDolbyMessagePageBranches 覆盖消息页请求的错误吞掉与令牌头剥离。
func TestHDDolbyMessagePageBranches(t *testing.T) {
	endpoint := probeServer(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("x-api-key") != "" {
			t.Fatalf("消息页不应携带 x-api-key")
		}
		_, _ = w.Write([]byte("page"))
	})
	adapter := &HDDolbyAdapter{
		client:  newIndexerHTTPClient(t, 1),
		headers: http.Header{"X-Api-Key": []string{"secret"}},
	}
	body, err := hddolbyMessagePage(context.Background(), adapter, endpoint)
	if err != nil || string(body) != "page" {
		t.Fatalf("消息页 = %q（%v）", body, err)
	}

	// 404 被吞成空响应，交由调用方跳过。
	missing := probeServer(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	})
	body, err = hddolbyMessagePage(context.Background(), adapter, missing)
	if err != nil || body != nil {
		t.Fatalf("404 = %q（%v）", body, err)
	}

	// 取消上下文必须原样返回，不能被吞掉。
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := hddolbyMessagePage(ctx, adapter, endpoint); !errors.Is(err, context.Canceled) {
		t.Fatalf("取消上下文 = %v", err)
	}
}

// TestRoundUserDataBranches 覆盖用户数据取整。
func TestRoundUserDataBranches(t *testing.T) {
	cases := map[float64]float64{1.234: 1.23, 1.235: 1.24, 2: 2}
	for input, want := range cases {
		if got := roundUserData(input, 2); got != want {
			t.Fatalf("roundUserData(%v) = %v，期望 %v", input, got, want)
		}
	}
	if strings.TrimSpace(hddolbyUserLevels["1"]) == "" {
		t.Fatal("默认档位缺失")
	}
}
