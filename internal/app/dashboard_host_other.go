//go:build !windows

package app

import (
	"errors"
	"os"
	"strconv"
	"strings"
)

// errHostStatsUnavailable 表示本平台既没有 /proc，也没有对应的平台实现。
//
// Linux/BSD/macOS 的生产环境都走 /proc；走到这里说明连 /proc 都读不到，
// 此时宁可返回错误让接口回零值并留 WARN，也不要编造内存、CPU、网络的数字。
var errHostStatsUnavailable = errors.New("本平台没有 /proc，且未提供平台实现")

func hostMemoryInfo() (total, available, cached int64, err error) {
	return 0, 0, 0, errHostStatsUnavailable
}

func hostCPUSample() (cpuSample, error) {
	return cpuSample{}, errHostStatsUnavailable
}

func hostNetSample() (netSample, error) {
	return netSample{}, errHostStatsUnavailable
}

// processRSSBytes 读取本进程常驻内存：/proc/self/statm 的第二个字段是常驻页数。
//
// V2 仪表盘的 `used` 是 psutil.Process().memory_info().rss，口径与这里一致
// （RSS 不含 page cache，和整机已用不是一回事）。
func processRSSBytes() (int64, error) {
	raw, err := os.ReadFile("/proc/self/statm")
	if err != nil {
		return 0, errHostStatsUnavailable
	}
	fields := strings.Fields(string(raw))
	if len(fields) < 2 {
		return 0, errHostStatsUnavailable
	}
	pages, convErr := strconv.ParseInt(fields[1], 10, 64)
	if convErr != nil || pages < 0 {
		return 0, errHostStatsUnavailable
	}
	return pages * int64(os.Getpagesize()), nil
}
