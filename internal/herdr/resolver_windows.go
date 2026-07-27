//go:build windows

package herdr

import (
	"context"
	"errors"
	"os"
	"time"
)

func resolveHomeSocket(sessionName string) (string, error) {
	if sessionName != "" && sessionName != "default" {
		return "", errors.New("命名会话不使用 Windows 默认 Socket")
	}
	path, err := windowsDefaultSocketPath(
		os.Getenv("XDG_CONFIG_HOME"),
		os.Getenv("APPDATA"),
		os.Getenv("USERPROFILE"),
		os.Getenv("HOME"),
	)
	if err != nil {
		return "", err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	connection, err := newLocalDialer().DialContext(ctx, localSocketNetwork(), path)
	if err != nil {
		return "", errors.New("Windows 默认 Socket 不可连接")
	}
	_ = connection.Close()
	return path, nil
}
