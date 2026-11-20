package app

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/moviepilot-go/moviepilot-go/internal/cache"
	"github.com/moviepilot-go/moviepilot-go/internal/config"
	"github.com/moviepilot-go/moviepilot-go/internal/doctor"
	"github.com/moviepilot-go/moviepilot-go/internal/downloadflow"
	"github.com/moviepilot-go/moviepilot-go/internal/indexer"
	"github.com/moviepilot-go/moviepilot-go/internal/metadata"
	"github.com/moviepilot-go/moviepilot-go/internal/notification"
	"github.com/moviepilot-go/moviepilot-go/internal/notificationconfig"
)

func discardDoctorLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// TestNewDoctorRunnerWiresEveryComponent 覆盖「依赖非空时逐个挂上探针」的四条分支，
// 并确认探针能直接调用。
func TestNewDoctorRunnerWiresEveryComponent(t *testing.T) {
	redis := &cache.Redis{}
	registry := notification.NewRegistry(discardDoctorLogger())
	reconcile := &downloadflow.ReconciliationHandler{}
	discover := metadata.NewDiscover(nil, nil, nil, nil)

	runner := newDoctorRunner(
		config.Config{Environment: "test"}, nil, redis, registry, reconcile, discover,
	)
	if runner.Probes.Redis == nil {
		t.Fatal("Redis 探针未挂上")
	}
	if runner.Probes.Database != nil {
		t.Fatal("没有数据库时不该挂 Database 探针")
	}
	if runner.Probes.Notification == nil || runner.Probes.Reconcile == nil || runner.Probes.Metadata == nil {
		t.Fatalf("探针缺失: %+v", runner.Probes)
	}

	// 通知探针：注册表为空时必须给出「尚未配置」的原因。
	if probe := runner.Probes.Notification(); probe.Configs != 0 || probe.Reason != "尚未配置任何通知渠道" {
		t.Fatalf("空注册表通知探针 = %+v", probe)
	}
	// 对账探针：没有跑过任何一轮时 Ran 为 false。
	if probe := runner.Probes.Reconcile(); probe.Ran {
		t.Fatalf("未运行的对账探针 = %+v", probe)
	}
	// 元数据探针：四个来源都未配置，全部记为跳过。
	probes := runner.Probes.Metadata(t.Context())
	if len(probes) != 4 {
		t.Fatalf("元数据探针数量 = %d", len(probes))
	}
	for _, probe := range probes {
		if probe.Error == "" {
			t.Fatalf("未配置的来源应带错误说明: %+v", probe)
		}
	}
}

// TestNotificationDoctorProbeDeliverable 覆盖「有渠道且能投递」与
// 「有渠道但一个业务类型都没勾选」两种统计口径。
func TestNotificationDoctorProbeDeliverable(t *testing.T) {
	deliverable := notification.NewRegistry(discardDoctorLogger())
	if err := deliverable.Replace(notificationconfig.Snapshot{
		Telegrams: []notificationconfig.Telegram{{
			Name: "Bot", BotToken: "1:token", ChatID: "1", Switchs: []string{"download"},
		}},
	}); err != nil {
		t.Fatalf("Replace() error = %v", err)
	}
	probe := notificationDoctorProbe(deliverable)
	if probe.Configs != 1 || probe.Deliverable != 1 || probe.Reason != "" {
		t.Fatalf("可投递通知探针 = %+v", probe)
	}

	undeliverable := notification.NewRegistry(discardDoctorLogger())
	if err := undeliverable.Replace(notificationconfig.Snapshot{
		Telegrams: []notificationconfig.Telegram{{
			Name: "Bot", BotToken: "1:token", ChatID: "1",
		}},
	}); err != nil {
		t.Fatalf("Replace() error = %v", err)
	}
	probe = notificationDoctorProbe(undeliverable)
	if probe.Configs != 1 || probe.Deliverable != 0 || !strings.Contains(probe.Reason, "switchs") {
		t.Fatalf("不可投递通知探针 = %+v", probe)
	}
}

// TestStartTelegramBotsMapsRuntimeChannels 覆盖入站机器人启动时的通道映射闭包：
// 运行态里的 Telegram 通道必须被翻译成轮询配置并真的发出 getUpdates 请求。
func TestStartTelegramBotsMapsRuntimeChannels(t *testing.T) {
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"ok":true,"result":[]}`)
	}))
	t.Cleanup(server.Close)

	runtime := &notificationRuntime{snapshot: notificationconfig.Snapshot{
		Telegrams: []notificationconfig.Telegram{{
			Name: "Inbound", BotToken: "123:abc", ChatID: "42", APIURL: server.URL,
		}},
	}}
	coordinator, err := indexer.NewCoordinator(1)
	if err != nil {
		t.Fatalf("NewCoordinator() error = %v", err)
	}
	t.Cleanup(coordinator.Close)

	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	manager := startTelegramBots(ctx, discardDoctorLogger(), runtime, coordinator, indexer.NewRegistry(nil), nil)
	if manager == nil {
		t.Fatal("依赖齐全时必须返回管理器")
	}

	deadline := time.Now().Add(5 * time.Second)
	for calls.Load() == 0 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if calls.Load() == 0 {
		t.Fatal("通道没有被翻译成轮询配置，未发出 getUpdates 请求")
	}
	// 停掉管理器，避免 goroutine 泄漏到后续用例。
	cancel()
}

// TestMetadataDoctorProbesReportStatuses 覆盖「有结果 → ok」与「空结果 → warning」
// 两档，其余未配置的来源记为跳过。
func TestMetadataDoctorProbesReportStatuses(t *testing.T) {
	cases := map[string]struct {
		body   string
		status doctor.Status
		items  int
	}{
		"有结果": {body: `{"results":[{"id":550,"media_type":"movie","title":"搏击俱乐部","release_date":"1999-10-15"}]}`,
			status: doctor.StatusOK, items: 1},
		"空结果": {body: `{"results":[]}`, status: doctor.StatusWarning},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				_, _ = io.WriteString(w, tc.body)
			}))
			t.Cleanup(server.Close)

			client, err := metadata.NewHTTPClient(2, 5*time.Second, 1<<20)
			if err != nil {
				t.Fatalf("NewHTTPClient() error = %v", err)
			}
			t.Cleanup(client.CloseIdleConnections)
			discover := metadata.NewDiscover(metadata.NewTMDB(client, server.URL, "token"), nil, nil, nil)

			probes := metadataDoctorProbes(t.Context(), discover)
			if len(probes) != 4 {
				t.Fatalf("探针数量 = %d", len(probes))
			}
			bySource := make(map[string]doctor.SourceProbe, len(probes))
			for _, probe := range probes {
				bySource[probe.Source] = probe
			}
			if probe := bySource["themoviedb"]; probe.Status != tc.status || probe.Items != tc.items {
				t.Fatalf("TMDB 探针 = %+v，期望 %s/%d", probe, tc.status, tc.items)
			}
			if probe := bySource["bangumi"]; probe.Status != doctor.StatusSkipped {
				t.Fatalf("未配置的 Bangumi 应跳过: %+v", probe)
			}
		})
	}
}

// TestStartTelegramBotsGuards 覆盖缺少上下文/日志/运行态时直接不启动。
func TestStartTelegramBotsGuards(t *testing.T) {
	if manager := startTelegramBots(nil, discardDoctorLogger(), &notificationRuntime{}, nil, nil, nil); manager != nil {
		t.Fatal("缺上下文时不该启动")
	}
	if manager := startTelegramBots(context.Background(), nil, &notificationRuntime{}, nil, nil, nil); manager != nil {
		t.Fatal("缺日志时不该启动")
	}
	if manager := startTelegramBots(context.Background(), discardDoctorLogger(), nil, nil, nil, nil); manager != nil {
		t.Fatal("缺运行态时不该启动")
	}
}
