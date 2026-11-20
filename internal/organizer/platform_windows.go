//go:build windows

package organizer

import (
	"os"
	"strings"
	"syscall"
)

func isUnsafePathLink(info os.FileInfo) bool {
	if info.Mode()&os.ModeSymlink != 0 {
		return true
	}
	data, ok := info.Sys().(*syscall.Win32FileAttributeData)
	return ok && data.FileAttributes&syscall.FILE_ATTRIBUTE_REPARSE_POINT != 0
}

func comparablePlanPath(path string) string {
	return strings.ToLower(path)
}
