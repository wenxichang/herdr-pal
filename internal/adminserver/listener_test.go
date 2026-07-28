//go:build !windows

package adminserver

import (
	"errors"
	"net"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestAdminListenerCreatesSecurePathsAcceptsSameUIDAndCleansUp(t *testing.T) {
	stateDir := shortStateDir(t, false)
	listener, err := Listen(ListenerConfig{StateDir: stateDir, PeerUID: fixedPeerUID(uint32(os.Geteuid()))})
	if err != nil {
		t.Fatal(err)
	}
	socketPath := filepath.Join(stateDir, DefaultSocketFileName)
	assertPathMode(t, stateDir, os.ModeDir, 0o700)
	assertPathMode(t, socketPath, os.ModeSocket, 0o600)

	accepted := make(chan net.Conn, 1)
	acceptErrors := make(chan error, 1)
	go func() {
		connection, acceptErr := listener.Accept()
		if acceptErr != nil {
			acceptErrors <- acceptErr
			return
		}
		accepted <- connection
	}()
	client, err := net.DialTimeout("unix", socketPath, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	select {
	case connection := <-accepted:
		defer connection.Close()
		peer, ok := connection.(interface{ PeerUID() uint32 })
		if !ok || peer.PeerUID() != uint32(os.Geteuid()) {
			t.Fatalf("accepted connection peer UID = %v, ok=%t", peer, ok)
		}
	case err := <-acceptErrors:
		t.Fatalf("Accept() error = %v", err)
	case <-time.After(time.Second):
		t.Fatal("same-UID connection was not accepted")
	}

	if err := listener.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Lstat(socketPath); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("socket still exists after close: %v", err)
	}
}

func TestAdminListenerRejectsUnsafeStateDirectory(t *testing.T) {
	tests := []struct {
		name    string
		prepare func(t *testing.T, root string) string
	}{
		{
			name: "symbolic link",
			prepare: func(t *testing.T, root string) string {
				target := filepath.Join(root, "target")
				if err := os.Mkdir(target, 0o700); err != nil {
					t.Fatal(err)
				}
				path := filepath.Join(root, "state")
				if err := os.Symlink(target, path); err != nil {
					t.Fatal(err)
				}
				return path
			},
		},
		{
			name: "regular file",
			prepare: func(t *testing.T, root string) string {
				path := filepath.Join(root, "state")
				if err := os.WriteFile(path, []byte("keep"), 0o600); err != nil {
					t.Fatal(err)
				}
				return path
			},
		},
		{
			name: "insecure permissions",
			prepare: func(t *testing.T, root string) string {
				path := filepath.Join(root, "state")
				if err := os.Mkdir(path, 0o755); err != nil {
					t.Fatal(err)
				}
				if err := os.Chmod(path, 0o755); err != nil {
					t.Fatal(err)
				}
				return path
			},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			stateDir := test.prepare(t, t.TempDir())
			if _, err := Listen(ListenerConfig{StateDir: stateDir, PeerUID: fixedPeerUID(uint32(os.Geteuid()))}); !errors.Is(err, ErrUnsafeStateDirectory) {
				t.Fatalf("Listen() error = %v, want ErrUnsafeStateDirectory", err)
			}
		})
	}
}

func TestAdminListenerRejectsNonSocketPathsWithoutDeletingThem(t *testing.T) {
	tests := []struct {
		name    string
		prepare func(t *testing.T, path string)
	}{
		{name: "regular file", prepare: func(t *testing.T, path string) {
			if err := os.WriteFile(path, []byte("keep"), 0o600); err != nil {
				t.Fatal(err)
			}
		}},
		{name: "directory", prepare: func(t *testing.T, path string) {
			if err := os.Mkdir(path, 0o700); err != nil {
				t.Fatal(err)
			}
		}},
		{name: "symbolic link", prepare: func(t *testing.T, path string) {
			target := filepath.Join(filepath.Dir(path), "target")
			if err := os.WriteFile(target, []byte("keep"), 0o600); err != nil {
				t.Fatal(err)
			}
			if err := os.Symlink(target, path); err != nil {
				t.Fatal(err)
			}
		}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			stateDir := filepath.Join(t.TempDir(), "state")
			if err := os.Mkdir(stateDir, 0o700); err != nil {
				t.Fatal(err)
			}
			path := filepath.Join(stateDir, DefaultSocketFileName)
			test.prepare(t, path)
			before, err := os.Lstat(path)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := Listen(ListenerConfig{StateDir: stateDir, PeerUID: fixedPeerUID(uint32(os.Geteuid()))}); !errors.Is(err, ErrUnsafeSocketPath) {
				t.Fatalf("Listen() error = %v, want ErrUnsafeSocketPath", err)
			}
			after, err := os.Lstat(path)
			if err != nil || after.Mode().Type() != before.Mode().Type() {
				t.Fatalf("existing path was changed: before=%s after=%v err=%v", before.Mode(), after, err)
			}
		})
	}
}

func TestStaleSocketIsReplacedButActiveListenerIsRejected(t *testing.T) {
	t.Run("stale socket", func(t *testing.T) {
		stateDir := shortStateDir(t, true)
		path := filepath.Join(stateDir, DefaultSocketFileName)
		stale, err := net.ListenUnix("unix", &net.UnixAddr{Name: path, Net: "unix"})
		if err != nil {
			t.Fatal(err)
		}
		stale.SetUnlinkOnClose(false)
		if err := stale.Close(); err != nil {
			t.Fatal(err)
		}
		listener, err := Listen(ListenerConfig{StateDir: stateDir, PeerUID: fixedPeerUID(uint32(os.Geteuid()))})
		if err != nil {
			t.Fatalf("Listen() did not replace stale socket: %v", err)
		}
		defer listener.Close()
		assertPathMode(t, path, os.ModeSocket, 0o600)
	})

	t.Run("active socket", func(t *testing.T) {
		stateDir := shortStateDir(t, true)
		path := filepath.Join(stateDir, DefaultSocketFileName)
		active, err := net.ListenUnix("unix", &net.UnixAddr{Name: path, Net: "unix"})
		if err != nil {
			t.Fatal(err)
		}
		defer active.Close()
		if _, err := Listen(ListenerConfig{StateDir: stateDir, PeerUID: fixedPeerUID(uint32(os.Geteuid()))}); !errors.Is(err, ErrAdminAlreadyRunning) {
			t.Fatalf("Listen() error = %v, want ErrAdminAlreadyRunning", err)
		}
		connection, err := net.DialTimeout("unix", path, time.Second)
		if err != nil {
			t.Fatalf("active socket was disturbed: %v", err)
		}
		connection.Close()
	})
}

func TestPeerUIDMismatchIsRejectedBeforeAcceptReturns(t *testing.T) {
	stateDir := shortStateDir(t, false)
	listener, err := Listen(ListenerConfig{StateDir: stateDir, PeerUID: fixedPeerUID(uint32(os.Geteuid() + 1))})
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	accepted := make(chan error, 1)
	go func() {
		connection, acceptErr := listener.Accept()
		if connection != nil {
			connection.Close()
		}
		accepted <- acceptErr
	}()
	client, err := net.DialTimeout("unix", filepath.Join(stateDir, DefaultSocketFileName), time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	if err := client.SetReadDeadline(time.Now().Add(time.Second)); err != nil {
		t.Fatal(err)
	}
	buffer := make([]byte, 1)
	if _, err := client.Read(buffer); err == nil {
		t.Fatal("mismatched peer connection remained open")
	}
	select {
	case err := <-accepted:
		t.Fatalf("Accept() returned mismatched peer: %v", err)
	default:
	}
	if err := listener.Close(); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-accepted:
		if err == nil {
			t.Fatal("Accept() should stop with listener close error")
		}
	case <-time.After(time.Second):
		t.Fatal("Accept() did not stop after listener close")
	}
}

func TestPeerUIDSystemLookupAcceptsCurrentUser(t *testing.T) {
	stateDir := shortStateDir(t, false)
	listener, err := Listen(ListenerConfig{StateDir: stateDir})
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	accepted := make(chan net.Conn, 1)
	errorsChannel := make(chan error, 1)
	go func() {
		connection, acceptErr := listener.Accept()
		if acceptErr != nil {
			errorsChannel <- acceptErr
			return
		}
		accepted <- connection
	}()
	client, err := net.DialTimeout("unix", listener.SocketPath(), time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	select {
	case connection := <-accepted:
		defer connection.Close()
		peer := connection.(interface{ PeerUID() uint32 })
		if peer.PeerUID() != uint32(os.Geteuid()) {
			t.Fatalf("system peer UID = %d, want %d", peer.PeerUID(), os.Geteuid())
		}
	case err := <-errorsChannel:
		t.Fatalf("Accept() error = %v", err)
	case <-time.After(time.Second):
		t.Fatal("system peer UID lookup did not accept current user")
	}
}

func fixedPeerUID(uid uint32) PeerUIDFunc {
	return func(*net.UnixConn) (uint32, error) { return uid, nil }
}

func shortStateDir(t *testing.T, create bool) string {
	t.Helper()
	root, err := os.MkdirTemp("", "hp-admin-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(root) })
	stateDir := filepath.Join(root, "s")
	if create {
		if err := os.Mkdir(stateDir, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	return stateDir
}

func assertPathMode(t *testing.T, path string, kind os.FileMode, permissions os.FileMode) {
	t.Helper()
	info, err := os.Lstat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Type() != kind || info.Mode().Perm() != permissions {
		t.Fatalf("%s mode = %s, want kind=%s permissions=%o", path, info.Mode(), kind, permissions)
	}
}
