package server

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
)

const MaxHelpFileBytes = 64 * 1024

// ErrHelpUnavailable 表示实时帮助文件当前无法安全读取。
var ErrHelpUnavailable = errors.New("帮助内容不可用")

// HelpProvider 为 ConversationRouter 提供每次请求时的最新帮助内容。
type HelpProvider interface {
	Read() (string, error)
}

// FileHelpProvider 每次调用都重新打开指定文件，不缓存内容。
type FileHelpProvider struct {
	path string
}

// NewFileHelpProvider 创建实时帮助文件读取器。
func NewFileHelpProvider(path string) (*FileHelpProvider, error) {
	path = filepath.Clean(strings.TrimSpace(path))
	if path == "." || path == "" {
		return nil, fmt.Errorf("%w: 文件路径为空", ErrHelpUnavailable)
	}
	return &FileHelpProvider{path: path}, nil
}

// Read 重新打开帮助文件并读取不超过 64 KiB 的非空内容。
func (provider *FileHelpProvider) Read() (string, error) {
	if provider == nil || provider.path == "" {
		return "", ErrHelpUnavailable
	}
	file, err := os.Open(provider.path)
	if err != nil {
		return "", fmt.Errorf("%w: 读取 %q: %v", ErrHelpUnavailable, provider.path, err)
	}
	defer file.Close()
	data, err := io.ReadAll(io.LimitReader(file, MaxHelpFileBytes+1))
	if err != nil {
		return "", fmt.Errorf("%w: 读取 %q: %v", ErrHelpUnavailable, provider.path, err)
	}
	if len(data) > MaxHelpFileBytes {
		return "", fmt.Errorf("%w: %q 超过 %d 字节", ErrHelpUnavailable, provider.path, MaxHelpFileBytes)
	}
	if strings.TrimSpace(string(data)) == "" {
		return "", fmt.Errorf("%w: %q 内容为空", ErrHelpUnavailable, provider.path)
	}
	return string(data), nil
}

type staticHelpProvider struct {
	content string
}

func (provider staticHelpProvider) Read() (string, error) {
	return provider.content, nil
}
