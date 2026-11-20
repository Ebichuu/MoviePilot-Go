package scrapeflow

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/google/uuid"
)

func TestPathWithinDirectory(t *testing.T) {
	root := filepath.Join(os.TempDir(), "mp-root")
	cases := []struct {
		name      string
		candidate string
		want      bool
	}{
		{"同目录", root, true},
		{"直接子项", filepath.Join(root, "a.nfo"), true},
		{"深层子项", filepath.Join(root, "season 1", "a.nfo"), true},
		{"父目录", filepath.Dir(root), false},
		{"跳出到父目录的相对路径", filepath.Join(root, "..", "other"), false},
		// 前缀相同但不是子目录：必须靠分隔符判断，不能只比较字符串前缀。
		{"同名前缀目录", root + "-shadow" + string(filepath.Separator) + "a.nfo", false},
		{"无关绝对路径", filepath.Join(os.TempDir(), "elsewhere", "a.nfo"), false},
	}
	for _, testCase := range cases {
		if got := pathWithinDirectory(root, testCase.candidate); got != testCase.want {
			t.Fatalf("%s: pathWithinDirectory(%q, %q) = %v，期望 %v", testCase.name, root, testCase.candidate, got, testCase.want)
		}
	}
}

func TestPlanCloneIsDetached(t *testing.T) {
	request := RequestPlan{}
	original := Plan{
		BatchID:  uuid.New(),
		IntentID: uuid.New(),
		Username: "alice",
		Requests: []RequestPlan{request},
	}
	clone := original.Clone()
	if clone.BatchID != original.BatchID || clone.IntentID != original.IntentID || clone.Username != original.Username {
		t.Fatalf("字段未复制: %+v", clone)
	}
	if len(clone.Requests) != 1 {
		t.Fatalf("请求数 = %d", len(clone.Requests))
	}
	// 切片必须是独立的：追加克隆不会影响原计划。
	clone.Requests = append(clone.Requests, RequestPlan{})
	if len(original.Requests) != 1 {
		t.Fatalf("原计划被污染: %d", len(original.Requests))
	}

	empty := Plan{Username: "bob"}
	if got := empty.Clone(); got.Requests != nil {
		t.Fatalf("空请求应保持为空: %+v", got.Requests)
	}
}
