package httpapi

import (
	"log/slog"
	"net/http"
	"net/url"
	"strings"

	"github.com/go-chi/chi/v5"
	"github.com/moviepilot-go/moviepilot-go/internal/metadata"
)

func (d Dependencies) bangumiPassthrough(w http.ResponseWriter, r *http.Request, build func(*http.Request) string) {
	if d.Metadata == nil {
		writeJSON(w, http.StatusOK, Response{Success: false, Message: "未配置元数据源"})
		return
	}
	provider, ok := d.Metadata.Provider("bangumi")
	bangumi, assertOK := provider.(*metadata.Bangumi)
	if !ok || !assertOK || bangumi == nil {
		writeJSON(w, http.StatusOK, Response{Success: false, Message: "未配置 Bangumi 数据源"})
		return
	}
	result, err := bangumi.Raw(r.Context(), build(r), url.Values{})
	if err != nil {
		if d.Logger != nil {
			d.Logger.Warn("Bangumi 请求失败", "path", build(r), "error", err)
		}
		writeJSON(w, http.StatusOK, Response{Success: false, Message: "Bangumi 请求失败，请检查网络"})
		return
	}
	writeJSON(w, http.StatusOK, result)
}

// bangumiSubject 对应 V2 `GET /bangumi/{bangumiid}`。
func (d Dependencies) bangumiSubject(w http.ResponseWriter, r *http.Request) {
	d.bangumiPassthrough(w, r, func(r *http.Request) string { return "v0/subjects/" + chi.URLParam(r, "bangumiid") })
}

// bangumiCredits 对应 V2 `GET /bangumi/credits/{bangumiid}`（人物列表）。
//
// 不能原样透传：V2 返回的是 MediaPerson 列表，每条要带 source="bangumi"（前端
// PersonCard 靠 source 决定头像字段，bangumi 读 images.medium）。透传原始
// 对象数组的话卡片只有名字没有头像。
func (d Dependencies) bangumiCredits(w http.ResponseWriter, r *http.Request) {
	persons := []any{}
	if d.Metadata == nil {
		writeJSON(w, http.StatusOK, persons)
		return
	}
	provider, ok := d.Metadata.Provider("bangumi")
	bangumi, assertOK := provider.(*metadata.Bangumi)
	if !ok || !assertOK || bangumi == nil {
		writeJSON(w, http.StatusOK, persons)
		return
	}
	result, err := bangumi.Raw(r.Context(), "v0/subjects/"+chi.URLParam(r, "bangumiid")+"/persons", nil)
	if err != nil {
		slog.WarnContext(r.Context(), "Bangumi 演职员查询失败", "bangumiid", chi.URLParam(r, "bangumiid"), "error", err)
		writeJSON(w, http.StatusOK, persons)
		return
	}
	entries, assertOK := result.([]any)
	if !assertOK {
		writeJSON(w, http.StatusOK, persons)
		return
	}
	for _, entry := range entries {
		item, itemOK := entry.(map[string]any)
		if !itemOK {
			continue
		}
		person := map[string]any{"source": "bangumi"}
		// 只保留 MediaPerson 里有的字段，避免把 bangumi 的私有结构灌给前端。
		for _, key := range []string{"id", "name", "type", "images", "relation", "career"} {
			if value, exists := item[key]; exists {
				person[key] = value
			}
		}
		persons = append(persons, person)
	}
	writeJSON(w, http.StatusOK, persons)
}

// bangumiPerson 对应 V2 `GET /bangumi/person/{person_id}`。
func (d Dependencies) bangumiPerson(w http.ResponseWriter, r *http.Request) {
	d.bangumiPassthrough(w, r, func(r *http.Request) string { return "v0/persons/" + chi.URLParam(r, "person_id") })
}

// bangumiPersonCredits 对应 V2 `GET /bangumi/person/credits/{person_id}`。
func (d Dependencies) bangumiPersonCredits(w http.ResponseWriter, r *http.Request) {
	d.bangumiPassthrough(w, r, func(r *http.Request) string { return "v0/persons/" + chi.URLParam(r, "person_id") + "/subjects" })
}

// bangumiRecommend 对应 V2 `GET /bangumi/recommend/{bangumiid}`（关联条目）。
func (d Dependencies) bangumiRecommend(w http.ResponseWriter, r *http.Request) {
	d.bangumiPassthrough(w, r, func(r *http.Request) string { return "v0/subjects/" + chi.URLParam(r, "bangumiid") + "/subjects" })
}

const (
	anilistSubjectQuery = "query($id:Int){Media(id:$id){id title{romaji native} coverImage{large} description(asHtml:false) startDate{year}}}"
	// 演职员照 V2 的写法取「角色边 + 日语配音演员」：给出来的 id 是 Staff id，
	// 前端点进去才落到正确的人物页（直接拿角色 id 会查错人）。
	anilistCreditsQuery       = "query($id:Int,$page:Int,$count:Int){Media(id:$id,type:ANIME){characters(page:$page,perPage:$count,sort:[ROLE,RELEVANCE]){edges{role node{name{full native}} voiceActors(language:JAPANESE,sort:[RELEVANCE]){id name{full native} image{large medium} siteUrl}}}}}"
	anilistDiscoverQuery      = "query{Page(page:1,perPage:20){media(sort:POPULARITY_DESC,type:ANIME){id title{romaji native} coverImage{large} averageScore}}}"
	anilistPersonQuery        = "query($id:Int){Staff(id:$id){id name{full native} image{large} description(asHtml:false)}}"
	anilistPersonCreditsQuery = "query($id:Int){Staff(id:$id){staffMedia(perPage:25,sort:START_DATE_DESC){nodes{id title{romaji native}}}}}"
	anilistRecommendQuery     = "query($id:Int){Media(id:$id){recommendations(perPage:25){nodes{mediaRecommendation{id title{romaji native} coverImage{large}}}}}}"
)

func (d Dependencies) anilistPassthrough(w http.ResponseWriter, r *http.Request, query string, variables map[string]any) {
	if d.Metadata == nil {
		writeJSON(w, http.StatusOK, Response{Success: false, Message: "未配置元数据源"})
		return
	}
	provider, ok := d.Metadata.Provider("anilist")
	anilist, assertOK := provider.(*metadata.AniList)
	if !ok || !assertOK || anilist == nil {
		writeJSON(w, http.StatusOK, Response{Success: false, Message: "未配置 AniList 数据源"})
		return
	}
	result, err := anilist.Raw(r.Context(), query, variables)
	if err != nil {
		if d.Logger != nil {
			d.Logger.Warn("AniList 请求失败", "error", err)
		}
		writeJSON(w, http.StatusOK, Response{Success: false, Message: "AniList 请求失败，请检查网络"})
		return
	}
	writeJSON(w, http.StatusOK, result)
}

func anilistIDParam(r *http.Request, name string) int {
	return parseHistoryInt(chi.URLParam(r, name), 0, 0, 2_000_000_000)
}

// anilistSubject 对应 V2 `GET /anilist/{anilist_id}`。
func (d Dependencies) anilistSubject(w http.ResponseWriter, r *http.Request) {
	d.anilistPassthrough(w, r, anilistSubjectQuery, map[string]any{"id": anilistIDParam(r, "anilist_id")})
}

// anilistCredits 对应 V2 `GET /anilist/credits/{anilist_id}`。
//
// 同样要把 GraphQL 返回的 edges 摊平成 MediaPerson 列表：前端是 `concat` 进数组的，
// 直接透传 `{Media:{characters:{edges:[...]}}}` 对象的话演员卡片整块空白。
func (d Dependencies) anilistCredits(w http.ResponseWriter, r *http.Request) {
	persons := []any{}
	if d.Metadata == nil {
		writeJSON(w, http.StatusOK, persons)
		return
	}
	provider, ok := d.Metadata.Provider("anilist")
	anilist, assertOK := provider.(*metadata.AniList)
	if !ok || !assertOK || anilist == nil {
		writeJSON(w, http.StatusOK, persons)
		return
	}
	data, err := anilist.Raw(r.Context(), anilistCreditsQuery, map[string]any{
		"id":    anilistIDParam(r, "anilist_id"),
		"page":  creditsPageFromRequest(r),
		"count": tmdbCreditsPageSize,
	})
	if err != nil {
		slog.WarnContext(r.Context(), "AniList 演职员查询失败", "anilist_id", chi.URLParam(r, "anilist_id"), "error", err)
		writeJSON(w, http.StatusOK, persons)
		return
	}
	media, mediaOK := data["Media"].(map[string]any)
	if !mediaOK {
		writeJSON(w, http.StatusOK, persons)
		return
	}
	characters, charactersOK := media["characters"].(map[string]any)
	if !charactersOK {
		writeJSON(w, http.StatusOK, persons)
		return
	}
	edges, edgesOK := characters["edges"].([]any)
	if !edgesOK {
		writeJSON(w, http.StatusOK, persons)
		return
	}
	for _, rawEdge := range edges {
		edge, edgeOK := rawEdge.(map[string]any)
		if !edgeOK {
			continue
		}
		voiceActors, actorsOK := edge["voiceActors"].([]any)
		if !actorsOK || len(voiceActors) == 0 {
			// 没有配音演员的角色（例如只出现在原作里的角色）不产出人物卡。
			continue
		}
		actor, actorOK := voiceActors[0].(map[string]any)
		if !actorOK {
			continue
		}
		name := anilistPersonName(actor)
		if name == "" {
			continue
		}
		person := map[string]any{"source": "anilist", "id": actor["id"], "name": name, "type": 1}
		if character, charOK := edge["node"].(map[string]any); charOK {
			if characterName := anilistPersonName(character); characterName != "" {
				person["character"] = characterName
			}
		}
		if image, hasImage := actor["image"]; hasImage {
			person["images"] = image
		}
		if siteURL, hasURL := actor["siteUrl"]; hasURL {
			person["url"] = siteURL
		}
		persons = append(persons, person)
	}
	writeJSON(w, http.StatusOK, persons)
}

// anilistPersonName 取 AniList 的 `name{full native}`，全名为空时退回原名。
func anilistPersonName(item map[string]any) string {
	name, nameOK := item["name"].(map[string]any)
	if !nameOK {
		return ""
	}
	if full, fullOK := name["full"].(string); fullOK && strings.TrimSpace(full) != "" {
		return strings.TrimSpace(full)
	}
	native, _ := name["native"].(string)
	return strings.TrimSpace(native)
}

// anilistDiscover 已迁到 recommend_compat.go：探索页要的是 V2 的 MediaInfo 列表，
// 不能像这里其它接口那样透传 GraphQL 的 `{Page:{media:[...]}}` 对象。

// anilistPerson 对应 V2 `GET /anilist/person/{person_id}`。

// anilistPerson 对应 V2 `GET /anilist/person/{person_id}`。
func (d Dependencies) anilistPerson(w http.ResponseWriter, r *http.Request) {
	d.anilistPassthrough(w, r, anilistPersonQuery, map[string]any{"id": anilistIDParam(r, "person_id")})
}

// anilistPersonCredits 对应 V2 `GET /anilist/person/credits/{person_id}`。
func (d Dependencies) anilistPersonCredits(w http.ResponseWriter, r *http.Request) {
	d.anilistPassthrough(w, r, anilistPersonCreditsQuery, map[string]any{"id": anilistIDParam(r, "person_id")})
}

// anilistRecommend 对应 V2 `GET /anilist/recommend/{anilist_id}`。
func (d Dependencies) anilistRecommend(w http.ResponseWriter, r *http.Request) {
	d.anilistPassthrough(w, r, anilistRecommendQuery, map[string]any{"id": anilistIDParam(r, "anilist_id")})
}
