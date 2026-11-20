package database

import (
	"errors"
	"strings"
	"testing"

	"github.com/moviepilot-go/moviepilot-go/internal/downloader"
)

// 本文件覆盖下载意图受理的纯函数分支：
// 非法初始状态、哈希非法或不一致、恢复目标 JSON 非法，
// 以及既非电影也非电视剧的媒体类型不可读。

func TestAcceptedDownloadIntentBranches(t *testing.T) {
	validHash := strings.Repeat("a", 40)

	if _, err := acceptedDownloadIntent(downloader.Intent{State: downloader.IntentCompleted}, validHash); !errors.Is(err, ErrDownloadIntentStateConflict) {
		t.Fatalf("非法初始状态 error = %v", err)
	}

	if _, err := acceptedDownloadIntent(downloader.Intent{
		State: downloader.IntentSubmitting, TorrentHash: "zz-not-a-hash",
	}, validHash); !errors.Is(err, ErrDownloadIntentPersistence) {
		t.Fatalf("已存哈希非法 error = %v", err)
	}

	if _, err := acceptedDownloadIntent(downloader.Intent{
		State: downloader.IntentSubmitting, TorrentHash: validHash,
	}, strings.Repeat("b", 40)); !errors.Is(err, ErrDownloadIntentStateConflict) {
		t.Fatalf("哈希不一致 error = %v", err)
	}

	if _, err := acceptedDownloadIntent(downloader.Intent{State: downloader.IntentSubmitting}, "zz-not-a-hash"); !errors.Is(err, ErrDownloadIntentPersistence) {
		t.Fatalf("新哈希非法 error = %v", err)
	}

	accepted, err := acceptedDownloadIntent(downloader.Intent{State: downloader.IntentSubmitting}, validHash)
	if err != nil {
		t.Fatalf("合法受理 error = %v", err)
	}
	if accepted.State != downloader.IntentDownloading || !strings.EqualFold(accepted.TorrentHash, validHash) {
		t.Fatalf("受理结果 = %+v", accepted)
	}
}

func TestDecodeDownloadIntentTVRecoveredTargetRejectsInvalidJSON(t *testing.T) {
	if _, err := decodeDownloadIntentTVRecoveredTarget([]byte("{")); err == nil {
		t.Fatal("非法 JSON 应返回错误")
	}
}

func TestDownloadIntentAcceptanceFactsReadableRejectsNonMediaType(t *testing.T) {
	parent := downloadIntentAcceptanceSubscription{mediaType: "anime", config: map[string]any{}}
	if downloadIntentAcceptanceFactsReadable(parent, downloadIntentTVFacts{}, nil) {
		t.Fatal("既非电影也非电视剧时应判为不可读")
	}
}
