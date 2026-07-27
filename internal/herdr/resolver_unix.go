//go:build !windows

package herdr

import (
	"errors"
	"net"
	"os"
	"path/filepath"
	"strings"
	"time"
)

func resolveHomeSocket(sessionName string) (string, error) {
	if sessionName != "" && sessionName != "default" {
		return "", errors.New("命名会话不使用 HOME 默认 Socket")
	}
	home := strings.TrimSpace(os.Getenv("HOME"))
	if home == "" {
		return "", errors.New("HOME 为空")
	}
	path := filepath.Join(home, ".config", "herdr", "herdr.sock")
	connection, err := net.DialTimeout("unix", path, 200*time.Millisecond)
	if err != nil {
		return "", errors.New("HOME Socket 不可连接")
	}
	_ = connection.Close()
	return path, nil
}
