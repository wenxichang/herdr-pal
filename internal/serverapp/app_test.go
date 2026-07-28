package serverapp

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/wenxichang/herdr-pal/internal/adminserver"
)

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
	runtimeLogger, err := newLogger(&logs, "error", true, "secret-sensitive")
	if err != nil {
		t.Fatal(err)
	}
	runtimeLogger.Logger.Debug("服务端详细诊断", "reason", "upstream rejected secret-sensitive")
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
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	stopped := make(chan string, 4)
	shutdownCalls := 0
	err := runServerComponents(ctx, nil, waitingServerComponents(stopped), func(context.Context) {
		shutdownCalls++
	}, nil)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("runServerComponents() error = %v", err)
	}
	if shutdownCalls != 1 {
		t.Fatalf("shutdown calls = %d, want 1", shutdownCalls)
	}
	for range 4 {
		select {
		case <-stopped:
		default:
			t.Fatal("runServerComponents returned before all components stopped")
		}
	}
}

func TestRunServerComponentsLogsFailedComponentWithReason(t *testing.T) {
	for failingIndex, failingName := range []string{"wecom_connection", "wecom_event_loop", "relay_http", "admin"} {
		t.Run(failingName, func(t *testing.T) {
			var logs bytes.Buffer
			logger := slog.New(slog.NewTextHandler(&logs, &slog.HandlerOptions{Level: slog.LevelDebug}))
			stopped := make(chan string, 3)
			components := waitingServerComponents(stopped)
			components[failingIndex].run = func(context.Context) error { return errors.New("component exhausted") }
			shutdownCalls := 0

			err := runServerComponents(context.Background(), nil, components, func(context.Context) {
				shutdownCalls++
			}, logger)

			if err == nil || !strings.Contains(err.Error(), "component exhausted") {
				t.Fatalf("runServerComponents() error = %v", err)
			}
			if shutdownCalls != 1 {
				t.Fatalf("shutdown calls = %d, want 1", shutdownCalls)
			}
			for range 3 {
				select {
				case <-stopped:
				default:
					t.Fatal("failed component did not trigger full component cleanup")
				}
			}
			output := logs.String()
			for _, want := range []string{"服务端组件异常退出", "component=" + failingName, "error_type=component_exit", "reason=\"component exhausted\""} {
				if !strings.Contains(output, want) {
					t.Fatalf("logs = %q, want %q", output, want)
				}
			}
		})
	}
}

func TestRunServerComponentsTreatsAdminStopAsNormalShutdown(t *testing.T) {
	stopRequested := make(chan struct{})
	close(stopRequested)
	stopped := make(chan string, 4)
	shutdownCalls := 0
	if err := runServerComponents(context.Background(), stopRequested, waitingServerComponents(stopped), func(context.Context) {
		shutdownCalls++
	}, nil); err != nil {
		t.Fatalf("runServerComponents(admin stop) error = %v", err)
	}
	if shutdownCalls != 1 || len(stopped) != 4 {
		t.Fatalf("admin stop cleanup = calls:%d stopped:%d", shutdownCalls, len(stopped))
	}
}

func TestRunServerFailsWhenAdminSocketCannotStartAndClosesRelay(t *testing.T) {
	stateDir, err := os.MkdirTemp("/tmp", "hp-serverapp-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(stateDir) })
	if err := os.Chmod(stateDir, 0o700); err != nil {
		t.Fatal(err)
	}
	adminPath := filepath.Join(stateDir, adminserver.DefaultSocketFileName)
	if err := os.WriteFile(adminPath, []byte("occupied"), 0o600); err != nil {
		t.Fatal(err)
	}
	probe, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	listenAddress := probe.Addr().String()
	probe.Close()
	configPath := filepath.Join(t.TempDir(), "server.json")
	raw := fmt.Sprintf(`{"wecom":{"bot_id":"bot-test"},"server":{"listen":%q,"state_dir":%q},"log":{}}`, listenAddress, stateDir)
	if err := os.WriteFile(configPath, []byte(raw), 0o600); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	err = Run(ctx, Options{
		ConfigPath: configPath,
		Getenv:     func(string) string { return "wecom-secret" },
		Stderr:     &bytes.Buffer{},
	})
	if err == nil || !strings.Contains(err.Error(), "HPAP Admin Socket") {
		t.Fatalf("Run() error = %v, want HPAP Admin Socket detail", err)
	}
	listener, listenErr := net.Listen("tcp", listenAddress)
	if listenErr != nil {
		t.Fatalf("Relay listener leaked after Admin startup failure: %v", listenErr)
	}
	listener.Close()
}

func waitingServerComponents(stopped chan<- string) []serverComponent {
	names := []string{"wecom_connection", "wecom_event_loop", "relay_http", "admin"}
	components := make([]serverComponent, 0, len(names))
	for _, name := range names {
		componentName := name
		components = append(components, serverComponent{name: componentName, run: func(ctx context.Context) error {
			<-ctx.Done()
			stopped <- componentName
			return ctx.Err()
		}})
	}
	return components
}
