//go:build linux

package organizer

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"golang.org/x/sys/unix"
)

// openParentDirNoFollow 通过逐级 `openat(O_NOFOLLOW|O_DIRECTORY|O_CLOEXEC)`
// 打开 path 的父目录，返回目录句柄与最后一段文件名。任一祖先是符号链接或非目录
// 时立即失败，消除“先 Lstat 校验、后按路径使用”之间的 TOCTOU 替换窗口。
//
// 对应 R21：Linux 使用 dirfd/openat 加固，调用方应基于返回的 *os.File 使用
// `*at` 系列操作，而不是再次按字符串路径访问。
func openParentDirNoFollow(path string) (*os.File, string, error) {
	cleaned := filepath.Clean(path)
	if !filepath.IsAbs(cleaned) {
		return nil, "", fmt.Errorf("路径必须为绝对路径: %s", path)
	}
	base := filepath.Base(cleaned)
	if base == "" || base == "." || base == string(filepath.Separator) {
		return nil, "", fmt.Errorf("路径缺少文件名: %s", path)
	}
	components := strings.Split(strings.Trim(cleaned, string(filepath.Separator)), string(filepath.Separator))
	if len(components) == 0 {
		return nil, "", fmt.Errorf("路径缺少目录: %s", path)
	}
	dirFD, err := unix.Open("/", unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC, 0)
	if err != nil {
		return nil, "", err
	}
	for _, component := range components[:len(components)-1] {
		if component == "" || component == "." || component == ".." {
			_ = unix.Close(dirFD)
			return nil, "", fmt.Errorf("路径包含非法规段: %s", path)
		}
		nextFD, openErr := unix.Openat(
			dirFD,
			component,
			unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC,
			0,
		)
		_ = unix.Close(dirFD)
		if openErr != nil {
			return nil, "", openErr
		}
		dirFD = nextFD
	}
	return os.NewFile(uintptr(dirFD), filepath.Dir(cleaned)), base, nil
}
