package pluginengine

import (
	"context"
	"errors"
	"strings"
	"sync"
)

// PersistedState 是一个插件的可持久化状态：清单是代码的一部分，不落库；
// 落库的只有「装没装、开没开、配置是什么」。
type PersistedState struct {
	Installed bool   `json:"installed"`
	Enabled   bool   `json:"enabled"`
	Config    Config `json:"config"`
}

// StateStore 负责把插件状态落盘。用 database.Store 的设置表实现它即可
// （GetSetting/SetSetting 已支持任意 JSON），本包不依赖具体存储。
type StateStore interface {
	// Load 返回上一次保存的完整状态；键是插件 ID。
	Load(ctx context.Context) (map[string]PersistedState, error)
	// Save 全量覆盖当前状态。
	Save(ctx context.Context, state map[string]PersistedState) error
}

// MemoryStore 是进程内的 StateStore，用于测试与「不落盘」的装配方式。
type MemoryStore struct {
	mu    sync.Mutex
	state map[string]PersistedState
}

// NewMemoryStore 创建一个空的内存状态存储。
func NewMemoryStore() *MemoryStore {
	return &MemoryStore{state: map[string]PersistedState{}}
}

// Load 返回状态副本，调用方改动不会影响存储。
func (s *MemoryStore) Load(_ context.Context) (map[string]PersistedState, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	copied := make(map[string]PersistedState, len(s.state))
	for id, state := range s.state {
		copied[id] = PersistedState{
			Installed: state.Installed,
			Enabled:   state.Enabled,
			Config:    cloneConfig(state.Config),
		}
	}
	return copied, nil
}

// Save 全量替换存储内容。
func (s *MemoryStore) Save(_ context.Context, state map[string]PersistedState) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	copied := make(map[string]PersistedState, len(state))
	for id, entry := range state {
		copied[id] = PersistedState{
			Installed: entry.Installed,
			Enabled:   entry.Enabled,
			Config:    cloneConfig(entry.Config),
		}
	}
	s.state = copied
	return nil
}

// cloneConfig 复制配置，避免调用方与注册表共享同一个 map。
func cloneConfig(config Config) Config {
	if config == nil {
		return nil
	}
	copied := make(Config, len(config))
	for key, value := range config {
		copied[key] = value
	}
	return copied
}

// DefaultStateSettingKey 是设置表里存插件状态的键。带 v1 前缀：将来形状变了就换键，
// 老数据读不到会回退默认值，而不是让新代码去猜旧 JSON。
const DefaultStateSettingKey = "PluginState:v1"

// SettingsStore 是设置表的读写契约，database.Store 已经满足。
// 用接口而不是具体类型，本包就不必依赖数据库实现，单测用假实现即可。
type SettingsStore interface {
	GetSetting(ctx context.Context, key string, target any) (bool, error)
	SetSetting(ctx context.Context, key string, value any, encrypted bool) error
}

type settingsStateStore struct {
	store SettingsStore
	key   string
}

// NewSettingsStateStore 用设置表实现 StateStore。key 为空时使用 DefaultStateSettingKey。
func NewSettingsStateStore(store SettingsStore, key string) (StateStore, error) {
	if store == nil {
		return nil, errors.New("设置存储未提供")
	}
	if strings.TrimSpace(key) == "" {
		key = DefaultStateSettingKey
	}
	return &settingsStateStore{store: store, key: key}, nil
}

// Load 读出整条设置记录；没有记录时返回空表而不是 nil，调用方不需要判空。
func (s *settingsStateStore) Load(ctx context.Context) (map[string]PersistedState, error) {
	state := make(map[string]PersistedState)
	found, err := s.store.GetSetting(ctx, s.key, &state)
	if err != nil {
		return nil, err
	}
	if !found || state == nil {
		return map[string]PersistedState{}, nil
	}
	return state, nil
}

// Save 全量覆盖：插件状态是一张很小的表，覆盖写比逐条合并更容易保证一致。
func (s *settingsStateStore) Save(ctx context.Context, state map[string]PersistedState) error {
	if state == nil {
		state = map[string]PersistedState{}
	}
	return s.store.SetSetting(ctx, s.key, state, false)
}
