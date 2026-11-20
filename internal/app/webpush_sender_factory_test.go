package app

import (
	"log/slog"
	"testing"

	"github.com/moviepilot-go/moviepilot-go/internal/config"
	"github.com/moviepilot-go/moviepilot-go/internal/database"
	"github.com/moviepilot-go/moviepilot-go/internal/notification"
)

// TestNewWebPushSenderConfigGate 覆盖 WebPush 渠道的构建门槛：
// 缺少存储、缺少私钥或缺少 subject 时不构建，三者齐备时构建真实发送器。
func TestNewWebPushSenderConfigGate(t *testing.T) {
	logger := slog.Default()
	valid := config.Config{
		VAPIDPrivateKey: "private",
		VAPIDPublicKey:  "public",
		VAPIDSubject:    "mailto:admin@example.com",
	}
	if sender := newWebPushSender(valid, nil, logger); sender != nil {
		t.Fatal("缺少存储时不应构建 WebPush 发送器")
	}

	store := &database.Store{}
	for _, testCase := range []struct {
		name string
		cfg  config.Config
	}{
		{"缺少私钥", config.Config{VAPIDSubject: "mailto:admin@example.com"}},
		{"缺少 subject", config.Config{VAPIDPrivateKey: "private"}},
		{"两者都空", config.Config{}},
		{"只有空白字符", config.Config{VAPIDPrivateKey: "   ", VAPIDSubject: "  "}},
	} {
		if sender := newWebPushSender(testCase.cfg, store, logger); sender != nil {
			t.Fatalf("%s 时不应构建 WebPush 发送器", testCase.name)
		}
	}

	sender := newWebPushSender(valid, store, logger)
	if sender == nil {
		t.Fatal("配置齐备时应构建 WebPush 发送器")
	}
	if err := newWebPushSender(valid, store, logger).Send(t.Context(), notification.Message{
		Channel: notification.ChannelWebPush, Title: "", Text: "",
	}); err != nil {
		t.Fatalf("空标题正文 Send() error = %v", err)
	}
}
