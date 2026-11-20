package indexer

import "testing"

// TestDirectoryStringListSkipsBlanks 覆盖目录字符串列表归一化时跳过空白项
// 与重复项（大小写不敏感）的分支。
func TestDirectoryStringListSkipsBlanks(t *testing.T) {
	got := directoryStringList([]string{"  ", "Example.COM", "example.com", " 其他 "})
	if len(got) != 2 {
		t.Fatalf("归一化结果 = %#v", got)
	}
	if got[0] != "Example.COM" || got[1] != "其他" {
		t.Fatalf("归一化结果 = %#v", got)
	}
	if list := directoryStringList(" , ; ;, "); len(list) != 0 {
		t.Fatalf("纯分隔符应得到空列表，实际 %#v", list)
	}
}
