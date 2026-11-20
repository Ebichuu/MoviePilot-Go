package pluginengine

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"sync"
)

// ActionFunc 是插件动作的实现。入参里的 Config 已按清单校验并补齐默认值，
// 因此实现方可以直接使用，不需要再判空。
type ActionFunc func(ctx context.Context, in RunInput) (Result, error)

// RunInput 是一次动作调用的入参。
type RunInput struct {
	Config Config
	Args   map[string]any
}

// Result 是动作的真实执行结果：Message 面向用户，Details 供日志与排障。
type Result struct {
	Message string
	Details map[string]any
}

// Definition 是注册插件时提交的完整定义。
type Definition struct {
	Manifest Manifest
	Actions  map[string]ActionFunc
}

// Plugin 是对外可见的插件快照：不含函数，可安全交给 HTTP 层序列化。
type Plugin struct {
	ID          string
	Name        string
	Author      string
	Version     string
	Label       string
	Icon        string
	Description string
	Level       int
	Installed   bool
	Enabled     bool
	Config      Config
	// Fields 是配置项声明，供 HTTP 层生成表单或文档；切片是副本，
	// 调用方改动不会影响注册表（Options 内的元素仍是共享值，但注册后不再变）。
	Fields  []Field
	Actions []string
}

// record 是注册表内部的插件实例状态。
type record struct {
	manifest  Manifest
	actions   map[string]ActionFunc
	installed bool
	enabled   bool
	config    Config
}

// clone 复制一份记录，供「先改副本、再落盘、最后提交」的写入流程使用。
func (r *record) clone() *record {
	return &record{
		manifest:  r.manifest,
		actions:   r.actions,
		installed: r.installed,
		enabled:   r.enabled,
		config:    cloneConfig(r.config),
	}
}

// persisted 提取需要落盘的部分。
func (r *record) persisted() PersistedState {
	return PersistedState{
		Installed: r.installed,
		Enabled:   r.enabled,
		Config:    cloneConfig(r.config),
	}
}

// plugin 生成对外快照，动作名排序保证输出稳定。
func (r *record) plugin() Plugin {
	actions := make([]string, 0, len(r.actions))
	for name := range r.actions {
		actions = append(actions, name)
	}
	sort.Strings(actions)
	return Plugin{
		ID:          r.manifest.ID,
		Name:        r.manifest.Name,
		Author:      r.manifest.Author,
		Version:     r.manifest.Version,
		Label:       r.manifest.Label,
		Icon:        r.manifest.Icon,
		Description: r.manifest.Description,
		Level:       r.manifest.Level,
		Installed:   r.installed,
		Enabled:     r.enabled,
		Config:      cloneConfig(r.config),
		Fields:      append([]Field(nil), r.manifest.Fields...),
		Actions:     actions,
	}
}

// ConfigListener 在配置成功写入后收到通知，用来把配置翻译成运行时效果
// （例如按 interval_hours 重建周期任务）。它没有返回值：配置已经落盘，
// 让它把 UpdateConfig 变成失败会让调用方以为没保存；失败由实现方自己记日志。
type ConfigListener func(ctx context.Context, id string, config Config)

// Registry 是原生插件的注册表。所有写操作先落盘再生效：保存失败时内存状态保持
// 原样，避免「界面显示已启用、重启后又变回去」这种不可解释的状态。
type Registry struct {
	mu       sync.RWMutex
	order    []string
	records  map[string]*record
	store    StateStore
	listener ConfigListener
}

// NewRegistry 创建注册表。store 传 nil 表示不落盘（进程内有效）。
func NewRegistry(store StateStore) *Registry {
	return &Registry{
		records: make(map[string]*record),
		store:   store,
	}
}

// Register 注册一个插件。清单不合法或 ID 重复都会被拒绝。
func (r *Registry) Register(def Definition) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if err := r.validateDefinitionLocked(def); err != nil {
		return err
	}
	r.addLocked(def)
	return nil
}

// RegisterAll 批量注册：先整体校验，任何一个不合法就一个都不注册，
// 避免半批插件进入注册表后与持久化状态错位。
func (r *Registry) RegisterAll(defs []Definition) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	staged := make(map[string]struct{}, len(defs))
	for _, def := range defs {
		if err := r.validateDefinitionLocked(def); err != nil {
			return err
		}
		if _, dup := staged[def.Manifest.ID]; dup {
			return fmt.Errorf("%w: %s", ErrPluginExists, def.Manifest.ID)
		}
		staged[def.Manifest.ID] = struct{}{}
	}
	for _, def := range defs {
		r.addLocked(def)
	}
	return nil
}

func (r *Registry) validateDefinitionLocked(def Definition) error {
	if err := def.Manifest.validate(); err != nil {
		return err
	}
	if _, exists := r.records[def.Manifest.ID]; exists {
		return fmt.Errorf("%w: %s", ErrPluginExists, def.Manifest.ID)
	}
	for name, action := range def.Actions {
		if name == "" {
			return fmt.Errorf("%w: 插件 %s 存在空动作名", ErrInvalidManifest, def.Manifest.ID)
		}
		// nil 动作是合法的：表示能力尚未接线，调用时返回 ErrServiceUnavailable。
		_ = action
	}
	return nil
}

func (r *Registry) addLocked(def Definition) {
	r.records[def.Manifest.ID] = &record{
		manifest: def.Manifest,
		actions:  def.Actions,
		config:   defaultsConfig(def.Manifest),
	}
	r.order = append(r.order, def.Manifest.ID)
}

// Restore 读取持久化状态并套用到已注册的插件上。
// 清单变更导致旧配置不合法时，该插件回退到默认配置并把原因返回给调用方，
// 而不是让整张表加载失败。
func (r *Registry) Restore(ctx context.Context) error {
	if r.store == nil {
		return nil
	}
	state, err := r.store.Load(ctx)
	if err != nil {
		return fmt.Errorf("读取插件状态失败: %w", err)
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	var problems []error
	for id, entry := range state {
		rec, ok := r.records[id]
		if !ok {
			// 插件被移除后遗留的状态：保留数据、不报错，等插件回来时仍可恢复。
			continue
		}
		config, err := normalizeConfig(rec.manifest, entry.Config)
		if err != nil {
			config = defaultsConfig(rec.manifest)
			problems = append(problems, fmt.Errorf("插件 %s 的已存配置已失效，回退默认值: %w", id, err))
		}
		rec.config = config
		if entry.Installed {
			rec.installed = true
			rec.enabled = entry.Enabled
		}
	}
	if len(problems) > 0 {
		return errors.Join(problems...)
	}
	return nil
}

// List 按注册顺序返回全部插件快照。
func (r *Registry) List() []Plugin {
	r.mu.RLock()
	defer r.mu.RUnlock()
	plugins := make([]Plugin, 0, len(r.order))
	for _, id := range r.order {
		plugins = append(plugins, r.records[id].plugin())
	}
	return plugins
}

// Get 返回单个插件快照。
func (r *Registry) Get(id string) (Plugin, error) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	rec, ok := r.records[id]
	if !ok {
		return Plugin{}, fmt.Errorf("%w: %s", ErrPluginNotFound, id)
	}
	return rec.plugin(), nil
}

// Install 安装插件：已安装时是空操作，便于前端重复点击。
func (r *Registry) Install(ctx context.Context, id string) error {
	return r.update(ctx, id, func(rec *record) error {
		rec.installed = true
		return nil
	})
}

// SetConfigListener 安装配置变更回调；传 nil 表示取消。
func (r *Registry) SetConfigListener(listener ConfigListener) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.listener = listener
}

// notifyConfig 在锁外回调，避免实现方反过来读注册表时死锁。
func (r *Registry) notifyConfig(ctx context.Context, id string, config Config) {
	r.mu.RLock()
	listener := r.listener
	r.mu.RUnlock()
	if listener == nil {
		return
	}
	listener(ctx, id, config)
}

// Uninstall 卸载插件：同时关闭它并清空配置，避免残留配置在重装后意外生效。
func (r *Registry) Uninstall(ctx context.Context, id string) error {
	var applied Config
	if err := r.update(ctx, id, func(rec *record) error {
		if !rec.installed {
			return nil
		}
		rec.installed = false
		rec.enabled = false
		rec.config = defaultsConfig(rec.manifest)
		applied = cloneConfig(rec.config)
		return nil
	}); err != nil {
		return err
	}
	// 卸载把配置打回默认值，周期任务也要跟着回到默认，否则会停在旧间隔上。
	// 本来就没装时 applied 为 nil：没有变化就不该回调。
	if applied != nil {
		r.notifyConfig(ctx, id, applied)
	}
	return nil
}

// Enable 启用插件。未安装的插件不允许启用。
func (r *Registry) Enable(ctx context.Context, id string) error {
	return r.update(ctx, id, func(rec *record) error {
		if !rec.installed {
			return fmt.Errorf("%w: %s", ErrPluginNotInstalled, id)
		}
		rec.enabled = true
		return nil
	})
}

// Disable 停用插件。未安装的插件同样可以调用，语义是「确保它是关的」。
func (r *Registry) Disable(ctx context.Context, id string) error {
	return r.update(ctx, id, func(rec *record) error {
		rec.enabled = false
		return nil
	})
}

// UpdateConfig 全量替换插件配置。未提供的可选项会丢弃，未提供且无默认值的
// 必填项会被拒绝。
func (r *Registry) UpdateConfig(ctx context.Context, id string, raw map[string]any) error {
	var applied Config
	if err := r.update(ctx, id, func(rec *record) error {
		if !rec.installed {
			return fmt.Errorf("%w: %s", ErrPluginNotInstalled, id)
		}
		config, err := normalizeConfig(rec.manifest, raw)
		if err != nil {
			return err
		}
		rec.config = config
		applied = cloneConfig(config)
		return nil
	}); err != nil {
		return err
	}
	r.notifyConfig(ctx, id, applied)
	return nil
}

// Run 执行插件动作。检查顺序固定：存在 → 已安装 → 已启用 → 动作存在 → 已接线，
// 这样调用方拿到的错误永远指向真正缺失的那一环。
func (r *Registry) Run(ctx context.Context, id, action string, args map[string]any) (Result, error) {
	if ctx == nil {
		return Result{}, fmt.Errorf("插件 %s 的动作 %s: 上下文不能为空", id, action)
	}
	r.mu.RLock()
	rec, ok := r.records[id]
	if !ok {
		r.mu.RUnlock()
		return Result{}, fmt.Errorf("%w: %s", ErrPluginNotFound, id)
	}
	installed, enabled := rec.installed, rec.enabled
	config := cloneConfig(rec.config)
	handler, hasAction := rec.actions[action]
	r.mu.RUnlock()
	if !installed {
		return Result{}, fmt.Errorf("%w: %s", ErrPluginNotInstalled, id)
	}
	if !enabled {
		return Result{}, fmt.Errorf("%w: %s", ErrPluginDisabled, id)
	}
	if !hasAction {
		return Result{}, fmt.Errorf("%w: 插件 %s 没有动作 %s", ErrUnknownAction, id, action)
	}
	if handler == nil {
		return Result{}, fmt.Errorf("%w: 插件 %s 的动作 %s", ErrServiceUnavailable, id, action)
	}
	return handler(ctx, RunInput{Config: config, Args: args})
}

// update 是唯一的写入路径：改副本 → 落盘 → 提交。
func (r *Registry) update(ctx context.Context, id string, apply func(*record) error) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	rec, ok := r.records[id]
	if !ok {
		return fmt.Errorf("%w: %s", ErrPluginNotFound, id)
	}
	candidate := rec.clone()
	if err := apply(candidate); err != nil {
		return err
	}
	if r.store != nil {
		state := make(map[string]PersistedState, len(r.records))
		for pluginID, entry := range r.records {
			state[pluginID] = entry.persisted()
		}
		state[id] = candidate.persisted()
		if err := r.store.Save(ctx, state); err != nil {
			return fmt.Errorf("保存插件状态失败: %w", err)
		}
	}
	r.records[id] = candidate
	return nil
}
