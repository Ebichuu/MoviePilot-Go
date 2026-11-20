package database

import (
	"encoding/json"
	"fmt"
	"reflect"
	"strings"
	"testing"
	"time"
)

func TestSiteStatisticJSONContract(t *testing.T) {
	t.Parallel()

	encoded, err := json.Marshal(SiteStatistic{
		Domain:       "example.com",
		Success:      2,
		Fail:         1,
		Seconds:      3,
		LastState:    0,
		LastModified: "2026-08-11 10:00:00",
		Note:         map[string]int{"2026-08-11 10:00:00": 3},
	})
	if err != nil {
		t.Fatalf("编码 V2 站点统计: %v", err)
	}
	const want = `{"domain":"example.com","success":2,"fail":1,"seconds":3,"lst_state":0,"lst_mod_date":"2026-08-11 10:00:00","note":{"2026-08-11 10:00:00":3}}`
	if string(encoded) != want {
		t.Fatalf("SiteStatistic JSON = %s, want %s", encoded, want)
	}
}

func TestScanSiteStatistic(t *testing.T) {
	t.Parallel()

	now := "2026-08-12 12:30:00"
	row := siteStatisticRowFunc(func(dest ...any) error {
		*dest[0].(*string) = "tracker.example"
		*dest[1].(*int) = 2
		*dest[2].(*int) = 1
		*dest[3].(*int) = 3
		*dest[4].(*int) = 0
		*dest[5].(*string) = now
		*dest[6].(*[]byte) = []byte(`{"2026-08-12 12:30:00":3}`)
		return nil
	})
	got, err := scanSiteStatistic(row)
	if err != nil {
		t.Fatalf("scanSiteStatistic() error = %v", err)
	}
	if got.Domain != "tracker.example" || got.Success != 2 || got.Fail != 1 || got.Note[now] != 3 {
		t.Fatalf("scanSiteStatistic() = %#v", got)
	}

	empty, err := scanSiteStatistic(siteStatisticRowFunc(func(dest ...any) error {
		if err := row.Scan(dest...); err != nil {
			return err
		}
		*dest[6].(*[]byte) = []byte("null")
		return nil
	}))
	if err != nil || empty.Note == nil || len(empty.Note) != 0 {
		t.Fatalf("scanSiteStatistic(null note) = %#v, %v; want empty non-nil map", empty, err)
	}

	if _, err := scanSiteStatistic(siteStatisticRowFunc(func(dest ...any) error {
		if err := row.Scan(dest...); err != nil {
			return err
		}
		*dest[6].(*[]byte) = []byte("{")
		return nil
	})); err == nil || !strings.Contains(err.Error(), "tracker.example") {
		t.Fatalf("scanSiteStatistic(corrupt note) error = %v", err)
	}
}

func TestApplySiteTestResult(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		current  SiteStatistic
		success  bool
		elapsed  int
		modified string
		want     SiteStatistic
	}{
		{
			name:     "首次成功最少记录一秒",
			current:  SiteStatistic{Domain: "example.com"},
			success:  true,
			elapsed:  0,
			modified: "2026-08-11 10:00:00",
			want: SiteStatistic{
				Domain:       "example.com",
				Success:      1,
				Seconds:      1,
				LastState:    0,
				LastModified: "2026-08-11 10:00:00",
				Note:         map[string]int{"2026-08-11 10:00:00": 1},
			},
		},
		{
			name: "累计成功并刷新平均耗时",
			current: SiteStatistic{
				Domain:       "example.com",
				Success:      2,
				Fail:         1,
				Seconds:      3,
				LastState:    1,
				LastModified: "2026-08-11 09:00:00",
				Note:         map[string]int{"2026-08-11 09:00:00": 3},
			},
			success:  true,
			elapsed:  5,
			modified: "2026-08-11 10:00:00",
			want: SiteStatistic{
				Domain:       "example.com",
				Success:      3,
				Fail:         1,
				Seconds:      4,
				LastState:    0,
				LastModified: "2026-08-11 10:00:00",
				Note: map[string]int{
					"2026-08-11 09:00:00": 3,
					"2026-08-11 10:00:00": 5,
				},
			},
		},
		{
			name: "失败只累计失败数和最后状态",
			current: SiteStatistic{
				Domain:       "example.com",
				Success:      4,
				Fail:         2,
				Seconds:      6,
				LastState:    0,
				LastModified: "2026-08-11 09:00:00",
				Note:         map[string]int{"2026-08-11 09:00:00": 6},
			},
			success:  false,
			elapsed:  99,
			modified: "2026-08-11 10:00:00",
			want: SiteStatistic{
				Domain:       "example.com",
				Success:      4,
				Fail:         3,
				Seconds:      6,
				LastState:    1,
				LastModified: "2026-08-11 10:00:00",
				Note:         map[string]int{"2026-08-11 09:00:00": 6},
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got := applySiteTestResult(tt.current, tt.success, tt.elapsed, tt.modified)
			if !reflect.DeepEqual(got, tt.want) {
				t.Fatalf("applySiteTestResult() = %#v, want %#v", got, tt.want)
			}
		})
	}
}

type siteStatisticRowFunc func(...any) error

func (scan siteStatisticRowFunc) Scan(dest ...any) error {
	return scan(dest...)
}

func TestAddSiteTimingSampleTrimsAndAveragesRecentTen(t *testing.T) {
	t.Parallel()

	samples := make(map[string]int)
	for hour := 0; hour < 10; hour++ {
		samples[fmt.Sprintf("2026-08-10 %02d:00:00", hour)] = hour + 1
	}
	samples["2026-08-10 10:00:00"] = 11

	got, average := addSiteTimingSample(samples, "2026-08-10 11:00:00", 12)
	if len(got) != maxSiteTimingSamples {
		t.Fatalf("样本数 = %d, want %d", len(got), maxSiteTimingSamples)
	}
	if _, exists := got["2026-08-10 00:00:00"]; exists {
		t.Fatal("最旧样本未被裁剪")
	}
	if _, exists := got["2026-08-10 01:00:00"]; exists {
		t.Fatal("第二旧样本未被裁剪")
	}
	if got["2026-08-10 11:00:00"] != 12 {
		t.Fatalf("最新样本 = %d, want 12", got["2026-08-10 11:00:00"])
	}
	if average != 7 {
		t.Fatalf("平均耗时 = %d, want 7", average)
	}
	if len(samples) != 11 {
		t.Fatalf("输入样本被原地修改，长度 = %d, want 11", len(samples))
	}
}

func TestAddSiteTimingSamplePreservesSameSecondSamples(t *testing.T) {
	t.Parallel()

	const modified = "2026-08-11 10:00:00"
	samples := map[string]int{modified: 2}
	got, average := addSiteTimingSample(samples, modified, 6)

	want := map[string]int{
		modified:          2,
		modified + ".001": 6,
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("同秒样本 = %#v, want %#v", got, want)
	}
	if average != 4 {
		t.Fatalf("同秒样本平均耗时 = %d, want 4", average)
	}
	if !reflect.DeepEqual(samples, map[string]int{modified: 2}) {
		t.Fatalf("输入样本被原地修改: %#v", samples)
	}
}

func TestNextSiteTimingKey(t *testing.T) {
	t.Parallel()

	const modified = "2026-08-11 10:00:00"
	tests := []struct {
		name    string
		samples map[string]int
		want    string
	}{
		{
			name: "首次使用秒级基础键",
			want: modified,
		},
		{
			name:    "基础键碰撞后增加毫秒序号",
			samples: map[string]int{modified: 1},
			want:    modified + ".001",
		},
		{
			name: "基础键已裁剪时继续现有最大序号",
			samples: map[string]int{
				modified + ".007": 2,
				modified + ".009": 4,
			},
			want: modified + ".010",
		},
		{
			name: "忽略其他秒和非法后缀",
			samples: map[string]int{
				"2026-08-11 10:00:01": 3,
				modified + ".bad":     4,
			},
			want: modified,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if got := nextSiteTimingKey(tt.samples, modified); got != tt.want {
				t.Fatalf("nextSiteTimingKey() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestFormatSiteStatisticTimeUsesConfiguredTimezone(t *testing.T) {
	t.Parallel()

	shanghai, err := time.LoadLocation("Asia/Shanghai")
	if err != nil {
		t.Fatalf("加载测试时区: %v", err)
	}
	instant := time.Date(2026, time.August, 11, 2, 3, 4, 0, time.UTC)
	if got, want := formatSiteStatisticTime(instant, shanghai), "2026-08-11 10:03:04"; got != want {
		t.Fatalf("formatSiteStatisticTime() = %q, want %q", got, want)
	}
}

func TestNormalizeStatisticDomain(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name   string
		domain string
		want   string
	}{
		{name: "去除空白并转小写", domain: "  Example.COM  ", want: "example.com"},
		{name: "空域名", domain: "  ", want: ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if got := normalizeStatisticDomain(tt.domain); got != tt.want {
				t.Fatalf("normalizeStatisticDomain(%q) = %q, want %q", tt.domain, got, tt.want)
			}
		})
	}
}

func TestElapsedSeconds(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		elapsed time.Duration
		want    int
	}{
		{name: "零耗时", elapsed: 0, want: 1},
		{name: "负耗时", elapsed: -time.Second, want: 1},
		{name: "不足一秒", elapsed: 999 * time.Millisecond, want: 1},
		{name: "按完整秒截断", elapsed: 2900 * time.Millisecond, want: 2},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if got := elapsedSeconds(tt.elapsed); got != tt.want {
				t.Fatalf("elapsedSeconds(%s) = %d, want %d", tt.elapsed, got, tt.want)
			}
		})
	}
}

func FuzzAddSiteTimingSample(f *testing.F) {
	f.Add("2026-08-11 10:00:00", 0)
	f.Add("2026-08-11 10:00:01", 7)
	f.Fuzz(func(t *testing.T, modified string, seconds int) {
		samples := make(map[string]int)
		for index := 0; index < 12; index++ {
			samples[fmt.Sprintf("2026-08-10 %02d:00:00", index)] = index + 1
		}
		original := cloneSiteTimingSamples(samples)
		got, average := addSiteTimingSample(samples, modified, seconds)
		if len(got) > maxSiteTimingSamples {
			t.Fatalf("样本数 = %d, 超过上限 %d", len(got), maxSiteTimingSamples)
		}
		if !reflect.DeepEqual(samples, original) {
			t.Fatalf("输入样本被修改: got %#v, want %#v", samples, original)
		}
		total := 0
		for key, value := range got {
			if value < 1 {
				t.Fatalf("样本 %q 的耗时 = %d, want >= 1", key, value)
			}
			total += value
		}
		if len(got) > 0 && average != total/len(got) {
			t.Fatalf("平均耗时 = %d, want %d", average, total/len(got))
		}
	})
}

func FuzzNormalizeStatisticDomain(f *testing.F) {
	f.Add("  Example.COM  ")
	f.Add("")
	f.Add("站点.Example")
	f.Fuzz(func(t *testing.T, domain string) {
		normalized := normalizeStatisticDomain(domain)
		if got := normalizeStatisticDomain(normalized); got != normalized {
			t.Fatalf("域名规范化不幂等: first %q, second %q", normalized, got)
		}
	})
}
