package database

import (
	"testing"
)

// 本文件覆盖意图确认链路里两个纯函数的拒绝分支：
// 电影缺省优先级合并对非法进度补丁的拒绝，以及电视剧目标解析对
// 非法质量标记的拒绝。

func TestAddDownloadIntentAcceptanceMoviePriorityRejects(t *testing.T) {
	priority := 2
	if _, err := addDownloadIntentAcceptanceMoviePriority("movie", []byte("{"), &priority); err == nil {
		t.Fatal("非法进度补丁应报错")
	}
	if _, err := addDownloadIntentAcceptanceMoviePriority("movie", []byte("null"), &priority); err == nil {
		t.Fatal("null 进度补丁应报错")
	}

	encoded, err := addDownloadIntentAcceptanceMoviePriority("movie", []byte(`{"lack_episode":1}`), &priority)
	if err != nil {
		t.Fatalf("合法补丁 error = %v", err)
	}
	if string(encoded) != `{"current_priority":2,"lack_episode":1}` {
		t.Fatalf("合并结果 = %s", encoded)
	}

	// 非电影或缺少记录优先级时原样返回。
	untouched, err := addDownloadIntentAcceptanceMoviePriority("tv", []byte(`{"lack_episode":1}`), &priority)
	if err != nil || string(untouched) != `{"lack_episode":1}` {
		t.Fatalf("电视剧 = %s, %v", untouched, err)
	}
}

func TestDownloadIntentAcceptanceTVTargetRejectsQualityFlag(t *testing.T) {
	if _, _, err := downloadIntentAcceptanceTVTarget(nil, map[string]any{"best_version": "abc"}); err == nil {
		t.Fatal("非法质量标记应报错")
	}
	if _, _, err := downloadIntentAcceptanceTVTarget(nil, map[string]any{"best_version_full": []any{1}}); err == nil {
		t.Fatal("非法完整质量标记应报错")
	}
}
