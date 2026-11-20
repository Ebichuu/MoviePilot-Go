package metadata

import (
	"context"
	"testing"
)

// TestDiscoverSetTMDB 锁住「设置里补了 TMDB 密钥后，发现/推荐立刻可用」。
func TestDiscoverSetTMDB(t *testing.T) {
	discover := NewDiscover(nil, nil, nil, nil)
	if discover.TMDBAvailable() {
		t.Fatal("没有客户端时 TMDB 不应可用")
	}
	if _, err := discover.TMDBTrending(context.Background(), 1); err == nil {
		t.Fatal("未配置时应当报错，而不是返回空列表冒充成功")
	}

	discover.SetTMDB(NewTMDB(nil, "https://api.example", "token"))
	if !discover.TMDBAvailable() {
		t.Fatal("设置客户端后 TMDB 应可用")
	}

	// 密钥为空串等同于不可用（和 V2 一样：没密钥就没有数据）。
	discover.SetTMDB(NewTMDB(nil, "https://api.example", ""))
	if discover.TMDBAvailable() {
		t.Fatal("空密钥时 TMDB 不应可用")
	}

	discover.SetTMDB(nil)
	if discover.TMDBAvailable() {
		t.Fatal("清空客户端后 TMDB 不应可用")
	}
	if _, err := discover.TMDBDiscover(context.Background(), "movie", TMDBListQuery{}); err == nil {
		t.Fatal("清空后应当报错")
	}
}
