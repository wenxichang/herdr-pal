package app

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/wenxichang/herdr-pal/internal/processlock"
)

func TestResolveRelayIdentityMatchesRelayLock(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	socketDirectory := filepath.Join(home, "sockets")
	if err := os.MkdirAll(socketDirectory, 0o700); err != nil {
		t.Fatalf("MkdirAll() error = %v", err)
	}
	rawSocket := filepath.Join(socketDirectory, "nested", "..", "herdr.sock")
	configPath := writeRelayIdentityConfig(t, rawSocket)

	identity, err := ResolveRelayIdentity(context.Background(), Options{ConfigPath: configPath, Getenv: func(string) string { return "" }})
	if err != nil {
		t.Fatalf("ResolveRelayIdentity() error = %v", err)
	}
	resolvedDirectory, err := filepath.EvalSymlinks(socketDirectory)
	if err != nil {
		t.Fatalf("EvalSymlinks() error = %v", err)
	}
	wantCanonical := filepath.Join(resolvedDirectory, "herdr.sock")
	if identity.CanonicalEndpoint != wantCanonical || identity.SocketHash != shortHash(wantCanonical) {
		t.Fatalf("identity = %#v", identity)
	}
	cacheDir, err := os.UserCacheDir()
	if err != nil {
		t.Fatalf("UserCacheDir() error = %v", err)
	}
	if identity.LockPath != filepath.Join(cacheDir, "herdr-pal", identity.SocketHash+".lock") {
		t.Fatalf("LockPath = %q", identity.LockPath)
	}
}

func TestRunManagedRelaySkipsOwnerLock(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	socketDirectory := filepath.Join(home, "sockets")
	if err := os.MkdirAll(socketDirectory, 0o700); err != nil {
		t.Fatalf("MkdirAll() error = %v", err)
	}
	configPath := writeRelayIdentityConfig(t, filepath.Join(socketDirectory, "herdr.sock"))
	options := Options{ConfigPath: configPath, Getenv: func(string) string { return "" }}
	identity, err := ResolveRelayIdentity(context.Background(), options)
	if err != nil {
		t.Fatalf("ResolveRelayIdentity() error = %v", err)
	}
	if err := os.MkdirAll(filepath.Dir(identity.LockPath), 0o700); err != nil {
		t.Fatalf("MkdirAll(lock) error = %v", err)
	}
	lock, err := processlock.Acquire(identity.LockPath)
	if err != nil {
		t.Fatalf("Acquire() error = %v", err)
	}
	defer lock.Release()

	if err := RunRelay(context.Background(), options); !errors.Is(err, processlock.ErrAlreadyRunning) {
		t.Fatalf("RunRelay() error = %v", err)
	}
	canceled, cancel := context.WithCancel(context.Background())
	cancel()
	if err := RunManagedRelay(canceled, options); !errors.Is(err, context.Canceled) {
		t.Fatalf("RunManagedRelay() error = %v", err)
	}
}

func writeRelayIdentityConfig(t *testing.T, socketPath string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "config.json")
	content := `{
  "relay": {
    "url": "wss://127.0.0.1:1/hprp",
    "key": "hpk_7_AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA",
    "skip_verify": true
  },
  "herdr": {
    "session": "",
    "socket_path": "` + socketPath + `"
  },
  "log": {"level": "info"}
}`
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatalf("WriteFile() error = %v", err)
	}
	return path
}
