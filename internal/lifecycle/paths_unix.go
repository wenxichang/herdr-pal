//go:build !windows

package lifecycle

import (
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
)

// DefaultRuntimePaths 按当前用户和平台生成 Pal 生命周期运行路径。
func DefaultRuntimePaths(socketIdentity string) (RuntimePaths, error) {
	cacheRoot, err := os.UserCacheDir()
	if err != nil {
		return RuntimePaths{}, fmt.Errorf("获取用户缓存目录: %w", err)
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return RuntimePaths{}, fmt.Errorf("获取用户 HOME: %w", err)
	}
	controlRoot := filepath.Join(os.TempDir(), fmt.Sprintf("herdr-pal-%d", os.Getuid()))
	logRoot := ""
	if runtime.GOOS == "darwin" {
		logRoot = filepath.Join(home, "Library", "Logs")
	} else if stateRoot := strings.TrimSpace(os.Getenv("XDG_STATE_HOME")); stateRoot != "" {
		logRoot = stateRoot
	} else {
		logRoot = filepath.Join(home, ".local", "state")
	}
	return NewRuntimePaths(cacheRoot, controlRoot, logRoot, socketIdentity)
}
