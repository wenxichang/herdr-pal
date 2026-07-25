package herdr

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// CommandRunner 定义调用 Herdr 公共 CLI 所需的最小能力。
type CommandRunner interface {
	Output(ctx context.Context, name string, args ...string) ([]byte, error)
}

type serverStatus struct {
	Running *bool   `json:"running"`
	Socket  *string `json:"socket"`
}

type sessionList struct {
	Sessions []sessionStatus `json:"sessions"`
}

type sessionStatus struct {
	Name       *string `json:"name"`
	Running    *bool   `json:"running"`
	SocketPath *string `json:"socket_path"`
}

// ResolveSocket 依次通过显式路径、Herdr 公共 CLI 和默认 HOME 路径解析本地 Socket。
func ResolveSocket(ctx context.Context, explicitPath, sessionName string, runner CommandRunner) (string, error) {
	if explicitPath != "" {
		return explicitPath, nil
	}
	var cliErr error
	if runner == nil {
		cliErr = errors.New("解析 Herdr Socket 失败：CommandRunner 不能为空")
	} else if sessionName == "" || sessionName == "default" {
		var path string
		path, cliErr = resolveDefaultSocket(ctx, runner)
		if cliErr == nil {
			return path, nil
		}
	} else {
		var path string
		path, cliErr = resolveNamedSocket(ctx, sessionName, runner)
		if cliErr == nil {
			return path, nil
		}
	}
	if ctx.Err() != nil {
		return "", cliErr
	}
	if path, err := resolveHomeSocket(sessionName); err == nil {
		return path, nil
	}
	return "", cliErr
}

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

func resolveDefaultSocket(ctx context.Context, runner CommandRunner) (string, error) {
	output, err := runner.Output(ctx, "herdr", "status", "server", "--json")
	if err != nil {
		return "", resolverCommandError(ctx, "查询 Herdr 默认服务状态失败")
	}
	var status serverStatus
	if err := json.Unmarshal(output, &status); err != nil {
		return "", errors.New("解析 Herdr 默认服务状态失败")
	}
	if status.Running == nil || !*status.Running {
		return "", errors.New("Herdr 默认服务未运行")
	}
	if status.Socket == nil || strings.TrimSpace(*status.Socket) == "" {
		return "", errors.New("Herdr 默认服务未提供 Socket")
	}
	return *status.Socket, nil
}

func resolveNamedSocket(ctx context.Context, sessionName string, runner CommandRunner) (string, error) {
	output, err := runner.Output(ctx, "herdr", "session", "list", "--json")
	if err != nil {
		return "", resolverCommandError(ctx, "查询 Herdr 命名会话失败")
	}
	var list sessionList
	if err := json.Unmarshal(output, &list); err != nil {
		return "", errors.New("解析 Herdr 命名会话列表失败")
	}
	for _, session := range list.Sessions {
		if session.Name == nil || *session.Name != sessionName {
			continue
		}
		if session.Running == nil || !*session.Running {
			return "", errors.New("Herdr 命名会话未运行")
		}
		if session.SocketPath == nil || strings.TrimSpace(*session.SocketPath) == "" {
			return "", errors.New("Herdr 命名会话未提供 Socket")
		}
		return *session.SocketPath, nil
	}
	return "", errors.New("未找到 Herdr 命名会话")
}

func resolverCommandError(ctx context.Context, message string) error {
	if contextError := ctx.Err(); contextError != nil {
		return fmt.Errorf("%s: %w", message, contextError)
	}
	return errors.New(message)
}
