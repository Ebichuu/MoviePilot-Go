package database

import (
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"testing"
	"time"

	"github.com/google/uuid"
)

// anyScanRow 是按位置给行扫描目标赋值的通用替身，用于在不伪造 I/O 错误
// 的前提下构造「值本身非法」的存储行。
type anyScanRow struct {
	values []any
}

func (r anyScanRow) Scan(dest ...any) error {
	if len(dest) != len(r.values) {
		return errors.New("列数与目标不匹配")
	}
	for index, value := range r.values {
		target := reflect.ValueOf(dest[index])
		if target.Kind() != reflect.Ptr || target.IsNil() {
			return errors.New("扫描目标不是指针")
		}
		element := target.Elem()
		if value == nil {
			element.SetZero()
			continue
		}
		source := reflect.ValueOf(value)
		switch {
		case source.Type().AssignableTo(element.Type()):
			element.Set(source)
		case source.Type().ConvertibleTo(element.Type()):
			element.Set(source.Convert(element.Type()))
		default:
			return fmt.Errorf("无法写入 %s", element.Type())
		}
	}
	return nil
}

func TestScanRSSItemRejectsInvalidResource(t *testing.T) {
	now := time.Now().UTC()
	row := anyScanRow{values: []any{
		int64(1), int64(2), "item-key", "", "", "",
		"https://tracker.example/detail/1", "https://tracker.example/download/1",
		"标题", "", 0, &now, json.RawMessage("{"),
		now, now, "pending", 0, "",
		(*time.Time)(nil), (*uuid.UUID)(nil), (*time.Time)(nil), now,
	}}
	if _, err := (&Store{}).scanRSSItem(row); !errors.Is(err, ErrInvalidRSSInput) {
		t.Fatalf("非法资源载荷 error = %v", err)
	}
}

func TestScanOrganizationSuccessSnapshotRejectsNonCanonicalFiles(t *testing.T) {
	files, err := json.Marshal([]map[string]any{{
		"ordinal": 1, "role": "video", "size_bytes": 10,
		"season": 1, "episodes": []int{1}, "resource_term": "WEB-DL",
	}})
	if err != nil {
		t.Fatalf("编码文件事实: %v", err)
	}
	row := anyScanRow{values: []any{
		uuid.New(), "group", uuid.New(), 1, false, "alice", true, false, false,
		"tmdb", "1", "2026", "tv", "电视剧", 8.5, "", "", files,
	}}
	if _, err := scanOrganizationSuccessSnapshot(row); !errors.Is(err, errStoredOrganizationSuccessSnapshotInvalid) {
		t.Fatalf("非规范文件事实 error = %v", err)
	}
}
