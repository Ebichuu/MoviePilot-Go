package indexer

import "testing"

// TestParseOptionalFloatBranches 覆盖 RSS 字段的可选浮点解析。
func TestParseOptionalFloatBranches(t *testing.T) {
	if value := parseOptionalFloat(""); value != nil {
		t.Fatalf("空串 = %v", value)
	}
	if value := parseOptionalFloat("   "); value != nil {
		t.Fatalf("空白 = %v", value)
	}
	for _, raw := range []string{" 50 ", "0", "100"} {
		value := parseOptionalFloat(raw)
		if value == nil {
			t.Fatalf("%q 应解析成功", raw)
		}
	}
	for _, raw := range []string{"101", "-1", "NaN", "Infinity", "abc"} {
		if value := parseOptionalFloat(raw); value != nil {
			t.Fatalf("%q 应被拒绝，得到 %v", raw, *value)
		}
	}
	if value := parseOptionalFloat("0.5"); value == nil || *value != 0.5 {
		t.Fatalf("0.5 = %v", value)
	}
}
