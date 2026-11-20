//go:build !windows

package organizer

import "os"

func isUnsafePathLink(info os.FileInfo) bool {
	return info.Mode()&os.ModeSymlink != 0
}

func comparablePlanPath(path string) string {
	return path
}
