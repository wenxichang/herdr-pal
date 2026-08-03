package buildscript

import (
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

const bundleInstallTestKey = "hpk_7_AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA"

func TestBundleInstallScriptLeavesMachineKeyEchoEnabled(t *testing.T) {
	content, err := os.ReadFile(filepath.Join(repositoryRoot(t), "packaging", "bundle", "install.sh"))
	if err != nil {
		t.Fatal(err)
	}
	script := string(content)
	if strings.Contains(script, "stty -echo") {
		t.Fatal("install.sh still disables terminal echo for machine key")
	}
	if !strings.Contains(script, "printf '机器 Key: '\nIFS= read -r relay_key") {
		t.Fatal("install.sh should read machine key directly after the prompt")
	}
}

func TestBundleInstallScriptInstallsConfiguresAndHandsOff(t *testing.T) {
	root := repositoryRoot(t)
	bundle := t.TempDir()
	home := t.TempDir()
	logPath := filepath.Join(t.TempDir(), "commands.log")
	renderInstallTemplate(t, filepath.Join(root, "packaging", "bundle", "install.sh"), filepath.Join(bundle, "install.sh"), runtime.GOOS, runtime.GOARCH)
	writeFakeBundleBinaries(t, bundle)
	target := filepath.Join(home, ".local", "bin")
	if err := os.MkdirAll(target, 0o755); err != nil {
		t.Fatal(err)
	}
	canonicalTarget, err := filepath.EvalSymlinks(target)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(target, "herdr"), []byte("old-herdr"), 0o755); err != nil {
		t.Fatal(err)
	}

	command := exec.Command("/bin/sh", filepath.Join(bundle, "install.sh"))
	command.Env = append(os.Environ(),
		"HOME="+home,
		"INSTALL_TEST_LOG="+logPath,
		"EXPECTED_KEY="+bundleInstallTestKey,
		"PATH=/usr/bin:/bin",
	)
	command.Stdin = strings.NewReader("\nwss://relay.example/hprp\n" + bundleInstallTestKey + "\n\n")
	output, err := command.CombinedOutput()
	if err != nil {
		t.Fatalf("install.sh error = %v\n%s", err, output)
	}
	if strings.Contains(string(output), bundleInstallTestKey) {
		t.Fatalf("installer output exposes key:\n%s", output)
	}
	if strings.Contains(string(output), "Sidecar") || !strings.Contains(string(output), "Startup 插件") {
		t.Fatalf("installer output should describe the startup plugin:\n%s", output)
	}
	for _, name := range []string{"herdr", "herdr-pal"} {
		path := filepath.Join(target, name)
		info, err := os.Lstat(path)
		if err != nil {
			t.Fatal(err)
		}
		if !info.Mode().IsRegular() || info.Mode().Perm() != 0o755 {
			t.Fatalf("%s mode = %s", path, info.Mode())
		}
	}
	if backups, err := filepath.Glob(filepath.Join(target, "herdr.bak-*")); err != nil || len(backups) != 1 {
		t.Fatalf("herdr backups = %v, error = %v", backups, err)
	}
	assertPathExists(t, filepath.Join(home, ".config", "herdr-pal", "config.json"))
	assertPathExists(t, filepath.Join(home, ".config", "herdr", "config.toml"))
	logData, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatal(err)
	}
	logText := string(logData)
	for _, want := range []string{
		"pal setup --url wss://relay.example/hprp",
		"herdr config check",
		"herdr status server --json",
		"herdr server live-handoff --import-exe " + filepath.Join(canonicalTarget, "herdr"),
	} {
		if !strings.Contains(logText, want) {
			t.Errorf("command log missing %q:\n%s", want, logText)
		}
	}
	if !strings.Contains(string(output), "PATH") {
		t.Fatalf("installer output should include PATH hint:\n%s", output)
	}
}

func TestBundleInstallScriptSupportsCustomDirectoryAndRejectsHandoff(t *testing.T) {
	root := repositoryRoot(t)
	bundle := t.TempDir()
	home := t.TempDir()
	target := filepath.Join(t.TempDir(), "custom-bin")
	logPath := filepath.Join(t.TempDir(), "commands.log")
	renderInstallTemplate(t, filepath.Join(root, "packaging", "bundle", "install.sh"), filepath.Join(bundle, "install.sh"), runtime.GOOS, runtime.GOARCH)
	writeFakeBundleBinaries(t, bundle)

	command := exec.Command("/bin/sh", filepath.Join(bundle, "install.sh"))
	command.Env = append(os.Environ(),
		"HOME="+home,
		"INSTALL_TEST_LOG="+logPath,
		"EXPECTED_KEY="+bundleInstallTestKey,
		"PATH=/usr/bin:/bin",
	)
	command.Stdin = strings.NewReader(target + "\nwss://relay.example/hprp\n" + bundleInstallTestKey + "\nn\n")
	if output, err := command.CombinedOutput(); err != nil {
		t.Fatalf("install.sh error = %v\n%s", err, output)
	}
	assertPathExists(t, filepath.Join(target, "herdr"))
	logData, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(logData), "server live-handoff") {
		t.Fatalf("live handoff should not run:\n%s", logData)
	}
}

func TestBundleInstallScriptRejectsPlatformMismatch(t *testing.T) {
	root := repositoryRoot(t)
	bundle := t.TempDir()
	home := t.TempDir()
	wrongOS := "linux"
	if runtime.GOOS == "linux" {
		wrongOS = "darwin"
	}
	renderInstallTemplate(t, filepath.Join(root, "packaging", "bundle", "install.sh"), filepath.Join(bundle, "install.sh"), wrongOS, runtime.GOARCH)
	writeFakeBundleBinaries(t, bundle)

	command := exec.Command("/bin/sh", filepath.Join(bundle, "install.sh"))
	command.Env = append(os.Environ(), "HOME="+home, "PATH=/usr/bin:/bin")
	output, err := command.CombinedOutput()
	if err == nil {
		t.Fatalf("install.sh should reject mismatched platform:\n%s", output)
	}
	if !strings.Contains(string(output), "平台不匹配") {
		t.Fatalf("unexpected output:\n%s", output)
	}
}

func TestBundleInstallScriptStopsWhenSetupFails(t *testing.T) {
	root := repositoryRoot(t)
	bundle := t.TempDir()
	home := t.TempDir()
	logPath := filepath.Join(t.TempDir(), "commands.log")
	renderInstallTemplate(t, filepath.Join(root, "packaging", "bundle", "install.sh"), filepath.Join(bundle, "install.sh"), runtime.GOOS, runtime.GOARCH)
	writeFakeBundleBinaries(t, bundle)

	command := exec.Command("/bin/sh", filepath.Join(bundle, "install.sh"))
	command.Env = append(os.Environ(),
		"HOME="+home,
		"INSTALL_TEST_LOG="+logPath,
		"EXPECTED_KEY="+bundleInstallTestKey,
		"SETUP_FAIL=1",
		"PATH=/usr/bin:/bin",
	)
	command.Stdin = strings.NewReader("\nwss://relay.example/hprp\n" + bundleInstallTestKey + "\n")
	output, err := command.CombinedOutput()
	if err == nil {
		t.Fatalf("install.sh should fail:\n%s", output)
	}
	if strings.Contains(string(output), bundleInstallTestKey) {
		t.Fatalf("installer output exposes key:\n%s", output)
	}
	logData, readErr := os.ReadFile(logPath)
	if readErr != nil {
		t.Fatal(readErr)
	}
	if strings.Contains(string(logData), "herdr config check") {
		t.Fatalf("final config check should not run after setup failure:\n%s", logData)
	}
}

func renderInstallTemplate(t *testing.T, source, target, goos, goarch string) {
	t.Helper()
	data, err := os.ReadFile(source)
	if err != nil {
		t.Fatal(err)
	}
	content := strings.NewReplacer(
		"@BUNDLE_OS@", goos,
		"@BUNDLE_ARCH@", goarch,
		"@BUNDLE_VERSION@", "test-version",
	).Replace(string(data))
	if err := os.WriteFile(target, []byte(content), 0o755); err != nil {
		t.Fatal(err)
	}
}

func writeFakeBundleBinaries(t *testing.T, bundle string) {
	t.Helper()
	writeExecutable(t, filepath.Join(bundle, "herdr"), `#!/bin/sh
set -eu
printf 'herdr %s\n' "$*" >> "$INSTALL_TEST_LOG"
case "$*" in
  "config check") exit 0 ;;
  "status server --json") printf '%s\n' '{"running":true}' ;;
  server\ live-handoff\ --import-exe\ *) exit 0 ;;
  *) exit 1 ;;
esac
`)
	writeExecutable(t, filepath.Join(bundle, "herdr-pal"), `#!/bin/sh
set -eu
printf 'pal %s\n' "$*" >> "$INSTALL_TEST_LOG"
client_config=''
herdr_config=''
while [ "$#" -gt 0 ]; do
  case "$1" in
    --config) shift; client_config=$1 ;;
    --herdr-config) shift; herdr_config=$1 ;;
  esac
  shift
done
IFS= read -r received_key
[ "$received_key" = "$EXPECTED_KEY" ]
[ "${SETUP_FAIL:-0}" != "1" ]
mkdir -p "$(dirname "$client_config")" "$(dirname "$herdr_config")"
printf '%s\n' '{}' > "$client_config"
printf '%s\n' '# startup plugin is linked by herdr-pal setup' > "$herdr_config"
`)
}

func assertPathExists(t *testing.T, path string) {
	t.Helper()
	if _, err := os.Stat(path); err != nil {
		t.Fatal(err)
	}
}
