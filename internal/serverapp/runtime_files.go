package serverapp

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/wenxichang/herdr-pal/internal/adminauth"
	"github.com/wenxichang/herdr-pal/internal/config"
	"github.com/wenxichang/herdr-pal/internal/server"
)

func resolveRuntimeFiles(configured config.ServerRuntimeFiles, options Options) config.ServerRuntimeFiles {
	resolved := configured
	if authFile := strings.TrimSpace(options.AuthFile); authFile != "" {
		resolved.AuthFile = filepath.Clean(authFile)
		directory := filepath.Dir(resolved.AuthFile)
		resolved.BootstrapFile = filepath.Join(directory, "bootstrap.txt")
		resolved.HelpFile = filepath.Join(directory, "help.md")
	}
	if bootstrapFile := strings.TrimSpace(options.BootstrapFile); bootstrapFile != "" {
		resolved.BootstrapFile = filepath.Clean(bootstrapFile)
	}
	if helpFile := strings.TrimSpace(options.HelpFile); helpFile != "" {
		resolved.HelpFile = filepath.Clean(helpFile)
	}
	return resolved
}

func ensureDefaultHelpFile(path string) error {
	_, err := createPrivateFileOnce(path, []byte(server.DefaultHelpText()))
	return err
}

func writeBootstrapFile(path string, bootstrap adminauth.Bootstrap) (bool, error) {
	if !bootstrap.Created {
		return false, nil
	}
	return createPrivateFileOnce(path, []byte(formatAdminBootstrap(bootstrap)))
}

func publishAdminBootstrap(writer io.Writer, path string, bootstrap adminauth.Bootstrap) error {
	fileErr := error(nil)
	if _, err := writeBootstrapFile(path, bootstrap); err != nil {
		fileErr = fmt.Errorf("写入初始管理员引导文件 %q: %w", path, err)
	}
	outputErr := printAdminBootstrap(writer, bootstrap)
	if outputErr != nil {
		outputErr = fmt.Errorf("输出初始管理员凭据: %w", outputErr)
	}
	return errors.Join(fileErr, outputErr)
}

func createPrivateFileOnce(path string, content []byte) (bool, error) {
	path = filepath.Clean(strings.TrimSpace(path))
	if path == "." || path == "" {
		return false, errors.New("运行文件路径为空")
	}
	directory := filepath.Dir(path)
	if err := os.MkdirAll(directory, 0o700); err != nil {
		return false, fmt.Errorf("创建运行文件目录 %q: %w", directory, err)
	}
	if err := os.Chmod(directory, 0o700); err != nil {
		return false, fmt.Errorf("设置运行文件目录权限 %q: %w", directory, err)
	}
	file, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if errors.Is(err, os.ErrExist) {
		info, statErr := os.Lstat(path)
		if statErr != nil {
			return false, fmt.Errorf("检查已有运行文件 %q: %w", path, statErr)
		}
		if !info.Mode().IsRegular() {
			return false, fmt.Errorf("已有运行文件 %q 不是普通文件", path)
		}
		if chmodErr := os.Chmod(path, 0o600); chmodErr != nil {
			return false, fmt.Errorf("设置运行文件权限 %q: %w", path, chmodErr)
		}
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("创建运行文件 %q: %w", path, err)
	}
	if _, err := file.Write(content); err != nil {
		file.Close()
		return false, fmt.Errorf("写入运行文件 %q: %w", path, err)
	}
	if err := file.Sync(); err != nil {
		file.Close()
		return false, fmt.Errorf("同步运行文件 %q: %w", path, err)
	}
	if err := file.Close(); err != nil {
		return false, fmt.Errorf("关闭运行文件 %q: %w", path, err)
	}
	return true, nil
}

func formatAdminBootstrap(bootstrap adminauth.Bootstrap) string {
	return fmt.Sprintf(
		"已创建 Herdr Pal Server 初始管理员，请立即登录并修改密码。\n管理员：%s\n初始密码：%s\n自动化 Token：%s\n",
		bootstrap.Username, bootstrap.InitialPassword, bootstrap.AutomationToken,
	)
}
