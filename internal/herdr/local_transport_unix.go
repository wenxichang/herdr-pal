//go:build !windows

package herdr

import "net"

func newLocalDialer() Dialer {
	return &net.Dialer{}
}

func localSocketNetwork() string {
	return "unix"
}
