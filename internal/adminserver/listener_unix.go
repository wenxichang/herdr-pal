//go:build !windows

package adminserver

import (
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"
)

const socketProbeTimeout = 500 * time.Millisecond

// Listener 是创建路径受限且只接受同 UID 对端的 Unix 管理监听器。
type Listener struct {
	listener    *net.UnixListener
	path        string
	peerUID     PeerUIDFunc
	expectedUID uint32
	socketInfo  os.FileInfo

	closeOnce sync.Once
	closeErr  error
}

// Listen 创建 `<state_dir>/admin.sock`，并在返回前完成目录、路径和权限校验。
func Listen(config ListenerConfig) (*Listener, error) {
	stateDir := strings.TrimSpace(config.StateDir)
	if stateDir == "" {
		return nil, fmt.Errorf("%w: 路径为空", ErrUnsafeStateDirectory)
	}
	stateDir = filepath.Clean(stateDir)
	if err := ensureSecureStateDirectory(stateDir); err != nil {
		return nil, err
	}
	path := filepath.Join(stateDir, DefaultSocketFileName)
	if err := prepareSocketPath(path); err != nil {
		return nil, err
	}
	unixListener, err := net.ListenUnix("unix", &net.UnixAddr{Name: path, Net: "unix"})
	if err != nil {
		return nil, fmt.Errorf("监听 HPAP Admin Socket: %w", err)
	}
	unixListener.SetUnlinkOnClose(false)
	createdInfo, err := os.Lstat(path)
	if err != nil {
		_ = unixListener.Close()
		return nil, fmt.Errorf("读取新建 HPAP Admin Socket 状态: %w", err)
	}
	cleanup := func() {
		_ = unixListener.Close()
		_ = removeSocketIfSame(path, createdInfo)
	}
	if createdInfo.Mode().Type() != os.ModeSocket {
		cleanup()
		return nil, fmt.Errorf("%w: 创建后的路径不是 Socket", ErrUnsafeSocketPath)
	}
	if err := os.Chmod(path, 0o600); err != nil {
		cleanup()
		return nil, fmt.Errorf("设置 HPAP Admin Socket 权限: %w", err)
	}
	socketInfo, err := os.Lstat(path)
	if err != nil {
		cleanup()
		return nil, fmt.Errorf("读取 HPAP Admin Socket 状态: %w", err)
	}
	if socketInfo.Mode().Type() != os.ModeSocket || !os.SameFile(createdInfo, socketInfo) || socketInfo.Mode().Perm() != 0o600 {
		cleanup()
		return nil, fmt.Errorf("%w: 创建后的类型或权限异常", ErrUnsafeSocketPath)
	}
	peerUID := config.PeerUID
	if peerUID == nil {
		peerUID = systemPeerUID
	}
	return &Listener{
		listener: unixListener, path: path, peerUID: peerUID,
		expectedUID: uint32(os.Geteuid()), socketInfo: socketInfo,
	}, nil
}

// Accept 接受下一条同 UID 连接；UID 查询失败或不匹配的连接会立即关闭。
func (listener *Listener) Accept() (net.Conn, error) {
	if listener == nil || listener.listener == nil {
		return nil, net.ErrClosed
	}
	for {
		connection, err := listener.listener.AcceptUnix()
		if err != nil {
			return nil, err
		}
		uid, err := listener.peerUID(connection)
		if err != nil || uid != listener.expectedUID {
			_ = connection.Close()
			continue
		}
		return &peerConnection{UnixConn: connection, uid: uid}, nil
	}
}

// Close 停止监听，并仅在路径仍指向本监听器创建的 Socket 时清理文件。
func (listener *Listener) Close() error {
	if listener == nil {
		return nil
	}
	listener.closeOnce.Do(func() {
		var closeErr error
		if listener.listener != nil {
			closeErr = listener.listener.Close()
		}
		removeErr := removeSocketIfSame(listener.path, listener.socketInfo)
		listener.closeErr = errors.Join(closeErr, removeErr)
	})
	return listener.closeErr
}

// Addr 返回底层 Unix Socket 地址。
func (listener *Listener) Addr() net.Addr {
	if listener == nil || listener.listener == nil {
		return nil
	}
	return listener.listener.Addr()
}

// SocketPath 返回当前监听器使用的规范化 Socket 路径。
func (listener *Listener) SocketPath() string {
	if listener == nil {
		return ""
	}
	return listener.path
}

type peerConnection struct {
	*net.UnixConn
	uid uint32
}

// PeerUID 返回连接建立时由内核提供并已校验的对端 UID。
func (connection *peerConnection) PeerUID() uint32 {
	if connection == nil {
		return 0
	}
	return connection.uid
}

func ensureSecureStateDirectory(path string) error {
	info, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		if err := os.MkdirAll(path, 0o700); err != nil {
			return fmt.Errorf("创建 HPAP state directory: %w", err)
		}
		if err := os.Chmod(path, 0o700); err != nil {
			return fmt.Errorf("设置 HPAP state directory 权限: %w", err)
		}
		info, err = os.Lstat(path)
	}
	if err != nil {
		return fmt.Errorf("读取 HPAP state directory: %w", err)
	}
	if info.Mode().Type() != os.ModeDir || info.Mode().Perm() != 0o700 {
		return fmt.Errorf("%w: 类型或权限不符合要求", ErrUnsafeStateDirectory)
	}
	owned, err := pathOwnedByCurrentUser(path)
	if err != nil {
		return fmt.Errorf("读取 HPAP state directory 所有者: %w", err)
	}
	if !owned {
		return fmt.Errorf("%w: 所有者不是当前用户", ErrUnsafeStateDirectory)
	}
	return nil
}

func prepareSocketPath(path string) error {
	info, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("读取现有 HPAP Admin Socket: %w", err)
	}
	if info.Mode().Type() != os.ModeSocket {
		return fmt.Errorf("%w: 现有路径类型为 %s", ErrUnsafeSocketPath, info.Mode().Type())
	}
	owned, err := pathOwnedByCurrentUser(path)
	if err != nil {
		return fmt.Errorf("读取现有 HPAP Admin Socket 所有者: %w", err)
	}
	if !owned {
		return fmt.Errorf("%w: 现有 Socket 不属于当前用户", ErrUnsafeSocketPath)
	}
	connection, dialErr := net.DialTimeout("unix", path, socketProbeTimeout)
	if dialErr == nil {
		_ = connection.Close()
		return ErrAdminAlreadyRunning
	}
	if !errors.Is(dialErr, syscall.ECONNREFUSED) && !errors.Is(dialErr, syscall.ENOENT) {
		return fmt.Errorf("探测现有 HPAP Admin Socket: %w", dialErr)
	}
	current, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("复核现有 HPAP Admin Socket: %w", err)
	}
	if current.Mode().Type() != os.ModeSocket || !os.SameFile(info, current) {
		return fmt.Errorf("%w: 探测期间路径已变化", ErrUnsafeSocketPath)
	}
	if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("删除陈旧 HPAP Admin Socket: %w", err)
	}
	return nil
}

func removeSocketIfSame(path string, expected os.FileInfo) error {
	if path == "" || expected == nil {
		return nil
	}
	current, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("读取待清理 HPAP Admin Socket: %w", err)
	}
	if current.Mode().Type() != os.ModeSocket || !os.SameFile(expected, current) {
		return nil
	}
	if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("清理 HPAP Admin Socket: %w", err)
	}
	return nil
}
