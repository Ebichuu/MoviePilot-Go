package app

import (
	"testing"

	"github.com/moviepilot-go/moviepilot-go/internal/mediaserver"
	"github.com/moviepilot-go/moviepilot-go/internal/mediaserver/emby"
)

// TestMediaServerPageConsistent 覆盖分页一致性校验：页大小上限、StartIndex
// 仅在服务端给出时校验、总数下限（fnOS 等分支返回 0 时放宽）。
func TestMediaServerPageConsistent(t *testing.T) {
	page := make([]emby.Item, 3)
	cases := []struct {
		name     string
		response emby.ItemsResponse
		start    int
		pageSize int
		want     bool
	}{
		{
			name:     "页大小恰好等于上限",
			response: emby.ItemsResponse{Items: page, StartIndex: 0, TotalRecordCount: 0},
			start:    0, pageSize: 3, want: true,
		},
		{
			name:     "页大小超过上限",
			response: emby.ItemsResponse{Items: make([]emby.Item, 4)},
			start:    0, pageSize: 3, want: false,
		},
		{
			name:     "StartIndex 与服务端一致",
			response: emby.ItemsResponse{Items: page, StartIndex: 6},
			start:    6, pageSize: 3, want: true,
		},
		{
			name:     "StartIndex 不一致",
			response: emby.ItemsResponse{Items: page, StartIndex: 3},
			start:    6, pageSize: 3, want: false,
		},
		{
			name:     "StartIndex 未回填按不校验处理",
			response: emby.ItemsResponse{Items: page, StartIndex: 0},
			start:    9, pageSize: 3, want: true,
		},
		{
			name:     "总数不足覆盖本页",
			response: emby.ItemsResponse{Items: page, TotalRecordCount: 5},
			start:    6, pageSize: 3, want: false,
		},
		{
			name:     "总数恰好覆盖本页",
			response: emby.ItemsResponse{Items: page, TotalRecordCount: 9},
			start:    6, pageSize: 3, want: true,
		},
		{
			name:     "总数为 0 时放宽",
			response: emby.ItemsResponse{Items: page, TotalRecordCount: 0},
			start:    6, pageSize: 3, want: true,
		},
		{
			name:     "空页始终一致",
			response: emby.ItemsResponse{},
			start:    0, pageSize: mediaServerItemPageSize, want: true,
		},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			if got := mediaServerPageConsistent(testCase.response, testCase.start, testCase.pageSize); got != testCase.want {
				t.Fatalf("mediaServerPageConsistent() = %v, want %v", got, testCase.want)
			}
		})
	}
}

// TestMediaServerPageDone 覆盖分页结束判定：有总数时按总数，无总数时按短页。
func TestMediaServerPageDone(t *testing.T) {
	fullPage := make([]emby.Item, 4)
	cases := []struct {
		name     string
		response emby.ItemsResponse
		next     int
		want     bool
	}{
		{"总数已读满", emby.ItemsResponse{Items: fullPage, TotalRecordCount: 8}, 8, true},
		{"总数未读满", emby.ItemsResponse{Items: fullPage, TotalRecordCount: 12}, 8, false},
		{"无总数短页即结束", emby.ItemsResponse{Items: make([]emby.Item, 3)}, 3, true},
		{"无总数满页继续", emby.ItemsResponse{Items: fullPage}, 4, false},
		{"无总数空页结束", emby.ItemsResponse{}, 0, true},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			if got := mediaServerPageDone(testCase.response, testCase.next, 4); got != testCase.want {
				t.Fatalf("mediaServerPageDone() = %v, want %v", got, testCase.want)
			}
		})
	}
}

// TestMediaServerItemClassification 覆盖条目类型与目录识别。
func TestMediaServerItemClassification(t *testing.T) {
	cases := []struct {
		itemType string
		wantType string
		wantOK   bool
	}{
		{"Series", mediaserver.ItemTypeTV, true},
		{"show", mediaserver.ItemTypeTV, true},
		{" Movie ", mediaserver.ItemTypeMovie, true},
		{"Episode", "", false},
		{"", "", false},
	}
	for _, testCase := range cases {
		gotType, gotOK := mediaServerItemType(testCase.itemType)
		if gotType != testCase.wantType || gotOK != testCase.wantOK {
			t.Fatalf("mediaServerItemType(%q) = (%q, %v), want (%q, %v)",
				testCase.itemType, gotType, gotOK, testCase.wantType, testCase.wantOK)
		}
	}

	if !isMediaServerFolder("Folder") || !isMediaServerFolder("CollectionFolder") {
		t.Fatal("目录类型应被判为目录")
	}
	if isMediaServerFolder("Movie") || isMediaServerFolder("") {
		t.Fatal("非目录类型不应被判为目录")
	}
}
