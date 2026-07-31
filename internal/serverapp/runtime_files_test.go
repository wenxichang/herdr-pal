package serverapp

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/wenxichang/herdr-pal/internal/adminauth"
	"github.com/wenxichang/herdr-pal/internal/config"
)

func TestPublishAdminBootstrapPrintsSecretsWhenFileWriteFails(t *testing.T) {
	bootstrap := adminauth.Bootstrap{Created: true, Username: "admin", InitialPassword: "initial-password", AutomationToken: "hpa_test_token"}
	invalidPath := t.TempDir()
	var output bytes.Buffer
	if err := publishAdminBootstrap(&output, invalidPath, bootstrap); err == nil {
		t.Fatal("publishAdminBootstrap() error = nil")
	}
	for _, want := range []string{"管理员：admin", "初始密码：initial-password", "自动化 Token：hpa_test_token"} {
		if !strings.Contains(output.String(), want) {
			t.Fatalf("bootstrap stdout %q lacks %q", output.String(), want)
		}
	}
}

func TestEnsureDefaultHelpFileCreatesPrivateFileWithoutOverwriting(t *testing.T) {
	directory := filepath.Join(t.TempDir(), "herdr-pal-server")
	path := filepath.Join(directory, "help.md")
	if err := ensureDefaultHelpFile(path); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), "Herdr Pal 快速上手") || strings.Contains(string(data), "addr_hint") {
		t.Fatalf("default help = %q", data)
	}
	assertPrivateRuntimePath(t, directory, 0o700)
	assertPrivateRuntimePath(t, path, 0o600)
	if err := os.WriteFile(path, []byte("管理员自定义帮助"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := ensureDefaultHelpFile(path); err != nil {
		t.Fatal(err)
	}
	data, err = os.ReadFile(path)
	if err != nil || string(data) != "管理员自定义帮助" {
		t.Fatalf("existing help = %q, %v", data, err)
	}
}

func TestWriteBootstrapFileCreatesOnceWithPrivatePermissions(t *testing.T) {
	path := filepath.Join(t.TempDir(), "runtime", "bootstrap.txt")
	bootstrap := adminauth.Bootstrap{Created: true, Username: "admin", InitialPassword: "initial-password", AutomationToken: "hpa_test_token"}
	created, err := writeBootstrapFile(path, bootstrap)
	if err != nil || !created {
		t.Fatalf("writeBootstrapFile() = %t, %v", created, err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"管理员：admin", "初始密码：initial-password", "自动化 Token：hpa_test_token"} {
		if !strings.Contains(string(data), want) {
			t.Fatalf("bootstrap file %q lacks %q", data, want)
		}
	}
	assertPrivateRuntimePath(t, path, 0o600)
	created, err = writeBootstrapFile(path, adminauth.Bootstrap{Created: true, Username: "other", InitialPassword: "replacement", AutomationToken: "replacement-token"})
	if err != nil || created {
		t.Fatalf("second writeBootstrapFile() = %t, %v", created, err)
	}
	unchanged, err := os.ReadFile(path)
	if err != nil || string(unchanged) != string(data) {
		t.Fatalf("bootstrap was overwritten: %q, %v", unchanged, err)
	}
}

func TestResolveRuntimeFilesDerivesTestFilesFromAuthOverride(t *testing.T) {
	configured := config.ServerRuntimeFiles{AuthFile: "/configured/auth.json", BootstrapFile: "/configured/bootstrap.txt", HelpFile: "/configured/help.md"}
	authFile := filepath.Join(t.TempDir(), "auth.json")
	resolved := resolveRuntimeFiles(configured, Options{AuthFile: authFile})
	if resolved.AuthFile != authFile || resolved.BootstrapFile != filepath.Join(filepath.Dir(authFile), "bootstrap.txt") || resolved.HelpFile != filepath.Join(filepath.Dir(authFile), "help.md") {
		t.Fatalf("resolveRuntimeFiles() = %#v", resolved)
	}
}

func assertPrivateRuntimePath(t *testing.T, path string, want os.FileMode) {
	t.Helper()
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != want {
		t.Fatalf("%s permissions = %o, want %o", path, info.Mode().Perm(), want)
	}
}
