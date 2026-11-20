package pluginengine

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"
)

// 本文件覆盖清单校验与配置归一：既保证「写错的清单注册不进来」，
// 也保证外部 JSON 里那些「看起来能填、其实不该接受」的值被如实拒绝。

func TestManifestValidateRejectsBadDefinitions(t *testing.T) {
	valid := Manifest{ID: "demo_plugin", Name: "示例", Version: "1.0.0"}
	if err := valid.validate(); err != nil {
		t.Fatalf("合法清单被拒绝: %v", err)
	}

	// 用构造器拼字段，避免在表里数括号——数错一个括号就得靠编译器来找。
	withFields := func(fields ...Field) Manifest {
		return Manifest{ID: "demo", Name: "x", Version: "1.0.0", Fields: fields}
	}
	selectField := func(options ...Option) Field {
		return Field{Key: "a", Type: FieldSelect, Options: options}
	}

	cases := []struct {
		name     string
		manifest Manifest
	}{
		{name: "ID 为空", manifest: Manifest{Name: "x", Version: "1.0.0"}},
		{name: "ID 含大写", manifest: Manifest{ID: "Demo", Name: "x", Version: "1.0.0"}},
		{name: "ID 含空格", manifest: Manifest{ID: "de mo", Name: "x", Version: "1.0.0"}},
		{name: "缺少名称", manifest: Manifest{ID: "demo", Version: "1.0.0"}},
		{name: "名称只有空白", manifest: Manifest{ID: "demo", Name: "  ", Version: "1.0.0"}},
		{name: "缺少版本", manifest: Manifest{ID: "demo", Name: "x"}},
		{name: "Level 为负", manifest: Manifest{ID: "demo", Name: "x", Version: "1.0.0", Level: -1}},
		{name: "配置项键非法", manifest: withFields(Field{Key: "Bad-Key", Type: FieldString})},
		{name: "配置项键重复", manifest: withFields(Field{Key: "a", Type: FieldString}, Field{Key: "a", Type: FieldBoolean})},
		{name: "类型未知", manifest: withFields(Field{Key: "a", Type: FieldType("money")})},
		{name: "select 无选项", manifest: withFields(Field{Key: "a", Type: FieldSelect})},
		{name: "select 选项重复", manifest: withFields(selectField(Option{Value: "x"}, Option{Value: "x"}))},
		{name: "select 选项为空", manifest: withFields(selectField(Option{Value: " "}))},
		{name: "默认值类型不符", manifest: withFields(Field{Key: "a", Type: FieldInteger, Default: true})},
		{name: "默认值超出选项", manifest: withFields(Field{Key: "a", Type: FieldSelect, Default: "z", Options: []Option{{Value: "y"}}})},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			if err := testCase.manifest.validate(); !errors.Is(err, ErrInvalidManifest) {
				t.Fatalf("error = %v, 期望 ErrInvalidManifest", err)
			}
		})
	}
}

func TestNormalizeConfig(t *testing.T) {
	manifest := Manifest{
		ID:      "demo",
		Name:    "示例",
		Version: "1.0.0",
		Fields: []Field{
			{Key: "text", Label: "文本", Type: FieldString, Required: true},
			{Key: "count", Label: "数量", Type: FieldInteger, Default: int64(3)},
			{Key: "ratio", Label: "比例", Type: FieldNumber, Default: 0.5},
			{Key: "flag", Label: "开关", Type: FieldBoolean, Default: false},
			{Key: "mode", Label: "模式", Type: FieldSelect, Default: "fast", Options: []Option{{Value: "fast"}, {Value: "slow"}}},
			{Key: "note", Label: "备注", Type: FieldTextarea},
		},
	}

	t.Run("补齐默认值", func(t *testing.T) {
		config, err := normalizeConfig(manifest, map[string]any{"text": "hello"})
		if err != nil {
			t.Fatalf("error = %v", err)
		}
		if config["text"] != "hello" {
			t.Fatalf("text = %v", config["text"])
		}
		if config["count"] != int64(3) || config["ratio"] != 0.5 || config["flag"] != false || config["mode"] != "fast" {
			t.Fatalf("默认值未生效: %#v", config)
		}
		if _, present := config["note"]; present {
			t.Fatalf("没有默认值的可选项不应出现: %#v", config)
		}
	})

	t.Run("JSON 解出的 float64 整数被接受", func(t *testing.T) {
		var raw map[string]any
		if err := json.Unmarshal([]byte(`{"text":"a","count":7,"ratio":2}`), &raw); err != nil {
			t.Fatalf("unmarshal: %v", err)
		}
		config, err := normalizeConfig(manifest, raw)
		if err != nil {
			t.Fatalf("error = %v", err)
		}
		if config["count"] != int64(7) {
			t.Fatalf("count = %v (%T)", config["count"], config["count"])
		}
		if config["ratio"] != float64(2) {
			t.Fatalf("ratio = %v (%T)", config["ratio"], config["ratio"])
		}
	})

	t.Run("未知键被拒绝", func(t *testing.T) {
		_, err := normalizeConfig(manifest, map[string]any{"text": "a", "unknown": 1})
		if !errors.Is(err, ErrInvalidConfig) {
			t.Fatalf("error = %v", err)
		}
	})

	t.Run("缺少必填", func(t *testing.T) {
		if _, err := normalizeConfig(manifest, map[string]any{"count": 1}); !errors.Is(err, ErrInvalidConfig) {
			t.Fatalf("error = %v", err)
		}
	})

	t.Run("必填为空串", func(t *testing.T) {
		if _, err := normalizeConfig(manifest, map[string]any{"text": "   "}); !errors.Is(err, ErrInvalidConfig) {
			t.Fatalf("error = %v", err)
		}
	})

	t.Run("必填项显式传 null 视为缺失", func(t *testing.T) {
		if _, err := normalizeConfig(manifest, map[string]any{"text": nil}); !errors.Is(err, ErrInvalidConfig) {
			t.Fatalf("error = %v", err)
		}
	})

	t.Run("整数不接受小数", func(t *testing.T) {
		if _, err := normalizeConfig(manifest, map[string]any{"text": "a", "count": 3.5}); !errors.Is(err, ErrInvalidConfig) {
			t.Fatalf("error = %v", err)
		}
	})

	t.Run("整数接受数字字符串", func(t *testing.T) {
		// 浏览器的 number 输入框交回的是字符串；不认它，界面上就改不了数字配置。
		config, err := normalizeConfig(manifest, map[string]any{"text": "a", "count": "3"})
		if err != nil {
			t.Fatalf("error = %v", err)
		}
		if config["count"] != int64(3) {
			t.Fatalf("count = %#v", config["count"])
		}
	})
	t.Run("整数不接受非整数文本", func(t *testing.T) {
		for _, value := range []any{"3.5", "abc", ""} {
			if _, err := normalizeConfig(manifest, map[string]any{"text": "a", "count": value}); !errors.Is(err, ErrInvalidConfig) {
				t.Fatalf("%#v 应当被拒绝: %v", value, err)
			}
		}
	})

	t.Run("布尔不接受字符串", func(t *testing.T) {
		if _, err := normalizeConfig(manifest, map[string]any{"text": "a", "flag": "true"}); !errors.Is(err, ErrInvalidConfig) {
			t.Fatalf("error = %v", err)
		}
	})

	t.Run("select 只接受声明过的值", func(t *testing.T) {
		if _, err := normalizeConfig(manifest, map[string]any{"text": "a", "mode": "turbo"}); !errors.Is(err, ErrInvalidConfig) {
			t.Fatalf("error = %v", err)
		}
	})

	t.Run("超长字符串被拒绝", func(t *testing.T) {
		_, err := normalizeConfig(manifest, map[string]any{"text": strings.Repeat("a", maxConfigStringLength+1)})
		if !errors.Is(err, ErrInvalidConfig) {
			t.Fatalf("error = %v", err)
		}
	})
}

func TestRequiredSemanticsByType(t *testing.T) {
	manifest := Manifest{
		ID:      "demo",
		Name:    "示例",
		Version: "1.0.0",
		Fields: []Field{
			{Key: "ratio", Label: "比例", Type: FieldNumber, Required: true},
			{Key: "flag", Label: "开关", Type: FieldBoolean, Required: true},
			{Key: "mode", Label: "模式", Type: FieldSelect, Required: true, Options: []Option{{Value: "fast"}, {Value: "slow"}}},
		},
	}

	t.Run("必填数字不接受 0", func(t *testing.T) {
		_, err := normalizeConfig(manifest, map[string]any{"ratio": 0, "flag": true, "mode": "fast"})
		if !errors.Is(err, ErrInvalidConfig) {
			t.Fatalf("error = %v", err)
		}
	})

	t.Run("必填布尔允许 false", func(t *testing.T) {
		config, err := normalizeConfig(manifest, map[string]any{"ratio": 1, "flag": false, "mode": "fast"})
		if err != nil {
			t.Fatalf("false 是合法取值，不应被当成空: %v", err)
		}
		if config["flag"] != false {
			t.Fatalf("flag = %#v", config["flag"])
		}
	})

	t.Run("必填 select 缺省仍报错", func(t *testing.T) {
		if _, err := normalizeConfig(manifest, map[string]any{"ratio": 1, "flag": true}); !errors.Is(err, ErrInvalidConfig) {
			t.Fatalf("error = %v", err)
		}
	})
}

func TestNumberFieldNormalization(t *testing.T) {
	manifest := Manifest{
		ID:      "demo",
		Name:    "示例",
		Version: "1.0.0",
		Fields:  []Field{{Key: "ratio", Label: "比例", Type: FieldNumber}},
	}
	cases := []struct {
		name  string
		input any
		want  float64
	}{
		{name: "int", input: 2, want: 2},
		{name: "int64", input: int64(3), want: 3},
		{name: "float64", input: 1.25, want: 1.25},
		{name: "json.Number", input: json.Number("0.75"), want: 0.75},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			config, err := normalizeConfig(manifest, map[string]any{"ratio": testCase.input})
			if err != nil {
				t.Fatalf("error = %v", err)
			}
			if config["ratio"] != testCase.want {
				t.Fatalf("ratio = %#v", config["ratio"])
			}
		})
	}

	t.Run("非法 json.Number", func(t *testing.T) {
		if _, err := normalizeConfig(manifest, map[string]any{"ratio": json.Number("abc")}); !errors.Is(err, ErrInvalidConfig) {
			t.Fatalf("error = %v", err)
		}
	})

	t.Run("数字字符串按浮点解析", func(t *testing.T) {
		config, err := normalizeConfig(manifest, map[string]any{"ratio": "1.5"})
		if err != nil {
			t.Fatalf("error = %v", err)
		}
		if config["ratio"] != 1.5 {
			t.Fatalf("ratio = %#v", config["ratio"])
		}
	})
	t.Run("非数字文本被拒绝", func(t *testing.T) {
		if _, err := normalizeConfig(manifest, map[string]any{"ratio": "abc"}); !errors.Is(err, ErrInvalidConfig) {
			t.Fatalf("error = %v", err)
		}
	})
}

func TestToInt64AcceptsJSONNumber(t *testing.T) {
	value, err := toInt64(json.Number("12"))
	if err != nil {
		t.Fatalf("error = %v", err)
	}
	if value != int64(12) {
		t.Fatalf("value = %v", value)
	}
	if _, err := toInt64(json.Number("12.5")); err == nil {
		t.Fatalf("带小数的 json.Number 应当被拒绝")
	}
	if _, err := toInt64(struct{}{}); err == nil {
		t.Fatalf("不支持的类型应当被拒绝")
	}
}
