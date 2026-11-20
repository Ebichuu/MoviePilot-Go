package notification

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"reflect"
	"strings"
	"sync"
	"testing"

	"github.com/moviepilot-go/moviepilot-go/internal/notificationconfig"
)

func TestRegistryTargets(t *testing.T) {
	t.Parallel()

	registry := newRegistryWithFactory(discardRegistryLogger(), registryTestFactory(nil))
	if err := registry.Replace(mustRegistrySnapshot(t, `[
		{"name":"Primary","type":"telegram","config":{"TELEGRAM_TOKEN":"primary-token","TELEGRAM_CHAT_ID":"1"},"switchs":["整理入库","订阅"],"enabled":true},
		{"name":"Secondary","type":"telegram","config":{"TELEGRAM_TOKEN":"secondary-token","TELEGRAM_CHAT_ID":"2"},"switchs":["资源下载"],"enabled":true},
		{"name":"Disabled","type":"telegram","config":{"TELEGRAM_TOKEN":"disabled-token","TELEGRAM_CHAT_ID":"3"},"switchs":["整理入库"],"enabled":false}
	]`)); err != nil {
		t.Fatalf("Replace() error = %v", err)
	}

	tests := []struct {
		name string
		msg  Message
		want []Target
	}{
		{name: "空过滤匹配全部", want: []Target{{Channel: ChannelTelegram, Source: "Primary"}, {Channel: ChannelTelegram, Source: "Secondary"}}},
		{name: "Telegram 渠道", msg: Message{Channel: ChannelTelegram}, want: []Target{{Channel: ChannelTelegram, Source: "Primary"}, {Channel: ChannelTelegram, Source: "Secondary"}}},
		{name: "拒绝 WebPush", msg: Message{Channel: ChannelWebPush}, want: []Target{}},
		{name: "严格匹配来源", msg: Message{Source: "Secondary"}, want: []Target{{Channel: ChannelTelegram, Source: "Secondary"}}},
		{name: "来源大小写不匹配", msg: Message{Source: "secondary"}, want: []Target{}},
		{name: "按通知类型开关", msg: Message{Type: TypeOrganize}, want: []Target{{Channel: ChannelTelegram, Source: "Primary"}}},
		{name: "定向用户绕过开关", msg: Message{Type: TypeMediaServer, UserID: "42"}, want: []Target{{Channel: ChannelTelegram, Source: "Primary"}, {Channel: ChannelTelegram, Source: "Secondary"}}},
		{name: "定向用户仍遵守来源", msg: Message{Source: "Secondary", Type: TypeOrganize, UserID: "42"}, want: []Target{{Channel: ChannelTelegram, Source: "Secondary"}}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got := registry.Targets(tt.msg)
			if got == nil {
				got = []Target{}
			}
			if !reflect.DeepEqual(got, tt.want) {
				t.Fatalf("Targets() = %#v, want %#v", got, tt.want)
			}
		})
	}
}

func TestRegistryTargetsAcceptsFrontendSwitchIdentifiers(t *testing.T) {
	t.Parallel()

	// 真实前端保存的 switchs 是英文标识；历史数据与单测里是中文显示名。
	// 两种写法必须都能选中渠道，否则业务通知会静默变成 0 条投递行。
	registry := newRegistryWithFactory(discardRegistryLogger(), registryTestFactory(nil))
	if err := registry.Replace(mustRegistrySnapshot(t, `[
		{"name":"FakeBot","type":"telegram","config":{"TELEGRAM_TOKEN":"fake-token","TELEGRAM_CHAT_ID":"100"},"switchs":["search","download","subscribe","site","mediaserver","plugin","agent","system"],"enabled":true}
	]`)); err != nil {
		t.Fatalf("Replace() error = %v", err)
	}

	for _, mtype := range []Type{
		TypeDownload, TypeSubscribe, TypeSiteMessage, TypeMediaServer, TypePlugin,
	} {
		targets := registry.Targets(Message{Type: mtype})
		if len(targets) != 1 || targets[0].Source != "FakeBot" {
			t.Fatalf("Targets(%s) = %#v, want FakeBot", mtype, targets)
		}
	}
	// 「整理入库」对应前端标识 organize，上面的开关没有勾选它，中文名同样不能命中。
	if targets := registry.Targets(Message{Type: TypeOrganize}); len(targets) != 0 {
		t.Fatalf("Targets(整理入库) = %#v, want none: organize 未被勾选", targets)
	}
	if targets := registry.Targets(Message{Type: TypeManual}); len(targets) != 0 {
		t.Fatalf("Targets(手动处理) = %#v, want none: manual 未被勾选", targets)
	}
}

func TestRegistryTargetsMatchesChineseAndEnglishSwitchesTogether(t *testing.T) {
	t.Parallel()

	registry := newRegistryWithFactory(discardRegistryLogger(), registryTestFactory(nil))
	if err := registry.Replace(mustRegistrySnapshot(t, `[
		{"name":"Mixed","type":"telegram","config":{"TELEGRAM_TOKEN":"token","TELEGRAM_CHAT_ID":"1"},"switchs":["Organize"," 订阅 "],"enabled":true}
	]`)); err != nil {
		t.Fatalf("Replace() error = %v", err)
	}
	for _, mtype := range []Type{TypeOrganize, TypeSubscribe} {
		if targets := registry.Targets(Message{Type: mtype}); len(targets) != 1 {
			t.Fatalf("Targets(%s) = %#v, want 1", mtype, targets)
		}
	}
	if targets := registry.Targets(Message{Type: TypeDownload}); len(targets) != 0 {
		t.Fatalf("Targets(资源下载) = %#v, want none", targets)
	}
}

func TestRegistrySendSelectsCurrentTelegramTargets(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		msg     Message
		want    []string
		wantErr bool
	}{
		{name: "类型只投递匹配配置", msg: Message{Type: TypeOrganize, Title: "title"}, want: []string{"Primary"}},
		{name: "来源只投递一个配置", msg: Message{Channel: ChannelTelegram, Source: "Secondary", Type: TypeDownload, Title: "title"}, want: []string{"Secondary"}},
		{name: "定向用户投递所有配置", msg: Message{Type: TypeMediaServer, UserID: "42", Title: "title"}, want: []string{"Primary", "Secondary"}},
		{name: "其它渠道没有目标", msg: Message{Channel: ChannelWebPush, Title: "title"}, wantErr: true},
		{name: "未知来源没有目标", msg: Message{Source: "Missing", Title: "title"}, wantErr: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			recorder := &registrySendRecorder{}
			registry := newRegistryWithFactory(discardRegistryLogger(), registryTestFactory(recorder))
			if err := registry.Replace(mustRegistrySnapshot(t, `[
				{"name":"Primary","type":"telegram","config":{"TELEGRAM_TOKEN":"primary-token","TELEGRAM_CHAT_ID":"1"},"switchs":["整理入库"],"enabled":true},
				{"name":"Secondary","type":"telegram","config":{"TELEGRAM_TOKEN":"secondary-token","TELEGRAM_CHAT_ID":"2"},"switchs":["资源下载"],"enabled":true}
			]`)); err != nil {
				t.Fatalf("Replace() error = %v", err)
			}

			err := registry.Send(t.Context(), tt.msg)
			if (err != nil) != tt.wantErr {
				t.Fatalf("Send() error = %v, wantErr %v", err, tt.wantErr)
			}
			if !reflect.DeepEqual(recorder.Calls(), tt.want) {
				t.Fatalf("Send() calls = %#v, want %#v", recorder.Calls(), tt.want)
			}
		})
	}
}

func TestRegistrySendPersistedTargetIgnoresReloadedSwitchs(t *testing.T) {
	t.Parallel()

	recorder := &registrySendRecorder{}
	registry := newRegistryWithFactory(discardRegistryLogger(), registryTestFactory(recorder))
	if err := registry.Replace(mustRegistrySnapshot(t, `[
		{"name":"Primary","type":"telegram","config":{"TELEGRAM_TOKEN":"primary-token","TELEGRAM_CHAT_ID":"1"},"switchs":["资源下载"],"enabled":true}
	]`)); err != nil {
		t.Fatalf("Replace() error = %v", err)
	}

	err := registry.Send(t.Context(), Message{
		Channel: ChannelTelegram,
		Source:  "Primary",
		Type:    TypeOrganize,
		Title:   "title",
	})
	if err != nil {
		t.Fatalf("Send() error = %v", err)
	}
	if got := recorder.Calls(); !reflect.DeepEqual(got, []string{"Primary"}) {
		t.Fatalf("Send() calls = %#v, want persisted target", got)
	}
}

func TestRegistryReplaceFailurePreservesPublishedSnapshotAndSecrets(t *testing.T) {
	t.Parallel()

	const (
		secretToken = "replace-secret-token"
		secretURL   = "https://replace-secret.example/api"
	)
	var logs bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&logs, nil))
	recorder := &registrySendRecorder{}
	factory := func(_ *slog.Logger, config notificationconfig.Telegram) (ChannelSender, error) {
		if config.BotToken == secretToken {
			return nil, errors.New(secretToken + " " + secretURL)
		}
		return &registryTestChannel{source: config.Name, recorder: recorder}, nil
	}
	registry := newRegistryWithFactory(logger, factory)
	if err := registry.Replace(mustRegistrySnapshot(t, `[
		{"name":"Old","type":"telegram","config":{"TELEGRAM_TOKEN":"old-token","TELEGRAM_CHAT_ID":"1"},"switchs":["整理入库"],"enabled":true}
	]`)); err != nil {
		t.Fatalf("initial Replace() error = %v", err)
	}

	failed := notificationconfig.Snapshot{Telegrams: []notificationconfig.Telegram{
		{Name: "NewReady", BotToken: "new-token", ChatID: "2", Switchs: []string{string(TypeOrganize)}},
		{Name: "NewBroken", BotToken: secretToken, ChatID: "3", APIURL: secretURL, Switchs: []string{string(TypeOrganize)}},
	}}
	err := registry.Replace(failed)
	if err == nil {
		t.Fatal("failed Replace() error = nil")
	}
	for _, secret := range []string{secretToken, secretURL} {
		if strings.Contains(err.Error(), secret) || strings.Contains(logs.String(), secret) {
			t.Fatalf("Replace() 泄漏秘密 %q: error=%v logs=%s", secret, err, logs.String())
		}
	}
	if got := registry.Targets(Message{Type: TypeOrganize}); !reflect.DeepEqual(got, []Target{{Channel: ChannelTelegram, Source: "Old"}}) {
		t.Fatalf("失败替换后 Targets() = %#v, want old snapshot", got)
	}
	if err := registry.Send(t.Context(), Message{Type: TypeOrganize, Title: "title"}); err != nil {
		t.Fatalf("失败替换后 Send() error = %v", err)
	}
	if got := recorder.Calls(); !reflect.DeepEqual(got, []string{"Old"}) {
		t.Fatalf("失败替换后 Send() calls = %#v, want Old", got)
	}
}

func TestRegistrySendFailureIsSanitizedAndDoesNotBlockOtherTargets(t *testing.T) {
	t.Parallel()

	const (
		secretToken = "send-secret-token"
		secretURL   = "https://send-secret.example/api"
	)
	var logs bytes.Buffer
	recorder := &registrySendRecorder{failures: map[string]error{
		"Primary": errors.New(secretToken + " " + secretURL),
	}}
	registry := newRegistryWithFactory(
		slog.New(slog.NewTextHandler(&logs, nil)),
		registryTestFactory(recorder),
	)
	if err := registry.Replace(mustRegistrySnapshot(t, `[
		{"name":"Primary","type":"telegram","config":{"TELEGRAM_TOKEN":"primary-token","TELEGRAM_CHAT_ID":"1"},"switchs":["整理入库"],"enabled":true},
		{"name":"Secondary","type":"telegram","config":{"TELEGRAM_TOKEN":"secondary-token","TELEGRAM_CHAT_ID":"2"},"switchs":["整理入库"],"enabled":true}
	]`)); err != nil {
		t.Fatalf("Replace() error = %v", err)
	}

	err := registry.Send(t.Context(), Message{Type: TypeOrganize, Title: "title"})
	if err == nil {
		t.Fatal("Send() error = nil")
	}
	for _, secret := range []string{secretToken, secretURL} {
		if strings.Contains(err.Error(), secret) || strings.Contains(logs.String(), secret) {
			t.Fatalf("Send() 泄漏秘密 %q: error=%v logs=%s", secret, err, logs.String())
		}
	}
	if got := recorder.Calls(); !reflect.DeepEqual(got, []string{"Primary", "Secondary"}) {
		t.Fatalf("Send() calls = %#v, want both targets", got)
	}
}

func TestRegistryNilAndEmptyStateAreSafe(t *testing.T) {
	t.Parallel()

	var nilRegistry *Registry
	if targets := nilRegistry.Targets(Message{}); len(targets) != 0 {
		t.Fatalf("nil Registry Targets() = %#v", targets)
	}
	if err := nilRegistry.Replace(notificationconfig.Snapshot{}); err == nil {
		t.Fatal("nil Registry Replace() error = nil")
	}
	if err := nilRegistry.Send(t.Context(), Message{Title: "title"}); err == nil {
		t.Fatal("nil Registry Send() error = nil")
	}

	registry := NewRegistry(nil)
	if targets := registry.Targets(Message{}); len(targets) != 0 {
		t.Fatalf("empty Registry Targets() = %#v", targets)
	}
	if err := registry.Send(nil, Message{Title: "title"}); err == nil {
		t.Fatal("nil context Send() error = nil")
	}
	if err := registry.Send(t.Context(), Message{Title: "title"}); err == nil {
		t.Fatal("empty Registry Send() error = nil")
	}
}

func TestRegistryConcurrentReplaceTargetsAndSendObserveWholeSnapshots(t *testing.T) {
	registry := newRegistryWithFactory(discardRegistryLogger(), func(_ *slog.Logger, config notificationconfig.Telegram) (ChannelSender, error) {
		return &registryTestChannel{source: config.Name}, nil
	})
	snapshotA := mustRegistrySnapshot(t, `[
		{"name":"A1","type":"telegram","config":{"TELEGRAM_TOKEN":"a1-token","TELEGRAM_CHAT_ID":"1"},"switchs":["整理入库"],"enabled":true},
		{"name":"A2","type":"telegram","config":{"TELEGRAM_TOKEN":"a2-token","TELEGRAM_CHAT_ID":"2"},"switchs":["整理入库"],"enabled":true}
	]`)
	snapshotB := mustRegistrySnapshot(t, `[
		{"name":"B1","type":"telegram","config":{"TELEGRAM_TOKEN":"b1-token","TELEGRAM_CHAT_ID":"1"},"switchs":["整理入库"],"enabled":true},
		{"name":"B2","type":"telegram","config":{"TELEGRAM_TOKEN":"b2-token","TELEGRAM_CHAT_ID":"2"},"switchs":["整理入库"],"enabled":true},
		{"name":"B3","type":"telegram","config":{"TELEGRAM_TOKEN":"b3-token","TELEGRAM_CHAT_ID":"3"},"switchs":["整理入库"],"enabled":true}
	]`)
	if err := registry.Replace(snapshotA); err != nil {
		t.Fatalf("initial Replace() error = %v", err)
	}

	const iterations = 300
	start := make(chan struct{})
	errCh := make(chan error, 8)
	var wait sync.WaitGroup
	wait.Add(1)
	go func() {
		defer wait.Done()
		<-start
		for index := range iterations {
			next := snapshotA
			if index%2 == 0 {
				next = snapshotB
			}
			if err := registry.Replace(next); err != nil {
				errCh <- err
				return
			}
		}
	}()
	for range 6 {
		wait.Add(1)
		go func() {
			defer wait.Done()
			<-start
			for range iterations {
				targets := registry.Targets(Message{Type: TypeMediaServer, UserID: "42"})
				names := registryTargetNames(targets)
				if !reflect.DeepEqual(names, []string{"A1", "A2"}) &&
					!reflect.DeepEqual(names, []string{"B1", "B2", "B3"}) {
					errCh <- errors.New("Targets observed a partial snapshot")
					return
				}
				if err := registry.Send(context.Background(), Message{Type: TypeMediaServer, UserID: "42", Title: "title"}); err != nil {
					errCh <- err
					return
				}
			}
		}()
	}
	close(start)
	wait.Wait()
	close(errCh)
	for err := range errCh {
		t.Fatalf("concurrent registry operation: %v", err)
	}
}

type registrySendRecorder struct {
	mu       sync.Mutex
	calls    []string
	failures map[string]error
}

func (r *registrySendRecorder) Record(source string) error {
	if r == nil {
		return nil
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.calls = append(r.calls, source)
	return r.failures[source]
}

func (r *registrySendRecorder) Calls() []string {
	if r == nil {
		return nil
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]string(nil), r.calls...)
}

type registryTestChannel struct {
	source   string
	recorder *registrySendRecorder
}

func (c *registryTestChannel) Name() Channel {
	return ChannelTelegram
}

func (c *registryTestChannel) Send(_ context.Context, _ Message) error {
	return c.recorder.Record(c.source)
}

func registryTestFactory(recorder *registrySendRecorder) telegramSenderFactory {
	return func(_ *slog.Logger, config notificationconfig.Telegram) (ChannelSender, error) {
		return &registryTestChannel{source: config.Name, recorder: recorder}, nil
	}
}

func mustRegistrySnapshot(t *testing.T, value string) notificationconfig.Snapshot {
	t.Helper()
	snapshot, err := notificationconfig.Decode(json.RawMessage(value))
	if err != nil {
		t.Fatalf("notificationconfig.Decode() error = %v", err)
	}
	return snapshot
}

func discardRegistryLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

func registryTargetNames(targets []Target) []string {
	names := make([]string, len(targets))
	for index := range targets {
		names[index] = targets[index].Source
	}
	return names
}
