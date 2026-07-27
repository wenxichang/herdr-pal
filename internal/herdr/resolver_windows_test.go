//go:build windows

package herdr

import (
	"context"
	"errors"
	"net"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/Microsoft/go-winio"
)

func TestWindowsResolveSocketFallsBackToDefaultNamedPipe(t *testing.T) {
	appData := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", "")
	t.Setenv("APPDATA", appData)
	t.Setenv("USERPROFILE", "")
	t.Setenv("HOME", "")
	markerPath := filepath.Join(appData, "herdr", "herdr.sock")
	if err := os.MkdirAll(filepath.Dir(markerPath), 0o700); err != nil {
		t.Fatalf("MkdirAll() error = %v", err)
	}
	if err := os.WriteFile(markerPath, []byte("test-marker"), 0o600); err != nil {
		t.Fatalf("WriteFile() error = %v", err)
	}

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

	runner := &fakeCommandRunner{err: errors.New("herdr CLI unavailable")}
	resolved, err := ResolveSocket(context.Background(), "", "", runner)
	if err != nil {
		t.Fatalf("ResolveSocket() error = %v", err)
	}
	if resolved != markerPath {
		t.Fatalf("ResolveSocket() = %q, want %q", resolved, markerPath)
	}

	select {
	case connection := <-accepted:
		_ = connection.Close()
	case err := <-acceptErr:
		t.Fatalf("Accept() error = %v", err)
	case <-time.After(time.Second):
		t.Fatal("default named pipe was not accepted")
	}
}
