package lifecycle

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

const defaultLogRotateSize int64 = 10 * 1024 * 1024

// ErrInvalidLogFile 表示后台日志路径不安全或无法创建。
var ErrInvalidLogFile = errors.New("Pal 后台日志文件无效")

// OpenLogFile 以私有追加模式打开日志，并在达到 maxSize 时保留一份 `.1` 轮转文件。
func OpenLogFile(path string, maxSize int64) (*os.File, error) {
	path = strings.TrimSpace(path)
	if path == "" {
		return nil, ErrInvalidLogFile
	}
	if maxSize <= 0 {
		maxSize = defaultLogRotateSize
	}
	directory := filepath.Dir(path)
	if err := os.MkdirAll(directory, 0o700); err != nil {
		return nil, fmt.Errorf("创建 Pal 日志目录: %w", err)
	}
	directoryInfo, err := os.Lstat(directory)
	if err != nil || !directoryInfo.IsDir() || directoryInfo.Mode()&os.ModeSymlink != 0 {
		return nil, ErrInvalidLogFile
	}
	if err := os.Chmod(directory, 0o700); err != nil {
		return nil, fmt.Errorf("设置 Pal 日志目录权限: %w", err)
	}
	if err := rotateLogIfNeeded(path, maxSize); err != nil {
		return nil, err
	}
	file, err := os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		return nil, fmt.Errorf("打开 Pal 日志: %w", err)
	}
	if err := file.Chmod(0o600); err != nil {
		_ = file.Close()
		return nil, fmt.Errorf("设置 Pal 日志权限: %w", err)
	}
	return file, nil
}

func rotateLogIfNeeded(path string, maxSize int64) error {
	info, err := os.Lstat(path)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("检查 Pal 日志: %w", err)
	}
	if !info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 {
		return ErrInvalidLogFile
	}
	if info.Size() < maxSize {
		return nil
	}
	rotated := path + ".1"
	if rotatedInfo, rotatedErr := os.Lstat(rotated); rotatedErr == nil {
		if !rotatedInfo.Mode().IsRegular() || rotatedInfo.Mode()&os.ModeSymlink != 0 {
			return ErrInvalidLogFile
		}
		if err := os.Remove(rotated); err != nil {
			return fmt.Errorf("清理旧 Pal 轮转日志: %w", err)
		}
	} else if !os.IsNotExist(rotatedErr) {
		return fmt.Errorf("检查旧 Pal 轮转日志: %w", rotatedErr)
	}
	if err := os.Rename(path, rotated); err != nil {
		return fmt.Errorf("轮转 Pal 日志: %w", err)
	}
	return nil
}
