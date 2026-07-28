package serverapp

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/wenxichang/herdr-pal/internal/credential"
	"github.com/wenxichang/herdr-pal/internal/im"
)

func TestRunIssuesMachineKeyWithoutWeComSecret(t *testing.T) {
	stateDir := t.TempDir()
	configPath := filepath.Join(t.TempDir(), "server.json")
	configJSON := `{"wecom":{},"server":{"state_dir":"` + filepath.ToSlash(stateDir) + `"},"log":{}}`
	if err := os.WriteFile(configPath, []byte(configJSON), 0o600); err != nil {
		t.Fatalf("WriteFile() error = %v", err)
	}
	var stdout bytes.Buffer
	err := Run(context.Background(), Options{
		ConfigPath: configPath, Stdout: &stdout,
		KeyIssue: &KeyIssueOptions{PrincipalID: "user-a", MachineID: "office-pc"},
	})
	if err != nil {
		t.Fatalf("Run(key issue) error = %v", err)
	}
	line := strings.TrimSpace(stdout.String())
	if !strings.HasPrefix(line, "hpk_") {
		t.Fatalf("key output = %q", line)
	}
	store, err := credential.LoadStore(filepath.Join(stateDir, "credentials.json"))
	if err != nil {
		t.Fatalf("LoadStore() error = %v", err)
	}
	identity, err := store.VerifyBearer(context.Background(), line)
	if err != nil || identity.PrincipalID != "user-a" || identity.MachineID != "office-pc" {
		t.Fatalf("VerifyBearer() = %#v, %v", identity, err)
	}
}

func TestBuildRelayURLHintUsesConfiguredAddressAndListenPort(t *testing.T) {
	got, err := buildRelayURLHint("10.1.3.4", "0.0.0.0:9443")
	if err != nil {
		t.Fatalf("buildRelayURLHint() error = %v", err)
	}
	if got != "wss://10.1.3.4:9443" {
		t.Fatalf("buildRelayURLHint() = %q, want wss://10.1.3.4:9443", got)
	}
}

func TestBuildRelayURLHintUsesPlaceholderWhenAddressIsEmpty(t *testing.T) {
	got, err := buildRelayURLHint("", "127.0.0.1:9555")
	if err != nil {
		t.Fatalf("buildRelayURLHint() error = %v", err)
	}
	if got != "wss://管理员提供的地址:9555" {
		t.Fatalf("buildRelayURLHint() = %q, want placeholder URL", got)
	}
}

func TestBuildRelayURLHintRejectsAddressWithPortOrScheme(t *testing.T) {
	for _, address := range []string{"10.1.3.4:9555", "wss://10.1.3.4", "host/path", "host\nother"} {
		if _, err := buildRelayURLHint(address, "0.0.0.0:9443"); err == nil {
			t.Fatalf("buildRelayURLHint(%q) should reject invalid address", address)
		}
	}
}

func TestNewLoggerRejectsUnknownLevel(t *testing.T) {
	if _, err := newLogger(&bytes.Buffer{}, "verbose", false); err == nil {
		t.Fatal("newLogger() should reject unknown level")
	}
}

func TestNewLoggerVerboseForcesDebugAndRedactsSecret(t *testing.T) {
	var logs bytes.Buffer
	logger, err := newLogger(&logs, "error", true, "secret-sensitive")
	if err != nil {
		t.Fatal(err)
	}
	logger.Debug("服务端详细诊断", "reason", "upstream rejected secret-sensitive")
	output := logs.String()
	for _, want := range []string{"level=DEBUG", "服务端详细诊断", "reason=\"upstream rejected [REDACTED]\""} {
		if !strings.Contains(output, want) {
			t.Fatalf("logs = %q, want %q", output, want)
		}
	}
	if strings.Contains(output, "secret-sensitive") {
		t.Fatalf("logs leaked secret: %q", output)
	}
}

func TestRunServerComponentsStopsHTTPAndWeComOnContextCancellation(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	weCom := &fakeWeComRuntime{events: make(chan im.IncomingText), stopped: make(chan struct{})}
	httpServer := &http.Server{Handler: http.HandlerFunc(func(http.ResponseWriter, *http.Request) {})}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	err = runServerComponents(ctx, weCom, nil, httpServer, listener, nil)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("runServerComponents() error = %v", err)
	}
	select {
	case <-weCom.stopped:
	default:
		t.Fatal("runServerComponents returned before WeCom runtime stopped")
	}
}

func TestRunServerComponentsLogsFailedComponentWithReason(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	var logs bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&logs, &slog.HandlerOptions{Level: slog.LevelDebug}))
	weCom := &failingWeComRuntime{events: make(chan im.IncomingText), err: errors.New("dial exhausted")}
	httpServer := &http.Server{Handler: http.HandlerFunc(func(http.ResponseWriter, *http.Request) {})}

	err = runServerComponents(context.Background(), weCom, nil, httpServer, listener, logger)

	if err == nil || !strings.Contains(err.Error(), "dial exhausted") {
		t.Fatalf("runServerComponents() error = %v", err)
	}
	output := logs.String()
	for _, want := range []string{"服务端组件异常退出", "component=wecom_connection", "error_type=component_exit", "reason=\"dial exhausted\""} {
		if !strings.Contains(output, want) {
			t.Fatalf("logs = %q, want %q", output, want)
		}
	}
}

type fakeWeComRuntime struct {
	events  chan im.IncomingText
	stopped chan struct{}
}

func (runtime *fakeWeComRuntime) Run(ctx context.Context) error {
	<-ctx.Done()
	close(runtime.stopped)
	return ctx.Err()
}

func (runtime *fakeWeComRuntime) Events() <-chan im.IncomingText { return runtime.events }

type failingWeComRuntime struct {
	events chan im.IncomingText
	err    error
}

func (runtime *failingWeComRuntime) Run(context.Context) error { return runtime.err }

func (runtime *failingWeComRuntime) Events() <-chan im.IncomingText { return runtime.events }
