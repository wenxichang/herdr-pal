//go:build !windows

package adminclient

import (
	"context"
	"net"
)

func defaultDial(ctx context.Context, path string) (net.Conn, error) {
	dialer := net.Dialer{}
	return dialer.DialContext(ctx, "unix", path)
}
