//go:build linux

package storage

import "golang.org/x/sys/unix"

// filesystemSpace 返回路径所在文件系统的总容量与可用字节（V2 本地存储
// `usage()` 取自 psutil 的同口径字节值）。
func filesystemSpace(path string) (int64, int64, bool) {
	var stat unix.Statfs_t
	if err := unix.Statfs(path, &stat); err != nil {
		return 0, 0, false
	}
	blockSize := int64(stat.Bsize)
	if blockSize <= 0 {
		return 0, 0, false
	}
	total := int64(stat.Blocks) * blockSize
	available := int64(stat.Bavail) * blockSize
	if total <= 0 {
		return 0, 0, false
	}
	if available < 0 {
		available = 0
	}
	return total, available, true
}
