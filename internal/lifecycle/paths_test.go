package lifecycle

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestNewRuntimePathsUsesStableSocketHash(t *testing.T) {
	paths, err := NewRuntimePaths("/cache", "/control", "/logs", "socket-a")
	if err != nil {
		t.Fatalf("NewRuntimePaths() error = %v", err)
	}
	const wantHash = "b6f13b50f554af2e"
	if paths.InstanceHash != wantHash {
		t.Fatalf("InstanceHash = %q, want %q", paths.InstanceHash, wantHash)
	}
	if paths.StartLock != filepath.Join("/cache", "herdr-pal", wantHash+".start.lock") {
		t.Fatalf("StartLock = %q", paths.StartLock)
	}
	if paths.OwnerLock != filepath.Join("/cache", "herdr-pal", wantHash+".lock") {
		t.Fatalf("OwnerLock = %q", paths.OwnerLock)
	}
	if paths.ControlSocket != filepath.Join("/control", "herdr-pal", wantHash+".sock") {
		t.Fatalf("ControlSocket = %q", paths.ControlSocket)
	}
	if paths.LogFile != filepath.Join("/logs", "herdr-pal", "herdr-pal.log") {
		t.Fatalf("LogFile = %q", paths.LogFile)
	}
	for _, path := range []string{paths.StartLock, paths.OwnerLock, paths.ControlSocket, paths.LogFile} {
		if strings.Contains(path, "socket-a") {
			t.Fatalf("runtime path leaks socket identity: %q", path)
		}
	}
}

func TestPrepareRuntimeDirectoriesCreatesPrivateLockDirectory(t *testing.T) {
	root := t.TempDir()
	paths := RuntimePaths{
		StartLock: filepath.Join(root, "locks", "start.lock"),
		OwnerLock: filepath.Join(root, "locks", "owner.lock"),
	}
	if err := PrepareRuntimeDirectories(paths); err != nil {
		t.Fatalf("PrepareRuntimeDirectories() error = %v", err)
	}
	info, err := os.Stat(filepath.Dir(paths.OwnerLock))
	if err != nil {
		t.Fatalf("Stat() error = %v", err)
	}
	if info.Mode().Perm() != 0o700 {
		t.Fatalf("lock directory permissions = %o", info.Mode().Perm())
	}
}

func TestDefaultRuntimePathsDoesNotExposeSocketIdentity(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_STATE_HOME", filepath.Join(home, "state"))
	paths, err := DefaultRuntimePaths("secret-socket-path")
	if err != nil {
		if err == ErrUnsupported {
			t.Skip("platform does not support managed lifecycle")
		}
		t.Fatalf("DefaultRuntimePaths() error = %v", err)
	}
	for _, path := range []string{paths.StartLock, paths.OwnerLock, paths.ControlSocket, paths.LogFile} {
		if strings.Contains(path, "secret-socket-path") {
			t.Fatalf("default path exposes socket identity: %q", path)
		}
	}
}

func TestNewRuntimePathsRejectsMissingInputs(t *testing.T) {
	tests := []struct {
		name        string
		cacheRoot   string
		controlRoot string
		logRoot     string
		identity    string
	}{
		{name: "cache", controlRoot: "/control", logRoot: "/logs", identity: "socket"},
		{name: "control", cacheRoot: "/cache", logRoot: "/logs", identity: "socket"},
		{name: "log", cacheRoot: "/cache", controlRoot: "/control", identity: "socket"},
		{name: "identity", cacheRoot: "/cache", controlRoot: "/control", logRoot: "/logs"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if _, err := NewRuntimePaths(test.cacheRoot, test.controlRoot, test.logRoot, test.identity); err == nil {
				t.Fatal("NewRuntimePaths() should fail")
			}
		})
	}
}
