package cache

import (
	"strings"
	"testing"
	"time"
)

func TestNamespacedAddsVersionedPrefix(t *testing.T) {
	got := namespaced("key")
	if !strings.HasPrefix(got, "mp:v1:") {
		t.Fatalf("缺少版本化前缀: %q", got)
	}
	if !strings.HasSuffix(got, "key") {
		t.Fatalf("命名空间后丢了原始 key: %q", got)
	}
	// namespaced 是纯前缀函数、不幂等：调用方只对原始 key 调一次。
	if twice := namespaced(got); twice != "mp:v1:"+got {
		t.Fatalf("重复调用应叠加前缀: %q", twice)
	}
}

func TestJitterStaysNearInput(t *testing.T) {
	ttl := 10 * time.Minute
	for i := 0; i < 20; i++ {
		got := jitter(ttl)
		if got <= 0 {
			t.Fatalf("抖动后 TTL 必须为正，实际 %v", got)
		}
		// 抖动幅度必须远小于 TTL 本身，否则缓存会提前大量失效。
		if got < ttl/2 || got > ttl*3/2 {
			t.Fatalf("抖动幅度过大: %v (基准 %v)", got, ttl)
		}
	}
}

func TestJitterZeroStaysZero(t *testing.T) {
	if got := jitter(0); got != 0 {
		t.Fatalf("零 TTL 不应被抖成正数: %v", got)
	}
}
