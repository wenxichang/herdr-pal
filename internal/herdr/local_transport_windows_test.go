//go:build windows

package herdr

import (
	"context"
	"net"
	"testing"
	"time"

	"github.com/Microsoft/go-winio"
)

func TestWindowsLocalDialerConnectsToHerdrNamedPipe(t *testing.T) {
	markerPath := `C:\Users\test\AppData\Local\herdr\herdr-pal-test.sock`
	listener, err := winio.ListenPipe(windowsNamedPipePath(markerPath), nil)
	if err != nil {
		t.Fatalf("ListenPipe() error = %v", err)
	}
	t.Cleanup(func() { _ = listener.Close() })

	accepted := make(chan net.Conn, 1)
	acceptErr := make(chan error, 1)
	go func() {
		connection, err := listener.Accept()
		if err != nil {
			acceptErr <- err
			return
		}
		accepted <- connection
	}()

	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	connection, err := newLocalDialer().DialContext(ctx, localSocketNetwork(), markerPath)
	if err != nil {
		t.Fatalf("DialContext() error = %v", err)
	}
	t.Cleanup(func() { _ = connection.Close() })

	select {
	case serverConnection := <-accepted:
		_ = serverConnection.Close()
	case err := <-acceptErr:
		t.Fatalf("Accept() error = %v", err)
	case <-ctx.Done():
		t.Fatalf("named pipe was not accepted: %v", ctx.Err())
	}
}
