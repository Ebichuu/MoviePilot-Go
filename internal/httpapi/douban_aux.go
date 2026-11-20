package httpapi

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"strconv"
	"strings"

	"github.com/go-chi/chi/v5"
	"github.com/moviepilot-go/moviepilot-go/internal/metadata"
)

// doubanSlug 把 V2 的 type_name 映射成豆瓣 API 的资源段。
func doubanSlug(typeName string) string {
	if strings.TrimSpace(typeName) == "电影" {
		return "movie"
	}
	return "tv"
}

// doubanMediaPerson 复刻 V2 `schemas.MediaPerson` 的字段，供演职员表与人物详情使用。
//
// 角色名放 `character`：V2 的 MediaPerson 里 `biography` 是人物简介，
// 前端 PersonCard 读 `character` 展示角色；以前塞进 biography，卡片上就只剩名字。
type doubanMediaPerson struct {
	ID          string   `json:"id"`
	Type        int      `json:"type"`
	Name        string   `json:"name"`
	Character   string   `json:"character"`
	Avatar      string   `json:"avatar"`
	Biography   string   `json:"biography"`
	LatinName   string   `json:"latin_name"`
	AlsoKnownAs []string `json:"also_known_as"`
	Source      string   `json:"source"`
}

type doubanAvatar struct {
	Large  string `json:"large"`
	Normal string `json:"normal"`
}

type doubanCreditEntry struct {
	ID        string       `json:"id"`
	Name      string       `json:"name"`
	Title     string       `json:"title"`
	LatinName string       `json:"latin_name"`
	Character string       `json:"character"`
	Avatar    doubanAvatar `json:"avatar"`
}

func (entry doubanCreditEntry) person() doubanMediaPerson {
	name := entry.Name
	if name == "" {
		name = entry.Title
	}
	alsoKnownAs := make([]string, 0, 1)
	if entry.LatinName != "" {
		alsoKnownAs = append(alsoKnownAs, entry.LatinName)
	}
	return doubanMediaPerson{
		ID:          entry.ID,
		Type:        1,
		Name:        name,
		Character:   entry.Character,
		Avatar:      entry.Avatar.Large,
		LatinName:   entry.LatinName,
		AlsoKnownAs: alsoKnownAs,
		Source:      "douban",
	}
}

type doubanCelebrity struct {
	ID         string `json:"id"`
	Title      string `json:"title"`
	LatinTitle string `json:"latin_title"`
	CoverImg   struct {
		URL string `json:"url"`
	} `json:"cover_img"`
	Extra struct {
		ShortInfo string     `json:"short_info"`
		Info      [][]string `json:"info"`
	} `json:"extra"`
}

type doubanRecommendEntry struct {
	ID           string `json:"id"`
	Title        string `json:"title"`
	CardSubtitle string `json:"card_subtitle"`
	Pic          struct {
		Large string `json:"large"`
	} `json:"pic"`
	Rating struct {
		Value float64 `json:"value"`
	} `json:"rating"`
}

// doubanClient 取出可用的豆瓣客户端；缺失时写出与 V2 一致的未配置提示。
func (d Dependencies) doubanClient(w http.ResponseWriter, r *http.Request) (*metadata.Douban, bool) {
	if d.Metadata == nil {
		writeJSON(w, http.StatusOK, Response{Success: false, Message: "未配置豆瓣数据源"})
		return nil, false
	}
	provider, ok := d.Metadata.Provider("douban")
	if !ok {
		writeJSON(w, http.StatusOK, Response{Success: false, Message: "未配置豆瓣数据源"})
		return nil, false
	}
	douban, assertOK := provider.(*metadata.Douban)
	if !assertOK || douban == nil {
		writeJSON(w, http.StatusOK, Response{Success: false, Message: "未配置豆瓣数据源"})
		return nil, false
	}
	return douban, true
}

// doubanFetch 转发一次豆瓣只读请求，失败时写可读错误并返回 false。
func (d Dependencies) doubanFetch(w http.ResponseWriter, r *http.Request, client *metadata.Douban, path string) (any, bool) {
	result, err := client.Raw(r.Context(), path, nil)
	if err != nil {
		slog.WarnContext(r.Context(), "豆瓣附属查询失败", "path", path, "error", err)
		writeJSON(w, http.StatusOK, Response{Success: false, Message: "豆瓣请求失败，请检查豆瓣数据源配置"})
		return nil, false
	}
	return result, true
}

// doubanFetchQuiet 与 doubanFetch 同源，只是不写响应，
// 供「第一个资源取不到、换另一个再试」的场景使用。
func doubanFetchQuiet(r *http.Request, client *metadata.Douban, path string) (any, error) {
	return client.Raw(r.Context(), path, nil)
}

func doubanDecode(source any, target any) error {
	encoded, err := json.Marshal(source)
	if err != nil {
		return err
	}
	return json.Unmarshal(encoded, target)
}

// doubanCredits 对应 V2 `GET /douban/credits/{doubanid}/{type_name}`，返回演职员表。
func (d Dependencies) doubanCredits(w http.ResponseWriter, r *http.Request) {
	id := strings.TrimSpace(chi.URLParam(r, "doubanid"))
	if id == "" {
		writeJSON(w, http.StatusOK, Response{Success: false, Message: "豆瓣ID不能为空"})
		return
	}
	client, ok := d.doubanClient(w, r)
	if !ok {
		return
	}
	slug := doubanSlug(chi.URLParam(r, "type_name"))
	payload, fetchErr := doubanFetchQuiet(r, client, slug+"/"+id+"/celebrities")
	if fetchErr != nil {
		// 前端详情页的 `type` 来自 URL 查询参数，电影/电视剧标错时会拿到 404
		// （`tv/{电影id}/celebrities`）。V2 这种情况下直接返回空数组，页面上
		// 演职员卡片就整块空白；这里换另一种资源再试一次，号码对得上就有数据。
		alternate := "movie"
		if slug == "movie" {
			alternate = "tv"
		}
		payload, fetchErr = doubanFetchQuiet(r, client, alternate+"/"+id+"/celebrities")
		if fetchErr != nil {
			slog.WarnContext(r.Context(), "豆瓣附属查询失败", "path", slug+"/"+id+"/celebrities", "error", fetchErr)
			writeJSON(w, http.StatusOK, Response{Success: false, Message: "豆瓣请求失败，请检查豆瓣数据源配置"})
			return
		}
		slog.InfoContext(r.Context(), "豆瓣演职员按另一种类型取到", "doubanid", id,
			"requested", slug, "used", alternate)
	}
	var credits struct {
		Actors    []doubanCreditEntry `json:"actors"`
		Directors []doubanCreditEntry `json:"directors"`
	}
	if err := doubanDecode(payload, &credits); err != nil {
		slog.WarnContext(r.Context(), "豆瓣演职员解析失败", "doubanid", id, "error", err)
		writeJSON(w, http.StatusOK, Response{Success: false, Message: "豆瓣返回数据无法解析"})
		return
	}
	people := make([]doubanMediaPerson, 0, len(credits.Directors)+len(credits.Actors))
	seen := make(map[string]bool, len(credits.Directors)+len(credits.Actors))
	for _, entry := range append(credits.Directors, credits.Actors...) {
		if entry.ID == "" || seen[entry.ID] {
			continue
		}
		seen[entry.ID] = true
		people = append(people, entry.person())
	}
	writeJSON(w, http.StatusOK, people)
}

// doubanPerson 对应 V2 `GET /douban/person/{person_id}`，返回人物详情。
// V2 的 `elessar/subject` 上游已下线，这里改用可用的 `celebrity` 资源。
func (d Dependencies) doubanPerson(w http.ResponseWriter, r *http.Request) {
	id := strings.TrimSpace(chi.URLParam(r, "person_id"))
	if id == "" {
		writeJSON(w, http.StatusOK, Response{Success: false, Message: "豆瓣人物ID不能为空"})
		return
	}
	client, ok := d.doubanClient(w, r)
	if !ok {
		return
	}
	payload, ok := d.doubanFetch(w, r, client, "celebrity/"+id)
	if !ok {
		return
	}
	var celebrity doubanCelebrity
	if err := doubanDecode(payload, &celebrity); err != nil {
		slog.WarnContext(r.Context(), "豆瓣人物解析失败", "person_id", id, "error", err)
		writeJSON(w, http.StatusOK, Response{Success: false, Message: "豆瓣返回数据无法解析"})
		return
	}
	alsoKnownAs := make([]string, 0, len(celebrity.Extra.Info))
	for _, info := range celebrity.Extra.Info {
		if len(info) >= 2 && strings.TrimSpace(info[1]) != "" {
			alsoKnownAs = append(alsoKnownAs, info[0]+"："+info[1])
		}
	}
	writeJSON(w, http.StatusOK, doubanMediaPerson{
		ID:          celebrity.ID,
		Name:        celebrity.Title,
		Avatar:      celebrity.CoverImg.URL,
		Biography:   celebrity.Extra.ShortInfo,
		AlsoKnownAs: alsoKnownAs,
		Source:      "douban",
	})
}

// doubanPersonCredits 对应 V2 `GET /douban/person/credits/{person_id}`。
// V2 先取人物详情里的作品集 ID 再查作品；上游 `elessar` 已下线，拿不到作品集时按 V2 语义返回空列表。
func (d Dependencies) doubanPersonCredits(w http.ResponseWriter, r *http.Request) {
	id := strings.TrimSpace(chi.URLParam(r, "person_id"))
	if id == "" {
		writeJSON(w, http.StatusOK, Response{Success: false, Message: "豆瓣人物ID不能为空"})
		return
	}
	client, ok := d.doubanClient(w, r)
	if !ok {
		return
	}
	collectionID := d.doubanWorkCollection(r.Context(), client, id)
	if collectionID == "" {
		writeJSON(w, http.StatusOK, []any{})
		return
	}
	payload, ok := d.doubanFetch(w, r, client, "elessar/work_collections/"+collectionID+"/works?start=0&count=20")
	if !ok {
		return
	}
	var works struct {
		Works []struct {
			Subject doubanRecommendEntry `json:"subject"`
		} `json:"works"`
	}
	if err := doubanDecode(payload, &works); err != nil {
		slog.WarnContext(r.Context(), "豆瓣人物作品解析失败", "person_id", id, "error", err)
		writeJSON(w, http.StatusOK, []any{})
		return
	}
	medias := make([]metadata.Media, 0, len(works.Works))
	for _, work := range works.Works {
		if work.Subject.ID == "" {
			continue
		}
		medias = append(medias, doubanRecommendMedia(work.Subject, "movie"))
	}
	writeJSON(w, http.StatusOK, medias)
}

// doubanWorkCollection 从人物详情里找作品集 ID，找不到返回空串。
func (d Dependencies) doubanWorkCollection(ctx context.Context, client *metadata.Douban, personID string) string {
	payload, err := client.Raw(ctx, "celebrity/"+personID, nil)
	if err != nil {
		slog.WarnContext(ctx, "豆瓣人物作品集查询失败", "person_id", personID, "error", err)
		return ""
	}
	var detail struct {
		Modules []struct {
			Type    string `json:"type"`
			Payload struct {
				ID string `json:"id"`
			} `json:"payload"`
		} `json:"modules"`
	}
	if err := doubanDecode(payload, &detail); err != nil {
		return ""
	}
	for _, module := range detail.Modules {
		if module.Type == "work_collections" && module.Payload.ID != "" {
			return module.Payload.ID
		}
	}
	return ""
}

// doubanRecommend 对应 V2 `GET /douban/recommend/{doubanid}/{type_name}`，返回推荐媒体列表。
func (d Dependencies) doubanRecommend(w http.ResponseWriter, r *http.Request) {
	id := strings.TrimSpace(chi.URLParam(r, "doubanid"))
	if id == "" {
		writeJSON(w, http.StatusOK, Response{Success: false, Message: "豆瓣ID不能为空"})
		return
	}
	client, ok := d.doubanClient(w, r)
	if !ok {
		return
	}
	slug := doubanSlug(chi.URLParam(r, "type_name"))
	payload, ok := d.doubanFetch(w, r, client, slug+"/"+id+"/recommendations")
	if !ok {
		return
	}
	var entries []doubanRecommendEntry
	if err := doubanDecode(payload, &entries); err != nil {
		slog.WarnContext(r.Context(), "豆瓣推荐解析失败", "doubanid", id, "error", err)
		writeJSON(w, http.StatusOK, Response{Success: false, Message: "豆瓣返回数据无法解析"})
		return
	}
	medias := make([]metadata.Media, 0, len(entries))
	for _, entry := range entries {
		if entry.ID == "" {
			continue
		}
		medias = append(medias, doubanRecommendMedia(entry, slug))
	}
	writeJSON(w, http.StatusOK, medias)
}

// doubanRecommendMedia 把推荐/作品条目转成与其它来源一致的媒体结构。
func doubanRecommendMedia(entry doubanRecommendEntry, slug string) metadata.Media {
	mediaType := "movie"
	if slug == "tv" {
		mediaType = "tv"
	}
	year := 0
	if len(entry.CardSubtitle) >= 4 {
		if parsed, err := strconv.Atoi(entry.CardSubtitle[:4]); err == nil {
			year = parsed
		}
	}
	return metadata.Media{
		Source:      "douban",
		ID:          entry.ID,
		Type:        mediaType,
		Title:       entry.Title,
		Year:        year,
		PosterURL:   entry.Pic.Large,
		VoteAverage: entry.Rating.Value,
	}
}
