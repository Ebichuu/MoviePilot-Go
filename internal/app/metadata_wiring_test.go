package app

import (
	"testing"
	"time"

	"github.com/moviepilot-go/moviepilot-go/internal/config"
	"github.com/moviepilot-go/moviepilot-go/internal/metadata"
)

func metadataWiringClient(t *testing.T) *metadata.HTTPClient {
	t.Helper()
	client, err := metadata.NewHTTPClient(4, 5*time.Second, 1<<20)
	if err != nil {
		t.Fatalf("创建元数据 HTTP 客户端失败: %v", err)
	}
	t.Cleanup(client.CloseIdleConnections)
	return client
}

func metadataWiringConfig() config.Config {
	return config.Config{
		Environment:        "test",
		TMDBBaseURL:        "https://tmdb.example",
		DoubanBaseURL:      "https://douban.example",
		BangumiBaseURL:     "https://bangumi.example",
		AniListEndpoint:    "https://anilist.example",
		MetadataCacheTTL:   time.Minute,
		MetadataTimeout:    5 * time.Second,
		MetadataMaxResults: 10,
	}
}

func TestNewMetadataCoordinatorWiring(t *testing.T) {
	client := metadataWiringClient(t)

	// 默认配置：只有不需要凭据的 bangumi 与 anilist。
	coordinator, err := newMetadataCoordinator(metadataWiringConfig(), client, nil)
	if err != nil {
		t.Fatalf("构造协调器失败: %v", err)
	}
	for _, absent := range []string{"themoviedb", "douban"} {
		if _, ok := coordinator.Provider(absent); ok {
			t.Fatalf("未配置凭据时不应注册 %s", absent)
		}
	}
	for _, present := range []string{"bangumi", "anilist"} {
		if _, ok := coordinator.Provider(present); !ok {
			t.Fatalf("应注册 %s", present)
		}
	}

	// 只给一半豆瓣凭据：不能注册（缺少 secret 会 401，等于没配）。
	halfDouban := metadataWiringConfig()
	halfDouban.DoubanAPIKey = "key-only"
	coordinator, err = newMetadataCoordinator(halfDouban, client, nil)
	if err != nil {
		t.Fatalf("构造协调器失败: %v", err)
	}
	if _, ok := coordinator.Provider("douban"); ok {
		t.Fatal("缺少 secret 时不应注册豆瓣")
	}

	// 凭据齐全时注册 TMDB 与豆瓣。
	full := metadataWiringConfig()
	full.TMDBToken = "tmdb-token"
	full.DoubanAPIKey, full.DoubanSecretKey = "key", "secret"
	coordinator, err = newMetadataCoordinator(full, client, nil)
	if err != nil {
		t.Fatalf("构造协调器失败: %v", err)
	}
	for _, present := range []string{"themoviedb", "douban", "bangumi", "anilist"} {
		if _, ok := coordinator.Provider(present); !ok {
			t.Fatalf("凭据齐全时应注册 %s", present)
		}
	}
}

func TestNewMetadataCoordinatorSurfacesInvalidConfig(t *testing.T) {
	client := metadataWiringClient(t)
	// 配置缺失必须显式报错，而不是悄悄用零值（零超时会让每个请求立刻失败）。
	broken := metadataWiringConfig()
	broken.MetadataTimeout = 0
	if coordinator, err := newMetadataCoordinator(broken, client, nil); err == nil || coordinator != nil {
		t.Fatalf("缺少超时配置应报错，实际 %v %v", coordinator, err)
	}

	broken = metadataWiringConfig()
	broken.MetadataMaxResults = 0
	if _, err := newMetadataCoordinator(broken, client, nil); err == nil {
		t.Fatal("缺少结果上限应报错")
	}
}

func TestNewMediaDiscoverHandlesMissingProviders(t *testing.T) {
	client := metadataWiringClient(t)
	// 什么都没配置：不能 panic，发现页应回退成空列表。
	if discover := newMediaDiscover(config.Config{}, client); discover == nil {
		t.Fatal("空配置也应返回可用的发现门面")
	}
	if discover := newMediaDiscover(metadataWiringConfig(), client); discover == nil {
		t.Fatal("有配置时应返回发现门面")
	}
	full := metadataWiringConfig()
	full.TMDBToken = "tmdb-token"
	full.DoubanAPIKey, full.DoubanSecretKey = "key", "secret"
	if discover := newMediaDiscover(full, client); discover == nil {
		t.Fatal("凭据齐全时应返回发现门面")
	}
}
