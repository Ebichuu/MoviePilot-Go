package metadata

import (
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func TestCategoryServiceCreatesDefaultsAndPreservesOrder(t *testing.T) {
	directory := t.TempDir()
	service := NewCategoryService(directory)
	if _, err := os.Stat(filepath.Join(directory, "category.yaml")); err != nil {
		t.Fatalf("默认 category.yaml 未生成: %v", err)
	}
	want := map[string][]string{
		"电影":  {"动画电影", "华语电影", "外语电影"},
		"电视剧": {"国漫", "日番", "纪录片", "儿童", "综艺", "国产剧", "欧美剧", "日韩剧", "未分类"},
	}
	if got := service.MediaCategories(); !reflect.DeepEqual(got, want) {
		t.Fatalf("默认分类顺序 = %#v, want %#v", got, want)
	}
}

func TestCategoryConfigJSONAndYAMLRoundTripPreservesOrder(t *testing.T) {
	original := CategoryConfig{
		Movie: []CategoryRule{{Name: "第二", Conditions: []CategoryCondition{{Attribute: "release_year", Value: "2020-2024"}}}, {Name: "兜底"}},
		TV:    []CategoryRule{{Name: "电视剧", Conditions: []CategoryCondition{{Attribute: "origin_country", Value: "CN,TW"}}}},
	}
	content, err := json.Marshal(original)
	if err != nil {
		t.Fatal(err)
	}
	var decoded CategoryConfig
	if err := json.Unmarshal(content, &decoded); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(decoded, original) {
		t.Fatalf("JSON round trip = %#v, want %#v", decoded, original)
	}

	yamlContent, err := marshalCategoryYAML(original)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "category.yaml")
	if err := os.WriteFile(path, yamlContent, 0o600); err != nil {
		t.Fatal(err)
	}
	loaded, err := readCategoryConfig(path)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(loaded, original) {
		t.Fatalf("YAML round trip = %#v, want %#v", loaded, original)
	}
}

func TestCategoryServiceSaveAndReload(t *testing.T) {
	directory := t.TempDir()
	service := NewCategoryService(directory)
	custom := CategoryConfig{Movie: []CategoryRule{
		{Name: "近年", Conditions: []CategoryCondition{{Attribute: "release_year", Value: "2024-2026"}}},
		{Name: "其他"},
	}}
	if err := service.SaveCategoryConfig(custom); err != nil {
		t.Fatal(err)
	}
	if got := service.Category("电影", Media{Year: 2025}); got != "近年" {
		t.Fatalf("保存后分类 = %q, want 近年", got)
	}
	if got := service.MediaCategories()["电影"]; !reflect.DeepEqual(got, []string{"近年", "其他"}) {
		t.Fatalf("保存后分类列表 = %#v", got)
	}

	external := "movie:\n  华语:\n    original_language: 'zh'\n  外语:\ntv: null\n"
	if err := os.WriteFile(filepath.Join(directory, "category.yaml"), []byte(external), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := service.Reload(); err != nil {
		t.Fatal(err)
	}
	loadedJSON, err := json.Marshal(service.LoadCategoryConfig())
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(loadedJSON), `"tv":null`) {
		t.Fatalf("外部 null 配置未保留: %s", loadedJSON)
	}
	if got := service.Category("movie", Media{OriginalLanguage: "en"}); got != "外语" {
		t.Fatalf("外部重载后 fallback 分类 = %q, want 外语", got)
	}
}

func TestCategoryServiceDamagedFileLoadIsEmptyAndReloadKeepsSnapshot(t *testing.T) {
	directory := t.TempDir()
	service := NewCategoryService(directory)
	if err := service.SaveCategoryConfig(CategoryConfig{Movie: []CategoryRule{{Name: "旧分类"}}}); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(directory, "category.yaml"), []byte("movie: ["), 0o600); err != nil {
		t.Fatal(err)
	}
	if got := service.LoadCategoryConfig(); !reflect.DeepEqual(got, CategoryConfig{}) {
		t.Fatalf("损坏文件 load = %#v, want empty", got)
	}
	if err := service.Reload(); err == nil {
		t.Fatal("损坏文件 Reload 未返回错误")
	}
	if got := service.Category("movie", Media{}); got != "旧分类" {
		t.Fatalf("损坏重载覆盖了旧快照: %q", got)
	}
}

func TestCategoryServiceConcurrentReloadCannotReplaceNewSaveWithOldSnapshot(t *testing.T) {
	directory := t.TempDir()
	service := NewCategoryService(directory)
	reloadStarted := make(chan struct{})
	allowReload := make(chan struct{})
	service.load = func(string) (CategoryConfig, error) {
		close(reloadStarted)
		<-allowReload
		return CategoryConfig{Movie: []CategoryRule{{Name: "旧快照"}}}, nil
	}
	reloadDone := make(chan error, 1)
	go func() { reloadDone <- service.Reload() }()
	<-reloadStarted
	if service.mu.TryLock() {
		service.mu.Unlock()
		close(allowReload)
		<-reloadDone
		t.Fatal("Reload 加载磁盘时未持有写锁")
	}

	saveStarted := make(chan struct{})
	service.beforeSaveLock = func() { close(saveStarted) }
	saveDone := make(chan error, 1)
	go func() {
		saveDone <- service.SaveCategoryConfig(CategoryConfig{Movie: []CategoryRule{{Name: "新快照"}}})
	}()
	<-saveStarted
	select {
	case err := <-saveDone:
		close(allowReload)
		<-reloadDone
		t.Fatalf("Reload 持锁读取时 Save 提前完成: %v", err)
	default:
	}
	close(allowReload)
	if err := <-reloadDone; err != nil {
		t.Fatal(err)
	}
	if err := <-saveDone; err != nil {
		t.Fatal(err)
	}
	if got := service.Category("movie", Media{}); got != "新快照" {
		t.Fatalf("并发重载覆盖了保存结果: %q", got)
	}
}

func TestCategoryServiceRejectsExpandedYAMLBeforeChangingDiskOrSnapshot(t *testing.T) {
	directory := t.TempDir()
	service := NewCategoryService(directory)
	if err := service.SaveCategoryConfig(CategoryConfig{Movie: []CategoryRule{{Name: "原分类"}}}); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(directory, "category.yaml")
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	oversized := CategoryConfig{Movie: []CategoryRule{{
		Name: "超限", Conditions: []CategoryCondition{{Attribute: "original_language", Value: strings.Repeat("'", maxCategoryConfigBytes/2+1)}},
	}}}
	if err := service.SaveCategoryConfig(oversized); !errors.Is(err, ErrInvalidCategoryConfig) {
		t.Fatalf("SaveCategoryConfig() error = %v, want ErrInvalidCategoryConfig", err)
	}
	after, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(after, before) {
		t.Fatal("超限保存改变了磁盘配置")
	}
	if got := service.Category("movie", Media{}); got != "原分类" {
		t.Fatalf("超限保存改变了运行快照: %q", got)
	}
	if temporary, err := filepath.Glob(filepath.Join(directory, ".category-*.yaml")); err != nil || len(temporary) != 0 {
		t.Fatalf("超限保存创建了临时文件: files=%v err=%v", temporary, err)
	}
}

func TestReadCategoryConfigRejectsInvalidOrOversizedInput(t *testing.T) {
	tests := []struct {
		name string
		body string
	}{
		{name: "顶层序列", body: "- movie"},
		{name: "规则序列", body: "movie: [one]\ntv: {}"},
		{name: "条件序列", body: "movie:\n  one: [genre_ids]\ntv: {}"},
		{name: "超过大小限制", body: strings.Repeat("x", maxCategoryConfigBytes+1)},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "category.yaml")
			if err := os.WriteFile(path, []byte(test.body), 0o600); err != nil {
				t.Fatal(err)
			}
			if _, err := readCategoryConfig(path); !errors.Is(err, ErrInvalidCategoryConfig) {
				t.Fatalf("error = %v, want ErrInvalidCategoryConfig", err)
			}
		})
	}
}
