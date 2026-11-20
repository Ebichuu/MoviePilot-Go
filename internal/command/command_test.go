package command

import (
	"context"
	"errors"
	"strings"
	"testing"
)

func TestParseAcceptsTelegramStyleCommands(t *testing.T) {
	t.Parallel()

	tests := []struct {
		raw   string
		name  string
		args  []string
		fails bool
	}{
		{raw: "/doctor", name: "doctor"},
		{raw: "  /sites@MoviePilotBot  ", name: "sites"},
		{raw: "/subscribes 1 2", name: "subscribes", args: []string{"1", "2"}},
		{raw: "help", name: "help"},
		{raw: "   ", fails: true},
		{raw: "/@bot", fails: true},
	}
	for _, tt := range tests {
		name, args, err := Parse(tt.raw)
		if tt.fails {
			if err == nil {
				t.Fatalf("Parse(%q) 期望失败，得到 name=%q", tt.raw, name)
			}
			continue
		}
		if err != nil {
			t.Fatalf("Parse(%q) 失败: %v", tt.raw, err)
		}
		if name != tt.name {
			t.Fatalf("Parse(%q) name=%q want %q", tt.raw, name, tt.name)
		}
		if strings.Join(args, ",") != strings.Join(tt.args, ",") {
			t.Fatalf("Parse(%q) args=%v want %v", tt.raw, args, tt.args)
		}
	}
}

func TestRegistryRejectsDuplicateNamesAndMissingHandler(t *testing.T) {
	t.Parallel()

	if _, err := NewRegistry(Command{Name: "a", Handler: stubHandler("a")}, Command{Name: "a", Handler: stubHandler("a")}); err == nil {
		t.Fatal("重复命令名应当被拒绝")
	}
	if _, err := NewRegistry(Command{Name: "a", Aliases: []string{"b"}, Handler: stubHandler("a")}, Command{Name: "b", Handler: stubHandler("b")}); err == nil {
		t.Fatal("别名与命令名冲突应当被拒绝")
	}
	if _, err := NewRegistry(Command{Name: "a"}); err == nil {
		t.Fatal("缺少 handler 应当被拒绝")
	}
}

func TestRegistryExecutesAliasesAndEnforcesAdmin(t *testing.T) {
	t.Parallel()

	registry, err := NewRegistry(
		Command{
			Name: "doctor", Aliases: []string{"d"}, Title: "自检", Category: "系统",
			Handler: func(context.Context, Request) (Result, error) {
				return Result{Text: "自检正常", Details: map[string]any{"ok": true}}, nil
			},
		},
		Command{
			Name: "reload", Title: "重载", Category: "系统", AdminOnly: true,
			Handler: stubHandler("reload"),
		},
	)
	if err != nil {
		t.Fatalf("构建注册表失败: %v", err)
	}

	result, err := registry.Execute(context.Background(), "/d@bot", Request{Username: "alice"})
	if err != nil {
		t.Fatalf("执行失败: %v", err)
	}
	if result.Command != "doctor" || result.Text != "自检正常" {
		t.Fatalf("结果错误: %+v", result)
	}

	if _, err := registry.Execute(context.Background(), "/reload", Request{Username: "alice"}); !errors.Is(err, ErrForbidden) {
		t.Fatalf("非管理员应被拒绝, got %v", err)
	}
	if _, err := registry.Execute(context.Background(), "/reload", Request{Username: "admin", Admin: true}); err != nil {
		t.Fatalf("管理员应可执行: %v", err)
	}
	if _, err := registry.Execute(context.Background(), "/nope", Request{}); !errors.Is(err, ErrUnknown) {
		t.Fatalf("未知命令应报错, got %v", err)
	}
	if _, err := registry.Execute(context.Background(), "", Request{}); !errors.Is(err, ErrEmpty) {
		t.Fatalf("空命令应报错, got %v", err)
	}
}

func TestRegistryListIsStable(t *testing.T) {
	t.Parallel()

	registry, err := NewRegistry(
		Command{Name: "b", Title: "B", Category: "网站", Handler: stubHandler("b")},
		Command{Name: "a", Title: "A", Category: "系统", Handler: stubHandler("a")},
		Command{Name: "c", Title: "C", Category: "网站", Handler: stubHandler("c")},
	)
	if err != nil {
		t.Fatalf("构建注册表失败: %v", err)
	}
	list := registry.List()
	got := make([]string, 0, len(list))
	for _, item := range list {
		got = append(got, item.Name)
	}
	if strings.Join(got, ",") != "a,b,c" {
		t.Fatalf("排序结果 = %v", got)
	}
}

func stubHandler(name string) Handler {
	return func(context.Context, Request) (Result, error) {
		return Result{Command: name, Text: name}, nil
	}
}
