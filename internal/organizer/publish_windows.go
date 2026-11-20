//go:build windows

package organizer

import "golang.org/x/sys/windows"

func publishNoReplace(source, target string) error {
	sourcePointer, err := windows.UTF16PtrFromString(source)
	if err != nil {
		return err
	}
	targetPointer, err := windows.UTF16PtrFromString(target)
	if err != nil {
		return err
	}
	return windows.MoveFileEx(
		sourcePointer,
		targetPointer,
		windows.MOVEFILE_WRITE_THROUGH,
	)
}
