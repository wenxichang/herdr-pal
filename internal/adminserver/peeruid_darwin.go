//go:build darwin

package adminserver

import (
	"fmt"
	"net"

	"golang.org/x/sys/unix"
)

func systemPeerUID(connection *net.UnixConn) (uint32, error) {
	raw, err := connection.SyscallConn()
	if err != nil {
		return 0, fmt.Errorf("访问 Unix Socket 描述符: %w", err)
	}
	var uid uint32
	var queryErr error
	if err := raw.Control(func(fd uintptr) {
		credential, err := unix.GetsockoptXucred(int(fd), unix.SOL_LOCAL, unix.LOCAL_PEERCRED)
		if err != nil {
			queryErr = err
			return
		}
		uid = credential.Uid
	}); err != nil {
		return 0, fmt.Errorf("读取 Unix Socket peer credential: %w", err)
	}
	if queryErr != nil {
		return 0, fmt.Errorf("读取 Unix Socket peer UID: %w", queryErr)
	}
	return uid, nil
}

func pathOwnedByCurrentUser(path string) (bool, error) {
	var status unix.Stat_t
	if err := unix.Lstat(path, &status); err != nil {
		return false, err
	}
	return status.Uid == uint32(unix.Geteuid()), nil
}
