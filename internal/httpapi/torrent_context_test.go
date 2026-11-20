package httpapi

import (
	"testing"
	"time"
)

// TestFreedateDiff 覆盖免费剩余时间的 V2 文案：零值/过期/不足一分钟为空，
// 分钟、小时、天数三档格式，以及 nil location 退回 UTC。
func TestFreedateDiff(t *testing.T) {
	// 多留 30 秒缓冲，避免 time.Now() 采样差异把分钟截断成上一档。
	const buffer = 30 * time.Second
	value := time.Now().Add(30*time.Minute + buffer)
	if got := freedateDiff(value, time.UTC); got != "30分钟" {
		t.Fatalf("30 分钟 = %q", got)
	}
	value = time.Now().Add(2*time.Hour + 15*time.Minute + buffer)
	if got := freedateDiff(value, time.UTC); got != "2小时15分钟" {
		t.Fatalf("2 小时 15 分钟 = %q", got)
	}
	value = time.Now().Add(25*time.Hour + 5*time.Minute + buffer)
	if got := freedateDiff(value, time.UTC); got != "1天1小时5分钟" {
		t.Fatalf("1 天 1 小时 5 分钟 = %q", got)
	}

	if got := freedateDiff(time.Time{}, time.UTC); got != "" {
		t.Fatalf("零值 = %q, want 空", got)
	}
	if got := freedateDiff(time.Now().Add(-time.Hour), time.UTC); got != "" {
		t.Fatalf("已过期 = %q, want 空", got)
	}
	if got := freedateDiff(time.Now().Add(30*time.Second), time.UTC); got != "" {
		t.Fatalf("不足一分钟 = %q, want 空", got)
	}

	// nil location 退回 UTC，未来时间照样给出文案。
	future := time.Now().UTC().Add(45*time.Minute + buffer)
	if got := freedateDiff(future, nil); got != "45分钟" {
		t.Fatalf("nil location = %q, want 45分钟", got)
	}

	// 非 UTC 时区同样按当地时钟计算剩余时长。
	zone := time.FixedZone("UTC+8", 8*3600)
	if got := freedateDiff(future.In(zone), zone); got != "45分钟" {
		t.Fatalf("固定时区 = %q, want 45分钟", got)
	}
}
