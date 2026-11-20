package indexer

import (
	"encoding/json"
	"strings"
	"testing"
)

// TestDecodeJSONObjectBranches 覆盖站点 JSON 对象的解析与尾部校验。
func TestDecodeJSONObjectBranches(t *testing.T) {
	if _, err := decodeJSONObject([]byte("not json")); err == nil ||
		!strings.Contains(err.Error(), "解析站点 JSON 失败") {
		t.Fatalf("非法 JSON = %v", err)
	}
	if _, err := decodeJSONObject([]byte("[1,2]")); err == nil {
		t.Fatal("根数组应报错")
	}
	if _, err := decodeJSONObject([]byte(`{"a":1} {"b":2}`)); err == nil ||
		!strings.Contains(err.Error(), "多余内容") {
		t.Fatalf("多余内容 = %v", err)
	}
	document, err := decodeJSONObject([]byte(`{"code":0,"data":{"n":1}}`))
	if err != nil || document["code"] == nil {
		t.Fatalf("合法对象 = %+v（%v）", document, err)
	}
}

// TestValueHelpersBranches 覆盖弱类型取值函数。
func TestValueHelpersBranches(t *testing.T) {
	if got := valueInt64("42"); got != 42 {
		t.Fatalf("valueInt64(42) = %d", got)
	}
	if got := valueInt64(" -1 "); got != 0 {
		t.Fatalf("负数 = %d", got)
	}
	if got := valueInt64("abc"); got != 0 {
		t.Fatalf("非数字 = %d", got)
	}
	if got := valueInt64(json.Number("7")); got != 7 {
		t.Fatalf("json.Number = %d", got)
	}

	if got := valueInt("5"); got != 5 {
		t.Fatalf("valueInt(5) = %d", got)
	}

	if got := valueFloat("50", 1); got != 50 {
		t.Fatalf("valueFloat(50) = %v", got)
	}
	for _, raw := range []string{"101", "abc", "NaN", "Infinity", "-1"} {
		if got := valueFloat(raw, 1); got != 1 {
			t.Fatalf("valueFloat(%q) = %v，期望回落值", raw, got)
		}
	}

	if !valueBool("TRUE") || valueBool("no") || !valueBool(1) || valueBool(0) {
		t.Fatal("valueBool 判定错误")
	}
	if valueBool(true) != true {
		t.Fatal("valueBool 布尔直通失败")
	}
}

// TestValidNumericIDBranches 覆盖站点 ID 的合法性。
func TestValidNumericIDBranches(t *testing.T) {
	valid := []string{"0", "007", "12345678901234567890"}
	for _, value := range valid {
		if !validNumericID(value) {
			t.Fatalf("%q 应合法", value)
		}
	}
	invalid := []string{"", "123456789012345678901", "12a", "-1", " 1"}
	for _, value := range invalid {
		if validNumericID(value) {
			t.Fatalf("%q 应非法", value)
		}
	}
}

// TestYemaLabelsBranches 覆盖 Yema 标签映射。
func TestYemaLabelsBranches(t *testing.T) {
	if labels := yemaLabels("nope"); labels != nil {
		t.Fatalf("非数组 = %+v", labels)
	}
	if labels := yemaLabels(nil); labels != nil {
		t.Fatalf("nil = %+v", labels)
	}
	labels := yemaLabels([]any{"1", json.Number("6"), "99", 5})
	if len(labels) != 3 || labels[0] != "禁转" || labels[1] != "中字" || labels[2] != "国语" {
		t.Fatalf("标签 = %+v", labels)
	}
	many := make([]any, 40)
	for index := range many {
		many[index] = "2"
	}
	if labels := yemaLabels(many); len(labels) != 32 {
		t.Fatalf("标签上限 = %d", len(labels))
	}
}

// TestYemaCategoryBranches 覆盖 Yema 分类映射。
func TestYemaCategoryBranches(t *testing.T) {
	cases := map[int]string{4: "电影", 5: "电视剧", 13: "电视剧", 17: "电视剧", 99: "未知", 0: "未知"}
	for value, want := range cases {
		if got := yemaCategory(value); got != want {
			t.Fatalf("yemaCategory(%d) = %q，期望 %q", value, got, want)
		}
	}
}

// TestConfigStringBranches 覆盖多键配置取值与空白回落。
func TestConfigStringBranches(t *testing.T) {
	config := map[string]any{"apikey": "  ", "api_key": " value ", "other": 1}
	if got := configString(config, "apikey", "api_key"); got != "value" {
		t.Fatalf("多键回落 = %q", got)
	}
	if got := configString(config, "missing"); got != "" {
		t.Fatalf("缺失键 = %q", got)
	}
	if got := configString(config, "other"); got != "1" {
		t.Fatalf("数字键 = %q", got)
	}
}
