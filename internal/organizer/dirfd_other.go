//go:build !linux

package organizer

import "os"

func openParentDirNoFollow(path string) (*os.File, string, error) {
	return nil, "", errDirfdUnsupported
}
