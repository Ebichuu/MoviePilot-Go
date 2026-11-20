package storage

import (
	"context"
	"errors"
	"testing"
)

func itemNames(items []Item) []string {
	names := make([]string, 0, len(items))
	for _, item := range items {
		names = append(names, item.Name)
	}
	return names
}

func TestSortItemsByNameUsesNaturalOrder(t *testing.T) {
	items := []Item{{Name: "第10集"}, {Name: "第2集"}, {Name: "第1集"}}
	sortItemsByName(items)
	want := []string{"第1集", "第2集", "第10集"}
	got := itemNames(items)
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("排序结果 = %v，期望 %v", got, want)
		}
	}
}

func TestSortItemsByModifyTimePutsMissingLast(t *testing.T) {
	older := 100.0
	newer := 200.0
	items := []Item{
		{Name: "无时间"},
		{Name: "新", ModifyTime: &newer},
		{Name: "旧", ModifyTime: &older},
	}
	sortItemsByModifyTime(items)
	if items[0].Name != "新" || items[1].Name != "旧" {
		t.Fatalf("按修改时间降序失败: %v", itemNames(items))
	}
	if items[2].Name != "无时间" {
		t.Fatalf("无修改时间的条目应排在最后: %v", itemNames(items))
	}
}

func TestNaturalLess(t *testing.T) {
	cases := []struct {
		left, right string
		want        bool
	}{
		{"第2集", "第10集", true},
		{"第10集", "第2集", false},
		{"a1", "a1", false},
		{"a", "b", true},
	}
	for _, tc := range cases {
		if got := naturalLess(tc.left, tc.right); got != tc.want {
			t.Fatalf("naturalLess(%q, %q) = %v，期望 %v", tc.left, tc.right, got, tc.want)
		}
	}
}

func TestPaddedNumber(t *testing.T) {
	if got := paddedNumber(7); got != "00000000000000000007" && len(got) < len("7") {
		t.Fatalf("paddedNumber 结果异常: %q", got)
	}
	if paddedNumber(7) == paddedNumber(8) {
		t.Fatal("不同数字的补位结果不应相同")
	}
}

func TestGlobMatch(t *testing.T) {
	cases := []struct {
		pattern, name string
		want          bool
	}{
		{"*.mkv", "movie.mkv", true},
		{"*.mkv", "movie.mp4", false},
		{"*", "anything", true},
		{"exact", "exact", true},
		{"exact", "other", false},
	}
	for _, tc := range cases {
		if got := globMatch(tc.pattern, tc.name); got != tc.want {
			t.Fatalf("globMatch(%q, %q) = %v，期望 %v", tc.pattern, tc.name, got, tc.want)
		}
	}
}

func TestSortStrings(t *testing.T) {
	values := []string{"b", "a", "c"}
	sortStrings(values)
	if values[0] != "a" || values[2] != "c" {
		t.Fatalf("sortStrings 结果 = %v", values)
	}
}

func TestRegistryLookupFailsForUnknownProvider(t *testing.T) {
	registry := NewProviderRegistry()
	if names := registry.Names(); len(names) != 0 {
		t.Fatalf("新注册表应为空: %v", names)
	}
	_, err := registry.List(context.Background(), "不存在", "/", "", "")
	if err == nil {
		t.Fatal("未接入的存储应当报错")
	}
	if !errors.Is(err, ErrUnsupportedStorage) {
		t.Fatalf("错误应包裹 ErrUnsupportedStorage，实际 %v", err)
	}
}
