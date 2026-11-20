package database

import (
	"errors"
	"testing"
)

// siteScanRow 是按列复刻 scanPublicSite 目标类型的行替身。
type siteScanRow struct {
	values []any
}

func (r siteScanRow) Scan(dest ...any) error {
	if len(dest) != len(r.values) {
		return errors.New("列数与目标不匹配")
	}
	for index, value := range r.values {
		switch target := dest[index].(type) {
		case *int:
			*target = value.(int)
		case *string:
			*target = value.(string)
		case *bool:
			*target = value.(bool)
		case *[]byte:
			*target = value.([]byte)
		default:
			return errors.New("不支持的目标类型")
		}
	}
	return nil
}

// TestScanPublicSiteRejectsBadConfig 覆盖公开站点投影里的两类拒绝：
// 配置不是合法 JSON、以及公开配置包含敏感字段。
func TestScanPublicSiteRejectsBadConfig(t *testing.T) {
	invalidJSON := siteScanRow{values: []any{1, "示例站", "example.com", "https://example.com", "nexusphp", true, []byte("{")}}
	if _, err := scanPublicSite(invalidJSON); err == nil {
		t.Fatal("非法 JSON 配置应报错")
	}

	sensitive := siteScanRow{values: []any{2, "示例站", "example.com", "https://example.com", "nexusphp", true, []byte(`{"cookie":"abc"}`)}}
	if _, err := scanPublicSite(sensitive); err == nil {
		t.Fatal("敏感字段应报错")
	}

	ok := siteScanRow{values: []any{3, "示例站", "example.com", "https://example.com", "nexusphp", true, []byte(`{"pri":1}`)}}
	site, err := scanPublicSite(ok)
	if err != nil {
		t.Fatalf("合法配置 error = %v", err)
	}
	if site.ID != 3 || site.Config["pri"] != float64(1) {
		t.Fatalf("站点 = %+v", site)
	}
}
