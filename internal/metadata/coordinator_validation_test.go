package metadata

import (
	"strings"
	"testing"
	"time"
)

func coordinatorOptions() Options {
	return Options{RequestTimeout: time.Second, CacheTTL: time.Minute, MaxResults: 10}
}

func TestNewCoordinatorValidation(t *testing.T) {
	valid := func() *stubProvider { return &stubProvider{name: "tmdb"} }

	tooMany := make([]Provider, maxProviders+1)
	for index := range tooMany {
		tooMany[index] = &stubProvider{name: "source"}
	}

	cases := []struct {
		name      string
		providers []Provider
		mutate    func(*Options)
		wantSubst string
	}{
		{"没有供应商", nil, nil, "至少需要一个"},
		{"供应商过多", tooMany, nil, "不能超过"},
		{"请求超时为零", []Provider{valid()}, func(o *Options) { o.RequestTimeout = 0 }, "请求超时"},
		{"缓存为零", []Provider{valid()}, func(o *Options) { o.CacheTTL = 0 }, "缓存 TTL"},
		{"结果下限", []Provider{valid()}, func(o *Options) { o.MaxResults = 0 }, "结果上限"},
		{"结果上限", []Provider{valid()}, func(o *Options) { o.MaxResults = 201 }, "结果上限"},
		{"流控负数", []Provider{valid()}, func(o *Options) { o.ProviderMinInterval = -time.Second }, "不能为负数"},
		{"退避负数", []Provider{valid()}, func(o *Options) { o.ProviderMaxBackoff = -time.Second }, "不能为负数"},
		{"空供应商", []Provider{nil, valid()}, nil, "不能为 nil"},
		{"空名称", []Provider{&stubProvider{name: "   "}}, nil, "名称不能为空"},
		{"名称重复", []Provider{valid(), valid()}, nil, "名称重复"},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			options := coordinatorOptions()
			if testCase.mutate != nil {
				testCase.mutate(&options)
			}
			if _, err := NewCoordinator(testCase.providers, options); err == nil {
				t.Fatalf("%s 应返回错误", testCase.name)
			} else if !strings.Contains(err.Error(), testCase.wantSubst) {
				t.Fatalf("%s error = %v，期望包含 %q", testCase.name, err, testCase.wantSubst)
			}
		})
	}

	// 默认退避 30 秒，来源名会归一化。
	coordinator, err := NewCoordinator(
		[]Provider{&stubProvider{name: " TMDB "}, &stubProvider{name: "Bangumi"}},
		coordinatorOptions(),
	)
	if err != nil {
		t.Fatalf("NewCoordinator() = %v", err)
	}
	if state, ok := coordinator.providers["themoviedb"]; !ok || state.maxBackoff != 30*time.Second {
		t.Fatalf("默认退避状态 = %#v", state)
	}
	if _, ok := coordinator.providers["bangumi"]; !ok {
		t.Fatal("来源名应小写归一化")
	}
	if coordinator.order["themoviedb"] != 0 || coordinator.order["bangumi"] != 1 {
		t.Fatalf("来源顺序 = %#v", coordinator.order)
	}
}

func TestDoubanWindow(t *testing.T) {
	cases := []struct {
		page, count int
		wantStart   int
		wantSize    int
	}{
		{1, 10, 0, 10},
		{2, 10, 10, 10},
		{0, 0, 0, 30},
		{3, 101, 60, 30},
		{-2, -1, 0, 30},
	}
	for _, testCase := range cases {
		start, size := doubanWindow(testCase.page, testCase.count)
		if start != testCase.wantStart || size != testCase.wantSize {
			t.Fatalf("doubanWindow(%d, %d) = (%d, %d)，期望 (%d, %d)",
				testCase.page, testCase.count, start, size, testCase.wantStart, testCase.wantSize)
		}
	}
}
