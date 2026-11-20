package app

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/moviepilot-go/moviepilot-go/internal/mediaserver"
	"github.com/moviepilot-go/moviepilot-go/internal/notification"
	"github.com/moviepilot-go/moviepilot-go/internal/notificationtemplate"
)

func renderDefaultTemplate(t *testing.T, contentType notification.ContentType, context map[string]any) (string, string) {
	t.Helper()
	renderer, err := notificationtemplate.New(defaultNotificationTemplates())
	if err != nil {
		t.Fatalf("默认模板解析失败: %v", err)
	}
	message, err := renderer.Apply(notification.Message{ContentType: contentType}, context)
	if err != nil {
		t.Fatalf("渲染 %s 失败: %v", contentType, err)
	}
	return message.Title, message.Text
}

func TestDefaultNotificationTemplatesCoverContentTypes(t *testing.T) {
	var templates map[string]json.RawMessage
	if err := json.Unmarshal(defaultNotificationTemplates(), &templates); err != nil {
		t.Fatalf("默认模板不是合法 JSON: %v", err)
	}
	for _, contentType := range []notification.ContentType{
		notification.ContentOrganizeSuccess,
		notification.ContentDownloadAdded,
		notification.ContentSubscribeAdded,
		notification.ContentSubscribeComplete,
	} {
		raw, exists := templates[string(contentType)]
		if !exists || len(raw) == 0 {
			t.Fatalf("缺少 %s 的默认模板", contentType)
		}
	}
	// 默认模板必须能被渲染器解析（解析失败会让所有通知退化成无正文）。
	if _, err := notificationtemplate.New(defaultNotificationTemplates()); err != nil {
		t.Fatalf("默认模板不可渲染: %v", err)
	}
}

func TestDefaultOrganizeSuccessTemplateRendersContextFields(t *testing.T) {
	title, text := renderDefaultTemplate(t, notification.ContentOrganizeSuccess, map[string]any{
		"title_year": "沙丘 (2021)", "season_episode": "S01E01", "vote_average": "8.2",
		"type": "电影", "category": "科幻", "resource_term": "WEB-DL",
		"file_count": 2, "total_size": "1.5 GB",
	})
	if !strings.Contains(title, "沙丘 (2021)") || !strings.Contains(title, "S01E01") || !strings.Contains(title, "已入库") {
		t.Fatalf("标题 = %q", title)
	}
	for _, want := range []string{"评分：8.2", "类型：电影", "类别：科幻", "质量：WEB-DL", "共2个文件", "大小：1.5 GB"} {
		if !strings.Contains(text, want) {
			t.Fatalf("正文缺少 %q：%q", want, text)
		}
	}
	// 没有失败文件时不能出现「以下文件处理失败」。
	if strings.Contains(text, "以下文件处理失败") {
		t.Fatalf("正文多出失败段: %q", text)
	}

	// 缺省上下文字段对应的句子要整段消失，而不是留空占位。
	title, text = renderDefaultTemplate(t, notification.ContentOrganizeSuccess, map[string]any{
		"title_year": "沙丘 (2021)", "type": "电影", "file_count": 1, "total_size": "1 GB",
	})
	if strings.Contains(title, "S01E01") {
		t.Fatalf("无季集时标题不应有季集: %q", title)
	}
	for _, unwanted := range []string{"评分：", "类别：", "质量：", "以下文件处理失败"} {
		if strings.Contains(text, unwanted) {
			t.Fatalf("缺字段时正文不应包含 %q：%q", unwanted, text)
		}
	}
	if !strings.Contains(text, "类型：电影") || !strings.Contains(text, "共1个文件") {
		t.Fatalf("正文 = %q", text)
	}
}

func TestDefaultDownloadAddedTemplateRendersContextFields(t *testing.T) {
	title, text := renderDefaultTemplate(t, notification.ContentDownloadAdded, map[string]any{
		"title_year": "沙丘 (2021)", "season_fmt": "S01", "download_episodes": "E01-E03",
		"site_name": "A站", "resource_term": "WEB-DL", "size": "1.5 GB",
		"torrent_title": "Dune.2021.1080p", "seeders": 12,
	})
	if !strings.Contains(title, "S01") || !strings.Contains(title, "E01-E03") || !strings.Contains(title, "开始下载") {
		t.Fatalf("标题 = %q", title)
	}
	for _, want := range []string{"站点：A站", "质量：WEB-DL", "大小：1.5 GB", "种子：Dune.2021.1080p", "做种数：12"} {
		if !strings.Contains(text, want) {
			t.Fatalf("正文缺少 %q：%q", want, text)
		}
	}
	// 换行必须是真正的换行符，不能泄漏 JSON 转义序列。
	if strings.Contains(text, `\n`) {
		t.Fatalf("正文含未解码的转义序列: %q", text)
	}
	if !strings.Contains(text, "\n质量：") {
		t.Fatalf("正文缺少真实换行: %q", text)
	}
}

func TestDefaultSubscribeTemplatesRender(t *testing.T) {
	title, text := renderDefaultTemplate(t, notification.ContentSubscribeAdded, map[string]any{
		"title_year": "沙丘 (2021)", "season_fmt": "S01",
	})
	if !strings.Contains(title, "沙丘 (2021)") || !strings.Contains(title, "S01") || !strings.Contains(title, "已添加订阅") {
		t.Fatalf("订阅新增标题 = %q", title)
	}
	if text != "" {
		t.Fatalf("订阅新增不应有正文: %q", text)
	}

	title, text = renderDefaultTemplate(t, notification.ContentSubscribeComplete, map[string]any{
		"title_year": "沙丘 (2021)", "season_fmt": "S01", "msgstr": "全部入库",
		"vote_average": "8.2", "username": "alice", "actors": "提莫西·查拉梅", "overview": "沙漠星球的故事",
	})
	if !strings.Contains(title, "已完成") || !strings.Contains(title, "全部入库") {
		t.Fatalf("订阅完成标题 = %q", title)
	}
	for _, want := range []string{"评分：8.2", "来自用户：alice", "演员：提莫西·查拉梅", "简介：沙漠星球的故事"} {
		if !strings.Contains(text, want) {
			t.Fatalf("订阅完成正文缺少 %q：%q", want, text)
		}
	}
}

func TestNotificationTemplateRuntimeLifecycle(t *testing.T) {
	// 空配置也要能构造（此时 Apply 原样返回）。
	empty, err := newNotificationTemplateRuntime(nil)
	if err != nil {
		t.Fatalf("空配置应可用: %v", err)
	}
	message := notification.Message{ContentType: notification.ContentOrganizeSuccess}
	got, err := empty.Apply(message, nil)
	if err != nil || got.Title != "" {
		t.Fatalf("空配置 Apply = %+v %v", got, err)
	}

	if _, err := newNotificationTemplateRuntime(json.RawMessage("{bad")); err == nil {
		t.Fatal("非法模板应报错")
	}

	// nil 运行时不能 panic，原样返回消息。
	var nilRuntime *notificationTemplateRuntime
	got, err = nilRuntime.Apply(message, nil)
	if err != nil || got != message {
		t.Fatalf("nil 运行时 = %+v %v", got, err)
	}
	if err := nilRuntime.Reload(defaultNotificationTemplates()); err == nil {
		t.Fatal("nil 运行时重载应报错")
	}

	runtime, err := newNotificationTemplateRuntime(defaultNotificationTemplates())
	if err != nil {
		t.Fatalf("构造失败: %v", err)
	}
	rendered, err := runtime.Apply(notification.Message{ContentType: notification.ContentSubscribeAdded}, map[string]any{
		"title_year": "沙丘", "season_fmt": "S01",
	})
	if err != nil || !strings.Contains(rendered.Title, "已添加订阅") {
		t.Fatalf("渲染 = %+v %v", rendered, err)
	}

	// 业务侧已经写好标题与正文时，模板不得覆盖。
	explicit := notification.Message{ContentType: notification.ContentSubscribeAdded, Title: "自定义标题", Text: "自定义正文"}
	got, err = runtime.Apply(explicit, map[string]any{"title_year": "沙丘"})
	if err != nil || got.Title != "自定义标题" || got.Text != "自定义正文" {
		t.Fatalf("覆盖了显式内容: %+v %v", got, err)
	}

	// 非法重载失败后必须保留旧渲染器（不能把通知模板整体清空）。
	if err := runtime.Reload(json.RawMessage("{bad")); err == nil {
		t.Fatal("非法重载应报错")
	}
	after, err := runtime.Apply(notification.Message{ContentType: notification.ContentSubscribeAdded}, map[string]any{"title_year": "沙丘"})
	if err != nil || !strings.Contains(after.Title, "已添加订阅") {
		t.Fatalf("重载失败后旧模板丢失: %+v %v", after, err)
	}

	// 合法重载生效。
	if err := runtime.Reload(json.RawMessage(`{"subscribeAdded":{"title":"新标题 {{ title_year }}"}}`)); err != nil {
		t.Fatalf("重载失败: %v", err)
	}
	reloaded, err := runtime.Apply(notification.Message{ContentType: notification.ContentSubscribeAdded}, map[string]any{"title_year": "沙丘"})
	if err != nil || !strings.Contains(reloaded.Title, "新标题 沙丘") {
		t.Fatalf("重载未生效: %+v %v", reloaded, err)
	}
}

func TestCloneStringMap(t *testing.T) {
	if cloneStringMap(nil) != nil {
		t.Fatal("nil 应保持 nil")
	}
	source := map[string]string{"a": "1"}
	clone := cloneStringMap(source)
	if clone["a"] != "1" || len(clone) != 1 {
		t.Fatalf("克隆 = %v", clone)
	}
	clone["a"] = "2"
	if source["a"] != "1" {
		t.Fatal("克隆必须与原 map 独立")
	}
}

func TestLocalMediaItemType(t *testing.T) {
	// 返回值是媒体服务器使用的本地语义值（中文常量），不是 TMDB 的 movie/tv。
	cases := map[string]string{
		"movie": mediaserver.ItemTypeMovie, " movies ": mediaserver.ItemTypeMovie, "MOVIE": mediaserver.ItemTypeMovie,
		"tv": mediaserver.ItemTypeTV, "tvshow": mediaserver.ItemTypeTV, "series": mediaserver.ItemTypeTV,
		"show": mediaserver.ItemTypeTV, " TvShow ": mediaserver.ItemTypeTV,
		"": "", "music": "", "unknown": "",
	}
	for input, want := range cases {
		if got := localMediaItemType(input); got != want {
			t.Fatalf("localMediaItemType(%q) = %q，期望 %q", input, got, want)
		}
	}
}
