package httpapi

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/moviepilot-go/moviepilot-go/internal/metadata"
)

// doubanFake 按路径返回固定的豆瓣响应，未匹配的路径回 404 以暴露拼错的 URL。
func doubanFake(t *testing.T) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/movie/9999/celebrities"):
			// 字段类型与契约不符，用来验证解析失败的处理。
			_, _ = w.Write([]byte(`{"actors":"不是数组"}`))
		case strings.HasSuffix(r.URL.Path, "/celebrities"):
			_, _ = w.Write([]byte(`{
				"directors":[{"id":"d1","title":"导演甲","latin_name":"Dir A"}],
				"actors":[
					{"id":"a1","name":"演员乙","avatar":{"large":"https://img/a1.jpg"},"character":"角色乙"},
					{"id":"d1","name":"重复的导演","avatar":{"large":"https://img/dup.jpg"}},
					{"id":"","name":"没有ID"}
				]
			}`))
		case strings.HasSuffix(r.URL.Path, "/work_collections/col-1/works"):
			_, _ = w.Write([]byte(`{"works":[
				{"subject":{"id":"11","title":"作品A","card_subtitle":"2021 / 中国大陆","pic":{"large":"https://img/11.jpg"},"rating":{"value":8.1}}},
				{"subject":{"id":""}}
			]}`))
		case strings.HasSuffix(r.URL.Path, "/recommendations"):
			_, _ = w.Write([]byte(`[
				{"id":"21","title":"推荐一","card_subtitle":"2019-05-01","pic":{"large":"https://img/21.jpg"},"rating":{"value":7.5}},
				{"id":""}
			]`))
		case strings.HasSuffix(r.URL.Path, "/celebrity/1054"):
			_, _ = w.Write([]byte(`{
				"id":"1054","title":"人物甲",
				"cover_img":{"url":"https://img/p.jpg"},
				"extra":{"short_info":"简介文字","info":[["性别","男"],["出生地",""]]},
				"modules":[{"type":"work_collections","payload":{"id":"col-1"}}]
			}`))
		case strings.HasSuffix(r.URL.Path, "/celebrity/2000"):
			// 没有作品集模块的人物。
			_, _ = w.Write([]byte(`{"id":"2000","title":"无作品集","modules":[{"type":"other","payload":{"id":"x"}}]}`))
		case strings.HasSuffix(r.URL.Path, "/celebrity/3000"):
			// 模块类型正确但 payload 为空。
			_, _ = w.Write([]byte(`{"id":"3000","title":"空作品集","modules":[{"type":"work_collections","payload":{}}]}`))
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
}

func doubanDeps(t *testing.T, baseURL string) Dependencies {
	t.Helper()
	client, err := metadata.NewHTTPClient(4, 5*time.Second, 1<<20)
	if err != nil {
		t.Fatalf("创建元数据 HTTP 客户端失败: %v", err)
	}
	t.Cleanup(client.CloseIdleConnections)
	deps, _ := metadataDeps(t, metadata.NewDouban(client, baseURL, "db-key", "db-secret"))
	return deps
}

func TestDoubanCreditsMergesAndDedupes(t *testing.T) {
	server := doubanFake(t)
	defer server.Close()
	deps := doubanDeps(t, server.URL)

	recorder := httptest.NewRecorder()
	deps.doubanCredits(recorder, chiRequest(http.MethodGet, "", map[string]string{
		"doubanid": "1292052", "type_name": "电影",
	}, ""))

	people := decodeBody[[]doubanMediaPerson](t, recorder)
	// 空的 ID 被跳过，重复 ID 只保留第一次（导演优先）。
	if len(people) != 2 {
		t.Fatalf("演职员 = %+v", people)
	}
	if people[0].ID != "d1" || people[0].Name != "导演甲" {
		t.Fatalf("导演 = %+v", people[0])
	}
	if len(people[0].AlsoKnownAs) != 1 || people[0].AlsoKnownAs[0] != "Dir A" {
		t.Fatalf("导演别名 = %+v", people[0].AlsoKnownAs)
	}
	if people[1].ID != "a1" || people[1].Name != "演员乙" || people[1].Avatar != "https://img/a1.jpg" {
		t.Fatalf("演员 = %+v", people[1])
	}
	// 演职员接口把角色名放 character，与 V2 schema 一致（biography 是人物简介）。
	if people[1].Character != "角色乙" {
		t.Fatalf("角色 = %q", people[1].Character)
	}
}

func TestDoubanPersonDetail(t *testing.T) {
	server := doubanFake(t)
	defer server.Close()
	deps := doubanDeps(t, server.URL)

	recorder := httptest.NewRecorder()
	deps.doubanPerson(recorder, chiRequest(http.MethodGet, "", map[string]string{"person_id": "1054"}, ""))

	person := decodeBody[doubanMediaPerson](t, recorder)
	if person.ID != "1054" || person.Name != "人物甲" {
		t.Fatalf("人物 = %+v", person)
	}
	if person.Avatar != "https://img/p.jpg" || person.Biography != "简介文字" {
		t.Fatalf("人物字段 = %+v", person)
	}
	if person.Source != "douban" {
		t.Fatalf("来源 = %q", person.Source)
	}
	// 只拼接第二列非空的信息项。
	if len(person.AlsoKnownAs) != 1 || person.AlsoKnownAs[0] != "性别：男" {
		t.Fatalf("别名 = %+v", person.AlsoKnownAs)
	}
}

func TestDoubanPersonCreditsUsesWorkCollection(t *testing.T) {
	server := doubanFake(t)
	defer server.Close()
	deps := doubanDeps(t, server.URL)

	recorder := httptest.NewRecorder()
	deps.doubanPersonCredits(recorder, chiRequest(http.MethodGet, "", map[string]string{"person_id": "1054"}, ""))

	medias := decodeBody[[]metadata.Media](t, recorder)
	if len(medias) != 1 {
		t.Fatalf("作品 = %+v", medias)
	}
	if medias[0].ID != "11" || medias[0].Title != "作品A" || medias[0].Source != "douban" {
		t.Fatalf("作品 = %+v", medias[0])
	}
	if medias[0].Year != 2021 {
		t.Fatalf("年份 = %d", medias[0].Year)
	}
	if medias[0].VoteAverage != 8.1 || medias[0].PosterURL != "https://img/11.jpg" {
		t.Fatalf("评分/海报 = %+v", medias[0])
	}
}

func TestDoubanPersonCreditsWithoutWorkCollection(t *testing.T) {
	server := doubanFake(t)
	defer server.Close()
	deps := doubanDeps(t, server.URL)

	for _, personID := range []string{"2000", "3000"} {
		recorder := httptest.NewRecorder()
		deps.doubanPersonCredits(recorder, chiRequest(http.MethodGet, "", map[string]string{"person_id": personID}, ""))
		// 上游拿不到作品集时按 V2 语义返回空列表，而不是报错。
		if body := strings.TrimSpace(recorder.Body.String()); body != "[]" {
			t.Fatalf("人物 %s 作品 = %q，期望 []", personID, body)
		}
	}
}

func TestDoubanRecommendMediaType(t *testing.T) {
	server := doubanFake(t)
	defer server.Close()
	deps := doubanDeps(t, server.URL)

	for typeName, wantType := range map[string]string{"电影": "movie", "电视剧": "tv"} {
		recorder := httptest.NewRecorder()
		deps.doubanRecommend(recorder, chiRequest(http.MethodGet, "", map[string]string{
			"doubanid": "7", "type_name": typeName,
		}, ""))
		medias := decodeBody[[]metadata.Media](t, recorder)
		if len(medias) != 1 {
			t.Fatalf("%s 推荐 = %+v", typeName, medias)
		}
		if medias[0].Type != wantType {
			t.Fatalf("%s 类型 = %q，期望 %q", typeName, medias[0].Type, wantType)
		}
		if medias[0].Year != 2019 {
			t.Fatalf("%s 年份 = %d", typeName, medias[0].Year)
		}
	}
}

func TestDoubanGuardsAndFailures(t *testing.T) {
	server := doubanFake(t)
	defer server.Close()
	deps := doubanDeps(t, server.URL)

	// 空 ID 在查询前就拒绝。
	recorder := httptest.NewRecorder()
	deps.doubanCredits(recorder, chiRequest(http.MethodGet, "", map[string]string{"doubanid": "  "}, ""))
	if !strings.Contains(recorder.Body.String(), "豆瓣ID不能为空") {
		t.Fatalf("空 ID 响应 = %s", recorder.Body.String())
	}
	recorder = httptest.NewRecorder()
	deps.doubanPerson(recorder, chiRequest(http.MethodGet, "", map[string]string{"person_id": ""}, ""))
	if !strings.Contains(recorder.Body.String(), "豆瓣人物ID不能为空") {
		t.Fatalf("空人物 ID 响应 = %s", recorder.Body.String())
	}

	// 未配置数据源。
	recorder = httptest.NewRecorder()
	Dependencies{}.doubanPerson(recorder, chiRequest(http.MethodGet, "", map[string]string{"person_id": "1"}, ""))
	if !strings.Contains(recorder.Body.String(), "未配置豆瓣数据源") {
		t.Fatalf("无数据源响应 = %s", recorder.Body.String())
	}
	// 上游 404：返回可读错误而不是空列表。
	recorder = httptest.NewRecorder()
	deps.doubanPerson(recorder, chiRequest(http.MethodGet, "", map[string]string{"person_id": "4242"}, ""))
	if !strings.Contains(recorder.Body.String(), "豆瓣请求失败") {
		t.Fatalf("404 上游响应 = %s", recorder.Body.String())
	}
}

func TestDoubanDecodeFailureIsReported(t *testing.T) {
	server := doubanFake(t)
	defer server.Close()
	deps := doubanDeps(t, server.URL)

	recorder := httptest.NewRecorder()
	deps.doubanCredits(recorder, chiRequest(http.MethodGet, "", map[string]string{
		"doubanid": "9999", "type_name": "电影",
	}, ""))
	// 上游字段类型不对时必须给出可读错误，而不是返回半个演职员表。
	if !strings.Contains(recorder.Body.String(), "豆瓣返回数据无法解析") {
		t.Fatalf("解析失败响应 = %s", recorder.Body.String())
	}
}

func TestDoubanWorkCollectionGuards(t *testing.T) {
	deps := Dependencies{}
	// 客户端未配置时返回空串，不 panic。
	if got := deps.doubanWorkCollection(context.Background(), metadata.NewDouban(nil, "", "", ""), "1"); got != "" {
		t.Fatalf("未配置客户端应返回空串，实际 %q", got)
	}
}
