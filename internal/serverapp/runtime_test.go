package serverapp

import (
	"bytes"
	"log/slog"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/wenxichang/herdr-pal/internal/adminproto"
	"github.com/wenxichang/herdr-pal/internal/credential"
	"github.com/wenxichang/herdr-pal/internal/hprp"
	"github.com/wenxichang/herdr-pal/internal/server"
	"github.com/wenxichang/herdr-pal/internal/wecom"
)

func TestDynamicLoggerSwitchesDebugAndRestoresBaseLevel(t *testing.T) {
	var logs bytes.Buffer
	runtimeLogger, err := newLogger(&logs, "warn", false)
	if err != nil {
		t.Fatal(err)
	}
	if runtimeLogger.BaseLevel() != slog.LevelWarn || runtimeLogger.DebugEnabled() {
		t.Fatalf("initial logger state: base=%s debug=%t", runtimeLogger.BaseLevel(), runtimeLogger.DebugEnabled())
	}
	runtimeLogger.Logger.Debug("hidden-before-enable")
	runtimeLogger.EnableDebug()
	if !runtimeLogger.DebugEnabled() {
		t.Fatal("debug should be enabled immediately")
	}
	runtimeLogger.Logger.Debug("visible-after-enable")
	runtimeLogger.DisableDebug()
	if runtimeLogger.DebugEnabled() || runtimeLogger.CurrentLevel() != slog.LevelWarn {
		t.Fatalf("disabled logger state: current=%s debug=%t", runtimeLogger.CurrentLevel(), runtimeLogger.DebugEnabled())
	}
	runtimeLogger.Logger.Debug("hidden-after-disable")
	runtimeLogger.Logger.Warn("visible-base-level")
	output := logs.String()
	if strings.Contains(output, "hidden-before-enable") || strings.Contains(output, "hidden-after-disable") {
		t.Fatalf("debug logs leaked outside override: %q", output)
	}
	for _, want := range []string{"visible-after-enable", "visible-base-level"} {
		if !strings.Contains(output, want) {
			t.Fatalf("logs = %q, want %q", output, want)
		}
	}

	restarted, err := newLogger(&bytes.Buffer{}, "warn", false)
	if err != nil {
		t.Fatal(err)
	}
	if restarted.CurrentLevel() != slog.LevelWarn || restarted.DebugEnabled() {
		t.Fatalf("restart did not restore base level: current=%s debug=%t", restarted.CurrentLevel(), restarted.DebugEnabled())
	}
}

func TestDynamicLoggerVerboseCanReturnToConfiguredBaseLevel(t *testing.T) {
	runtimeLogger, err := newLogger(&bytes.Buffer{}, "error", true)
	if err != nil {
		t.Fatal(err)
	}
	if runtimeLogger.BaseLevel() != slog.LevelError || !runtimeLogger.DebugEnabled() {
		t.Fatalf("verbose logger state: base=%s current=%s", runtimeLogger.BaseLevel(), runtimeLogger.CurrentLevel())
	}
	runtimeLogger.DisableDebug()
	if runtimeLogger.CurrentLevel() != slog.LevelError || runtimeLogger.DebugEnabled() {
		t.Fatalf("disable did not restore configured level: current=%s", runtimeLogger.CurrentLevel())
	}
}

func TestRuntimeServerStatusAggregatesSafeSnapshots(t *testing.T) {
	now := time.Date(2026, 7, 28, 9, 30, 0, 0, time.UTC)
	startedAt := now.Add(-5 * time.Minute)
	runtimeLogger, err := newLogger(&bytes.Buffer{}, "warn", true)
	if err != nil {
		t.Fatal(err)
	}
	expiresLater := now.Add(time.Hour)
	expiredAt := now.Add(-time.Second)
	weComStatus := wecom.StatusSnapshot{State: wecom.StatusReconnecting, ChangedAt: now.Add(-time.Minute), LastErrorType: "dns"}
	connections := []server.ConnectionView{
		{ConnectionID: "c-1", PrincipalID: "user-a", MachineID: "home"},
		{ConnectionID: "c-2", PrincipalID: "user-a", MachineID: "office"},
		{ConnectionID: "c-3", PrincipalID: "user-b", MachineID: "home"},
	}
	sessions := []server.SessionView{
		{PrincipalID: "user-a", Number: 1},
		{PrincipalID: "user-a", Number: 2},
		{PrincipalID: "user-b", Number: 1},
		{PrincipalID: "user-b", Number: 2},
	}
	credentials := []credential.Record{
		{CredentialID: 1, Status: credential.StatusEnabled, ExpiresAt: &expiresLater},
		{CredentialID: 2, Status: credential.StatusDisabled},
		{CredentialID: 3, Status: credential.StatusEnabled, ExpiresAt: &expiredAt},
	}
	stops := 0
	inspector, err := NewRuntimeInspector(RuntimeConfig{
		StartedAt:      startedAt,
		Now:            func() time.Time { return now },
		PID:            4321,
		GOOS:           "test-os",
		GOARCH:         "test-arch",
		Version:        "v9.8.7",
		Commit:         "abcdef0",
		BuiltAt:        "2026-07-28T08:00:00Z",
		RelayListen:    "0.0.0.0:9443",
		AdminSocket:    "/state/admin.sock",
		WebAdminListen: "0.0.0.0:4001",
		TLS: server.TLSInfo{
			Mode:              server.TLSModeAutomatic,
			NotAfter:          now.Add(24 * time.Hour),
			SHA256Fingerprint: strings.Repeat("a", 64),
		},
		Stop: func() { stops++ },
	}, runtimeLogger, fakeWeComStatusProvider{status: weComStatus}, fakeConnectionSnapshotProvider{connections: connections}, fakeSessionSnapshotProvider{sessions: sessions}, fakeCredentialSnapshotProvider{records: credentials})
	if err != nil {
		t.Fatal(err)
	}

	status := inspector.Status()
	if status.ObservedAt != now || status.StartedAt != startedAt || status.UptimeMS != (5*time.Minute).Milliseconds() {
		t.Fatalf("runtime times = %#v", status)
	}
	if status.Version != "v9.8.7" || status.Commit != "abcdef0" || status.BuiltAt != "2026-07-28T08:00:00Z" || status.PID != 4321 || status.GOOS != "test-os" || status.GOARCH != "test-arch" {
		t.Fatalf("runtime build info = %#v", status)
	}
	if status.HPAP != adminproto.Protocol || status.HPRP != hprp.ProtocolVersion || status.RelayListen != "0.0.0.0:9443" || status.AdminSocket != "/state/admin.sock" || status.WebAdminListen != "0.0.0.0:4001" {
		t.Fatalf("runtime protocol/listener info = %#v", status)
	}
	if status.TLS.Mode != server.TLSModeAutomatic || status.WeCom.State != string(weComStatus.State) || status.WeCom.ChangedAt != weComStatus.ChangedAt || status.WeCom.LastErrorType != weComStatus.LastErrorType {
		t.Fatalf("dependency status = %#v", status)
	}
	if !status.DebugEnabled || status.BaseLogLevel != "warn" || status.PrincipalCount != 2 || status.ConnectionCount != 3 || status.SessionCount != 4 {
		t.Fatalf("runtime counts/logger = %#v", status)
	}
	if status.Credentials != (CredentialCounts{Enabled: 1, Disabled: 1, Expired: 1}) {
		t.Fatalf("credential counts = %#v", status.Credentials)
	}
	if !inspector.RequestStop() || inspector.RequestStop() || stops != 1 {
		t.Fatalf("stop results: calls=%d", stops)
	}

	inspector.DisableDebug()
	if inspector.Status().DebugEnabled {
		t.Fatal("runtime debug disable was not reflected")
	}
	inspector.EnableDebug()
	if !inspector.Status().DebugEnabled {
		t.Fatal("runtime debug enable was not reflected")
	}
}

func TestRuntimeConfigDefaultsToCurrentProcessMetadata(t *testing.T) {
	logger, err := newLogger(&bytes.Buffer{}, "info", false)
	if err != nil {
		t.Fatal(err)
	}
	inspector, err := NewRuntimeInspector(RuntimeConfig{}, logger, fakeWeComStatusProvider{}, fakeConnectionSnapshotProvider{}, fakeSessionSnapshotProvider{}, fakeCredentialSnapshotProvider{})
	if err != nil {
		t.Fatal(err)
	}
	status := inspector.Status()
	if status.PID <= 0 || status.GOOS != runtime.GOOS || status.GOARCH != runtime.GOARCH || status.Version == "" || status.StartedAt.IsZero() || status.ObservedAt.Before(status.StartedAt) {
		t.Fatalf("default runtime status = %#v", status)
	}
}

type fakeWeComStatusProvider struct{ status wecom.StatusSnapshot }

func (provider fakeWeComStatusProvider) Status() wecom.StatusSnapshot { return provider.status }

type fakeConnectionSnapshotProvider struct{ connections []server.ConnectionView }

func (provider fakeConnectionSnapshotProvider) Connections() []server.ConnectionView {
	return append([]server.ConnectionView(nil), provider.connections...)
}

type fakeSessionSnapshotProvider struct{ sessions []server.SessionView }

func (provider fakeSessionSnapshotProvider) ManagementSessions(server.SessionFilter) []server.SessionView {
	return append([]server.SessionView(nil), provider.sessions...)
}

type fakeCredentialSnapshotProvider struct{ records []credential.Record }

func (provider fakeCredentialSnapshotProvider) List() []credential.Record {
	return append([]credential.Record(nil), provider.records...)
}
