//go:build windows

package storage

import "golang.org/x/sys/windows"

// filesystemSpace 返回路径所在卷的总容量与可用字节。
func filesystemSpace(path string) (int64, int64, bool) {
	pathPtr, err := windows.UTF16PtrFromString(path)
	if err != nil {
		return 0, 0, false
	}
	var freeBytesAvailable, totalBytes, totalFreeBytes uint64
	if err := windows.GetDiskFreeSpaceEx(pathPtr, &freeBytesAvailable, &totalBytes, &totalFreeBytes); err != nil {
		return 0, 0, false
	}
	if totalBytes == 0 {
		return 0, 0, false
	}
	return int64(totalBytes), int64(freeBytesAvailable), true
}
