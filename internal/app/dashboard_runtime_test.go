package app

import "testing"

func TestCPUPercent(t *testing.T) {
	tests := []struct {
		name     string
		previous cpuSample
		current  cpuSample
		want     float64
	}{
		{name: "没有新增 tick 时为零", previous: cpuSample{total: 100, idle: 90}, current: cpuSample{total: 100, idle: 90}, want: 0},
		{name: "全部空闲时为零", previous: cpuSample{total: 100}, current: cpuSample{total: 200, idle: 100}, want: 0},
		{name: "半忙", previous: cpuSample{total: 100}, current: cpuSample{total: 200, idle: 50}, want: 50},
		{name: "全忙", previous: cpuSample{total: 0}, current: cpuSample{total: 100, idle: 0}, want: 100},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := cpuPercent(tt.previous, tt.current); got != tt.want {
				t.Fatalf("cpuPercent() = %v, want %v", got, tt.want)
			}
		})
	}
}
