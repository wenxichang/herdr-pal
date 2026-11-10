//go:build !windows

package dsh

import (
	"os"
	"syscall"
)

// openCredentialFile 以 O_NOFOLLOW 打开认证文件，避免间接跟随符号链接。
// 打开后使用 fstat 校验当前用户所有权、权限不向组或其他用户开放、
// 普通文件类型与大小上限；检查作用于已打开文件，避免路径替换竞态。
func openCredentialFile(path string) (*os.File, error) {
	file, err := os.OpenFile(path, os.O_RDONLY|syscall.O_NOFOLLOW, 0)
	if err != nil {
		return nil, errCredentialFile
	}
	info, err := file.Stat()
	if err != nil {
		file.Close()
		return nil, errCredentialFile
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok || !info.Mode().IsRegular() || int(stat.Uid) != os.Geteuid() || info.Mode().Perm()&0o077 != 0 {
		file.Close()
		return nil, errCredentialFile
	}
	if info.Size() > maxCredentialBytes {
		file.Close()
		return nil, errCredentialSize
	}
	return file, nil
}
