// Package command 是 V2 `app/command.py` 的 Go 等价物：把「预设命令」注册成
// 一张表，支持从聊天文本（`/name@bot args`）或结构化调用执行，并统一返回
// 一段可读文本 + 结构化明细。
//
// 设计约束：
//   - 只读：本轮不提供会修改系统状态的命令；
//   - 权限显式：每条命令标注是否仅管理员可用，执行期校验；
//   - 拒绝要诚实：未知命令、缺权限都返回明确中文原因，不静默成功。
package command

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
)

var (
	// ErrEmpty 表示没有给出命令文本。
	ErrEmpty = errors.New("命令为空")
	// ErrUnknown 表示命令不存在。
	ErrUnknown = errors.New("未知命令")
	// ErrForbidden 表示当前用户无权执行该命令。
	ErrForbidden = errors.New("没有权限执行该命令")
)

// Request 是一次命令执行的调用者上下文。
type Request struct {
	Username string
	Admin    bool
	Args     []string
}

// Result 是一条命令的输出。Text 面向人，Details 面向调用方。
type Result struct {
	Command string         `json:"command"`
	Text    string         `json:"text"`
	Details map[string]any `json:"details,omitempty"`
}

// Handler 执行一条命令。
type Handler func(ctx context.Context, req Request) (Result, error)

// Command 是一条预设命令。
type Command struct {
	Name        string
	Aliases     []string
	Title       string
	Description string
	Category    string
	AdminOnly   bool
	Handler     Handler
}

// Descriptor 是命令的对外描述，用于列表展示。
type Descriptor struct {
	Name        string   `json:"name"`
	Aliases     []string `json:"aliases,omitempty"`
	Title       string   `json:"title"`
	Description string   `json:"description"`
	Category    string   `json:"category"`
	AdminOnly   bool     `json:"admin_only"`
}

// Registry 是只读的命令注册表。
type Registry struct {
	commands []Command
	index    map[string]int
}

// NewRegistry 构建注册表。命令名为空、重复或缺少 handler 都视为编程错误。
func NewRegistry(commands ...Command) (*Registry, error) {
	registry := &Registry{
		commands: make([]Command, 0, len(commands)),
		index:    make(map[string]int, len(commands)*2),
	}
	for _, item := range commands {
		name := normalize(item.Name)
		if name == "" {
			return nil, errors.New("命令名不能为空")
		}
		if item.Handler == nil {
			return nil, fmt.Errorf("命令 %s 缺少处理函数", name)
		}
		item.Name = name
		item.Aliases = normalizeAliases(item.Aliases)
		for _, key := range append([]string{name}, item.Aliases...) {
			if _, ok := registry.index[key]; ok {
				return nil, fmt.Errorf("命令名或别名重复: %s", key)
			}
		}
		for _, key := range append([]string{name}, item.Aliases...) {
			registry.index[key] = len(registry.commands)
		}
		registry.commands = append(registry.commands, item)
	}
	return registry, nil
}

// List 返回按分类、名称排序的命令描述。
func (r *Registry) List() []Descriptor {
	if r == nil {
		return nil
	}
	descriptors := make([]Descriptor, 0, len(r.commands))
	for _, item := range r.commands {
		descriptors = append(descriptors, Descriptor{
			Name:        item.Name,
			Aliases:     append([]string(nil), item.Aliases...),
			Title:       item.Title,
			Description: item.Description,
			Category:    item.Category,
			AdminOnly:   item.AdminOnly,
		})
	}
	sort.SliceStable(descriptors, func(i, j int) bool {
		if descriptors[i].Category != descriptors[j].Category {
			return descriptors[i].Category < descriptors[j].Category
		}
		return descriptors[i].Name < descriptors[j].Name
	})
	return descriptors
}

// Lookup 按名称或别名查找命令。
func (r *Registry) Lookup(name string) (Command, bool) {
	if r == nil {
		return Command{}, false
	}
	position, ok := r.index[normalize(name)]
	if !ok {
		return Command{}, false
	}
	return r.commands[position], true
}

// Execute 解析并执行一条命令文本（允许 `/name@botname arg1 arg2` 形式）。
func (r *Registry) Execute(ctx context.Context, raw string, req Request) (Result, error) {
	name, args, err := Parse(raw)
	if err != nil {
		return Result{}, err
	}
	return r.ExecuteName(ctx, name, Request{Username: req.Username, Admin: req.Admin, Args: args})
}

// ExecuteName 按已解析的命令名执行。
func (r *Registry) ExecuteName(ctx context.Context, name string, req Request) (Result, error) {
	item, ok := r.Lookup(name)
	if !ok {
		return Result{}, fmt.Errorf("%w: %s", ErrUnknown, name)
	}
	if item.AdminOnly && !req.Admin {
		return Result{}, fmt.Errorf("%w: %s", ErrForbidden, item.Name)
	}
	result, err := item.Handler(ctx, req)
	if err != nil {
		return Result{}, err
	}
	if result.Command == "" {
		result.Command = item.Name
	}
	return result, nil
}

// Parse 拆分命令文本。与 V2/Telegram 一致：去掉开头的 `/`，去掉 `@botname`
// 后缀，按空白切分参数。
func Parse(raw string) (string, []string, error) {
	fields := strings.Fields(strings.TrimSpace(raw))
	if len(fields) == 0 {
		return "", nil, ErrEmpty
	}
	name := fields[0]
	if at := strings.IndexByte(name, '@'); at >= 0 {
		name = name[:at]
	}
	name = normalize(name)
	if name == "" {
		return "", nil, ErrEmpty
	}
	return name, fields[1:], nil
}

func normalize(name string) string {
	return strings.ToLower(strings.TrimLeft(strings.TrimSpace(name), "/"))
}

func normalizeAliases(aliases []string) []string {
	result := make([]string, 0, len(aliases))
	for _, alias := range aliases {
		if key := normalize(alias); key != "" {
			result = append(result, key)
		}
	}
	return result
}
