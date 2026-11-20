package app

import (
	"encoding/json"
	"log/slog"
	"testing"

	"github.com/moviepilot-go/moviepilot-go/internal/config"
	"github.com/moviepilot-go/moviepilot-go/internal/notification"
)

func TestNotificationRuntimeTelegramsReturnsSnapshotCopy(t *testing.T) {
	t.Parallel()

	var nilRuntime *notificationRuntime
	if got := nilRuntime.Telegrams(); got != nil {
		t.Fatalf("nil runtime Telegrams() = %#v", got)
	}
	// 没有配置时返回 nil，入站机器人据此停掉轮询。
	if got := (&notificationRuntime{}).Telegrams(); got != nil {
		t.Fatalf("空运行时 Telegrams() = %#v", got)
	}

	raw := json.RawMessage(`[
		{"name":"Primary","type":"telegram","config":{"TELEGRAM_TOKEN":"token-a","TELEGRAM_CHAT_ID":"1"},"switchs":["整理入库"],"enabled":true},
		{"name":"Backup","type":"telegram","config":{"TELEGRAM_TOKEN":"token-b","TELEGRAM_CHAT_ID":"2"},"switchs":["下载完成"],"enabled":true}
	]`)
	runtime, err := loadNotificationRuntime(t.Context(), &appNotificationRuntimeTestStore{found: true, raw: raw}, slog.Default())
	if err != nil {
		t.Fatalf("loadNotificationRuntime() error = %v", err)
	}

	telegrams := runtime.Telegrams()
	if len(telegrams) != 2 || telegrams[0].Name != "Primary" || telegrams[1].Name != "Backup" {
		t.Fatalf("Telegrams() = %+v", telegrams)
	}
	if telegrams[1].ChatID != "2" || telegrams[1].BotToken != "token-b" {
		t.Fatalf("第二个通道 = %+v", telegrams[1])
	}
	// 返回的是副本：入站轮询改字段不能污染已发布的快照。
	telegrams[0].Name = "被改掉"
	telegrams[0].ChatID = "999"
	again := runtime.Telegrams()
	if again[0].Name != "Primary" || again[0].ChatID != "1" {
		t.Fatalf("快照被外部修改 = %+v", again[0])
	}
}

func TestBuildNotificationSenderWithoutLegacyEnvironment(t *testing.T) {
	// 并行会与本用例的环境变量设置互相干扰，故不 t.Parallel。
	t.Setenv("MOVIEPILOT_TELEGRAM_BOT_TOKEN", "")
	t.Setenv("MOVIEPILOT_TELEGRAM_CHAT_ID", "")

	sender, channels := buildNotificationSender(config.Config{}, slog.Default())
	if sender == nil {
		t.Fatal("sender = nil，应返回可用的空注册表")
	}
	// 没配置就没有任何已启用渠道，调用方据此跳过发送而不是报错。
	if channels != nil {
		t.Fatalf("channels = %v", channels)
	}
	if got := notificationRuntimeTargetNames(sender.(*notification.Registry), notification.Message{Type: notification.TypeOrganize}); len(got) != 0 {
		t.Fatalf("空环境不应有目标 = %v", got)
	}
}

func TestBuildNotificationSenderWithLegacyEnvironment(t *testing.T) {
	t.Setenv("MOVIEPILOT_TELEGRAM_BOT_TOKEN", " legacy-token ")
	t.Setenv("MOVIEPILOT_TELEGRAM_CHAT_ID", " chat-42 ")
	t.Setenv("MOVIEPILOT_TELEGRAM_API_URL", "https://api.example.test")

	sender, channels := buildNotificationSender(config.Config{}, slog.Default())
	if sender == nil {
		t.Fatal("sender = nil")
	}
	if len(channels) != 1 || channels[0] != notification.ChannelTelegram {
		t.Fatalf("channels = %v", channels)
	}
	registry, ok := sender.(*notification.Registry)
	if !ok {
		t.Fatalf("sender 类型 = %T", sender)
	}
	if got := notificationRuntimeTargetNames(registry, notification.Message{Type: notification.TypeOrganize}); len(got) != 1 || got[0] != "Telegram" {
		t.Fatalf("整理入库目标 = %v", got)
	}
}

func TestLegacyNotificationSnapshotTrimsEnvironment(t *testing.T) {
	t.Setenv("MOVIEPILOT_TELEGRAM_BOT_TOKEN", " legacy-token ")
	t.Setenv("MOVIEPILOT_TELEGRAM_CHAT_ID", " chat-42 ")
	t.Setenv("MOVIEPILOT_TELEGRAM_API_URL", " https://api.example.test ")

	snapshot := legacyNotificationSnapshot()
	if len(snapshot.Telegrams) != 1 {
		t.Fatalf("Telegram 快照 = %+v", snapshot.Telegrams)
	}
	telegram := snapshot.Telegrams[0]
	// 环境变量两端空白要裁掉，否则 Telegram 会拿带空格的 Token 去请求。
	if telegram.Name != "Telegram" || telegram.BotToken != "legacy-token" ||
		telegram.ChatID != "chat-42" || telegram.APIURL != "https://api.example.test" {
		t.Fatalf("Telegram 快照 = %+v", telegram)
	}
	// 旧环境变量没有分类开关，所有历史分类都启用。
	want := []string{
		string(notification.TypeDownload), string(notification.TypeOrganize),
		string(notification.TypeSubscribe), string(notification.TypeSiteMessage),
		string(notification.TypeMediaServer), string(notification.TypeManual),
		string(notification.TypePlugin),
	}
	if len(telegram.Switchs) != len(want) {
		t.Fatalf("分类开关 = %v", telegram.Switchs)
	}
	for index, value := range want {
		if telegram.Switchs[index] != value {
			t.Fatalf("分类开关[%d] = %q, want %q", index, telegram.Switchs[index], value)
		}
	}

	// 只配一半时整体不启用（旧的兼容回退不能造出半个通道）。
	t.Setenv("MOVIEPILOT_TELEGRAM_CHAT_ID", "")
	if got := legacyNotificationSnapshot(); len(got.Telegrams) != 0 {
		t.Fatalf("缺 ChatID 时快照 = %+v", got.Telegrams)
	}
}
