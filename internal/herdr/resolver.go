package herdr

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
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

// ResolveSocket 通过显式路径或 Herdr 公共 CLI 解析本地 Socket 路径。
func ResolveSocket(ctx context.Context, explicitPath, sessionName string, runner CommandRunner) (string, error) {
	if explicitPath != "" {
		return explicitPath, nil
	}
	if runner == nil {
		return "", errors.New("解析 Herdr Socket 失败：CommandRunner 不能为空")
	}
	if sessionName == "" || sessionName == "default" {
		return resolveDefaultSocket(ctx, runner)
	}
	return resolveNamedSocket(ctx, sessionName, runner)
}

func resolveDefaultSocket(ctx context.Context, runner CommandRunner) (string, error) {
	output, err := runner.Output(ctx, "herdr", "status", "server", "--json")
	if err != nil {
		return "", errors.New("查询 Herdr 默认服务状态失败")
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
		return "", errors.New("查询 Herdr 命名会话失败")
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
