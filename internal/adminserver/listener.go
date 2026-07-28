// Package adminserver 实现仅限本机同 UID 访问的 HPAP 管理服务。
package adminserver

import (
	"errors"
	"net"
)

const (
	// DefaultSocketFileName 是 state directory 下的默认 HPAP Socket 文件名。
	DefaultSocketFileName = "admin.sock"
)

var (
	// ErrUnsafeStateDirectory 表示 state directory 的类型、所有者或权限不满足安全边界。
	ErrUnsafeStateDirectory = errors.New("HPAP state directory 不安全")
	// ErrUnsafeSocketPath 表示现有 Admin Socket 路径不是可安全探测和替换的 Socket。
	ErrUnsafeSocketPath = errors.New("HPAP Admin Socket 路径不安全")
	// ErrAdminAlreadyRunning 表示目标 Socket 已经有管理服务监听。
	ErrAdminAlreadyRunning = errors.New("HPAP Admin Server 已运行")
)

// PeerUIDFunc 从已接受的 Unix 连接读取对端进程 UID。
type PeerUIDFunc func(*net.UnixConn) (uint32, error)

// ListenerConfig 指定 Admin Socket 的安全目录和可测试的 peer UID 查询函数。
type ListenerConfig struct {
	StateDir string
	PeerUID  PeerUIDFunc
}
