package app

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/wenxichang/herdr-pal/internal/bridge"
	"github.com/wenxichang/herdr-pal/internal/config"
	"github.com/wenxichang/herdr-pal/internal/herdr"
	"github.com/wenxichang/herdr-pal/internal/hprp"
	"github.com/wenxichang/herdr-pal/internal/interactive"
	"github.com/wenxichang/herdr-pal/internal/processlock"
	"github.com/wenxichang/herdr-pal/internal/relayclient"
	"github.com/wenxichang/herdr-pal/internal/session"
	"github.com/wenxichang/herdr-pal/internal/wecom"
)

func TestRunRejectsConfigurationBeforeStartingConnections(t *testing.T) {
	connectionCalls := 0
	options := testOptions(t)
	options.dependencies.loadConfig = func(string, func(string) string) (config.Config, error) {
		return config.Config{}, errors.New("配置损坏")
	}
	options.dependencies.acquireLock = func(string) (processLock, error) {
		connectionCalls++
		return &fakeLock{}, nil
	}
	options.dependencies.resolveSocket = func(context.Context, string, string, herdr.CommandRunner) (string, error) {
		connectionCalls++
		return "", nil
	}
	options.dependencies.assemble = func(config.Config, string, *slog.Logger) (*applicationRuntime, error) {
		connectionCalls++
		return nil, nil
	}

	err := Run(context.Background(), options)
	if !errors.Is(err, ErrConfig) || !strings.Contains(err.Error(), "配置损坏") {
		t.Fatalf("Run() error = %v, want wrapped configuration error", err)
	}
	if connectionCalls != 0 {
		t.Fatalf("connection setup calls = %d, want 0", connectionCalls)
	}
}

func TestRelayAppBuildsTerminalRendererAndAdvertisesImage(t *testing.T) {
	renderer, err := newRelayTerminalRenderer()
	if err != nil {
		t.Fatalf("newRelayTerminalRenderer() error = %v", err)
	}
	result, err := renderer.Render(context.Background(), "\x1b[31m红色终端\x1b[0m")
	if err != nil {
		t.Fatalf("Render() error = %v", err)
	}
	if len(result.PNG) < 8 || !bytes.Equal(result.PNG[:8], []byte{'\x89', 'P', 'N', 'G', '\r', '\n', '\x1a', '\n'}) {
		t.Fatalf("renderer PNG invalid: %x", result.PNG)
	}
}

func TestRunReportsLockConflictWithoutResolvingSocket(t *testing.T) {
	options := testOptions(t)
	resolveCalls := 0
	options.dependencies.acquireLock = func(string) (processLock, error) {
		return nil, processlock.ErrAlreadyRunning
	}
	options.dependencies.resolveSocket = func(context.Context, string, string, herdr.CommandRunner) (string, error) {
		resolveCalls++
		return "", nil
	}

	err := Run(context.Background(), options)
	if !errors.Is(err, processlock.ErrAlreadyRunning) || !strings.Contains(err.Error(), "已在运行") {
		t.Fatalf("Run() error = %v, want clear lock conflict", err)
	}
	if resolveCalls != 0 {
		t.Fatalf("ResolveSocket calls = %d, want 0", resolveCalls)
	}
}

func TestOptionsDoesNotExposeRemovedUserDiscoveryMode(t *testing.T) {
	if _, exists := reflect.TypeOf(Options{}).FieldByName("DiscoverUser"); exists {
		t.Fatal("Options still exposes removed DiscoverUser mode")
	}
}

func TestRunInteractiveLoadsOptionalConfigWithoutWeComDependencies(t *testing.T) {
	options := testInteractiveOptions(t)
	options.ConfigPath = ""
	loadInteractiveCalls := 0
	getenvCalls := 0
	options.Getenv = func(string) string {
		getenvCalls++
		return "must-not-be-read"
	}
	options.dependencies.loadConfig = func(string, func(string) string) (config.Config, error) {
		t.Fatal("interactive Run called normal config loader")
		return config.Config{}, nil
	}
	options.dependencies.loadInteractiveConfig = func(path string) (config.Config, error) {
		loadInteractiveCalls++
		if path != "" {
			t.Fatalf("LoadInteractive path = %q, want empty", path)
		}
		return testConfig(), nil
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := Run(ctx, options); err != nil {
		t.Fatalf("Run() error = %v", err)
	}
	if loadInteractiveCalls != 1 || getenvCalls != 0 {
		t.Fatalf("interactive/getenv calls = %d/%d, want 1/0", loadInteractiveCalls, getenvCalls)
	}
}

func TestRunInteractiveResolvesSocketBeforeSocketScopedLock(t *testing.T) {
	options := testInteractiveOptions(t)
	socketDir := t.TempDir()
	separator := string(filepath.Separator)
	rawSocketPath := filepath.Join(socketDir, "nested") + separator + ".." + separator + "final-interactive.sock"
	canonicalSocketPath := resolvedChildPath(t, socketDir, "final-interactive.sock")
	var order []string
	options.dependencies.loadInteractiveConfig = func(string) (config.Config, error) {
		order = append(order, "load")
		return testConfig(), nil
	}
	options.dependencies.resolveSocket = func(_ context.Context, explicit, sessionName string, runner herdr.CommandRunner) (string, error) {
		order = append(order, "resolve")
		if explicit != "" || sessionName != "named-session" || runner == nil {
			t.Fatalf("ResolveSocket args = %q, %q, %v", explicit, sessionName, runner)
		}
		return rawSocketPath, nil
	}
	options.dependencies.userCacheDir = func() (string, error) {
		order = append(order, "cache")
		return "/cache", nil
	}
	options.dependencies.mkdirAll = func(string, os.FileMode) error {
		order = append(order, "mkdir")
		return nil
	}
	options.dependencies.acquireLock = func(path string) (processLock, error) {
		order = append(order, "lock")
		want := filepath.Join("/cache", "herdr-pal", "interactive-"+shortHash(canonicalSocketPath)+".lock")
		if path != want {
			t.Fatalf("interactive lock path = %q, want %q", path, want)
		}
		return &fakeLock{}, nil
	}
	options.dependencies.prepareStableDialPath = func(canonicalEndpoint string) (*dialPathLease, error) {
		order = append(order, "dial")
		return prepareStableDialPath(canonicalEndpoint)
	}
	options.dependencies.assembleInteractive = func(_ config.Config, socketPath string, _ io.Reader, _ io.Writer, _ *slog.Logger) (*applicationRuntime, error) {
		order = append(order, "assemble")
		if identity := resolvedDialPath(t, socketPath); identity != canonicalSocketPath {
			t.Fatalf("assembled socket identity = %q, want canonical endpoint %q", identity, canonicalSocketPath)
		}
		return canceledRuntime(), nil
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := Run(ctx, options); err != nil {
		t.Fatalf("Run() error = %v", err)
	}
	wantOrder := []string{"load", "resolve", "cache", "mkdir", "lock", "dial", "assemble"}
	if fmt.Sprint(order) != fmt.Sprint(wantOrder) {
		t.Fatalf("interactive startup order = %v, want %v", order, wantOrder)
	}
}

func TestRunInteractiveCanonicalizesSocketAliasesForLockIdentity(t *testing.T) {
	tempDir := t.TempDir()
	target := filepath.Join(tempDir, "agent.sock")
	separator := string(filepath.Separator)
	cleanAlias := filepath.Join(tempDir, "nested") + separator + ".." + separator + "agent.sock"
	workingDir, err := os.Getwd()
	if err != nil {
		t.Fatalf("os.Getwd() error = %v", err)
	}
	relativeTarget, err := filepath.Rel(workingDir, target)
	if err != nil {
		t.Fatalf("filepath.Rel() error = %v", err)
	}

	assertSameSocketPlan := func(name, first, second string) {
		t.Helper()
		firstPlan := captureInteractiveSocketPlan(t, first, nil)
		secondPlan := captureInteractiveSocketPlan(t, second, nil)
		if firstPlan.lockPath != secondPlan.lockPath || firstPlan.assembledIdentity != secondPlan.assembledIdentity {
			t.Fatalf("%s plans = %#v/%#v, want same lock and connection path", name, firstPlan, secondPlan)
		}
	}
	assertSameSocketPlan("clean alias", target, cleanAlias)
	assertSameSocketPlan("relative alias", target, relativeTarget)

	other := filepath.Join(tempDir, "other.sock")
	if first, second := captureInteractiveSocketPlan(t, target, nil), captureInteractiveSocketPlan(t, other, nil); first.lockPath == second.lockPath || first.assembledIdentity == second.assembledIdentity {
		t.Fatalf("different sockets share plan %#v/%#v", first, second)
	}

	t.Run("existing symlink leaf", func(t *testing.T) {
		if err := os.WriteFile(target, nil, 0o600); err != nil {
			t.Fatalf("create symlink target: %v", err)
		}
		symlink := filepath.Join(tempDir, "agent-link.sock")
		if err := os.Symlink(target, symlink); err != nil {
			t.Skipf("platform does not support symlink: %v", err)
		}
		assertSameSocketPlan("symlink alias", target, symlink)
	})

}

func TestCanonicalSocketPathPreservesWindowsMarkerSpelling(t *testing.T) {
	tempDir := t.TempDir()
	target := filepath.Join(tempDir, "target.sock")
	if err := os.WriteFile(target, nil, 0o600); err != nil {
		t.Fatalf("create marker target: %v", err)
	}
	alias := filepath.Join(tempDir, "marker-link.sock")
	if err := os.Symlink(target, alias); err != nil {
		t.Skipf("platform does not support symlink: %v", err)
	}
	want, err := filepath.Abs(filepath.Clean(alias))
	if err != nil {
		t.Fatalf("filepath.Abs() error = %v", err)
	}

	got, err := canonicalSocketPathForPlatform(alias, false)
	if err != nil {
		t.Fatalf("canonicalSocketPathForPlatform() error = %v", err)
	}
	if got != want {
		t.Fatalf("Windows marker identity = %q, want spelling-preserving path %q", got, want)
	}
}

func TestRunInteractiveResolvesSymlinkParentWithMissingSocketLeaf(t *testing.T) {
	tempDir := t.TempDir()
	realDir := filepath.Join(tempDir, "real-parent")
	if err := os.Mkdir(realDir, 0o700); err != nil {
		t.Fatalf("create real parent: %v", err)
	}
	aliasDir := filepath.Join(tempDir, "alias-parent")
	if err := os.Symlink(realDir, aliasDir); err != nil {
		t.Skipf("platform does not support symlink: %v", err)
	}
	realSocket := resolvedChildPath(t, realDir, "missing.sock")
	aliasSocket := filepath.Join(aliasDir, "missing.sock")
	realPlan := captureInteractiveSocketPlan(t, filepath.Join(realDir, "missing.sock"), nil)
	aliasPlan := captureInteractiveSocketPlan(t, aliasSocket, nil)
	if realPlan.lockPath != aliasPlan.lockPath || realPlan.assembledIdentity != aliasPlan.assembledIdentity {
		t.Fatalf("missing-leaf plans = %#v/%#v, want same stable path", realPlan, aliasPlan)
	}
	if aliasPlan.assembledIdentity != realSocket {
		t.Fatalf("assembled socket identity = %q, want resolved parent path %q", aliasPlan.assembledIdentity, realSocket)
	}
}

func TestRunInteractiveRejectsMissingSocketParentBeforeLockAndAlias(t *testing.T) {
	tempDir := t.TempDir()
	missingParent := filepath.Join(tempDir, "future-parent")
	aliasEndpoint := filepath.Join(missingParent, "herdr.sock")
	options := testInteractiveOptions(t)
	options.dependencies.resolveSocket = func(context.Context, string, string, herdr.CommandRunner) (string, error) {
		return aliasEndpoint, nil
	}
	lockCalls := 0
	prepareCalls := 0
	assembleCalls := 0
	options.dependencies.acquireLock = func(string) (processLock, error) {
		lockCalls++
		return &fakeLock{}, nil
	}
	options.dependencies.prepareStableDialPath = func(string) (*dialPathLease, error) {
		prepareCalls++
		return &dialPathLease{path: "/tmp/must-not-run.sock"}, nil
	}
	options.dependencies.assembleInteractive = func(config.Config, string, io.Reader, io.Writer, *slog.Logger) (*applicationRuntime, error) {
		assembleCalls++
		return canceledRuntime(), nil
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	err := Run(ctx, options)
	if err == nil || !strings.Contains(err.Error(), "无法可靠") {
		t.Fatalf("Run() error = %v, want safe missing-parent rejection", err)
	}
	if lockCalls != 0 || prepareCalls != 0 || assembleCalls != 0 {
		t.Fatalf("lock/prepare/assemble calls = %d/%d/%d, want 0/0/0", lockCalls, prepareCalls, assembleCalls)
	}
	if strings.Contains(err.Error(), aliasEndpoint) || strings.Contains(err.Error(), missingParent) {
		t.Fatalf("Run() error leaked missing path: %v", err)
	}

	realParent := filepath.Join(tempDir, "real-parent")
	if err := os.Mkdir(realParent, 0o700); err != nil {
		t.Fatalf("create real parent: %v", err)
	}
	if err := os.Symlink(realParent, missingParent); err != nil {
		t.Skipf("platform does not support symlink: %v", err)
	}
	aliasPlan := captureInteractiveSocketPlan(t, aliasEndpoint, nil)
	realPlan := captureInteractiveSocketPlan(t, filepath.Join(realParent, "herdr.sock"), nil)
	if aliasPlan.lockPath != realPlan.lockPath || aliasPlan.assembledIdentity != realPlan.assembledIdentity {
		t.Fatalf("created-parent plans = %#v/%#v, want one canonical identity", aliasPlan, realPlan)
	}
}

func TestRunInteractiveRejectsSocketParentThatIsNotDirectory(t *testing.T) {
	tempDir := t.TempDir()
	parentFile := filepath.Join(tempDir, "not-a-directory")
	if err := os.WriteFile(parentFile, nil, 0o600); err != nil {
		t.Fatalf("create parent file: %v", err)
	}
	assertInteractiveSocketRejectedBeforeLock(t, filepath.Join(parentFile, "herdr.sock"), []string{parentFile})
}

func TestRunInteractiveFreezesCanonicalSocketAcrossSymlinkRetarget(t *testing.T) {
	tempDir := t.TempDir()
	longParent := filepath.Join(tempDir, strings.Repeat("long-parent-", 9))
	dirA := filepath.Join(longParent, "socket-a")
	dirB := filepath.Join(longParent, "socket-b")
	for _, dir := range []string{dirA, dirB} {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			t.Fatalf("create socket directory: %v", err)
		}
	}
	aliasDir := filepath.Join(tempDir, "current")
	if err := os.Symlink(dirA, aliasDir); err != nil {
		t.Skipf("platform does not support symlink: %v", err)
	}
	aliasSocket := filepath.Join(aliasDir, "herdr.sock")
	wantA := resolvedChildPath(t, dirA, "herdr.sock")
	wantB := resolvedChildPath(t, dirB, "herdr.sock")
	for _, endpoint := range []string{wantA, wantB} {
		if err := os.WriteFile(endpoint, nil, 0o600); err != nil {
			t.Fatalf("create socket endpoint marker: %v", err)
		}
		if len([]byte(endpoint)) < unixSocketPathByteLimit {
			t.Fatalf("test endpoint length = %d, want at least %d", len([]byte(endpoint)), unixSocketPathByteLimit)
		}
	}

	planA := captureInteractiveSocketPlan(t, aliasSocket, func() {
		if err := os.Remove(aliasDir); err != nil {
			t.Fatalf("remove original alias: %v", err)
		}
		if err := os.Symlink(dirB, aliasDir); err != nil {
			t.Fatalf("retarget alias: %v", err)
		}
	})
	if planA.assembledIdentity != wantA {
		t.Fatalf("first assembled socket identity = %q, want frozen %q", planA.assembledIdentity, wantA)
	}
	if planA.assembledSocket == wantA || len([]byte(planA.assembledSocket)) >= unixSocketPathByteLimit {
		t.Fatalf("first dial path length/path = %d/%q, want short private alias", len([]byte(planA.assembledSocket)), planA.assembledSocket)
	}
	wantLockA := filepath.Join("/cache", "herdr-pal", "interactive-"+shortHash(wantA)+".lock")
	if planA.lockPath != wantLockA {
		t.Fatalf("first lock path = %q, want frozen target lock %q", planA.lockPath, wantLockA)
	}
	planB := captureInteractiveSocketPlan(t, aliasSocket, nil)
	if planB.assembledIdentity != wantB {
		t.Fatalf("second assembled socket identity = %q, want retargeted %q", planB.assembledIdentity, wantB)
	}
	wantLockB := filepath.Join("/cache", "herdr-pal", "interactive-"+shortHash(wantB)+".lock")
	if planB.lockPath != wantLockB {
		t.Fatalf("second lock path = %q, want retargeted target lock %q", planB.lockPath, wantLockB)
	}
	if planA.lockPath == planB.lockPath {
		t.Fatalf("different retargeted endpoints share lock %q", planA.lockPath)
	}
}

func TestPrepareStableDialPathKeepsShortCanonicalEndpoint(t *testing.T) {
	canonicalEndpoint := "/tmp/herdr-short.sock"
	lease, err := prepareStableDialPath(canonicalEndpoint)
	if err != nil {
		t.Fatalf("prepareStableDialPath() error = %v", err)
	}
	t.Cleanup(func() { _ = lease.Close() })
	if lease.Path() != canonicalEndpoint {
		t.Fatalf("stable dial path = %q, want canonical endpoint %q", lease.Path(), canonicalEndpoint)
	}
	if lease.aliasDir != "" {
		t.Fatalf("short canonical endpoint created alias directory %q", lease.aliasDir)
	}
}

func TestPrepareStableDialPathCreatesDialableLeafAliasForLongEndpointAndBasename(t *testing.T) {
	longDirectory := filepath.Join(t.TempDir(), strings.Repeat("long-directory-", 8))
	if err := os.MkdirAll(longDirectory, 0o700); err != nil {
		t.Fatalf("create long socket directory: %v", err)
	}
	socketBasename := strings.Repeat("s", 78)
	canonicalEndpoint := resolvedChildPath(t, longDirectory, socketBasename)
	if len([]byte(canonicalEndpoint)) < unixSocketPathByteLimit {
		t.Fatalf("test endpoint length = %d, want at least %d", len([]byte(canonicalEndpoint)), unixSocketPathByteLimit)
	}
	legacyAliasPath := filepath.Join("/tmp", stableDialAliasPattern, stableDialAliasLinkName, socketBasename)
	if len([]byte(legacyAliasPath)) < unixSocketPathByteLimit {
		t.Fatalf("legacy parent-alias fixture length = %d, want at least %d", len([]byte(legacyAliasPath)), unixSocketPathByteLimit)
	}

	listenerAliasDir, err := os.MkdirTemp("/tmp", "h")
	if err != nil {
		t.Fatalf("create listener alias directory: %v", err)
	}
	listenerAlias := filepath.Join(listenerAliasDir, "s")
	if err := os.Symlink(longDirectory, listenerAlias); err != nil {
		_ = os.Remove(listenerAliasDir)
		t.Skipf("platform does not support symlink: %v", err)
	}
	t.Cleanup(func() {
		_ = os.Remove(listenerAlias)
		_ = os.Remove(listenerAliasDir)
	})
	listener, err := net.Listen("unix", filepath.Join(listenerAlias, filepath.Base(canonicalEndpoint)))
	if err != nil {
		t.Fatalf("listen through short alias: %v", err)
	}
	t.Cleanup(func() { _ = listener.Close() })

	lease, err := prepareStableDialPath(canonicalEndpoint)
	if err != nil {
		t.Fatalf("prepareStableDialPath() error = %v", err)
	}
	t.Cleanup(func() { _ = lease.Close() })
	if len([]byte(lease.Path())) >= unixSocketPathByteLimit {
		t.Fatalf("stable dial path length = %d, want below %d", len([]byte(lease.Path())), unixSocketPathByteLimit)
	}
	info, err := os.Stat(lease.aliasDir)
	if err != nil {
		t.Fatalf("stat alias directory: %v", err)
	}
	if permissions := info.Mode().Perm(); permissions != 0o700 {
		t.Fatalf("alias directory permissions = %o, want 700", permissions)
	}
	if filepath.Dir(lease.Path()) != lease.aliasDir {
		t.Fatalf("stable dial path = %q, want direct leaf alias in %q", lease.Path(), lease.aliasDir)
	}
	if identity := resolvedDialPath(t, lease.Path()); identity != canonicalEndpoint {
		t.Fatalf("stable dial identity = %q, want %q", identity, canonicalEndpoint)
	}
	connection, err := net.DialTimeout("unix", lease.Path(), time.Second)
	if err != nil {
		t.Fatalf("dial stable path: %v", err)
	}
	_ = connection.Close()
}

func TestPrepareStableDialPathUsesByteLimitAndShortensLongBasename(t *testing.T) {
	withinLimit := "/" + strings.Repeat("a", unixSocketPathByteLimit-2)
	if len([]byte(withinLimit)) != unixSocketPathByteLimit-1 {
		t.Fatalf("within-limit fixture length = %d", len([]byte(withinLimit)))
	}
	direct, err := prepareStableDialPath(withinLimit)
	if err != nil {
		t.Fatalf("prepare within-limit path: %v", err)
	}
	if direct.Path() != withinLimit || direct.aliasDir != "" {
		t.Fatalf("within-limit stable path = %q alias=%q, want direct", direct.Path(), direct.aliasDir)
	}

	atLimit := "/" + strings.Repeat("b", unixSocketPathByteLimit-3) + "/s"
	if len([]byte(atLimit)) != unixSocketPathByteLimit {
		t.Fatalf("at-limit fixture length = %d", len([]byte(atLimit)))
	}
	aliased, err := prepareStableDialPath(atLimit)
	if err != nil {
		t.Fatalf("prepare at-limit path: %v", err)
	}
	t.Cleanup(func() { _ = aliased.Close() })
	if aliased.Path() == atLimit || len([]byte(aliased.Path())) >= unixSocketPathByteLimit {
		t.Fatalf("at-limit stable path length/path = %d/%q", len([]byte(aliased.Path())), aliased.Path())
	}

	sensitiveEndpoint := "/" + strings.Repeat("socket-sensitive-", 10)
	longBasename, err := prepareStableDialPath(sensitiveEndpoint)
	if err != nil {
		t.Fatalf("prepare long basename endpoint: %v", err)
	}
	t.Cleanup(func() { _ = longBasename.Close() })
	if len([]byte(longBasename.Path())) >= unixSocketPathByteLimit || filepath.Dir(longBasename.Path()) != longBasename.aliasDir {
		t.Fatalf("long-basename stable path length/path = %d/%q", len([]byte(longBasename.Path())), longBasename.Path())
	}

	if strings.Contains(errStableDialPathTooLong.Error(), sensitiveEndpoint) {
		t.Fatalf("stable path length error leaked canonical endpoint: %v", errStableDialPathTooLong)
	}
}

func TestPrepareStableDialPathBypassesUnixAliasForWindowsTransport(t *testing.T) {
	endpoint := `C:\Users\alice\AppData\Local\herdr\` + strings.Repeat("long-", 32) + `herdr.sock`

	lease, err := prepareStableDialPathForPlatform(endpoint, false)
	if err != nil {
		t.Fatalf("prepareStableDialPathForPlatform() error = %v", err)
	}
	t.Cleanup(func() { _ = lease.Close() })
	if lease.Path() != endpoint {
		t.Fatalf("Windows dial path = %q, want marker path %q", lease.Path(), endpoint)
	}
	if lease.aliasDir != "" || lease.aliasLink != "" {
		t.Fatalf("Windows dial path created Unix alias: %#v", lease)
	}
}

func TestRunInteractiveRejectsBlankResolvedSocketBeforeLock(t *testing.T) {
	options := testInteractiveOptions(t)
	options.dependencies.resolveSocket = func(context.Context, string, string, herdr.CommandRunner) (string, error) {
		return " \t\n ", nil
	}
	lockCalls := 0
	assembleCalls := 0
	options.dependencies.acquireLock = func(string) (processLock, error) {
		lockCalls++
		return &fakeLock{}, nil
	}
	options.dependencies.assembleInteractive = func(config.Config, string, io.Reader, io.Writer, *slog.Logger) (*applicationRuntime, error) {
		assembleCalls++
		return canceledRuntime(), nil
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	err := Run(ctx, options)
	if err == nil || !strings.Contains(err.Error(), "Socket 路径不能为空") {
		t.Fatalf("Run() error = %v, want clear blank socket error", err)
	}
	if lockCalls != 0 || assembleCalls != 0 {
		t.Fatalf("lock/assemble calls = %d/%d, want 0/0", lockCalls, assembleCalls)
	}
}

func TestRunInteractiveRejectsDanglingSymlinksBeforeLock(t *testing.T) {
	tempDir := t.TempDir()
	tests := []struct {
		name       string
		socketPath func(*testing.T) (string, []string)
	}{
		{
			name: "dangling leaf",
			socketPath: func(t *testing.T) (string, []string) {
				targetDir := filepath.Join(tempDir, "leaf-target")
				if err := os.Mkdir(targetDir, 0o700); err != nil {
					t.Fatalf("create target directory: %v", err)
				}
				target := filepath.Join(targetDir, "missing.sock")
				alias := filepath.Join(tempDir, "dangling-leaf.sock")
				if err := os.Symlink(target, alias); err != nil {
					t.Skipf("platform does not support symlink: %v", err)
				}
				return alias, []string{alias, target}
			},
		},
		{
			name: "dangling parent",
			socketPath: func(t *testing.T) (string, []string) {
				targetDir := filepath.Join(tempDir, "missing-parent")
				aliasDir := filepath.Join(tempDir, "dangling-parent")
				if err := os.Symlink(targetDir, aliasDir); err != nil {
					t.Skipf("platform does not support symlink: %v", err)
				}
				socketPath := filepath.Join(aliasDir, "herdr.sock")
				return socketPath, []string{socketPath, aliasDir, targetDir}
			},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			socketPath, sensitivePaths := test.socketPath(t)
			assertInteractiveSocketRejectedBeforeLock(t, socketPath, sensitivePaths)
		})
	}
}

func TestRunInteractiveSymlinkLoopIsBounded(t *testing.T) {
	tempDir := t.TempDir()
	first := filepath.Join(tempDir, "first")
	second := filepath.Join(tempDir, "second")
	if err := os.Symlink(second, first); err != nil {
		t.Skipf("platform does not support symlink: %v", err)
	}
	if err := os.Symlink(first, second); err != nil {
		t.Skipf("platform does not support symlink loop: %v", err)
	}
	socketPath := filepath.Join(first, "herdr.sock")
	assertInteractiveSocketRejectedBeforeLock(t, socketPath, []string{socketPath, first, second})
}

func TestRunInteractiveConflictsOnSameSocketWithIndependentLockName(t *testing.T) {
	options := testInteractiveOptions(t)
	var lockMu sync.Mutex
	locked := false
	var lockPaths []string
	options.dependencies.acquireLock = func(path string) (processLock, error) {
		lockMu.Lock()
		defer lockMu.Unlock()
		lockPaths = append(lockPaths, path)
		if locked {
			return nil, processlock.ErrAlreadyRunning
		}
		locked = true
		return &fakeLock{onRelease: func() {
			lockMu.Lock()
			locked = false
			lockMu.Unlock()
		}}, nil
	}
	im := newFakeWeCom()
	supervisor := newFakeRunner()
	var assemblyCalls atomic.Int32
	options.dependencies.assembleInteractive = func(config.Config, string, io.Reader, io.Writer, *slog.Logger) (*applicationRuntime, error) {
		if assemblyCalls.Add(1) > 1 {
			return canceledRuntime(), nil
		}
		return &applicationRuntime{im: im, supervisor: supervisor, handler: &fakeHandler{}}, nil
	}

	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	firstResult := make(chan error, 1)
	go func() { firstResult <- Run(ctx, options) }()
	waitClosed(t, im.started, "first interactive IM")
	waitClosed(t, supervisor.started, "first interactive Supervisor")

	secondCtx, cancelSecond := context.WithCancel(context.Background())
	secondResult := make(chan error, 1)
	go func() { secondResult <- Run(secondCtx, options) }()
	var err error
	timedOut := false
	select {
	case err = <-secondResult:
	case <-time.After(time.Second):
		timedOut = true
		cancelSecond()
		err = waitResult(t, secondResult)
	}
	cancelSecond()
	cancel()
	firstErr := waitResult(t, firstResult)
	if timedOut {
		t.Fatalf("second Run did not reach bounded lock result: %v", err)
	}
	if !errors.Is(err, processlock.ErrAlreadyRunning) {
		t.Fatalf("second Run() error = %v, want lock conflict", err)
	}
	if firstErr != nil {
		t.Fatalf("first Run() error = %v", firstErr)
	}

	lockMu.Lock()
	defer lockMu.Unlock()
	if len(lockPaths) != 2 || lockPaths[0] != lockPaths[1] {
		t.Fatalf("interactive lock paths = %#v, want same socket lock", lockPaths)
	}
	normalLock := filepath.Join("/cache", "herdr-pal", shortHash("bot-sensitive")+".lock")
	if lockPaths[0] == normalLock || !strings.Contains(filepath.Base(lockPaths[0]), "interactive-") {
		t.Fatalf("interactive lock = %q, must differ from bot lock %q", lockPaths[0], normalLock)
	}
}

func TestRunInteractiveUsesDefaultStreamsAndRunnerWithoutGetenv(t *testing.T) {
	options := testInteractiveOptions(t)
	options.Stdin = nil
	options.Stdout = nil
	options.Runner = nil
	options.Getenv = func(string) string {
		t.Fatal("interactive Run called Getenv")
		return ""
	}
	options.dependencies.resolveSocket = func(_ context.Context, _, _ string, runner herdr.CommandRunner) (string, error) {
		if runner == nil {
			t.Fatal("interactive Run passed nil command runner")
		}
		return "/tmp/default-streams.sock", nil
	}
	options.dependencies.assembleInteractive = func(_ config.Config, _ string, input io.Reader, output io.Writer, _ *slog.Logger) (*applicationRuntime, error) {
		if input != os.Stdin || output != os.Stdout {
			t.Fatalf("interactive streams = %v/%v, want os.Stdin/os.Stdout", input, output)
		}
		return canceledRuntime(), nil
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := Run(ctx, options); err != nil {
		t.Fatalf("Run() error = %v", err)
	}
}

func TestRunResolvesSocketBeforeAssemblyAndUsesSafeLockName(t *testing.T) {
	options := testOptions(t)
	var lockPath, assembledSocket string
	options.dependencies.acquireLock = func(path string) (processLock, error) {
		lockPath = path
		return &fakeLock{}, nil
	}
	options.dependencies.resolveSocket = func(_ context.Context, explicit, sessionName string, runner herdr.CommandRunner) (string, error) {
		if explicit != "" || sessionName != "named-session" || runner == nil {
			t.Fatalf("ResolveSocket args = %q, %q, %v", explicit, sessionName, runner)
		}
		return "/tmp/herdr-test.sock", nil
	}
	options.dependencies.assemble = func(_ config.Config, socket string, _ *slog.Logger) (*applicationRuntime, error) {
		assembledSocket = socket
		return canceledRuntime(), nil
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := Run(ctx, options); err != nil {
		t.Fatalf("Run() error = %v", err)
	}
	if assembledSocket != "/tmp/herdr-test.sock" {
		t.Fatalf("assembled socket = %q", assembledSocket)
	}
	if !strings.HasPrefix(lockPath, "/cache/herdr-pal/") || !strings.HasSuffix(lockPath, ".lock") {
		t.Fatalf("lock path = %q", lockPath)
	}
	for _, sensitive := range []string{"bot-sensitive", "secret-sensitive"} {
		if strings.Contains(lockPath, sensitive) {
			t.Fatalf("lock path contains sensitive value %q: %q", sensitive, lockPath)
		}
	}
}

func TestAssembleBridgeRuntimeSharesOneHerdrClientAcrossAllBridgeUsers(t *testing.T) {
	managed := newFakeManagedHerdr()
	im := newFakeWeCom()
	runtime, err := assembleBridgeRuntime(im, "bridge-user", managed, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatalf("assembleBridgeRuntime() error = %v", err)
	}
	connected, err := runtime.factory.Connect(context.Background())
	if err != nil || connected != managed {
		t.Fatalf("factory.Connect() = %v, %v, want shared client", connected, err)
	}

	runtime.service.SetHerdr(managed)
	runtime.service.ReplaceSnapshot(managed.snapshot, false)
	for index, content := range []string{"/ls", "/1", "继续处理"} {
		runtime.service.HandleMessage(context.Background(), incomingForUser("bridge-user", "service-"+string(rune('a'+index)), content))
	}
	if got := managed.promptCount(); got != 1 {
		t.Fatalf("shared client prompt calls = %d, want 1", got)
	}
	getsBeforeNotification := managed.getCount()

	target := (&session.Registry{})
	target.Replace(managed.snapshot, false)
	selected := target.CreateListSnapshot()[0]
	err = runtime.notifier.HandleTransition(context.Background(), session.Transition{
		Target: selected, Previous: herdr.AgentStatusWorking, Current: herdr.AgentStatusBlocked,
	})
	if err != nil {
		t.Fatalf("Notifier.HandleTransition() error = %v", err)
	}
	if gotGet, gotRead := managed.getCount()-getsBeforeNotification, managed.readCount(); gotGet != 1 || gotRead != 1 {
		t.Fatalf("shared notifier get/read calls = %d/%d, want 1/1", gotGet, gotRead)
	}
}

func TestAssembleBridgeRuntimeRejectsNilLogger(t *testing.T) {
	_, err := assembleBridgeRuntime(newFakeWeCom(), "user-sensitive", newFakeManagedHerdr(), nil)
	if err == nil || !strings.Contains(err.Error(), "结构化日志器无效") {
		t.Fatalf("assembleBridgeRuntime() error = %v, want invalid logger", err)
	}
}

func TestAssembleBridgeRuntimeRejectsNilIM(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	_, err := assembleBridgeRuntime(nil, "user-sensitive", newFakeManagedHerdr(), logger)
	if err == nil || !strings.Contains(err.Error(), "IM Adapter 无效") {
		t.Fatalf("assembleBridgeRuntime() error = %v, want invalid IM adapter", err)
	}
}

func TestAssembleBridgeRuntimeRejectsNilHerdrClient(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	_, err := assembleBridgeRuntime(newFakeWeCom(), "user-sensitive", nil, logger)
	if err == nil || !strings.Contains(err.Error(), "Herdr Client 无效") {
		t.Fatalf("assembleBridgeRuntime() error = %v, want invalid Herdr client", err)
	}
}

func TestAssembleRuntimeCreatesConfiguredClients(t *testing.T) {
	managed := newFakeManagedHerdr()
	im := newFakeWeCom()
	dependencies := defaultAssemblyDependencies()
	dependencies.newHerdr = func(socketPath string) bridge.ManagedHerdr {
		if socketPath != "/tmp/shared.sock" {
			t.Fatalf("newHerdr socket = %q", socketPath)
		}
		return managed
	}
	dependencies.newWeCom = func(clientConfig wecom.ClientConfig) (imRuntime, error) {
		if clientConfig.Endpoint != wecom.DefaultEndpoint || clientConfig.BotID != "bot-sensitive" ||
			clientConfig.AllowedUserID != "user-sensitive" || clientConfig.Secret != "secret-sensitive" {
			t.Fatalf("WeCom config = %s", clientConfig)
		}
		return im, nil
	}

	runtime, err := assembleRuntime(testConfig(), "/tmp/shared.sock", slog.New(slog.NewTextHandler(io.Discard, nil)), dependencies)
	if err != nil {
		t.Fatalf("assembleRuntime() error = %v", err)
	}
	if runtime.im != im || runtime.herdr != managed {
		t.Fatalf("assembleRuntime() clients = %v/%v, want configured IM and Herdr clients", runtime.im, runtime.herdr)
	}
}

func TestAssembleInteractiveUsesLocalGuardAndSharedHerdrClient(t *testing.T) {
	managed := newFakeManagedHerdr()
	im := newFakeWeCom()
	input := strings.NewReader("")
	var output bytes.Buffer
	dependencies := defaultAssemblyDependencies()
	dependencies.newHerdr = func(socketPath string) bridge.ManagedHerdr {
		if socketPath != "/tmp/interactive.sock" {
			t.Fatalf("newHerdr socket = %q", socketPath)
		}
		return managed
	}
	dependencies.newInteractive = func(gotInput io.Reader, gotOutput io.Writer) (imRuntime, error) {
		if gotInput != input || gotOutput != &output {
			t.Fatalf("interactive streams = %v/%v, want injected streams", gotInput, gotOutput)
		}
		return im, nil
	}

	runtime, err := assembleInteractiveRuntime(
		testConfig(), "/tmp/interactive.sock", input, &output,
		slog.New(slog.NewTextHandler(io.Discard, nil)), dependencies,
	)
	if err != nil {
		t.Fatalf("assembleInteractiveRuntime() error = %v", err)
	}
	if runtime.im != im || runtime.herdr != managed {
		t.Fatalf("interactive runtime clients = %v/%v, want shared clients", runtime.im, runtime.herdr)
	}
	connected, err := runtime.factory.Connect(context.Background())
	if err != nil || connected != managed {
		t.Fatalf("factory.Connect() = %v, %v, want shared Herdr client", connected, err)
	}
	if runtime.normalIMExit == nil || !runtime.normalIMExit(fmt.Errorf("wrapped: %w", interactive.ErrInputClosed)) || runtime.normalIMExit(errors.New("fatal")) {
		t.Fatal("interactive normal IM exit matcher does not recognize only ErrInputClosed")
	}

	runtime.service.SetHerdr(managed)
	runtime.service.ReplaceSnapshot(managed.snapshot, false)
	for index, content := range []string{"/ls", "/1", "继续处理"} {
		runtime.service.HandleMessage(context.Background(), incomingForUser(interactive.UserID, fmt.Sprintf("interactive-%d", index), content))
	}
	runtime.service.HandleMessage(context.Background(), incomingForUser("unknown-local-user", "interactive-unknown", "不得转发"))
	if got := managed.promptCount(); got != 1 {
		t.Fatalf("interactive shared client prompt calls = %d, want only allowed local user", got)
	}
}

func TestAssembleInteractiveNormalExitMatcherRejectsMixedJoinedError(t *testing.T) {
	runtime := assembledInteractiveTestRuntime(t, newFakeWeCom(), newFakeRunner(), &fakeHandler{})
	fatal := errors.New("fatal")
	deepEOF := error(interactive.ErrInputClosed)
	for range 128 {
		deepEOF = fmt.Errorf("wrapped: %w", deepEOF)
	}
	tests := []struct {
		name string
		err  error
		want bool
	}{
		{name: "直接 EOF", err: interactive.ErrInputClosed, want: true},
		{name: "单链包装 EOF", err: fmt.Errorf("wrapped: %w", interactive.ErrInputClosed), want: true},
		{name: "联合纯 EOF", err: errors.Join(interactive.ErrInputClosed, interactive.ErrInputClosed), want: true},
		{name: "联合 EOF 和 fatal", err: errors.Join(interactive.ErrInputClosed, fatal), want: false},
		{name: "仅实现 Is", err: isOnlyError{target: interactive.ErrInputClosed}, want: false},
		{name: "过深包装链", err: deepEOF, want: false},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got := runtime.normalIMExit(test.err); got != test.want {
				t.Fatalf("normalIMExit(%v) = %v, want %v", test.err, got, test.want)
			}
		})
	}
}

func TestIsPureErrorTreeRejectsCyclicUnwrap(t *testing.T) {
	cycle := &cyclicUnwrapError{}
	cycle.next = cycle
	if isPureErrorTree(cycle, interactive.ErrInputClosed) {
		t.Fatal("isPureErrorTree() accepted cyclic unwrap")
	}
}

func TestAssembleInteractiveWrapsAdapterCreationError(t *testing.T) {
	root := errors.New("adapter creation failed")
	dependencies := defaultAssemblyDependencies()
	dependencies.newHerdr = func(string) bridge.ManagedHerdr { return newFakeManagedHerdr() }
	dependencies.newInteractive = func(io.Reader, io.Writer) (imRuntime, error) { return nil, root }

	_, err := assembleInteractiveRuntime(
		testConfig(), "/tmp/interactive.sock", strings.NewReader(""), io.Discard,
		slog.New(slog.NewTextHandler(io.Discard, nil)), dependencies,
	)
	if !errors.Is(err, root) || !strings.Contains(err.Error(), "创建交互适配器") {
		t.Fatalf("assembleInteractiveRuntime() error = %v, want wrapped adapter creation error", err)
	}
}

func TestAssembleInteractiveRejectsNilLogger(t *testing.T) {
	_, err := assembleInteractiveRuntime(
		testConfig(), "/tmp/interactive.sock", strings.NewReader(""), io.Discard,
		nil, defaultAssemblyDependencies(),
	)
	if err == nil || !strings.Contains(err.Error(), "结构化日志器无效") {
		t.Fatalf("assembleInteractiveRuntime() error = %v, want invalid logger", err)
	}
}

func TestInteractiveDefaultAssemblyCreatesAdapter(t *testing.T) {
	adapter, err := defaultAssemblyDependencies().newInteractive(strings.NewReader(""), io.Discard)
	if err != nil || adapter == nil {
		t.Fatalf("newInteractive() = %v, %v, want adapter", adapter, err)
	}
}

func TestAssembleBridgeRuntimeInjectsSafeStructuredKeyAuditLogger(t *testing.T) {
	managed := newFakeManagedHerdr()
	im := newFakeWeCom()
	var output bytes.Buffer
	logger := slog.New(slog.NewJSONHandler(&output, &slog.HandlerOptions{Level: slog.LevelError}))
	logger.Info("普通信息应被过滤", slog.String("content", "private ordinary log"))
	runtime, err := assembleBridgeRuntime(im, "user-sensitive", managed, logger)
	if err != nil {
		t.Fatalf("assembleBridgeRuntime() error = %v", err)
	}
	runtime.service.SetHerdr(managed)
	runtime.service.ReplaceSnapshot(managed.snapshot, false)
	for index, content := range []string{"/ls", "/1", "/enter"} {
		runtime.service.HandleMessage(context.Background(), incoming(fmt.Sprintf("audit-%d", index), content))
	}

	var record map[string]any
	if err := json.Unmarshal(bytes.TrimSpace(output.Bytes()), &record); err != nil {
		t.Fatalf("audit log is not one JSON record: %v; output=%q", err, output.String())
	}
	if record["msg"] != "显式按键审计" || record["user_hash"] != shortHash("user-sensitive") || record["user_id"] != nil ||
		record["pane_id"] != "pane-1" || record["key"] != "enter" || record["result"] != "sent" {
		t.Fatalf("audit log fields = %#v", record)
	}
	occupantHash, ok := record["occupant_hash"].(string)
	if !ok || len(occupantHash) != 64 || record["at"] == nil {
		t.Fatalf("audit log occupant/time = %#v/%#v", record["occupant_hash"], record["at"])
	}
	for _, sensitive := range []string{"user-sensitive", "secret-sensitive", "bot-sensitive", "recent terminal"} {
		if strings.Contains(output.String(), sensitive) {
			t.Fatalf("audit log leaked %q: %q", sensitive, output.String())
		}
	}
	if strings.Contains(output.String(), "普通信息应被过滤") || strings.Contains(output.String(), "private ordinary log") {
		t.Fatalf("audit handler changed ordinary logger filtering: %q", output.String())
	}
}

func TestAssembleRuntimeUsesOfficialWeComEndpointByDefault(t *testing.T) {
	dependencies := defaultAssemblyDependencies()
	dependencies.newHerdr = func(string) bridge.ManagedHerdr { return newFakeManagedHerdr() }
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	dependencies.newWeCom = func(clientConfig wecom.ClientConfig) (imRuntime, error) {
		if clientConfig.Endpoint != wecom.DefaultEndpoint {
			t.Fatalf("WeCom endpoint = %q, want %q", clientConfig.Endpoint, wecom.DefaultEndpoint)
		}
		if clientConfig.Logger != logger {
			t.Fatal("WeCom client 未复用应用结构化日志器")
		}
		return newFakeWeCom(), nil
	}

	if _, err := assembleRuntime(testConfig(), "/tmp/shared.sock", logger, dependencies); err != nil {
		t.Fatalf("assembleRuntime() error = %v", err)
	}
}

func TestRunStartsAllLoopsAndConsumesMessages(t *testing.T) {
	im := newFakeWeCom()
	supervisor := newFakeRunner()
	handler := &fakeHandler{handled: make(chan wecom.IncomingText, 1)}
	runtime := &applicationRuntime{im: im, supervisor: supervisor, handler: handler}
	options := testOptions(t)
	options.dependencies.assemble = func(config.Config, string, *slog.Logger) (*applicationRuntime, error) { return runtime, nil }

	ctx, cancel := context.WithCancel(context.Background())
	result := make(chan error, 1)
	go func() { result <- Run(ctx, options) }()
	waitClosed(t, im.started, "IM Run")
	waitClosed(t, supervisor.started, "Supervisor Run")
	im.events <- incoming("message-1", "/ls")
	select {
	case message := <-handler.handled:
		if message.MessageID != "message-1" {
			t.Fatalf("handled message = %#v", message)
		}
	case <-time.After(time.Second):
		t.Fatal("message consumer did not start")
	}
	cancel()
	if err := waitResult(t, result); err != nil {
		t.Fatalf("Run() error = %v", err)
	}
}

func TestRunDoesNotStartLoopsWhenContextIsAlreadyCanceled(t *testing.T) {
	im := newFakeWeCom()
	supervisor := newFakeRunner()
	lock := &fakeLock{}
	options := testOptions(t)
	options.dependencies.acquireLock = func(string) (processLock, error) { return lock, nil }
	options.dependencies.assemble = func(config.Config, string, *slog.Logger) (*applicationRuntime, error) {
		return &applicationRuntime{im: im, supervisor: supervisor, handler: &fakeHandler{}}, nil
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	if err := Run(ctx, options); err != nil {
		t.Fatalf("Run() error = %v", err)
	}
	for name, started := range map[string]<-chan struct{}{"IM": im.started, "Supervisor": supervisor.started} {
		select {
		case <-started:
			t.Fatalf("%s loop started with an already canceled context", name)
		default:
		}
	}
	if got := lock.releases.Load(); got != 1 {
		t.Fatalf("lock releases = %d, want 1 without live components", got)
	}
}

func TestRunCancelsOtherLoopsAfterFatalError(t *testing.T) {
	fatal := errors.New("supervisor fatal")
	im := newFakeWeCom()
	supervisor := newFakeRunner()
	supervisor.result = fatal
	lock := &fakeLock{}
	options := testOptions(t)
	options.dependencies.acquireLock = func(string) (processLock, error) { return lock, nil }
	options.dependencies.assemble = func(config.Config, string, *slog.Logger) (*applicationRuntime, error) {
		return &applicationRuntime{im: im, supervisor: supervisor, handler: &fakeHandler{}}, nil
	}

	err := Run(context.Background(), options)
	if !errors.Is(err, fatal) {
		t.Fatalf("Run() error = %v, want fatal supervisor error", err)
	}
	waitClosed(t, im.stopped, "IM cancellation")
	if got := lock.releases.Load(); got != 1 {
		t.Fatalf("lock releases = %d, want 1 after fatal shutdown", got)
	}
}

func TestRunPreservesIMFatalRegardlessOfResultOrder(t *testing.T) {
	fatal := errors.New("im fatal")
	tests := []struct {
		name        string
		closeEvents bool
		waitForStop bool
	}{
		{name: "IM 错误先到"},
		{name: "Events 关闭先到", closeEvents: true, waitForStop: true},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			supervisor := newFakeRunner()
			im := newOrderedResultWeCom(fatal)
			im.closeEvents = test.closeEvents
			if test.waitForStop {
				im.returnAfter = supervisor.stopped
			}
			options := testOptions(t)
			options.dependencies.assemble = func(config.Config, string, *slog.Logger) (*applicationRuntime, error) {
				return &applicationRuntime{im: im, supervisor: supervisor, handler: &fakeHandler{}}, nil
			}

			err := Run(context.Background(), options)
			if !errors.Is(err, fatal) {
				t.Fatalf("Run() error = %v, want IM fatal", err)
			}
			waitClosed(t, supervisor.stopped, "Supervisor cancellation")
		})
	}
}

func TestRunInteractiveTreatsInputEOFAsNormalAndReleasesLock(t *testing.T) {
	im := newOrderedResultWeCom(interactive.ErrInputClosed)
	supervisor := newFakeRunner()
	lock := &fakeLock{}
	options := testInteractiveOptions(t)
	options.dependencies.acquireLock = func(string) (processLock, error) { return lock, nil }
	options.dependencies.assembleInteractive = func(config.Config, string, io.Reader, io.Writer, *slog.Logger) (*applicationRuntime, error) {
		return &applicationRuntime{
			im: im, supervisor: supervisor, handler: &fakeHandler{},
			normalIMExit: func(err error) bool { return errors.Is(err, interactive.ErrInputClosed) },
		}, nil
	}

	if err := Run(context.Background(), options); err != nil {
		t.Fatalf("Run() error = %v, want normal EOF shutdown", err)
	}
	waitClosed(t, supervisor.stopped, "interactive EOF Supervisor cancellation")
	if got := lock.releases.Load(); got != 1 {
		t.Fatalf("lock releases = %d, want 1 after normal EOF", got)
	}
}

func TestRunInteractiveCleansLongDialAliasAfterNormalExit(t *testing.T) {
	canonicalEndpoint := longCanonicalEndpoint(t)
	im := newOrderedResultWeCom(interactive.ErrInputClosed)
	supervisor := newFakeRunner()
	options := testInteractiveOptions(t)
	options.dependencies.resolveSocket = func(context.Context, string, string, herdr.CommandRunner) (string, error) {
		return canonicalEndpoint, nil
	}
	var dialPath string
	options.dependencies.assembleInteractive = func(_ config.Config, stableDialPath string, _ io.Reader, _ io.Writer, _ *slog.Logger) (*applicationRuntime, error) {
		dialPath = stableDialPath
		return &applicationRuntime{
			im: im, supervisor: supervisor, handler: &fakeHandler{},
			normalIMExit: func(err error) bool { return errors.Is(err, interactive.ErrInputClosed) },
		}, nil
	}

	if err := Run(context.Background(), options); err != nil {
		t.Fatalf("Run() error = %v, want normal EOF shutdown", err)
	}
	assertPrivateDialAliasRemoved(t, dialPath)
}

func TestRunInteractiveCleansLongDialAliasAfterAssemblyFailure(t *testing.T) {
	canonicalEndpoint := longCanonicalEndpoint(t)
	assemblyFailure := errors.New("assembly failure")
	lock := &fakeLock{}
	options := testInteractiveOptions(t)
	options.dependencies.resolveSocket = func(context.Context, string, string, herdr.CommandRunner) (string, error) {
		return canonicalEndpoint, nil
	}
	options.dependencies.acquireLock = func(string) (processLock, error) { return lock, nil }
	var dialPath string
	options.dependencies.assembleInteractive = func(_ config.Config, stableDialPath string, _ io.Reader, _ io.Writer, _ *slog.Logger) (*applicationRuntime, error) {
		dialPath = stableDialPath
		return nil, assemblyFailure
	}

	err := Run(context.Background(), options)
	if !errors.Is(err, assemblyFailure) {
		t.Fatalf("Run() error = %v, want assembly failure", err)
	}
	assertPrivateDialAliasRemoved(t, dialPath)
	if got := lock.releases.Load(); got != 1 {
		t.Fatalf("lock releases = %d, want 1 after assembly failure", got)
	}
}

func TestRunInteractiveRetainsLongDialAliasUntilTimedOutComponentsFinish(t *testing.T) {
	canonicalEndpoint := longCanonicalEndpoint(t)
	unblock := make(chan struct{})
	var unblockOnce sync.Once
	releaseRunners := func() { unblockOnce.Do(func() { close(unblock) }) }
	t.Cleanup(releaseRunners)
	imRunner := &blockingRunner{started: make(chan struct{}), unblock: unblock}
	supervisor := &blockingRunner{started: make(chan struct{}), unblock: unblock}
	lock := &fakeLock{}
	options := testInteractiveOptions(t)
	options.dependencies.shutdownTimeout = 20 * time.Millisecond
	options.dependencies.resolveSocket = func(context.Context, string, string, herdr.CommandRunner) (string, error) {
		return canonicalEndpoint, nil
	}
	options.dependencies.acquireLock = func(string) (processLock, error) { return lock, nil }
	var dialPath string
	options.dependencies.assembleInteractive = func(_ config.Config, stableDialPath string, _ io.Reader, _ io.Writer, _ *slog.Logger) (*applicationRuntime, error) {
		dialPath = stableDialPath
		return &applicationRuntime{
			im:         &blockingWeCom{blockingRunner: imRunner, events: make(chan wecom.IncomingText)},
			supervisor: supervisor,
			handler:    &fakeHandler{},
		}, nil
	}

	ctx, cancel := context.WithCancel(context.Background())
	result := make(chan error, 1)
	go func() { result <- Run(ctx, options) }()
	waitClosed(t, imRunner.started, "interactive IM")
	waitClosed(t, supervisor.started, "interactive Supervisor")
	cancel()
	err := waitResult(t, result)
	if !errors.Is(err, ErrShutdownTimeout) {
		t.Fatalf("Run() error = %v, want ErrShutdownTimeout", err)
	}
	aliasDir := privateDialAliasDir(t, dialPath)
	if _, statErr := os.Stat(aliasDir); statErr != nil {
		t.Fatalf("timed-out dial alias was removed early: %v", statErr)
	}
	if identity := resolvedDialPath(t, dialPath); identity != canonicalEndpoint {
		t.Fatalf("retained dial identity = %q, want %q", identity, canonicalEndpoint)
	}
	if got := lock.releases.Load(); got != 0 || !retainsFakeLock(lock) {
		t.Fatalf("timed-out lock releases/retained = %d/%v, want 0/true", got, retainsFakeLock(lock))
	}

	releaseRunners()
	waitPathRemoved(t, aliasDir)
	waitLockReleasedAndUnretained(t, lock)
}

func TestRunInteractiveLogsSafeDialAliasCleanupFailureAfterTimeout(t *testing.T) {
	canonicalEndpoint := longCanonicalEndpoint(t)
	unblock := make(chan struct{})
	var unblockOnce sync.Once
	releaseRunners := func() { unblockOnce.Do(func() { close(unblock) }) }
	t.Cleanup(releaseRunners)
	imRunner := &blockingRunner{started: make(chan struct{}), unblock: unblock}
	supervisor := &blockingRunner{started: make(chan struct{}), unblock: unblock}
	lock := &fakeLock{}
	logs := &synchronizedBuffer{}
	options := testInteractiveOptions(t)
	options.Stderr = logs
	options.dependencies.shutdownTimeout = 20 * time.Millisecond
	options.dependencies.resolveSocket = func(context.Context, string, string, herdr.CommandRunner) (string, error) {
		return canonicalEndpoint, nil
	}
	options.dependencies.acquireLock = func(string) (processLock, error) { return lock, nil }
	var dialPath string
	options.dependencies.assembleInteractive = func(_ config.Config, stableDialPath string, _ io.Reader, _ io.Writer, _ *slog.Logger) (*applicationRuntime, error) {
		dialPath = stableDialPath
		return &applicationRuntime{
			im:         &blockingWeCom{blockingRunner: imRunner, events: make(chan wecom.IncomingText)},
			supervisor: supervisor,
			handler:    &fakeHandler{},
		}, nil
	}

	ctx, cancel := context.WithCancel(context.Background())
	result := make(chan error, 1)
	go func() { result <- Run(ctx, options) }()
	waitClosed(t, imRunner.started, "interactive IM")
	waitClosed(t, supervisor.started, "interactive Supervisor")
	cancel()
	if err := waitResult(t, result); !errors.Is(err, ErrShutdownTimeout) {
		t.Fatalf("Run() error = %v, want ErrShutdownTimeout", err)
	}

	aliasDir := privateDialAliasDir(t, dialPath)
	if err := os.Remove(dialPath); err != nil {
		t.Fatalf("replace dial symlink: %v", err)
	}
	if err := os.Mkdir(dialPath, 0o700); err != nil {
		t.Fatalf("create blocking alias directory: %v", err)
	}
	blocker := filepath.Join(dialPath, "blocker")
	if err := os.WriteFile(blocker, nil, 0o600); err != nil {
		t.Fatalf("create alias cleanup blocker: %v", err)
	}
	t.Cleanup(func() {
		_ = os.Remove(blocker)
		_ = os.Remove(dialPath)
		_ = os.Remove(aliasDir)
	})

	releaseRunners()
	waitLockReleasedAndUnretained(t, lock)
	waitBufferContains(t, logs, "error_type=socket_path_cleanup")
	output := logs.String()
	for _, sensitivePath := range []string{canonicalEndpoint, dialPath, aliasDir} {
		if strings.Contains(output, sensitivePath) {
			t.Fatalf("cleanup log leaked path %q: %s", sensitivePath, output)
		}
	}
}

func TestRunInteractiveEOFPreservesSupervisorFatalDuringDrain(t *testing.T) {
	fatal := errors.New("supervisor fatal after EOF")
	im := newOrderedResultWeCom(interactive.ErrInputClosed)
	supervisor := runtimeRunnerFunc(func(ctx context.Context) error {
		<-ctx.Done()
		return fatal
	})
	options := testInteractiveOptions(t)
	options.dependencies.assembleInteractive = func(config.Config, string, io.Reader, io.Writer, *slog.Logger) (*applicationRuntime, error) {
		return assembledInteractiveTestRuntime(t, im, supervisor, &fakeHandler{}), nil
	}

	err := Run(context.Background(), options)
	if !errors.Is(err, fatal) {
		t.Fatalf("Run() error = %v, want Supervisor fatal after normal EOF", err)
	}
}

func TestRunInteractiveEOFFatalTimeoutPreservesFatalAndRetainsLock(t *testing.T) {
	fatal := errors.New("supervisor fatal after EOF")
	releaseMessages := make(chan struct{})
	var releaseOnce sync.Once
	release := func() { releaseOnce.Do(func() { close(releaseMessages) }) }
	t.Cleanup(release)
	handler := newGatedCancellationHandler(releaseMessages)
	im := newOrderedResultWeCom(interactive.ErrInputClosed)
	im.events = make(chan wecom.IncomingText, 1)
	im.events <- incomingForUser(interactive.UserID, "eof-timeout", "/ls")
	im.returnAfter = handler.started
	supervisor := runtimeRunnerFunc(func(ctx context.Context) error {
		<-ctx.Done()
		return fatal
	})
	lock := &fakeLock{}
	options := testInteractiveOptions(t)
	options.dependencies.shutdownTimeout = 20 * time.Millisecond
	options.dependencies.acquireLock = func(string) (processLock, error) { return lock, nil }
	options.dependencies.assembleInteractive = func(config.Config, string, io.Reader, io.Writer, *slog.Logger) (*applicationRuntime, error) {
		return assembledInteractiveTestRuntime(t, im, supervisor, handler), nil
	}

	err := Run(context.Background(), options)
	if !errors.Is(err, fatal) || !errors.Is(err, ErrShutdownTimeout) {
		t.Fatalf("Run() error = %v, want Supervisor fatal joined with ErrShutdownTimeout", err)
	}
	if got := lock.releases.Load(); got != 0 || !retainsFakeLock(lock) {
		t.Fatalf("timed-out lock releases/retained = %d/%v, want 0/true", got, retainsFakeLock(lock))
	}
	release()
	waitLockReleasedAndUnretained(t, lock)
}

func TestRunInteractiveEOFTimeoutRetainsLockUntilComponentsFinish(t *testing.T) {
	releaseSupervisor := make(chan struct{})
	var releaseOnce sync.Once
	release := func() { releaseOnce.Do(func() { close(releaseSupervisor) }) }
	t.Cleanup(release)
	supervisor := newGatedCancellationRunner(releaseSupervisor)
	im := newOrderedResultWeCom(interactive.ErrInputClosed)
	lock := &fakeLock{}
	options := testInteractiveOptions(t)
	options.dependencies.shutdownTimeout = 20 * time.Millisecond
	options.dependencies.acquireLock = func(string) (processLock, error) { return lock, nil }
	options.dependencies.assembleInteractive = func(config.Config, string, io.Reader, io.Writer, *slog.Logger) (*applicationRuntime, error) {
		return &applicationRuntime{
			im: im, supervisor: supervisor, handler: &fakeHandler{},
			normalIMExit: func(err error) bool { return errors.Is(err, interactive.ErrInputClosed) },
		}, nil
	}

	err := Run(context.Background(), options)
	if !errors.Is(err, ErrShutdownTimeout) {
		t.Fatalf("Run() error = %v, want ErrShutdownTimeout", err)
	}
	if errors.Is(err, interactive.ErrInputClosed) {
		t.Fatalf("Run() error = %v, normal EOF must not hide or join into timeout", err)
	}
	if got := lock.releases.Load(); got != 0 || !retainsFakeLock(lock) {
		t.Fatalf("timed-out lock releases/retained = %d/%v, want 0/true", got, retainsFakeLock(lock))
	}
	release()
	waitLockReleasedAndUnretained(t, lock)
}

func TestRunInteractiveJoinedEOFFatalPreservesFatalRootCause(t *testing.T) {
	fatal := errors.New("interactive fatal")
	im := newOrderedResultWeCom(errors.Join(interactive.ErrInputClosed, fatal))
	supervisor := newFakeRunner()
	options := testInteractiveOptions(t)
	options.dependencies.assembleInteractive = func(config.Config, string, io.Reader, io.Writer, *slog.Logger) (*applicationRuntime, error) {
		return assembledInteractiveTestRuntime(t, im, supervisor, &fakeHandler{}), nil
	}

	err := Run(context.Background(), options)
	if !errors.Is(err, fatal) {
		t.Fatalf("Run() error = %v, want joined fatal root cause", err)
	}
	if errors.Is(err, ErrShutdownTimeout) {
		t.Fatalf("Run() error = %v, unexpected shutdown timeout", err)
	}
	waitClosed(t, supervisor.stopped, "joined EOF fatal Supervisor cancellation")
}

func TestRunInteractiveJoinedEOFFatalTimeoutPreservesFatalAndLock(t *testing.T) {
	fatal := errors.New("interactive fatal")
	releaseSupervisor := make(chan struct{})
	var releaseOnce sync.Once
	release := func() { releaseOnce.Do(func() { close(releaseSupervisor) }) }
	t.Cleanup(release)
	supervisor := newGatedCancellationRunner(releaseSupervisor)
	im := newOrderedResultWeCom(errors.Join(interactive.ErrInputClosed, fatal))
	lock := &fakeLock{}
	options := testInteractiveOptions(t)
	options.dependencies.shutdownTimeout = 20 * time.Millisecond
	options.dependencies.acquireLock = func(string) (processLock, error) { return lock, nil }
	options.dependencies.assembleInteractive = func(config.Config, string, io.Reader, io.Writer, *slog.Logger) (*applicationRuntime, error) {
		return assembledInteractiveTestRuntime(t, im, supervisor, &fakeHandler{}), nil
	}

	err := Run(context.Background(), options)
	if !errors.Is(err, fatal) || !errors.Is(err, ErrShutdownTimeout) {
		t.Fatalf("Run() error = %v, want fatal joined with ErrShutdownTimeout", err)
	}
	if got := lock.releases.Load(); got != 0 || !retainsFakeLock(lock) {
		t.Fatalf("timed-out lock releases/retained = %d/%v, want 0/true", got, retainsFakeLock(lock))
	}
	release()
	waitLockReleasedAndUnretained(t, lock)
}

func TestRunInteractiveFatalCancelsHerdrAndMessagesAndWrapsRootCause(t *testing.T) {
	root := errors.New("stdout write failed")
	fatal := fmt.Errorf("交互输出写入失败: %w", root)
	releaseIM := make(chan struct{})
	im := newGatedResultIM(releaseIM, fatal)
	handler := newCancelAwareHandler()
	im.events <- incomingForUser(interactive.UserID, "fatal-message", "/ls")
	supervisor := newFakeRunner()
	options := testInteractiveOptions(t)
	options.dependencies.assembleInteractive = func(config.Config, string, io.Reader, io.Writer, *slog.Logger) (*applicationRuntime, error) {
		return &applicationRuntime{
			im: im, supervisor: supervisor, handler: handler,
			normalIMExit: func(err error) bool { return errors.Is(err, interactive.ErrInputClosed) },
		}, nil
	}

	result := make(chan error, 1)
	go func() { result <- Run(context.Background(), options) }()
	waitClosed(t, im.started, "interactive IM")
	waitClosed(t, supervisor.started, "interactive Supervisor")
	waitClosed(t, handler.started, "interactive message handler")
	close(releaseIM)

	err := waitResult(t, result)
	if !errors.Is(err, root) || !strings.Contains(err.Error(), "im 运行失败") {
		t.Fatalf("Run() error = %v, want wrapped IM root cause", err)
	}
	waitClosed(t, supervisor.stopped, "interactive fatal Supervisor cancellation")
	waitClosed(t, handler.canceled, "interactive fatal message cancellation")
}

func TestRunInteractiveParentCancellationRemainsNormal(t *testing.T) {
	im := newFakeWeCom()
	supervisor := newFakeRunner()
	options := testInteractiveOptions(t)
	options.dependencies.assembleInteractive = func(config.Config, string, io.Reader, io.Writer, *slog.Logger) (*applicationRuntime, error) {
		return &applicationRuntime{
			im: im, supervisor: supervisor, handler: &fakeHandler{},
			normalIMExit: func(err error) bool { return errors.Is(err, interactive.ErrInputClosed) },
		}, nil
	}
	ctx, cancel := context.WithCancel(context.Background())
	result := make(chan error, 1)
	go func() { result <- Run(ctx, options) }()
	waitClosed(t, im.started, "interactive IM")
	waitClosed(t, supervisor.started, "interactive Supervisor")
	cancel()

	if err := waitResult(t, result); err != nil {
		t.Fatalf("Run() error = %v, want normal parent cancellation", err)
	}
}

func TestRunPreservesPrimarySupervisorFatalAfterMessageLoopClosure(t *testing.T) {
	fatal := errors.New("supervisor fatal after shutdown")
	im := newOrderedResultWeCom(nil)
	im.closeEvents = true
	supervisor := runtimeRunnerFunc(func(ctx context.Context) error {
		<-ctx.Done()
		return fatal
	})
	options := testOptions(t)
	options.dependencies.assemble = func(config.Config, string, *slog.Logger) (*applicationRuntime, error) {
		return &applicationRuntime{im: im, supervisor: supervisor, handler: &fakeHandler{}}, nil
	}

	err := Run(context.Background(), options)
	if !errors.Is(err, fatal) {
		t.Fatalf("Run() error = %v, want primary Supervisor fatal", err)
	}
	if errors.Is(err, ErrLoopStopped) {
		t.Fatalf("Run() error = %v, message loop closure must not replace primary Supervisor fatal", err)
	}
}

func TestRunKeepsFatalThatTriggeredShutdownWhenParentCancelsDuringDrain(t *testing.T) {
	fatal := errors.New("im fatal")
	releaseSupervisor := make(chan struct{})
	supervisor := newGatedCancellationRunner(releaseSupervisor)
	im := newOrderedResultWeCom(fatal)
	options := testOptions(t)
	options.dependencies.assemble = func(config.Config, string, *slog.Logger) (*applicationRuntime, error) {
		return &applicationRuntime{im: im, supervisor: supervisor, handler: &fakeHandler{}}, nil
	}
	ctx, cancel := context.WithCancel(context.Background())
	result := make(chan error, 1)
	go func() { result <- Run(ctx, options) }()
	waitClosed(t, supervisor.canceled, "fatal-triggered Supervisor cancellation")
	cancel()
	close(releaseSupervisor)

	err := waitResult(t, result)
	if !errors.Is(err, fatal) {
		t.Fatalf("Run() error = %v, want original fatal after late parent cancellation", err)
	}
}

func TestRunTimeoutKeepsFatalThatTriggeredShutdownBeforeParentCancellation(t *testing.T) {
	fatal := errors.New("im fatal")
	releaseSupervisor := make(chan struct{})
	supervisor := newGatedCancellationRunner(releaseSupervisor)
	im := newOrderedResultWeCom(fatal)
	lock := &fakeLock{}
	options := testOptions(t)
	options.dependencies.shutdownTimeout = 20 * time.Millisecond
	options.dependencies.acquireLock = func(string) (processLock, error) { return lock, nil }
	options.dependencies.assemble = func(config.Config, string, *slog.Logger) (*applicationRuntime, error) {
		return &applicationRuntime{im: im, supervisor: supervisor, handler: &fakeHandler{}}, nil
	}
	ctx, cancel := context.WithCancel(context.Background())
	result := make(chan error, 1)
	go func() { result <- Run(ctx, options) }()
	waitClosed(t, supervisor.canceled, "fatal-triggered Supervisor cancellation")
	cancel()

	err := waitResult(t, result)
	if !errors.Is(err, fatal) || !errors.Is(err, ErrShutdownTimeout) {
		t.Fatalf("Run() error = %v, want fatal joined with ErrShutdownTimeout", err)
	}
	if got := lock.releases.Load(); got != 0 {
		t.Fatalf("lock releases = %d, want 0 while Supervisor remains blocked", got)
	}
	close(releaseSupervisor)
	waitLockReleasedAndUnretained(t, lock)
}

func TestRunParentCancellationFirstRemainsNormal(t *testing.T) {
	im := newFakeWeCom()
	supervisor := newFakeRunner()
	options := testOptions(t)
	options.dependencies.assemble = func(config.Config, string, *slog.Logger) (*applicationRuntime, error) {
		return &applicationRuntime{im: im, supervisor: supervisor, handler: &fakeHandler{}}, nil
	}
	ctx, cancel := context.WithCancel(context.Background())
	result := make(chan error, 1)
	go func() { result <- Run(ctx, options) }()
	waitClosed(t, im.started, "IM Run")
	waitClosed(t, supervisor.started, "Supervisor Run")
	cancel()

	if err := waitResult(t, result); err != nil {
		t.Fatalf("Run() error = %v, want nil when parent cancellation triggered shutdown", err)
	}
}

func TestRunParentCancellationWithIMClosingEventsRemainsNormal(t *testing.T) {
	for iteration := range 50 {
		im := newCancelClosingWeCom()
		supervisor := newFakeRunner()
		handler := &fakeHandler{handled: make(chan wecom.IncomingText, 1)}
		options := testOptions(t)
		options.dependencies.assemble = func(config.Config, string, *slog.Logger) (*applicationRuntime, error) {
			return &applicationRuntime{im: im, supervisor: supervisor, handler: handler}, nil
		}
		ctx, cancel := context.WithCancel(context.Background())
		result := make(chan error, 1)
		go func() { result <- Run(ctx, options) }()
		waitClosed(t, im.started, "WeCom Run")
		waitClosed(t, supervisor.started, "Supervisor Run")
		im.events <- incoming(fmt.Sprintf("warmup-%d", iteration), "/ls")
		select {
		case <-handler.handled:
		case <-time.After(time.Second):
			t.Fatal("message consumer did not process warmup event")
		}
		cancel()
		if err := waitResult(t, result); err != nil {
			t.Fatalf("iteration %d: Run() error = %v, want nil", iteration, err)
		}
	}
}

func TestConsumeSelectedMessagePrefersCancellationOverClosedEvents(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	done, err := consumeSelectedMessage(ctx, &fakeHandler{}, wecom.IncomingText{}, false)
	if !done || !errors.Is(err, context.Canceled) {
		t.Fatalf("consumeSelectedMessage() = %v, %v, want done with context.Canceled", done, err)
	}

	done, err = consumeSelectedMessage(context.Background(), &fakeHandler{}, wecom.IncomingText{}, false)
	if !done || err != nil {
		t.Fatalf("active closed Events = %v, %v, want clean unexpected closure", done, err)
	}
}

func TestParentTriggeredShutdownClassifiesSimultaneousResultDeterministically(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	tests := []struct {
		name           string
		selectedParent bool
		result         componentResult
		selected       bool
		want           bool
	}{
		{name: "直接选中 parent", selectedParent: true, want: true},
		{name: "选中 parent 派生取消", result: componentResult{err: context.Canceled, shutdownDerived: true}, selected: true, want: true},
		{name: "选中同时到达的真实错误", result: componentResult{err: errors.New("fatal")}, selected: true, want: false},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got := parentTriggeredShutdown(ctx, test.selectedParent, test.result, test.selected); got != test.want {
				t.Fatalf("parentTriggeredShutdown() = %v, want %v", got, test.want)
			}
		})
	}
}

func TestRuntimeRootErrorPrioritizesPrimaryComponentByRole(t *testing.T) {
	primaryFatal := errors.New("console fatal")
	nonPrimaryFatal := errors.New("named wecom but not primary")
	results := []componentResult{
		{name: "wecom", primary: false, err: nonPrimaryFatal},
		{name: "messages", primary: false, err: context.Canceled, shutdownDerived: true},
		{name: "console", primary: true, err: primaryFatal},
	}

	err := runtimeRootError(shutdownCauseComponent, results)
	if !errors.Is(err, primaryFatal) {
		t.Fatalf("runtimeRootError() = %v, want primary console fatal", err)
	}
	if errors.Is(err, nonPrimaryFatal) {
		t.Fatalf("runtimeRootError() = %v, non-primary component name must not raise priority", err)
	}
}

func TestRuntimeRootErrorDoesNotPromoteNonPrimaryWeComName(t *testing.T) {
	nonPrimaryFatal := errors.New("non-primary fatal")
	results := []componentResult{
		{name: "wecom", primary: false, err: nonPrimaryFatal},
		{name: "messages", primary: false},
		{name: "herdr", primary: true, err: context.Canceled, shutdownDerived: true},
	}

	err := runtimeRootError(shutdownCauseComponent, results)
	if !errors.Is(err, ErrLoopStopped) {
		t.Fatalf("runtimeRootError() = %v, want ErrLoopStopped before non-primary fallback", err)
	}
	if errors.Is(err, nonPrimaryFatal) {
		t.Fatalf("runtimeRootError() = %v, non-primary wecom name must not raise priority", err)
	}
}

func TestRuntimeRootErrorAfterNormalIMPreservesNonPrimaryFatal(t *testing.T) {
	fatal := errors.New("message fatal")
	results := []componentResult{
		{name: "im", primary: true, err: interactive.ErrInputClosed},
		{name: "herdr", primary: true, err: context.Canceled, shutdownDerived: true},
		{name: "messages", primary: false, err: fatal},
	}

	err := runtimeRootError(shutdownCauseNormalIM, results)
	if !errors.Is(err, fatal) {
		t.Fatalf("runtimeRootError() = %v, want non-primary fatal fallback", err)
	}
}

func TestRuntimeRootErrorAfterNormalIMIgnoresCleanDrain(t *testing.T) {
	results := []componentResult{
		{name: "im", primary: true, err: interactive.ErrInputClosed},
		{name: "herdr", primary: true},
		{name: "messages", primary: false, err: context.Canceled, shutdownDerived: true},
	}
	if err := runtimeRootError(shutdownCauseNormalIM, results); err != nil {
		t.Fatalf("runtimeRootError() = %v, want clean normal IM drain", err)
	}
}

func TestRunComponentRecordsPrimaryRole(t *testing.T) {
	for _, test := range []struct {
		name    string
		primary bool
	}{
		{name: "console", primary: true},
		{name: "messages", primary: false},
	} {
		t.Run(test.name, func(t *testing.T) {
			results := make(chan componentResult, 1)
			runComponent(context.Background(), test.name, test.primary, func(context.Context) error { return nil }, results)

			result := <-results
			if result.primary != test.primary {
				t.Fatalf("runComponent() primary = %v, want %v", result.primary, test.primary)
			}
		})
	}
}

func TestRunTreatsIndependentEventsClosureAsUnexpectedLoopStop(t *testing.T) {
	supervisor := newFakeRunner()
	im := newOrderedResultWeCom(nil)
	im.closeEvents = true
	options := testOptions(t)
	options.dependencies.assemble = func(config.Config, string, *slog.Logger) (*applicationRuntime, error) {
		return &applicationRuntime{im: im, supervisor: supervisor, handler: &fakeHandler{}}, nil
	}

	err := Run(context.Background(), options)
	if !errors.Is(err, ErrLoopStopped) {
		t.Fatalf("Run() error = %v, want ErrLoopStopped", err)
	}
	if errors.Is(err, context.Canceled) {
		t.Fatalf("Run() error = %v, cancellation must not replace ErrLoopStopped", err)
	}
	waitClosed(t, supervisor.stopped, "Supervisor cancellation")
}

func TestRunStopsConsumptionAndConnectionsBeforeReleasingLock(t *testing.T) {
	var orderMu sync.Mutex
	var order []string
	record := func(item string) {
		orderMu.Lock()
		order = append(order, item)
		orderMu.Unlock()
	}
	im := newFakeWeCom()
	im.onStop = func() { record("im-closed") }
	supervisor := newFakeRunner()
	supervisor.onStop = func() { record("herdr-closed") }
	handler := &fakeHandler{handled: make(chan wecom.IncomingText, 2)}
	lock := &fakeLock{onRelease: func() { record("lock-released") }}
	options := testOptions(t)
	options.dependencies.acquireLock = func(string) (processLock, error) { return lock, nil }
	options.dependencies.assemble = func(config.Config, string, *slog.Logger) (*applicationRuntime, error) {
		return &applicationRuntime{im: im, supervisor: supervisor, handler: handler}, nil
	}

	ctx, cancel := context.WithCancel(context.Background())
	result := make(chan error, 1)
	go func() { result <- Run(ctx, options) }()
	waitClosed(t, im.started, "IM Run")
	waitClosed(t, supervisor.started, "Supervisor Run")
	cancel()
	im.events <- incoming("late-message", "must-not-run")
	if err := waitResult(t, result); err != nil {
		t.Fatalf("Run() error = %v", err)
	}
	select {
	case message := <-handler.handled:
		t.Fatalf("handled message after cancellation: %#v", message)
	default:
	}
	orderMu.Lock()
	defer orderMu.Unlock()
	if len(order) != 3 || order[2] != "lock-released" {
		t.Fatalf("shutdown order = %#v, want both connections before lock", order)
	}
}

func TestRunBoundsGracefulShutdown(t *testing.T) {
	unblock := make(chan struct{})
	var unblockOnce sync.Once
	releaseRunners := func() { unblockOnce.Do(func() { close(unblock) }) }
	t.Cleanup(releaseRunners)
	stuck := &blockingRunner{started: make(chan struct{}), unblock: unblock}
	supervisor := &blockingRunner{started: make(chan struct{}), unblock: unblock}
	lock := &fakeLock{}
	options := testOptions(t)
	options.dependencies.acquireLock = func(string) (processLock, error) { return lock, nil }
	options.dependencies.shutdownTimeout = 20 * time.Millisecond
	options.dependencies.assemble = func(config.Config, string, *slog.Logger) (*applicationRuntime, error) {
		return &applicationRuntime{
			im:         &blockingWeCom{blockingRunner: stuck, events: make(chan wecom.IncomingText)},
			supervisor: supervisor,
			handler:    &fakeHandler{},
		}, nil
	}
	ctx, cancel := context.WithCancel(context.Background())
	result := make(chan error, 1)
	go func() { result <- Run(ctx, options) }()
	waitClosed(t, stuck.started, "stuck IM Run")
	waitClosed(t, supervisor.started, "stuck Supervisor Run")
	cancel()

	err := waitResult(t, result)
	if !errors.Is(err, ErrShutdownTimeout) {
		t.Fatalf("Run() error = %v, want ErrShutdownTimeout", err)
	}
	if got := lock.releases.Load(); got != 0 {
		t.Fatalf("lock releases = %d, want 0 while timed-out components may still run", got)
	}
	if !retainsFakeLock(lock) {
		t.Fatal("timed-out lock is not strongly retained")
	}
	releaseRunners()
	waitLockReleasedAndUnretained(t, lock)
}

func TestRunLogsOnlySafeIdentifiers(t *testing.T) {
	var logs bytes.Buffer
	unsafeError := errors.New("secret-sensitive complete-prompt complete-terminal")
	options := testOptions(t)
	options.Stderr = &logs
	options.dependencies.assemble = func(config.Config, string, *slog.Logger) (*applicationRuntime, error) {
		im := newFakeWeCom()
		supervisor := newFakeRunner()
		supervisor.result = unsafeError
		return &applicationRuntime{im: im, supervisor: supervisor, handler: &fakeHandler{}}, nil
	}

	if err := Run(context.Background(), options); !errors.Is(err, unsafeError) {
		t.Fatalf("Run() error = %v", err)
	}
	output := logs.String()
	for _, sensitive := range []string{"secret-sensitive", "bot-sensitive", "user-sensitive", "complete-prompt", "complete-terminal"} {
		if strings.Contains(output, sensitive) {
			t.Fatalf("log contains sensitive value %q: %s", sensitive, output)
		}
	}
	if !strings.Contains(output, "bot_hash=") || !strings.Contains(output, "user_hash=") {
		t.Fatalf("safe identifier hashes missing from log: %s", output)
	}
}

func TestRunInteractiveLogsOnlyModeSocketHashAndSafeErrorType(t *testing.T) {
	var logs bytes.Buffer
	unsafeError := errors.New("stdout-sensitive complete-terminal")
	socketDir := t.TempDir()
	separator := string(filepath.Separator)
	rawSocketPath := filepath.Join(socketDir, "nested") + separator + ".." + separator + "socket-sensitive.sock"
	canonicalSocketPath := resolvedChildPath(t, socketDir, "socket-sensitive.sock")
	options := testInteractiveOptions(t)
	options.Stderr = &logs
	options.dependencies.resolveSocket = func(context.Context, string, string, herdr.CommandRunner) (string, error) {
		return rawSocketPath, nil
	}
	options.dependencies.assembleInteractive = func(config.Config, string, io.Reader, io.Writer, *slog.Logger) (*applicationRuntime, error) {
		return &applicationRuntime{
			im: newOrderedResultWeCom(unsafeError), supervisor: newFakeRunner(), handler: &fakeHandler{},
			normalIMExit: func(err error) bool { return errors.Is(err, interactive.ErrInputClosed) },
		}, nil
	}

	if err := Run(context.Background(), options); !errors.Is(err, unsafeError) {
		t.Fatalf("Run() error = %v", err)
	}
	output := logs.String()
	for _, sensitive := range []string{
		"stdout-sensitive", "complete-terminal", rawSocketPath, canonicalSocketPath,
		"bot-sensitive", "user-sensitive", "secret-sensitive",
	} {
		if strings.Contains(output, sensitive) {
			t.Fatalf("interactive log contains sensitive value %q: %s", sensitive, output)
		}
	}
	if !strings.Contains(output, "mode=interactive") ||
		!strings.Contains(output, "socket_hash="+shortHash(canonicalSocketPath)) ||
		!strings.Contains(output, "error_type=runtime_error") {
		t.Fatalf("interactive safe log fields missing: %s", output)
	}
}

func TestRunInteractiveWrapsConfigurationErrors(t *testing.T) {
	tests := []struct {
		name  string
		setup func(*appDependencies)
	}{
		{
			name: "配置加载",
			setup: func(dependencies *appDependencies) {
				dependencies.loadInteractiveConfig = func(string) (config.Config, error) {
					return config.Config{}, errors.New("interactive config failed")
				}
			},
		},
		{
			name: "日志级别",
			setup: func(dependencies *appDependencies) {
				dependencies.loadInteractiveConfig = func(string) (config.Config, error) {
					loaded := testConfig()
					loaded.Log.Level = "trace"
					return loaded, nil
				}
			},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			options := testInteractiveOptions(t)
			test.setup(options.dependencies)
			err := Run(context.Background(), options)
			if !errors.Is(err, ErrConfig) {
				t.Fatalf("Run() error = %v, want ErrConfig", err)
			}
		})
	}
}

func TestSafeErrorTypeUsesFixedSemanticCategories(t *testing.T) {
	tests := []struct {
		name string
		err  error
		want string
	}{
		{name: "退出超时", err: errors.Join(errors.New("secret"), ErrShutdownTimeout), want: "shutdown_timeout"},
		{name: "循环停止", err: fmt.Errorf("wrapped: %w", ErrLoopStopped), want: "loop_stopped"},
		{name: "连接路径清理", err: errStableDialAliasCleanup, want: "socket_path_cleanup"},
		{name: "上下文", err: context.Canceled, want: "context"},
		{name: "通知队列", err: bridge.ErrNotificationQueueFull, want: "notification_queue_full"},
		{name: "Herdr 协议", err: herdr.ErrProtocolMismatch, want: "herdr_protocol"},
		{name: "Herdr 不可用", err: herdr.ErrUnavailable, want: "herdr_unavailable"},
		{name: "Relay 协议", err: hprp.ErrProtocolMismatch, want: "relay_protocol"},
		{name: "Relay 不可用", err: relayclient.ErrUnavailable, want: "relay_unavailable"},
		{name: "企业微信协议", err: wecom.ErrProtocol, want: "wecom_protocol"},
		{name: "企业微信不可用", err: wecom.ErrUnavailable, want: "wecom_unavailable"},
		{name: "其它错误", err: errors.New("secret"), want: "runtime_error"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got := safeErrorType(test.err); got != test.want {
				t.Fatalf("safeErrorType() = %q, want %q", got, test.want)
			}
		})
	}
}

func TestRunRelayComponentsIdentifiesFailingComponent(t *testing.T) {
	tests := []struct {
		name         string
		failedName   string
		failedRun    func(context.Context) error
		secondaryRun func(context.Context) error
		invoke       func(context.Context, func(context.Context) error, func(context.Context) error) (error, <-chan struct{})
	}{
		{
			name:       "Relay 连接失败",
			failedName: "relay_connection",
			failedRun:  func(context.Context) error { return errors.New("relay dial failed") },
			secondaryRun: func(ctx context.Context) error {
				<-ctx.Done()
				return ctx.Err()
			},
			invoke: func(ctx context.Context, failed, secondary func(context.Context) error) (error, <-chan struct{}) {
				return runRelayComponents(ctx, failed, secondary, time.Second)
			},
		},
		{
			name:       "Herdr 监督器失败",
			failedName: "herdr_supervisor",
			failedRun:  func(context.Context) error { return errors.New("snapshot failed") },
			secondaryRun: func(ctx context.Context) error {
				<-ctx.Done()
				return ctx.Err()
			},
			invoke: func(ctx context.Context, failed, secondary func(context.Context) error) (error, <-chan struct{}) {
				return runRelayComponents(ctx, secondary, failed, time.Second)
			},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			err, componentsDone := test.invoke(context.Background(), test.failedRun, test.secondaryRun)
			if componentsDone != nil {
				t.Fatal("组件已及时退出，componentsDone 应为 nil")
			}
			if err == nil || !strings.Contains(err.Error(), "failed") {
				t.Fatalf("runRelayComponents() error = %v", err)
			}
			if got := relayComponentName(err); got != test.failedName {
				t.Fatalf("relayComponentName() = %q, want %q", got, test.failedName)
			}
		})
	}
}

func TestRunRelayComponentsRetainsRootErrorOnShutdownTimeout(t *testing.T) {
	fatal := errors.New("relay dial failed")
	release := make(chan struct{})
	err, componentsDone := runRelayComponents(
		context.Background(),
		func(context.Context) error { return fatal },
		func(context.Context) error {
			<-release
			return nil
		},
		10*time.Millisecond,
	)
	if !errors.Is(err, fatal) || !errors.Is(err, ErrShutdownTimeout) {
		t.Fatalf("runRelayComponents() error = %v, want fatal joined with ErrShutdownTimeout", err)
	}
	if got := relayComponentName(err); got != "relay_connection" {
		t.Fatalf("relayComponentName() = %q, want relay_connection", got)
	}
	if componentsDone == nil {
		t.Fatal("退出超时时应返回组件完成通知")
	}
	close(release)
	select {
	case <-componentsDone:
	case <-time.After(time.Second):
		t.Fatal("释放组件后 componentsDone 未关闭")
	}
}

func TestSafeRelayRuntimeReasonRedactsUserIDAndEndpointDetails(t *testing.T) {
	const (
		userID   = "user-sensitive"
		endpoint = "wss://account:password@relay.internal:9443/ws?access_token=private-query"
	)
	err := fmt.Errorf("relay_connection 运行失败: userid=%s endpoint=%s: connection refused\nretrying", userID, endpoint)
	reason := safeRelayRuntimeReason(err, userID, endpoint)
	for _, sensitive := range []string{userID, "account", "password", "private-query"} {
		if strings.Contains(reason, sensitive) {
			t.Fatalf("安全错误原因泄露 %q：%q", sensitive, reason)
		}
	}
	for _, want := range []string{"relay_connection", "wss://relay.internal:9443", "connection refused retrying"} {
		if !strings.Contains(reason, want) {
			t.Fatalf("安全错误原因缺少 %q：%q", want, reason)
		}
	}
}

func testOptions(t *testing.T) Options {
	t.Helper()
	dependencies := defaultAppDependencies()
	dependencies.loadConfig = func(string, func(string) string) (config.Config, error) { return testConfig(), nil }
	dependencies.userCacheDir = func() (string, error) { return "/cache", nil }
	dependencies.mkdirAll = func(string, os.FileMode) error { return nil }
	dependencies.acquireLock = func(string) (processLock, error) { return &fakeLock{}, nil }
	dependencies.resolveSocket = func(context.Context, string, string, herdr.CommandRunner) (string, error) {
		return "/tmp/test.sock", nil
	}
	dependencies.assemble = func(config.Config, string, *slog.Logger) (*applicationRuntime, error) { return canceledRuntime(), nil }
	return Options{
		ConfigPath:   "/tmp/config.json",
		Getenv:       func(string) string { return "secret-sensitive" },
		Runner:       fakeCommandRunner{},
		Stdout:       io.Discard,
		Stderr:       io.Discard,
		dependencies: dependencies,
	}
}

func testInteractiveOptions(t *testing.T) Options {
	t.Helper()
	options := testOptions(t)
	options.Interactive = true
	options.ConfigPath = ""
	options.Stdin = strings.NewReader("")
	options.dependencies.loadInteractiveConfig = func(string) (config.Config, error) { return testConfig(), nil }
	options.dependencies.assembleInteractive = func(config.Config, string, io.Reader, io.Writer, *slog.Logger) (*applicationRuntime, error) {
		return canceledRuntime(), nil
	}
	return options
}

type interactiveSocketPlan struct {
	lockPath          string
	assembledSocket   string
	assembledIdentity string
}

type synchronizedBuffer struct {
	mu     sync.Mutex
	buffer bytes.Buffer
}

func (b *synchronizedBuffer) Write(value []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buffer.Write(value)
}

func (b *synchronizedBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buffer.String()
}

func resolvedChildPath(t *testing.T, parent, child string) string {
	t.Helper()
	resolvedParent, err := filepath.EvalSymlinks(parent)
	if err != nil {
		t.Fatalf("resolve existing parent %q: %v", parent, err)
	}
	return filepath.Join(resolvedParent, child)
}

func resolvedDialPath(t *testing.T, dialPath string) string {
	t.Helper()
	if resolved, err := filepath.EvalSymlinks(dialPath); err == nil {
		return filepath.Clean(resolved)
	}
	if info, err := os.Lstat(dialPath); err == nil && info.Mode()&os.ModeSymlink != 0 {
		target, err := os.Readlink(dialPath)
		if err != nil {
			t.Fatalf("read dial symlink %q: %v", dialPath, err)
		}
		if !filepath.IsAbs(target) {
			target = filepath.Join(filepath.Dir(dialPath), target)
		}
		return filepath.Clean(target)
	}
	resolvedParent, err := filepath.EvalSymlinks(filepath.Dir(dialPath))
	if err != nil {
		t.Fatalf("resolve dial parent %q: %v", filepath.Dir(dialPath), err)
	}
	return filepath.Join(resolvedParent, filepath.Base(dialPath))
}

func longCanonicalEndpoint(t *testing.T) string {
	t.Helper()
	directory := filepath.Join(t.TempDir(), strings.Repeat("long-directory-", 8))
	if err := os.MkdirAll(directory, 0o700); err != nil {
		t.Fatalf("create long canonical directory: %v", err)
	}
	endpoint := resolvedChildPath(t, directory, "herdr.sock")
	if len([]byte(endpoint)) < unixSocketPathByteLimit {
		t.Fatalf("canonical endpoint length = %d, want at least %d", len([]byte(endpoint)), unixSocketPathByteLimit)
	}
	return endpoint
}

func privateDialAliasDir(t *testing.T, dialPath string) string {
	t.Helper()
	if len([]byte(dialPath)) >= unixSocketPathByteLimit {
		t.Fatalf("dial path length = %d, want below %d", len([]byte(dialPath)), unixSocketPathByteLimit)
	}
	aliasDir := filepath.Dir(dialPath)
	if filepath.Dir(aliasDir) != "/tmp" || !strings.HasPrefix(filepath.Base(aliasDir), "herdr-pal-") {
		t.Fatalf("dial alias directory = %q, want private /tmp/herdr-pal-* directory", aliasDir)
	}
	return aliasDir
}

func assertPrivateDialAliasRemoved(t *testing.T, dialPath string) {
	t.Helper()
	aliasDir := privateDialAliasDir(t, dialPath)
	if _, err := os.Lstat(aliasDir); !os.IsNotExist(err) {
		t.Fatalf("dial alias directory still exists: %v", err)
	}
}

func waitPathRemoved(t *testing.T, path string) {
	t.Helper()
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		if _, err := os.Lstat(path); os.IsNotExist(err) {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatalf("path was not removed before deadline: %q", path)
}

func waitBufferContains(t *testing.T, buffer *synchronizedBuffer, fragment string) {
	t.Helper()
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		if strings.Contains(buffer.String(), fragment) {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatalf("buffer did not contain %q before deadline: %s", fragment, buffer.String())
}

func assertInteractiveSocketRejectedBeforeLock(t *testing.T, socketPath string, sensitivePaths []string) {
	t.Helper()
	options := testInteractiveOptions(t)
	options.dependencies.resolveSocket = func(context.Context, string, string, herdr.CommandRunner) (string, error) {
		return socketPath, nil
	}
	lockCalls := 0
	prepareCalls := 0
	assembleCalls := 0
	options.dependencies.acquireLock = func(string) (processLock, error) {
		lockCalls++
		return &fakeLock{}, nil
	}
	options.dependencies.prepareStableDialPath = func(string) (*dialPathLease, error) {
		prepareCalls++
		return &dialPathLease{path: "/tmp/must-not-run.sock"}, nil
	}
	options.dependencies.assembleInteractive = func(config.Config, string, io.Reader, io.Writer, *slog.Logger) (*applicationRuntime, error) {
		assembleCalls++
		return canceledRuntime(), nil
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	err := Run(ctx, options)
	if err == nil || !strings.Contains(err.Error(), "无法可靠") {
		t.Fatalf("Run() error = %v, want safe canonicalization failure", err)
	}
	if lockCalls != 0 || prepareCalls != 0 || assembleCalls != 0 {
		t.Fatalf("lock/prepare/assemble calls = %d/%d/%d, want 0/0/0", lockCalls, prepareCalls, assembleCalls)
	}
	for _, sensitivePath := range sensitivePaths {
		if strings.Contains(err.Error(), sensitivePath) {
			t.Fatalf("Run() error leaked path %q: %v", sensitivePath, err)
		}
	}
}

func captureInteractiveSocketPlan(t *testing.T, socketPath string, onAcquire func()) interactiveSocketPlan {
	t.Helper()
	options := testInteractiveOptions(t)
	options.dependencies.resolveSocket = func(context.Context, string, string, herdr.CommandRunner) (string, error) {
		return socketPath, nil
	}
	var plan interactiveSocketPlan
	options.dependencies.acquireLock = func(path string) (processLock, error) {
		plan.lockPath = path
		if onAcquire != nil {
			onAcquire()
		}
		return &fakeLock{}, nil
	}
	options.dependencies.assembleInteractive = func(_ config.Config, assembledSocket string, _ io.Reader, _ io.Writer, _ *slog.Logger) (*applicationRuntime, error) {
		plan.assembledSocket = assembledSocket
		plan.assembledIdentity = resolvedDialPath(t, assembledSocket)
		return canceledRuntime(), nil
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := Run(ctx, options); err != nil {
		t.Fatalf("Run() error = %v", err)
	}
	return plan
}

func assembledInteractiveTestRuntime(t *testing.T, im imRuntime, supervisor runtimeRunner, handler messageHandler) *applicationRuntime {
	t.Helper()
	dependencies := defaultAssemblyDependencies()
	dependencies.newHerdr = func(string) bridge.ManagedHerdr { return newFakeManagedHerdr() }
	dependencies.newInteractive = func(io.Reader, io.Writer) (imRuntime, error) { return im, nil }
	runtime, err := assembleInteractiveRuntime(
		testConfig(), "/tmp/interactive.sock", strings.NewReader(""), io.Discard,
		slog.New(slog.NewTextHandler(io.Discard, nil)), dependencies,
	)
	if err != nil {
		t.Fatalf("assembleInteractiveRuntime() error = %v", err)
	}
	runtime.supervisor = supervisor
	runtime.handler = handler
	return runtime
}

func testConfig() config.Config {
	return config.Config{
		WeCom: config.WeComConfig{BotID: "bot-sensitive", AllowedUserID: "user-sensitive", Secret: "secret-sensitive"},
		Herdr: config.HerdrConfig{Session: "named-session"},
		Log:   config.LogConfig{Level: "info"},
	}
}

func canceledRuntime() *applicationRuntime {
	return &applicationRuntime{im: newFakeWeCom(), supervisor: newFakeRunner(), handler: &fakeHandler{}}
}

func incoming(messageID, content string) wecom.IncomingText {
	return incomingForUser("user-sensitive", messageID, content)
}

func incomingForUser(userID, messageID, content string) wecom.IncomingText {
	return wecom.IncomingText{
		RequestID: "request-" + messageID, MessageID: messageID, BotID: "bot-sensitive",
		UserID: userID, ChatType: "single", Content: content,
	}
}

type fakeCommandRunner struct{}

func (fakeCommandRunner) Output(context.Context, string, ...string) ([]byte, error) { return nil, nil }

type isOnlyError struct {
	target error
}

func (e isOnlyError) Error() string { return "is-only" }

func (e isOnlyError) Is(target error) bool { return target == e.target }

type cyclicUnwrapError struct {
	next error
}

func (e *cyclicUnwrapError) Error() string { return "cyclic" }

func (e *cyclicUnwrapError) Unwrap() error { return e.next }

type fakeLock struct {
	onRelease func()
	releases  atomic.Int32
}

func (l *fakeLock) Release() error {
	l.releases.Add(1)
	if l.onRelease != nil {
		l.onRelease()
	}
	return nil
}

type fakeRunner struct {
	started chan struct{}
	stopped chan struct{}
	result  error
	onStop  func()
	once    sync.Once
}

type runtimeRunnerFunc func(context.Context) error

func (f runtimeRunnerFunc) Run(ctx context.Context) error { return f(ctx) }

func newFakeRunner() *fakeRunner {
	return &fakeRunner{started: make(chan struct{}), stopped: make(chan struct{})}
}

func (r *fakeRunner) Run(ctx context.Context) error {
	r.once.Do(func() { close(r.started) })
	if r.result != nil {
		return r.result
	}
	<-ctx.Done()
	if r.onStop != nil {
		r.onStop()
	}
	close(r.stopped)
	return ctx.Err()
}

type fakeWeCom struct {
	*fakeRunner
	events chan wecom.IncomingText
	mu     sync.Mutex
	sent   []string
}

func newFakeWeCom() *fakeWeCom {
	return &fakeWeCom{fakeRunner: newFakeRunner(), events: make(chan wecom.IncomingText, 8)}
}

func (w *fakeWeCom) Events() <-chan wecom.IncomingText { return w.events }

func (w *fakeWeCom) RespondMarkdown(context.Context, string, string) error { return nil }

func (w *fakeWeCom) SendMarkdown(_ context.Context, content string) error {
	w.mu.Lock()
	w.sent = append(w.sent, content)
	w.mu.Unlock()
	return nil
}

type fakeHandler struct {
	handled chan wecom.IncomingText
}

func (h *fakeHandler) HandleMessage(_ context.Context, message wecom.IncomingText) {
	if h.handled != nil {
		h.handled <- message
	}
}

type blockingRunner struct {
	started chan struct{}
	unblock <-chan struct{}
	once    sync.Once
}

func (r *blockingRunner) Run(context.Context) error {
	r.once.Do(func() { close(r.started) })
	<-r.unblock
	return nil
}

type blockingWeCom struct {
	*blockingRunner
	events <-chan wecom.IncomingText
}

func (w *blockingWeCom) Events() <-chan wecom.IncomingText { return w.events }

func (w *blockingWeCom) RespondMarkdown(context.Context, string, string) error { return nil }

func (w *blockingWeCom) SendMarkdown(context.Context, string) error { return nil }

type orderedResultWeCom struct {
	events      chan wecom.IncomingText
	fatal       error
	closeEvents bool
	returnAfter <-chan struct{}
}

func newOrderedResultWeCom(fatal error) *orderedResultWeCom {
	return &orderedResultWeCom{events: make(chan wecom.IncomingText), fatal: fatal}
}

func (w *orderedResultWeCom) Run(ctx context.Context) error {
	if w.closeEvents {
		close(w.events)
	}
	if w.returnAfter != nil {
		select {
		case <-w.returnAfter:
		case <-ctx.Done():
			// 结果顺序测试仍需返回预设错误，不能让派生取消覆盖根因。
		}
	}
	if w.fatal != nil {
		return w.fatal
	}
	<-ctx.Done()
	return ctx.Err()
}

func (w *orderedResultWeCom) Events() <-chan wecom.IncomingText { return w.events }

func (w *orderedResultWeCom) RespondMarkdown(context.Context, string, string) error { return nil }

func (w *orderedResultWeCom) SendMarkdown(context.Context, string) error { return nil }

type cancelClosingWeCom struct {
	events  chan wecom.IncomingText
	started chan struct{}
}

func newCancelClosingWeCom() *cancelClosingWeCom {
	return &cancelClosingWeCom{events: make(chan wecom.IncomingText, 1), started: make(chan struct{})}
}

func (w *cancelClosingWeCom) Run(ctx context.Context) error {
	close(w.started)
	<-ctx.Done()
	close(w.events)
	return ctx.Err()
}

func (w *cancelClosingWeCom) Events() <-chan wecom.IncomingText { return w.events }

func (w *cancelClosingWeCom) RespondMarkdown(context.Context, string, string) error { return nil }

func (w *cancelClosingWeCom) SendMarkdown(context.Context, string) error { return nil }

type delayedCancelWeCom struct {
	events   chan wecom.IncomingText
	started  chan struct{}
	canceled chan struct{}
	release  <-chan struct{}
}

func newDelayedCancelWeCom(release <-chan struct{}) *delayedCancelWeCom {
	return &delayedCancelWeCom{
		events: make(chan wecom.IncomingText), started: make(chan struct{}),
		canceled: make(chan struct{}), release: release,
	}
}

func (w *delayedCancelWeCom) Run(ctx context.Context) error {
	close(w.started)
	<-ctx.Done()
	close(w.canceled)
	<-w.release
	return ctx.Err()
}

func (w *delayedCancelWeCom) Events() <-chan wecom.IncomingText { return w.events }

func (w *delayedCancelWeCom) RespondMarkdown(context.Context, string, string) error { return nil }

func (w *delayedCancelWeCom) SendMarkdown(context.Context, string) error { return nil }

type gatedResultIM struct {
	events  chan wecom.IncomingText
	started chan struct{}
	release <-chan struct{}
	result  error
}

func newGatedResultIM(release <-chan struct{}, result error) *gatedResultIM {
	return &gatedResultIM{
		events: make(chan wecom.IncomingText, 1), started: make(chan struct{}),
		release: release, result: result,
	}
}

func (i *gatedResultIM) Run(ctx context.Context) error {
	close(i.started)
	select {
	case <-i.release:
		return i.result
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (i *gatedResultIM) Events() <-chan wecom.IncomingText { return i.events }

func (i *gatedResultIM) RespondMarkdown(context.Context, string, string) error { return nil }

func (i *gatedResultIM) SendMarkdown(context.Context, string) error { return nil }

type cancelAwareHandler struct {
	started  chan struct{}
	canceled chan struct{}
	once     sync.Once
}

func newCancelAwareHandler() *cancelAwareHandler {
	return &cancelAwareHandler{started: make(chan struct{}), canceled: make(chan struct{})}
}

func (h *cancelAwareHandler) HandleMessage(ctx context.Context, _ wecom.IncomingText) {
	h.once.Do(func() { close(h.started) })
	<-ctx.Done()
	close(h.canceled)
}

type gatedCancellationHandler struct {
	started chan struct{}
	release <-chan struct{}
	once    sync.Once
}

func newGatedCancellationHandler(release <-chan struct{}) *gatedCancellationHandler {
	return &gatedCancellationHandler{started: make(chan struct{}), release: release}
}

func (h *gatedCancellationHandler) HandleMessage(ctx context.Context, _ wecom.IncomingText) {
	h.once.Do(func() { close(h.started) })
	<-ctx.Done()
	<-h.release
}

type gatedCancellationRunner struct {
	canceled chan struct{}
	release  <-chan struct{}
}

func newGatedCancellationRunner(release <-chan struct{}) *gatedCancellationRunner {
	return &gatedCancellationRunner{canceled: make(chan struct{}), release: release}
}

func (r *gatedCancellationRunner) Run(ctx context.Context) error {
	<-ctx.Done()
	close(r.canceled)
	<-r.release
	return ctx.Err()
}

type fakeManagedHerdr struct {
	mu       sync.Mutex
	snapshot herdr.Snapshot
	prompts  int
	gets     int
	reads    int
}

func newFakeManagedHerdr() *fakeManagedHerdr {
	agent := "codex"
	title := "Agent"
	return &fakeManagedHerdr{snapshot: herdr.Snapshot{
		Version: "test", Protocol: herdr.RequiredProtocol,
		Panes: []herdr.Pane{{
			PaneID: "pane-1", TerminalID: "terminal-1", WorkspaceID: "workspace-1", TabID: "tab-1",
			Agent: &agent, Title: &title, AgentStatus: herdr.AgentStatusIdle,
		}},
		Agents: []herdr.AgentInfo{{
			PaneID: "pane-1", TerminalID: "terminal-1", WorkspaceID: "workspace-1", TabID: "tab-1",
			Agent: &agent, Title: &title, AgentStatus: herdr.AgentStatusIdle, StateChangeSeq: 1,
		}},
	}}
}

func (f *fakeManagedHerdr) CheckCompatible(context.Context) error { return nil }

func (f *fakeManagedHerdr) Snapshot(context.Context) (herdr.Snapshot, error) { return f.snapshot, nil }

func (f *fakeManagedHerdr) Subscribe(context.Context, []herdr.SubscriptionSpec) (herdr.SubscriptionStream, error) {
	return nil, errors.New("not used")
}

func (f *fakeManagedHerdr) GetAgent(context.Context, string) (herdr.AgentInfo, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.gets++
	return f.snapshot.Agents[0], nil
}

func (f *fakeManagedHerdr) ReadRecent(context.Context, string, int) (herdr.ReadResult, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.reads++
	return herdr.ReadResult{PaneID: "pane-1", Text: "recent terminal"}, nil
}

func (f *fakeManagedHerdr) ReadRecentANSI(ctx context.Context, target string, lines int) (herdr.ReadResult, error) {
	return f.ReadRecent(ctx, target, lines)
}

func (f *fakeManagedHerdr) ReadVisibleANSI(ctx context.Context, target string, lines int) (herdr.ReadResult, error) {
	return f.ReadRecent(ctx, target, lines)
}

func (f *fakeManagedHerdr) PromptUntilStateChange(context.Context, string, string) (herdr.AgentInfo, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.prompts++
	agent := f.snapshot.Agents[0]
	agent.AgentStatus = herdr.AgentStatusWorking
	agent.StateChangeSeq++
	return agent, nil
}

func (f *fakeManagedHerdr) WaitForStateChange(context.Context, string, uint64, time.Duration) (herdr.AgentInfo, error) {
	return herdr.AgentInfo{}, errors.New("not used")
}

func (f *fakeManagedHerdr) SendKey(context.Context, string, string) error { return nil }

func (f *fakeManagedHerdr) promptCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.prompts
}

func (f *fakeManagedHerdr) getCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.gets
}

func (f *fakeManagedHerdr) readCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.reads
}

func waitClosed(t *testing.T, channel <-chan struct{}, name string) {
	t.Helper()
	select {
	case <-channel:
	case <-time.After(time.Second):
		t.Fatalf("timed out waiting for %s", name)
	}
}

func waitResult(t *testing.T, result <-chan error) error {
	t.Helper()
	select {
	case err := <-result:
		return err
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for Run")
		return nil
	}
}

func waitLockReleasedAndUnretained(t *testing.T, lock *fakeLock) {
	t.Helper()
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		if lock.releases.Load() == 1 && !retainsFakeLock(lock) {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatalf("lock releases = %d, retained = %v, want released and removed", lock.releases.Load(), retainsFakeLock(lock))
}

func retainsFakeLock(lock *fakeLock) bool {
	retainedLocksMu.Lock()
	defer retainedLocksMu.Unlock()
	for _, retained := range retainedLocks {
		candidate, ok := retained.lock.(*fakeLock)
		if ok && candidate == lock {
			return true
		}
	}
	return false
}
