package installer

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"time"
)

func writePrivateFile(path string, data []byte, now time.Time) (string, error) {
	path = filepath.Clean(path)
	directory := filepath.Dir(path)
	if err := ensurePrivateDirectory(directory); err != nil {
		return "", fmt.Errorf("准备配置目录 %s: %w", directory, err)
	}
	info, err := os.Lstat(path)
	if err != nil && !os.IsNotExist(err) {
		return "", fmt.Errorf("检查配置文件 %s: %w", path, err)
	}
	if err == nil && (!info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0) {
		return "", fmt.Errorf("配置文件 %s 不是普通文件", path)
	}

	backupPath := ""
	if err == nil {
		backupPath, err = backupPrivateFile(path, now)
		if err != nil {
			return "", fmt.Errorf("备份配置文件 %s: %w", path, err)
		}
	}
	if err := replacePrivateFile(path, data); err != nil {
		return backupPath, err
	}
	return backupPath, nil
}

func ensurePrivateDirectory(path string) error {
	if err := os.MkdirAll(path, 0o700); err != nil {
		return err
	}
	info, err := os.Lstat(path)
	if err != nil {
		return err
	}
	if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return errors.New("目标不是普通目录")
	}
	return os.Chmod(path, 0o700)
}

func backupPrivateFile(path string, now time.Time) (string, error) {
	if now.IsZero() {
		now = time.Now()
	}
	base := fmt.Sprintf("%s.bak-%s-%d", path, now.UTC().Format("20060102T150405.000000000Z"), os.Getpid())
	for index := 0; index < 100; index++ {
		candidate := base
		if index > 0 {
			candidate = fmt.Sprintf("%s-%d", base, index)
		}
		err := copyPrivateFile(path, candidate, os.O_CREATE|os.O_EXCL|os.O_WRONLY)
		if err == nil {
			return candidate, nil
		}
		if !os.IsExist(err) {
			return "", err
		}
	}
	return "", errors.New("无法分配唯一备份文件名")
}

func replacePrivateFile(path string, data []byte) (returnErr error) {
	directory := filepath.Dir(path)
	temporary, err := os.CreateTemp(directory, ".herdr-pal-install-*")
	if err != nil {
		return fmt.Errorf("创建配置临时文件 %s: %w", path, err)
	}
	temporaryPath := temporary.Name()
	defer func() {
		_ = temporary.Close()
		_ = os.Remove(temporaryPath)
	}()
	if err := temporary.Chmod(0o600); err != nil {
		return fmt.Errorf("设置配置临时文件权限 %s: %w", path, err)
	}
	if _, err := temporary.Write(data); err != nil {
		return fmt.Errorf("写入配置临时文件 %s: %w", path, err)
	}
	if err := temporary.Sync(); err != nil {
		return fmt.Errorf("同步配置临时文件 %s: %w", path, err)
	}
	if err := temporary.Close(); err != nil {
		return fmt.Errorf("关闭配置临时文件 %s: %w", path, err)
	}
	if err := os.Rename(temporaryPath, path); err != nil {
		return fmt.Errorf("替换配置文件 %s: %w", path, err)
	}
	return nil
}

func copyPrivateFile(source, target string, flags int) (returnErr error) {
	input, err := os.Open(source)
	if err != nil {
		return err
	}
	defer input.Close()
	output, err := os.OpenFile(target, flags, 0o600)
	if err != nil {
		return err
	}
	defer func() {
		if err := output.Close(); returnErr == nil && err != nil {
			returnErr = err
		}
	}()
	if _, err := io.Copy(output, input); err != nil {
		return err
	}
	if err := output.Chmod(0o600); err != nil {
		return err
	}
	return output.Sync()
}

func restoreBackup(path, backupPath string) error {
	if backupPath == "" {
		if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
			return fmt.Errorf("移除新配置文件 %s: %w", path, err)
		}
		return nil
	}
	data, err := os.ReadFile(backupPath)
	if err != nil {
		return fmt.Errorf("读取配置备份 %s: %w", backupPath, err)
	}
	if err := replacePrivateFile(path, data); err != nil {
		return fmt.Errorf("恢复配置备份 %s: %w", backupPath, err)
	}
	return nil
}
