package httpapi

import (
	"net/http/httptest"
	"strings"
	"testing"
)

func TestSuccessOnlyWritesSuccessEnvelope(t *testing.T) {
	recorder := httptest.NewRecorder()
	successOnly(recorder, httptest.NewRequest("GET", "/api/v1/torrent/cache", nil))
	if recorder.Code != 200 {
		t.Fatalf("状态码 = %d", recorder.Code)
	}
	if body := recorder.Body.String(); !strings.Contains(body, `"success":true`) {
		t.Fatalf("响应缺少 success 标记: %s", body)
	}
}

func TestEmptySuccessWritesNullData(t *testing.T) {
	recorder := httptest.NewRecorder()
	emptySuccess(recorder, httptest.NewRequest("POST", "/api/v1/system/message", nil))
	if recorder.Code != 200 {
		t.Fatalf("状态码 = %d", recorder.Code)
	}
	body := recorder.Body.String()
	// 注意：这里不是 `data:null`，而是空对象——`writeSuccess(w, nil)` 的既有行为，
	// 测试按实际形状锁定，避免将来误以为它返回 null。
	if !strings.Contains(body, `"success":true`) || !strings.Contains(body, `"data":{}`) {
		t.Fatalf("空成功响应应为 success + data:{} : %s", body)
	}
}

func TestSortedIntKeysIsDeterministic(t *testing.T) {
	values := map[string]int{"c": 3, "a": 1, "b": 2}
	first := sortedIntKeys(values)
	if len(first) != 3 || first[0] != "a" || first[1] != "b" || first[2] != "c" {
		t.Fatalf("键未按字典序排列: %v", first)
	}
	// 多次调用结果必须一致，避免日志/响应顺序抖动。
	for i := 0; i < 5; i++ {
		again := sortedIntKeys(values)
		for index := range first {
			if again[index] != first[index] {
				t.Fatalf("多次调用顺序不一致: %v vs %v", first, again)
			}
		}
	}
	if got := sortedIntKeys(nil); len(got) != 0 {
		t.Fatalf("空 map 应返回空切片，实际 %v", got)
	}
}

func TestNormalizeDownloadControlHash(t *testing.T) {
	sha1Hash := strings.Repeat("a", 40)
	sha256Hash := strings.Repeat("b", 64)

	// 归一方向是**大写**（qBittorrent 的 hash 惯例），不是小写；两种大小写输入都要能对上。
	if got, ok := normalizeDownloadControlHash("  " + sha1Hash + "  "); !ok || got != strings.ToUpper(sha1Hash) {
		t.Fatalf("40 位十六进制应通过、去空格并转大写: %q %v", got, ok)
	}
	if got, ok := normalizeDownloadControlHash(sha256Hash); !ok || got != strings.ToUpper(sha256Hash) {
		t.Fatalf("64 位十六进制应通过并转大写: %q %v", got, ok)
	}
	upper := strings.Repeat("A", 40)
	if got, ok := normalizeDownloadControlHash(upper); !ok || got != upper {
		t.Fatalf("大写输入应原样保留: %q %v", got, ok)
	}
	lowerNorm, _ := normalizeDownloadControlHash(sha1Hash)
	upperNorm, _ := normalizeDownloadControlHash(upper)
	if lowerNorm != upperNorm {
		t.Fatalf("同一 hash 的大小写两种写法应归一到同一结果: %q vs %q", lowerNorm, upperNorm)
	}

	invalid := []string{"", "abc", strings.Repeat("a", 39), strings.Repeat("a", 41), strings.Repeat("z", 40)}
	for _, value := range invalid {
		if got, ok := normalizeDownloadControlHash(value); ok {
			t.Fatalf("%q 不应通过，实际 %q", value, got)
		}
	}
}

func TestCaptureWriterCapturesStatusAndBody(t *testing.T) {
	writer := newCaptureWriter()
	if writer.status != 200 {
		t.Fatalf("默认状态码应为 200，实际 %d", writer.status)
	}
	writer.Header().Set("Content-Type", "application/json")
	writer.WriteHeader(418)
	if _, err := writer.Write([]byte("teapot")); err != nil {
		t.Fatalf("写入失败: %v", err)
	}
	if writer.status != 418 {
		t.Fatalf("状态码未捕获: %d", writer.status)
	}
	if writer.body.String() != "teapot" {
		t.Fatalf("响应体未捕获: %q", writer.body.String())
	}
	if writer.Header().Get("Content-Type") != "application/json" {
		t.Fatal("响应头未捕获")
	}
}
