//go:build !linux && !darwin && !windows

package adminserver

import (
	"errors"
	"net"
)

func systemPeerUID(*net.UnixConn) (uint32, error) {
	return 0, errors.New("当前系统不支持安全的 Unix Socket peer UID 查询")
}

func pathOwnedByCurrentUser(string) (bool, error) {
	return false, errors.New("当前系统不支持安全的路径所有者查询")
}
