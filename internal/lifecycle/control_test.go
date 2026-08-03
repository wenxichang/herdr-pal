//go:build !windows

package lifecycle

import (
	"bufio"
	"context"
	"encoding/json"
	"net"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestControlServerReturnsStatusAndCleansSocket(t *testing.T) {
	controlPath := shortControlPath(t)
	ctx, cancel := context.WithCancel(context.Background())
	serverResult := make(chan error, 1)
	server := NewControlServer()
	go func() {
		serverResult <- server.Run(ctx, controlPath, func() Status {
			return Status{State: StateRunning, Herdr: HerdrHealthy, WorkerPID: 42}
		})
	}()
	waitForControlSocket(t, controlPath)

	got, err := NewControlClient().Status(context.Background(), controlPath)
	if err != nil {
		t.Fatalf("Status() error = %v", err)
	}
	if got.State != StateRunning || got.Herdr != HerdrHealthy || got.WorkerPID != 42 {
		t.Fatalf("Status() = %#v", got)
	}
	info, err := os.Stat(controlPath)
	if err != nil {
		t.Fatalf("Stat(control socket) error = %v", err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("control socket permissions = %o, want 600", info.Mode().Perm())
	}
	parentInfo, err := os.Stat(filepath.Dir(controlPath))
	if err != nil {
		t.Fatalf("Stat(control directory) error = %v", err)
	}
	if parentInfo.Mode().Perm() != 0o700 {
		t.Fatalf("control directory permissions = %o, want 700", parentInfo.Mode().Perm())
	}

	cancel()
	select {
	case err := <-serverResult:
		if err != nil {
			t.Fatalf("ControlServer.Run() error = %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("ControlServer.Run() did not stop")
	}
	if _, err := os.Lstat(controlPath); !os.IsNotExist(err) {
		t.Fatalf("control socket still exists: %v", err)
	}
}

func TestControlServerRejectsUnknownAndMalformedRequests(t *testing.T) {
	controlPath := shortControlPath(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	serverResult := make(chan error, 1)
	go func() {
		serverResult <- NewControlServer().Run(ctx, controlPath, func() Status {
			return Status{State: StateRunning, Herdr: HerdrHealthy}
		})
	}()
	waitForControlSocket(t, controlPath)

	unknown := rawControlRequest(t, controlPath, `{"method":"restart"}`)
	if unknown.Error == "" || unknown.Status != nil {
		t.Fatalf("unknown response = %#v", unknown)
	}
	malformed := rawControlRequest(t, controlPath, `{not-json}`)
	if malformed.Error == "" || malformed.Status != nil {
		t.Fatalf("malformed response = %#v", malformed)
	}
}

func shortControlPath(t *testing.T) string {
	t.Helper()
	root, err := os.MkdirTemp("/tmp", "hpctl-")
	if err != nil {
		t.Fatalf("MkdirTemp() error = %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(root) })
	return filepath.Join(root, "runtime", "control.sock")
}

func waitForControlSocket(t *testing.T, path string) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if info, err := os.Stat(path); err == nil && info.Mode()&os.ModeSocket != 0 {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("control socket %q was not created", path)
}

func rawControlRequest(t *testing.T, path, request string) ControlResponse {
	t.Helper()
	connection, err := net.DialTimeout("unix", path, time.Second)
	if err != nil {
		t.Fatalf("Dial() error = %v", err)
	}
	defer connection.Close()
	if _, err := connection.Write([]byte(request + "\n")); err != nil {
		t.Fatalf("Write() error = %v", err)
	}
	line, err := bufio.NewReader(connection).ReadBytes('\n')
	if err != nil {
		t.Fatalf("ReadBytes() error = %v", err)
	}
	var response ControlResponse
	if err := json.Unmarshal(line, &response); err != nil {
		t.Fatalf("Unmarshal() error = %v", err)
	}
	return response
}
