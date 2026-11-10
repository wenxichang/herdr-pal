//go:build windows

package dsh

import "os"

// openCredentialFile 在 Windows 上打开并校验认证文件。
//
// Windows 不是 dsh 的自动发现平台，只支持显式配置 dsh.url 与 dsh.home。
// 这里用 Lstat 拒绝符号链接，并对已打开文件复核普通文件类型与大小上限；
// ACL 校验为 best-effort：不解析 DACL，调用方需确保 DSH_HOME 及其中的
// 认证文件只对当前用户可读。
func openCredentialFile(path string) (*os.File, error) {
	info, err := os.Lstat(path)
	if err != nil {
		return nil, errCredentialFile
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() {
		return nil, errCredentialFile
	}
	if info.Size() > maxCredentialBytes {
		return nil, errCredentialSize
	}

	file, err := os.Open(path)
	if err != nil {
		return nil, errCredentialFile
	}
	opened, err := file.Stat()
	if err != nil {
		file.Close()
		return nil, errCredentialFile
	}
	if !opened.Mode().IsRegular() {
		file.Close()
		return nil, errCredentialFile
	}
	if opened.Size() > maxCredentialBytes {
		file.Close()
		return nil, errCredentialSize
	}
	return file, nil
}
