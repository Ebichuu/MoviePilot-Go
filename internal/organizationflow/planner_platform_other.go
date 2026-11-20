//go:build !windows

package organizationflow

import "os"

func isUnsafeOrganizationPathLink(info os.FileInfo) bool {
	return info.Mode()&os.ModeSymlink != 0
}
