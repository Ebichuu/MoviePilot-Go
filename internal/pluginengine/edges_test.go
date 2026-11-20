package pluginengine

import (
	"context"
	"errors"
	"testing"
)

// 本文件补几处「从公开入口很难走到」的分支：类型转换兜底、空动作名，
// 以及状态读取失败时 NewBuiltinRegistry 仍然把可用的注册表交还调用方。

func TestCoerceRejectsUnknownFieldType(t *testing.T) {
	if _, err := (Field{Key: "a", Type: FieldType("money")}).coerce("x"); err == nil {
		t.Fatalf("未知类型应当被拒绝")
	}
}

func TestCoerceRejectsTypeMismatches(t *testing.T) {
	cases := []struct {
		name  string
		field Field
		value any
	}{
		{name: "字符串字段收到数字", field: Field{Key: "s", Type: FieldString}, value: 123},
		{name: "布尔字段收到字符串", field: Field{Key: "b", Type: FieldBoolean}, value: "true"},
		{name: "select 字段收到数字", field: Field{Key: "m", Type: FieldSelect, Options: []Option{{Value: "a"}}}, value: 1},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			if _, err := testCase.field.coerce(testCase.value); err == nil {
				t.Fatalf("类型不符应当被拒绝")
			}
		})
	}
}

func TestIsZero(t *testing.T) {
	cases := []struct {
		value any
		want  bool
	}{
		{value: nil, want: true},
		{value: "  ", want: true},
		{value: "x", want: false},
		{value: int64(0), want: true},
		{value: int64(1), want: false},
		{value: float64(0), want: true},
		{value: float64(0.5), want: false},
		{value: struct{}{}, want: false},
	}
	for _, testCase := range cases {
		if got := isZero(testCase.value); got != testCase.want {
			t.Fatalf("isZero(%#v) = %v, 期望 %v", testCase.value, got, testCase.want)
		}
	}
}

func TestToInt64AcceptsWideningIntegers(t *testing.T) {
	if value, err := toInt64(int32(5)); err != nil || value != int64(5) {
		t.Fatalf("value = %v, error = %v", value, err)
	}
	if value, err := toInt64(int(6)); err != nil || value != int64(6) {
		t.Fatalf("value = %v, error = %v", value, err)
	}
}

func TestEmptyActionNameRejected(t *testing.T) {
	registry := NewRegistry(nil)
	def := testDefinition("alpha", nil)
	def.Actions[""] = nil
	if err := registry.Register(def); !errors.Is(err, ErrInvalidManifest) {
		t.Fatalf("error = %v", err)
	}
	if _, err := registry.Get("alpha"); !errors.Is(err, ErrPluginNotFound) {
		t.Fatalf("非法定义不应入表: %v", err)
	}
}

func TestRegisterSinglePlugin(t *testing.T) {
	registry := NewRegistry(nil)
	if err := registry.Register(testDefinition("solo", nil)); err != nil {
		t.Fatalf("注册失败: %v", err)
	}
	plugin, err := registry.Get("solo")
	if err != nil {
		t.Fatalf("error = %v", err)
	}
	if plugin.ID != "solo" || plugin.Installed {
		t.Fatalf("plugin = %#v", plugin)
	}
}

func TestRunUnknownPlugin(t *testing.T) {
	registry := NewRegistry(nil)
	if _, err := registry.Run(context.Background(), "ghost", ActionRun, nil); !errors.Is(err, ErrPluginNotFound) {
		t.Fatalf("error = %v", err)
	}
}

func TestNewBuiltinRegistryReportsRestoreProblem(t *testing.T) {
	registry, err := NewBuiltinRegistry(context.Background(), failingStore{}, Services{})
	if err == nil {
		t.Fatalf("读取状态失败应当上抛")
	}
	if registry == nil {
		t.Fatalf("读取失败时仍应返回可用的注册表")
	}
	if len(registry.List()) != 5 {
		t.Fatalf("插件数量 = %d", len(registry.List()))
	}
}
