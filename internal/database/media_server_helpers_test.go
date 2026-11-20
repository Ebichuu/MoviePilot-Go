package database

import "testing"

func TestMediaServerSeasonInfoJSONUsesStringKeys(t *testing.T) {
	input := map[int][]int{1: {1, 2}, 0: {5}}
	result := mediaServerSeasonInfoJSON(input)

	if len(result) != 2 {
		t.Fatalf("应保留两个季节，实际 %+v", result)
	}
	// JSON 对象的键必须是字符串，否则序列化后的形状与 V2 不一致。
	first, ok := result["1"]
	if !ok || len(first) != 2 || first[0] != 1 || first[1] != 2 {
		t.Fatalf("季节 1 映射错误: %+v", result)
	}
	if special, ok := result["0"]; !ok || len(special) != 1 || special[0] != 5 {
		t.Fatalf("特别篇映射错误: %+v", result)
	}

	// 必须是副本：调用方后续改动不能影响已构造的结构。
	input[1][0] = 99
	if result["1"][0] != 1 {
		t.Fatalf("结果暴露了内部切片: %+v", result)
	}

	if empty := mediaServerSeasonInfoJSON(nil); len(empty) != 0 {
		t.Fatalf("nil 输入应返回空 map，实际 %+v", empty)
	}
}

func TestMediaServerNullText(t *testing.T) {
	if got := mediaServerNullText(""); got != nil {
		t.Fatalf("空串应写入 NULL，实际 %#v", got)
	}
	if got := mediaServerNullText("   "); got != nil {
		t.Fatalf("空白串应写入 NULL，实际 %#v", got)
	}
	if got := mediaServerNullText("  片名  "); got != "片名" {
		t.Fatalf("应去空格后写入，实际 %#v", got)
	}
}
