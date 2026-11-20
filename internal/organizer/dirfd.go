package organizer

import (
	"errors"
	"os"
)

// errDirfdUnsupported 表示当前平台未实现目录句柄级加固（R21 只对 Linux 要求
// dirfd/openat；Windows 继续使用 Lstat + 重解析点检查）。
var errDirfdUnsupported = errors.New("当前平台不支持目录句柄校验")

// verifyParentDirNoFollow 在支持 dirfd 的平台上重新校验 path 的父目录链未包含
// 符号链接；不支持的平台返回 nil，继续使用调用方的 Lstat 校验。目标目录尚未
// 创建（ENOENT）时视为通过，由后续受边界约束的创建步骤负责。
func verifyParentDirNoFollow(path string) error {
	dir, _, err := openParentDirNoFollow(path)
	if err != nil {
		if errors.Is(err, errDirfdUnsupported) || errors.Is(err, os.ErrNotExist) {
			return nil
		}
		return err
	}
	return dir.Close()
}
