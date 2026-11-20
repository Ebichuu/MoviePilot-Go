package jobs

import (
	"context"
	"testing"
	"time"

	"github.com/riverqueue/river"
)

// 本文件用「同名 worker 注册第二次」这一真实重复状态覆盖各注册函数里
// AddWorkerSafely 的失败返回：river 以 job kind 为唯一键，重复注册必然报错。

type duplicateRegistrationHandler struct{}

func (duplicateRegistrationHandler) HandleDownload(context.Context, DownloadArgs) (DownloadResult, error) {
	return DownloadResult{}, nil
}

func (duplicateRegistrationHandler) HandleMediaIndex(context.Context, MediaIndexArgs) error {
	return nil
}

func (duplicateRegistrationHandler) HandleMediaIndexSweep(context.Context) error { return nil }

func (duplicateRegistrationHandler) HandleMediaServerSync(context.Context, string) error { return nil }

func (duplicateRegistrationHandler) HandleRSSSweep(
	context.Context, time.Time, RSSSweepState,
) (RSSSweepState, error) {
	return RSSSweepState{}, nil
}

func (duplicateRegistrationHandler) HandleRSSPoll(context.Context, RSSPollArgs) error { return nil }

func (duplicateRegistrationHandler) HandleRSSMatch(context.Context, RSSMatchArgs) error { return nil }

func (duplicateRegistrationHandler) HandleOrganization(context.Context, OrganizationArgs) error {
	return nil
}

func (duplicateRegistrationHandler) HandleScrape(context.Context, ScrapeArgs) error { return nil }

func (duplicateRegistrationHandler) HandleNotification(context.Context, NotificationArgs) error {
	return nil
}

func TestRegisterWorkersRejectDuplicateKinds(t *testing.T) {
	handler := duplicateRegistrationHandler{}
	workers := river.NewWorkers()
	ctx := context.Background()

	// 下载 worker：配置齐备后注册两次，第二次失败。
	downloadConfig := ManagerConfig{DownloadHandler: handler, DownloadMaxWorkers: 1}
	if count, err := registerDownloadWorker(workers, downloadConfig); err != nil || count != 1 {
		t.Fatalf("首次注册下载 worker = %d, %v", count, err)
	}
	if _, err := registerDownloadWorker(workers, downloadConfig); err == nil {
		t.Fatal("重复注册下载 worker 应报错")
	}

	mediaIndexConfig := ManagerConfig{MediaIndexHandler: handler, MediaIndexMaxWorkers: 1}
	if count, err := registerMediaIndexWorker(workers, mediaIndexConfig); err != nil || count != 1 {
		t.Fatalf("首次注册媒体入库 worker = %d, %v", count, err)
	}
	if _, err := registerMediaIndexWorker(workers, mediaIndexConfig); err == nil {
		t.Fatal("重复注册媒体入库 worker 应报错")
	}

	cases := []struct {
		name string
		add  func() (bool, error)
	}{
		{"媒体入库确认恢复", func() (bool, error) { return registerMediaIndexSweepWorker(workers, handler) }},
		{"媒体服务器条目同步", func() (bool, error) { return registerMediaServerSyncWorker(workers, handler) }},
		{"RSS 增量扫描", func() (bool, error) { return registerRSSSweepWorker(workers, handler) }},
		{"RSS 站点轮询", func() (bool, error) { return registerRSSPollWorker(workers, handler) }},
		{"RSS 全局匹配", func() (bool, error) { return registerRSSMatchWorker(workers, handler) }},
	}
	for _, testCase := range cases {
		registered, err := testCase.add()
		if err != nil || !registered {
			t.Fatalf("首次注册 %s worker = %v, %v", testCase.name, registered, err)
		}
		if _, err := testCase.add(); err == nil {
			t.Fatalf("重复注册 %s worker 应报错", testCase.name)
		}
	}
	_ = ctx
}
