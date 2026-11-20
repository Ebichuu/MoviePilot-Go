//go:build !linux && !windows

package organizer

import "os"

func publishNoReplace(source, target string) error {
	if err := os.Link(source, target); err != nil {
		return err
	}
	return os.Remove(source)
}
