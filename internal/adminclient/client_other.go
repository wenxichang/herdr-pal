//go:build windows

package adminclient

import (
	"context"
	"net"
)

func defaultDial(context.Context, string) (net.Conn, error) {
	return nil, ErrUnsupported
}
