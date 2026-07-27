//go:build windows

package herdr

import (
	"context"
	"net"

	"github.com/Microsoft/go-winio"
)

type windowsLocalDialer struct{}

func (windowsLocalDialer) DialContext(ctx context.Context, _ string, markerPath string) (net.Conn, error) {
	return winio.DialPipeContext(ctx, windowsNamedPipePath(markerPath))
}

func newLocalDialer() Dialer {
	return windowsLocalDialer{}
}

func localSocketNetwork() string {
	return "npipe"
}
