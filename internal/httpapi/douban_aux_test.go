package httpapi

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/go-chi/chi/v5"
)

func TestDoubanSlug(t *testing.T) {
	cases := map[string]string{
		"电影":      "movie",
		" 电影 ":    "movie",
		"电视剧":     "tv",
		"":        "tv",
		"unknown": "tv",
	}
	for input, want := range cases {
		if got := doubanSlug(input); got != want {
			t.Fatalf("doubanSlug(%q) = %q，期望 %q", input, got, want)
		}
	}
}

func TestDoubanCreditPersonMapsFields(t *testing.T) {
	entry := doubanCreditEntry{
		ID:        "1234",
		Name:      "演员甲",
		Title:     "备用名字",
		LatinName: "Actor A",
		Character: "角色乙",
		Avatar:    doubanAvatar{Large: "https://img/large.jpg", Normal: "https://img/normal.jpg"},
	}
	person := entry.person()
	if person.ID != "1234" || person.Name != "演员甲" {
		t.Fatalf("基础字段映射错误: %+v", person)
	}
	if person.Avatar != "https://img/large.jpg" {
		t.Fatalf("应优先使用大图: %q", person.Avatar)
	}
	if person.Character != "角色乙" {
		t.Fatalf("角色名应映射到 character: %q", person.Character)
	}
	if person.Type != 1 || person.Source != "douban" {
		t.Fatalf("type/source 映射错误: %+v", person)
	}
	if len(person.AlsoKnownAs) != 1 || person.AlsoKnownAs[0] != "Actor A" {
		t.Fatalf("also_known_as 映射错误: %+v", person.AlsoKnownAs)
	}
	if person.Source != "douban" {
		t.Fatalf("source = %q，期望 douban", person.Source)
	}
}

func TestDoubanCreditPersonFallsBackToTitle(t *testing.T) {
	entry := doubanCreditEntry{ID: "9", Title: "只有标题"}
	person := entry.person()
	if person.Name != "只有标题" {
		t.Fatalf("Name 为空时应回落到 Title，实际 %q", person.Name)
	}
	if person.AlsoKnownAs == nil {
		t.Fatal("also_known_as 应为空切片而不是 nil，避免序列化成 null")
	}
	if len(person.AlsoKnownAs) != 0 {
		t.Fatalf("没有拉丁名时 also_known_as 应为空，实际 %+v", person.AlsoKnownAs)
	}
}

func TestDoubanDecodeAcceptsJSONShapes(t *testing.T) {
	type target struct {
		ID    string `json:"id"`
		Title string `json:"title"`
	}

	var fromMap target
	if err := doubanDecode(map[string]any{"id": "1", "title": "沙丘"}, &fromMap); err != nil {
		t.Fatalf("从 map 解码失败: %v", err)
	}
	if fromMap.ID != "1" || fromMap.Title != "沙丘" {
		t.Fatalf("map 解码结果不对: %+v", fromMap)
	}

	var fromRaw target
	raw := json.RawMessage(`{"id":"2","title":"球2"}`)
	if err := doubanDecode(raw, &fromRaw); err != nil {
		t.Fatalf("从 RawMessage 解码失败: %v", err)
	}
	if fromRaw.ID != "2" {
		t.Fatalf("RawMessage 解码结果不对: %+v", fromRaw)
	}

	// 无法编码的输入必须报错，而不是静默给出零值。
	var into target
	if err := doubanDecode(func() {}, &into); err == nil {
		t.Fatal("不可序列化的输入应当报错")
	}
}

// anilistIDParam 读的是 chi 的**路径参数**（V2 的 /anilist/{anilist_id}），并对取值做区间钳制。
func TestAnilistIDParam(t *testing.T) {
	withParam := func(name, value string) *http.Request {
		request := httptest.NewRequest("GET", "/api/v1/anilist/subject", nil)
		routeContext := chi.NewRouteContext()
		routeContext.URLParams.Add(name, value)
		return request.WithContext(context.WithValue(request.Context(), chi.RouteCtxKey, routeContext))
	}

	if got := anilistIDParam(withParam("id", "42"), "id"); got != 42 {
		t.Fatalf("anilistIDParam(id) = %d，期望 42", got)
	}
	for _, value := range []string{"abc", "", "-3"} {
		if got := anilistIDParam(withParam("id", value), "id"); got != 0 {
			t.Fatalf("非法 ID %q 应返回 0，实际 %d", value, got)
		}
	}
	// 超上限是「钳制」而不是拒绝：保证不会把巨大 ID 透传给上游。
	if got := anilistIDParam(withParam("id", "2000000001"), "id"); got != 2000000000 {
		t.Fatalf("超上限应钳制到 2000000000，实际 %d", got)
	}
	if got := anilistIDParam(httptest.NewRequest("GET", "/api/v1/anilist/subject", nil), "id"); got != 0 {
		t.Fatalf("缺失参数应返回 0，实际 %d", got)
	}
}
