package lifecycle

import (
	"crypto/sha256"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// ErrInvalidRuntimePaths 表示运行目录或 Socket 身份不足以生成安全路径。
var ErrInvalidRuntimePaths = errors.New("Pal 生命周期运行路径无效")

// RuntimePaths 汇总一个 Herdr Socket 身份对应的本地锁、控制端点和日志路径。
type RuntimePaths struct {
	InstanceHash  string
	StartLock     string
	OwnerLock     string
	ControlSocket string
	LogFile       string
}

// PrepareRuntimeDirectories 创建并收紧 Launcher 与 Supervisor 共用的私有锁目录。
func PrepareRuntimeDirectories(paths RuntimePaths) error {
	if strings.TrimSpace(paths.StartLock) == "" || strings.TrimSpace(paths.OwnerLock) == "" {
		return ErrInvalidRuntimePaths
	}
	startDirectory := filepath.Dir(paths.StartLock)
	ownerDirectory := filepath.Dir(paths.OwnerLock)
	if startDirectory != ownerDirectory {
		return ErrInvalidRuntimePaths
	}
	if err := os.MkdirAll(ownerDirectory, 0o700); err != nil {
		return fmt.Errorf("创建 Pal 生命周期锁目录: %w", err)
	}
	info, err := os.Lstat(ownerDirectory)
	if err != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return ErrInvalidRuntimePaths
	}
	if err := os.Chmod(ownerDirectory, 0o700); err != nil {
		return fmt.Errorf("设置 Pal 生命周期锁目录权限: %w", err)
	}
	return nil
}

// NewRuntimePaths 根据规范化 Socket 身份生成不泄露原始路径的运行文件位置。
func NewRuntimePaths(cacheRoot, controlRoot, logRoot, socketIdentity string) (RuntimePaths, error) {
	cacheRoot = strings.TrimSpace(cacheRoot)
	controlRoot = strings.TrimSpace(controlRoot)
	logRoot = strings.TrimSpace(logRoot)
	socketIdentity = strings.TrimSpace(socketIdentity)
	if cacheRoot == "" || controlRoot == "" || logRoot == "" || socketIdentity == "" {
		return RuntimePaths{}, ErrInvalidRuntimePaths
	}
	hash := runtimeIdentityHash(socketIdentity)
	lockDirectory := filepath.Join(cacheRoot, "herdr-pal")
	return RuntimePaths{
		InstanceHash:  hash,
		StartLock:     filepath.Join(lockDirectory, hash+".start.lock"),
		OwnerLock:     filepath.Join(lockDirectory, hash+".lock"),
		ControlSocket: filepath.Join(controlRoot, "herdr-pal", hash+".sock"),
		LogFile:       filepath.Join(logRoot, "herdr-pal", "herdr-pal.log"),
	}, nil
}

func runtimeIdentityHash(value string) string {
	digest := sha256.Sum256([]byte(value))
	return fmt.Sprintf("%x", digest[:8])
}
