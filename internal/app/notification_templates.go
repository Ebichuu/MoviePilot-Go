package app

import (
	"encoding/json"
	"errors"
	"sync/atomic"

	"github.com/moviepilot-go/moviepilot-go/internal/notification"
	"github.com/moviepilot-go/moviepilot-go/internal/notificationtemplate"
)

type notificationTemplateRuntime struct {
	current atomic.Pointer[notificationtemplate.Renderer]
}

func newNotificationTemplateRuntime(raw json.RawMessage) (*notificationTemplateRuntime, error) {
	runtime := &notificationTemplateRuntime{}
	if err := runtime.Reload(raw); err != nil {
		return nil, err
	}
	return runtime, nil
}

func (r *notificationTemplateRuntime) Reload(raw json.RawMessage) error {
	if r == nil {
		return errors.New("通知模板运行时不可用")
	}
	renderer, err := notificationtemplate.New(raw)
	if err != nil {
		return err
	}
	r.current.Store(renderer)
	return nil
}

func (r *notificationTemplateRuntime) Apply(msg notification.Message, context map[string]any) (notification.Message, error) {
	if r == nil {
		return msg, nil
	}
	renderer := r.current.Load()
	if renderer == nil {
		return msg, nil
	}
	return renderer.Apply(msg, context)
}

func defaultNotificationTemplates() json.RawMessage {
	value := map[string]string{
		"organizeSuccess": `{
    'title': '{{ title_year }}' '{% if season_episode %} {{ season_episode }}{% endif %} 已入库',
    'text': '{% if vote_average %}评分：{{ vote_average }}，{% endif %}类型：{{ type }}{% if category %}，类别：{{ category }}{% endif %}{% if resource_term %}，质量：{{ resource_term }}{% endif %}，共{{ file_count }}个文件，大小：{{ total_size }}{% if err_msg %}，以下文件处理失败：{{ err_msg }}{% endif %}'
}`,
		"downloadAdded": `{
    'title': '{{ title_year }}' '{% if download_episodes %} {{ season_fmt }} {{ download_episodes }}{% else %}{{ season_episode }}{% endif %} 开始下载',
    'text': '{% if site_name %}站点：{{ site_name }}{% endif %}{% if resource_term %}\\n质量：{{ resource_term }}{% endif %}{% if size %}\\n大小：{{ size }}{% endif %}{% if torrent_title %}\\n种子：{{ torrent_title }}{% endif %}{% if pubdate %}\\n发布时间：{{ pubdate }}{% endif %}{% if freedate %}\\n免费时间：{{ freedate }}{% endif %}{% if seeders %}\\n做种数：{{ seeders }}{% endif %}{% if volume_factor %}\\n促销：{{ volume_factor }}{% endif %}{% if hit_and_run %}\\nHit&Run：{{ hit_and_run }}{% endif %}{% if labels %}\\n标签：{{ labels }}{% endif %}{% if description %}\\n描述：{{ description }}{% endif %}'
}`,
		"subscribeAdded": `{'title': '{{ title_year }}{% if season_fmt %} {{ season_fmt }}{% endif %} 已添加订阅'}`,
		"subscribeComplete": `{
    'title': '{{ title_year }}' '{% if season_fmt %} {{ season_fmt }}{% endif %} 已完成{{ msgstr }}',
    'text': '{% if vote_average %}评分：{{ vote_average }}{% endif %}{% if username %}，来自用户：{{ username }}{% endif %}{% if actors %}\\n演员：{{ actors }}{% endif %}{% if overview %}\\n简介：{{ overview }}{% endif %}'
}`,
	}
	raw, _ := json.Marshal(value)
	return raw
}
