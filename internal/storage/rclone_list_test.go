package storage

import (
	"context"
	"errors"
	"reflect"
	"testing"
	"time"
)

func rcloneModifyTime(year int, month time.Month, day, hour int) float64 {
	return float64(time.Date(year, month, day, hour, 0, 0, 0, time.UTC).Unix())
}

func TestRcloneListProjectsEntries(t *testing.T) {
	t.Parallel()

	provider, fake := newRcloneFake(t, RcloneConfig{})
	fake.setListBody(`{"list":[` +
		`{"Name":"第2集.mp4","Size":2048,"ModTime":"2026-11-19T10:00:00Z"},` +
		`{"Name":"Subs","IsDir":true},` +
		`{"Name":"海报.JPG","Size":512,"ModTime":"not-a-time"},` +
		`{"Name":"noext","Size":0}` +
		`]}`)

	items, err := provider.List(context.Background(), "rclone", "/剧集/", "", "")
	if err != nil {
		t.Fatalf("List() error = %v", err)
	}
	byName := make(map[string]Item, len(items))
	for _, item := range items {
		byName[item.Name] = item
	}

	file := byName["第2集.mp4"]
	if file.Type != "file" || file.Extension != "mp4" || file.Basename != "第2集" {
		t.Fatalf("文件投影错误: %+v", file)
	}
	if file.Size == nil || *file.Size != 2048 {
		t.Fatalf("文件大小 = %v", file.Size)
	}
	if file.ModifyTime == nil || *file.ModifyTime != rcloneModifyTime(2026, time.November, 19, 10) {
		t.Fatalf("修改时间 = %v", file.ModifyTime)
	}
	if file.Path != "剧集/第2集.mp4" {
		t.Fatalf("路径 = %q", file.Path)
	}
	if file.Storage != "rclone" || file.FileID != "第2集.mp4" {
		t.Fatalf("归属字段 = %+v", file)
	}
	// Children 必须是空切片而不是 nil：前端靠它判断「已展开且为空」。
	if file.Children == nil || len(file.Children) != 0 {
		t.Fatalf("Children = %#v", file.Children)
	}

	directory := byName["Subs"]
	if directory.Type != "dir" || directory.Size != nil || directory.Extension != "" {
		t.Fatalf("目录投影错误: %+v", directory)
	}

	// 大小 0 必须保留为「0 字节」，不能因为零值被当成未知。
	emptyFile := byName["noext"]
	if emptyFile.Size == nil || *emptyFile.Size != 0 {
		t.Fatalf("0 字节文件大小 = %v", emptyFile.Size)
	}
	if emptyFile.Extension != "" {
		t.Fatalf("无扩展名文件 Extension = %q", emptyFile.Extension)
	}

	// 读不懂的修改时间宁可留空，也不能编一个假时间（前端会按它排序）。
	if poster := byName["海报.JPG"]; poster.ModifyTime != nil {
		t.Fatalf("非法修改时间被解析成了 %v", *poster.ModifyTime)
	}
	// 扩展名保留远端原始大小写，由调用方决定是否归一。
	if byName["海报.JPG"].Extension != "JPG" {
		t.Fatalf("Extension = %q", byName["海报.JPG"].Extension)
	}

	// 默认排序是修改时间倒序：有时间的最前面，时间相同的保持服务端顺序。
	names := make([]string, 0, len(items))
	for _, item := range items {
		names = append(names, item.Name)
	}
	wantOrder := []string{"第2集.mp4", "Subs", "海报.JPG", "noext"}
	if !reflect.DeepEqual(names, wantOrder) {
		t.Fatalf("默认排序 = %v, want %v", names, wantOrder)
	}

	requests := fake.recorded()
	if len(requests) != 1 || requests[0].Path != "/operations/list" {
		t.Fatalf("端点 = %v", fake.endpoints())
	}
	if requests[0].Body["fs"] != "gdrive:media" || requests[0].Body["remote"] != "剧集" {
		t.Fatalf("请求参数 = %v", requests[0].Body)
	}
}

func TestRcloneListFiltersAndSorts(t *testing.T) {
	t.Parallel()

	provider, fake := newRcloneFake(t, RcloneConfig{})
	fake.setListBody(`{"list":[` +
		`{"Name":"第10集.mp4","Size":10,"ModTime":"2026-11-19T12:00:00Z"},` +
		`{"Name":"第2集.mp4","Size":2,"ModTime":"2026-11-19T11:00:00Z"},` +
		`{"Name":"第1集.mkv","Size":1,"ModTime":"2026-11-19T10:00:00Z"}` +
		`]}`)
	ctx := context.Background()

	named, err := provider.List(ctx, "rclone", "", "name", "")
	if err != nil {
		t.Fatalf("List(name) error = %v", err)
	}
	// 名称排序是自然序：第2集必须排在第10集之前。
	wantNamed := []string{"第1集.mkv", "第2集.mp4", "第10集.mp4"}
	if got := itemNames(named); !reflect.DeepEqual(got, wantNamed) {
		t.Fatalf("name 排序 = %v, want %v", got, wantNamed)
	}

	defaultOrder, err := provider.List(ctx, "rclone", "", "", "")
	if err != nil {
		t.Fatalf("List(default) error = %v", err)
	}
	wantDefault := []string{"第10集.mp4", "第2集.mp4", "第1集.mkv"}
	if got := itemNames(defaultOrder); !reflect.DeepEqual(got, wantDefault) {
		t.Fatalf("默认排序 = %v, want %v", got, wantDefault)
	}

	tests := []struct {
		name    string
		keyword string
		want    []string
	}{
		{name: "通配符扩展名", keyword: "*.mp4", want: []string{"第10集.mp4", "第2集.mp4"}},
		{name: "大小写不敏感", keyword: "*.MKV", want: []string{"第1集.mkv"}},
		{name: "单字符通配", keyword: "第?集.mp4", want: []string{"第2集.mp4"}},
		{name: "全部匹配", keyword: "*", want: []string{"第10集.mp4", "第2集.mp4", "第1集.mkv"}},
		{name: "无命中", keyword: "不存在", want: []string{}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			items, err := provider.List(ctx, "rclone", "", "", tt.keyword)
			if err != nil {
				t.Fatalf("List(关键字=%q) error = %v", tt.keyword, err)
			}
			if items == nil {
				t.Fatalf("List(关键字=%q) 返回 nil，应为空切片", tt.keyword)
			}
			if got := itemNames(items); !reflect.DeepEqual(got, tt.want) {
				t.Fatalf("过滤结果 = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestRcloneListMapsUpstreamFailure(t *testing.T) {
	t.Parallel()

	provider, fake := newRcloneFake(t, RcloneConfig{})
	if _, err := provider.List(nil, "rclone", "", "", ""); !errors.Is(err, ErrInvalidRequest) {
		t.Fatalf("nil ctx error = %v", err)
	}
	if _, err := provider.List(context.Background(), "other", "", "", ""); !errors.Is(err, ErrUnsupportedStorage) {
		t.Fatalf("错误存储名 error = %v", err)
	}

	fake.setFail("/operations/list", 500)
	if _, err := provider.List(context.Background(), "rclone", "", "", ""); !errors.Is(err, ErrOperationFailed) {
		t.Fatalf("上游 500 error = %v", err)
	}

	// 2xx 但正文读不懂时不能当成「空目录」，否则用户会以为文件丢了。
	fake.setFail("/operations/list", 0)
	fake.setListBody(`{"list":"nope"}`)
	if _, err := provider.List(context.Background(), "rclone", "", "", ""); !errors.Is(err, ErrOperationFailed) {
		t.Fatalf("非法响应 error = %v", err)
	}

	fake.setListBody(`{}`)
	items, err := provider.List(context.Background(), "rclone", "", "", "")
	if err != nil || items == nil || len(items) != 0 {
		t.Fatalf("空目录 = %#v %v", items, err)
	}
}

func TestRcloneJoin(t *testing.T) {
	t.Parallel()

	tests := map[string]struct{ base, name, want string }{
		"都为空":  {base: "", name: "", want: ""},
		"只有目录": {base: "a/b/", name: "", want: "a/b"},
		"只有名字": {base: "", name: "/x/y/", want: "x/y"},
		"正常拼接": {base: "a", name: "b", want: "a/b"},
		"多级目录": {base: "a/b//", name: "c", want: "a/b/c"},
	}
	for name, tt := range tests {
		if got := rcloneJoin(tt.base, tt.name); got != tt.want {
			t.Fatalf("%s: rcloneJoin(%q, %q) = %q, want %q", name, tt.base, tt.name, got, tt.want)
		}
	}
}
