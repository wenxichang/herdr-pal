package buildscript

import (
	"crypto/sha256"
	"encoding/hex"
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

func TestBundleInstallScriptReusesCompatibleHerdr(t *testing.T) {
	root := repositoryRoot(t)
	bundle := t.TempDir()
	home := t.TempDir()
	logPath := filepath.Join(t.TempDir(), "commands.log")
	renderInstallTemplate(t, filepath.Join(root, "packaging", "bundle", "install.sh"), filepath.Join(bundle, "install.sh"), runtime.GOOS, runtime.GOARCH)
	writeFakeBundleBinaries(t, bundle)
	fakeBin := filepath.Join(t.TempDir(), "bin")
	if err := os.MkdirAll(fakeBin, 0o755); err != nil {
		t.Fatal(err)
	}
	herdrPath := filepath.Join(fakeBin, "herdr")
	writeFakeHerdr(t, herdrPath, "0.7.5", "17", true)
	target := filepath.Join(home, ".local", "bin")
	if err := os.MkdirAll(target, 0o755); err != nil {
		t.Fatal(err)
	}
	canonicalHerdr, err := filepath.EvalSymlinks(herdrPath)
	if err != nil {
		t.Fatal(err)
	}

	command := exec.Command("/bin/sh", filepath.Join(bundle, "install.sh"))
	command.Env = append(os.Environ(),
		"HOME="+home,
		"INSTALL_TEST_LOG="+logPath,
		"EXPECTED_KEY="+bundleInstallTestKey,
		"PATH="+fakeBin+":/usr/bin:/bin",
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
	palPath := filepath.Join(target, "herdr-pal")
	info, err := os.Lstat(palPath)
	if err != nil {
		t.Fatal(err)
	}
	if !info.Mode().IsRegular() || info.Mode().Perm() != 0o755 {
		t.Fatalf("%s mode = %s", palPath, info.Mode())
	}
	if _, err := os.Stat(filepath.Join(target, "herdr")); !os.IsNotExist(err) {
		t.Fatalf("compatible external Herdr should not be copied into install directory: %v", err)
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
		"--herdr-bin " + canonicalHerdr,
		"herdr --version",
		"herdr api schema --json",
		"herdr config check",
		"herdr status server --json",
		"herdr server live-handoff --import-exe " + canonicalHerdr,
	} {
		if !strings.Contains(logText, want) {
			t.Errorf("command log missing %q:\n%s", want, logText)
		}
	}
	if !strings.Contains(string(output), "PATH") {
		t.Fatalf("installer output should include PATH hint:\n%s", output)
	}
}

func TestBundleInstallScriptRejectsUnsupportedHerdrWithoutChanges(t *testing.T) {
	root := repositoryRoot(t)
	bundle := t.TempDir()
	home := t.TempDir()
	logPath := filepath.Join(t.TempDir(), "commands.log")
	renderInstallTemplate(t, filepath.Join(root, "packaging", "bundle", "install.sh"), filepath.Join(bundle, "install.sh"), runtime.GOOS, runtime.GOARCH)
	writeFakeBundleBinaries(t, bundle)
	fakeBin := filepath.Join(t.TempDir(), "bin")
	if err := os.MkdirAll(fakeBin, 0o755); err != nil {
		t.Fatal(err)
	}
	herdrPath := filepath.Join(fakeBin, "herdr")
	writeFakeHerdr(t, herdrPath, "0.7.4", "17", false)

	command := exec.Command("/bin/sh", filepath.Join(bundle, "install.sh"))
	command.Env = append(os.Environ(),
		"HOME="+home,
		"INSTALL_TEST_LOG="+logPath,
		"PATH="+fakeBin+":/usr/bin:/bin",
	)
	command.Stdin = strings.NewReader("\nn\n")
	output, err := command.CombinedOutput()
	if err == nil {
		t.Fatalf("install.sh should stop when the user rejects the compatible Herdr:\n%s", output)
	}
	for _, want := range []string{herdrPath, "版本 0.7.4", "协议 17", "用户取消安装兼容 Herdr"} {
		if !strings.Contains(string(output), want) {
			t.Fatalf("installer output missing %q:\n%s", want, output)
		}
	}
	if _, err := os.Stat(filepath.Join(home, ".local", "bin", "herdr-pal")); !os.IsNotExist(err) {
		t.Fatalf("Pal should not be installed after rejection: %v", err)
	}
	if _, err := os.Stat(filepath.Join(home, ".config", "herdr-pal", "config.json")); !os.IsNotExist(err) {
		t.Fatalf("client config should not be written after rejection: %v", err)
	}
}

func TestBundleInstallScriptDownloadsHerdrWhenMissing(t *testing.T) {
	root := repositoryRoot(t)
	bundle := t.TempDir()
	home := t.TempDir()
	logPath := filepath.Join(t.TempDir(), "commands.log")
	downloadSource := filepath.Join(t.TempDir(), "herdr-download")
	writeFakeHerdr(t, downloadSource, "0.7.5", "17", false)
	downloadSHA := fileSHA256(t, downloadSource)
	renderInstallTemplateWithHerdr(
		t,
		filepath.Join(root, "packaging", "bundle", "install.sh"),
		filepath.Join(bundle, "install.sh"),
		runtime.GOOS,
		runtime.GOARCH,
		"https://downloads.example/herdr",
		downloadSHA,
	)
	writeFakeBundleBinaries(t, bundle)
	fakeBin := writeFakeDownloadTools(t, runtime.GOOS, runtime.GOARCH)

	command := exec.Command("/bin/sh", filepath.Join(bundle, "install.sh"))
	command.Env = append(os.Environ(),
		"HOME="+home,
		"INSTALL_TEST_LOG="+logPath,
		"INSTALL_TEST_DOWNLOAD_SOURCE="+downloadSource,
		"EXPECTED_KEY="+bundleInstallTestKey,
		"PATH="+fakeBin+":/usr/bin:/bin",
	)
	command.Stdin = strings.NewReader("\nwss://relay.example/hprp\n" + bundleInstallTestKey + "\n")
	output, err := command.CombinedOutput()
	if err != nil {
		t.Fatalf("install.sh error = %v\n%s", err, output)
	}
	for _, name := range []string{"herdr", "herdr-pal"} {
		path := filepath.Join(home, ".local", "bin", name)
		info, err := os.Stat(path)
		if err != nil {
			t.Fatal(err)
		}
		if !info.Mode().IsRegular() || info.Mode().Perm() != 0o755 {
			t.Fatalf("%s mode = %s", path, info.Mode())
		}
	}
	logData, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatal(err)
	}
	canonicalInstallDir, err := filepath.EvalSymlinks(filepath.Join(home, ".local", "bin"))
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"curl https://downloads.example/herdr",
		"pal setup --url wss://relay.example/hprp",
		"--herdr-bin " + filepath.Join(canonicalInstallDir, "herdr"),
	} {
		if !strings.Contains(string(logData), want) {
			t.Fatalf("command log missing %q:\n%s", want, logData)
		}
	}
	if !strings.Contains(string(output), "未检测到 Herdr，将安装兼容版本 0.7.5") {
		t.Fatalf("installer output should explain automatic Herdr installation:\n%s", output)
	}
}

func TestBundleInstallScriptUpdatesUnsupportedHerdrAfterConfirmation(t *testing.T) {
	root := repositoryRoot(t)
	bundle := t.TempDir()
	home := t.TempDir()
	logPath := filepath.Join(t.TempDir(), "commands.log")
	downloadSource := filepath.Join(t.TempDir(), "herdr-download")
	writeFakeHerdr(t, downloadSource, "0.7.5", "17", false)
	renderInstallTemplateWithHerdr(
		t,
		filepath.Join(root, "packaging", "bundle", "install.sh"),
		filepath.Join(bundle, "install.sh"),
		runtime.GOOS,
		runtime.GOARCH,
		"https://downloads.example/herdr",
		fileSHA256(t, downloadSource),
	)
	writeFakeBundleBinaries(t, bundle)
	fakeBin := writeFakeDownloadTools(t, runtime.GOOS, runtime.GOARCH)
	writeFakeHerdr(t, filepath.Join(fakeBin, "herdr"), "0.7.4", "17", false)

	command := exec.Command("/bin/sh", filepath.Join(bundle, "install.sh"))
	command.Env = append(os.Environ(),
		"HOME="+home,
		"INSTALL_TEST_LOG="+logPath,
		"INSTALL_TEST_DOWNLOAD_SOURCE="+downloadSource,
		"EXPECTED_KEY="+bundleInstallTestKey,
		"PATH="+fakeBin+":/usr/bin:/bin",
	)
	command.Stdin = strings.NewReader("\n\nwss://relay.example/hprp\n" + bundleInstallTestKey + "\n")
	output, err := command.CombinedOutput()
	if err != nil {
		t.Fatalf("install.sh error = %v\n%s", err, output)
	}
	if !strings.Contains(string(output), "当前 Herdr 不在兼容名单中") {
		t.Fatalf("installer should ask before updating unsupported Herdr:\n%s", output)
	}
	installedHerdr := filepath.Join(home, ".local", "bin", "herdr")
	versionOutput, err := exec.Command(installedHerdr, "--version").CombinedOutput()
	if err != nil || !strings.Contains(string(versionOutput), "0.7.5") {
		t.Fatalf("installed Herdr version = %q, %v", versionOutput, err)
	}
}

func TestBundleInstallScriptUsesLaterCompatibleCandidate(t *testing.T) {
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
	writeFakeHerdr(t, filepath.Join(target, "herdr"), "0.7.4", "17", false)
	fakeBin := filepath.Join(t.TempDir(), "bin")
	if err := os.MkdirAll(fakeBin, 0o755); err != nil {
		t.Fatal(err)
	}
	compatibleHerdr := filepath.Join(fakeBin, "herdr")
	writeFakeHerdr(t, compatibleHerdr, "0.7.5", "17", false)

	command := exec.Command("/bin/sh", filepath.Join(bundle, "install.sh"))
	command.Env = append(os.Environ(),
		"HOME="+home,
		"INSTALL_TEST_LOG="+logPath,
		"EXPECTED_KEY="+bundleInstallTestKey,
		"PATH="+fakeBin+":/usr/bin:/bin",
	)
	command.Stdin = strings.NewReader("\nwss://relay.example/hprp\n" + bundleInstallTestKey + "\n")
	output, err := command.CombinedOutput()
	if err != nil {
		t.Fatalf("install.sh error = %v\n%s", err, output)
	}
	if strings.Contains(string(output), "安装兼容 Herdr 0.7.5 到") {
		t.Fatalf("installer should reuse the later compatible candidate:\n%s", output)
	}
	versionOutput, err := exec.Command(filepath.Join(target, "herdr"), "--version").CombinedOutput()
	if err != nil || !strings.Contains(string(versionOutput), "0.7.4") {
		t.Fatalf("target Herdr should remain unchanged: %q, %v", versionOutput, err)
	}
}

func TestBundleInstallScriptRejectsInvalidHerdrDownloads(t *testing.T) {
	tests := []struct {
		name        string
		version     string
		protocol    string
		shaOverride string
		environment []string
		want        string
	}{
		{name: "curl failure", version: "0.7.5", protocol: "17", environment: []string{"INSTALL_TEST_CURL_FAIL=1"}, want: "下载 Herdr 0.7.5 失败"},
		{name: "checksum mismatch", version: "0.7.5", protocol: "17", shaOverride: strings.Repeat("f", 64), want: "SHA-256 校验失败"},
		{name: "wrong platform", version: "0.7.5", protocol: "17", environment: []string{"INSTALL_TEST_FILE_OUTPUT=ASCII text"}, want: "下载的 Herdr 不是"},
		{name: "wrong version", version: "0.7.4", protocol: "17", want: "版本 0.7.4，协议 17"},
		{name: "wrong protocol", version: "0.7.5", protocol: "16", want: "版本 0.7.5，协议 16"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			root := repositoryRoot(t)
			bundle := t.TempDir()
			home := t.TempDir()
			logPath := filepath.Join(t.TempDir(), "commands.log")
			downloadSource := filepath.Join(t.TempDir(), "herdr-download")
			writeFakeHerdr(t, downloadSource, test.version, test.protocol, false)
			downloadSHA := fileSHA256(t, downloadSource)
			if test.shaOverride != "" {
				downloadSHA = test.shaOverride
			}
			renderInstallTemplateWithHerdr(
				t,
				filepath.Join(root, "packaging", "bundle", "install.sh"),
				filepath.Join(bundle, "install.sh"),
				runtime.GOOS,
				runtime.GOARCH,
				"https://downloads.example/herdr",
				downloadSHA,
			)
			writeFakeBundleBinaries(t, bundle)
			fakeBin := writeFakeDownloadTools(t, runtime.GOOS, runtime.GOARCH)
			command := exec.Command("/bin/sh", filepath.Join(bundle, "install.sh"))
			command.Env = append(os.Environ(),
				"HOME="+home,
				"INSTALL_TEST_LOG="+logPath,
				"INSTALL_TEST_DOWNLOAD_SOURCE="+downloadSource,
				"PATH="+fakeBin+":/usr/bin:/bin",
			)
			command.Env = append(command.Env, test.environment...)
			command.Stdin = strings.NewReader("\n")
			output, err := command.CombinedOutput()
			if err == nil {
				t.Fatalf("install.sh should reject invalid download:\n%s", output)
			}
			if !strings.Contains(string(output), test.want) {
				t.Fatalf("installer output missing %q:\n%s", test.want, output)
			}
			if _, err := os.Stat(filepath.Join(home, ".local", "bin", "herdr-pal")); !os.IsNotExist(err) {
				t.Fatalf("Pal should not be installed after download failure: %v", err)
			}
		})
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
	fakeBin := filepath.Join(t.TempDir(), "bin")
	if err := os.MkdirAll(fakeBin, 0o755); err != nil {
		t.Fatal(err)
	}
	writeFakeHerdr(t, filepath.Join(fakeBin, "herdr"), "0.7.5", "17", true)

	command := exec.Command("/bin/sh", filepath.Join(bundle, "install.sh"))
	command.Env = append(os.Environ(),
		"HOME="+home,
		"INSTALL_TEST_LOG="+logPath,
		"EXPECTED_KEY="+bundleInstallTestKey,
		"PATH="+fakeBin+":/usr/bin:/bin",
	)
	command.Stdin = strings.NewReader(target + "\nwss://relay.example/hprp\n" + bundleInstallTestKey + "\nn\n")
	if output, err := command.CombinedOutput(); err != nil {
		t.Fatalf("install.sh error = %v\n%s", err, output)
	}
	assertPathExists(t, filepath.Join(target, "herdr-pal"))
	if _, err := os.Stat(filepath.Join(target, "herdr")); !os.IsNotExist(err) {
		t.Fatalf("external compatible Herdr should not be copied: %v", err)
	}
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
	downloadSource := filepath.Join(t.TempDir(), "herdr-download")
	writeFakeHerdr(t, downloadSource, "0.7.5", "17", false)
	renderInstallTemplateWithHerdr(
		t,
		filepath.Join(root, "packaging", "bundle", "install.sh"),
		filepath.Join(bundle, "install.sh"),
		runtime.GOOS,
		runtime.GOARCH,
		"https://downloads.example/herdr",
		fileSHA256(t, downloadSource),
	)
	writeFakeBundleBinaries(t, bundle)
	fakeBin := writeFakeDownloadTools(t, runtime.GOOS, runtime.GOARCH)
	target := filepath.Join(home, ".local", "bin")
	if err := os.MkdirAll(target, 0o755); err != nil {
		t.Fatal(err)
	}
	writeFakeHerdr(t, filepath.Join(target, "herdr"), "0.7.4", "17", false)
	oldPal := []byte("old-pal")
	if err := os.WriteFile(filepath.Join(target, "herdr-pal"), oldPal, 0o755); err != nil {
		t.Fatal(err)
	}

	command := exec.Command("/bin/sh", filepath.Join(bundle, "install.sh"))
	command.Env = append(os.Environ(),
		"HOME="+home,
		"INSTALL_TEST_LOG="+logPath,
		"INSTALL_TEST_DOWNLOAD_SOURCE="+downloadSource,
		"EXPECTED_KEY="+bundleInstallTestKey,
		"SETUP_FAIL=1",
		"PATH="+fakeBin+":/usr/bin:/bin",
	)
	command.Stdin = strings.NewReader("\n\nwss://relay.example/hprp\n" + bundleInstallTestKey + "\n")
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
	restoredPal, readErr := os.ReadFile(filepath.Join(target, "herdr-pal"))
	if readErr != nil {
		t.Fatal(readErr)
	}
	if string(restoredPal) != string(oldPal) {
		t.Fatalf("Pal was not restored after setup failure: %q", restoredPal)
	}
	restoredHerdr, readErr := exec.Command(filepath.Join(target, "herdr"), "--version").CombinedOutput()
	if readErr != nil || !strings.Contains(string(restoredHerdr), "0.7.4") {
		t.Fatalf("Herdr was not restored after setup failure: %q, %v", restoredHerdr, readErr)
	}
}

func renderInstallTemplate(t *testing.T, source, target, goos, goarch string) {
	t.Helper()
	renderInstallTemplateWithHerdr(t, source, target, goos, goarch, "https://downloads.example/herdr", strings.Repeat("0", 64))
}

func renderInstallTemplateWithHerdr(t *testing.T, source, target, goos, goarch, downloadURL, downloadSHA string) {
	t.Helper()
	data, err := os.ReadFile(source)
	if err != nil {
		t.Fatal(err)
	}
	content := strings.NewReplacer(
		"@BUNDLE_OS@", goos,
		"@BUNDLE_ARCH@", goarch,
		"@BUNDLE_VERSION@", "test-version",
		"@HERDR_VERSION@", "0.7.5",
		"@HERDR_PROTOCOL@", "17",
		"@HERDR_DOWNLOAD_URL@", downloadURL,
		"@HERDR_SHA256@", downloadSHA,
	).Replace(string(data))
	if err := os.WriteFile(target, []byte(content), 0o755); err != nil {
		t.Fatal(err)
	}
}

func writeFakeDownloadTools(t *testing.T, goos, goarch string) string {
	t.Helper()
	fakeBin := filepath.Join(t.TempDir(), "bin")
	if err := os.MkdirAll(fakeBin, 0o755); err != nil {
		t.Fatal(err)
	}
	writeExecutable(t, filepath.Join(fakeBin, "curl"), `#!/bin/sh
set -eu
output=''
url=''
while [ "$#" -gt 0 ]; do
  case "$1" in
    -o) shift; output=$1 ;;
    http://*|https://*) url=$1 ;;
  esac
  shift
done
printf 'curl %s\n' "$url" >> "$INSTALL_TEST_LOG"
[ "${INSTALL_TEST_CURL_FAIL:-0}" != "1" ]
cp "$INSTALL_TEST_DOWNLOAD_SOURCE" "$output"
`)
	fileOutput := "Mach-O 64-bit executable arm64"
	switch goos + "-" + goarch {
	case "darwin-amd64":
		fileOutput = "Mach-O 64-bit executable x86_64"
	case "linux-amd64":
		fileOutput = "ELF 64-bit LSB executable, x86-64, statically linked"
	case "linux-arm64":
		fileOutput = "ELF 64-bit LSB executable, ARM aarch64, statically linked"
	}
	writeExecutable(t, filepath.Join(fakeBin, "file"), "#!/bin/sh\nprintf '%s\\n' \"${INSTALL_TEST_FILE_OUTPUT:-"+fileOutput+"}\"\n")
	return fakeBin
}

func fileSHA256(t *testing.T, path string) string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	digest := sha256.Sum256(data)
	return hex.EncodeToString(digest[:])
}

func writeFakeBundleBinaries(t *testing.T, bundle string) {
	t.Helper()
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

func writeFakeHerdr(t *testing.T, path, version, protocol string, running bool) {
	t.Helper()
	runningJSON := "false"
	if running {
		runningJSON = "true"
	}
	script := `#!/bin/sh
set -eu
if [ -n "${INSTALL_TEST_LOG:-}" ]; then
  printf 'herdr %s\n' "$*" >> "$INSTALL_TEST_LOG"
fi
case "$*" in
  "--version") printf '%s\n' 'herdr ` + version + `' ;;
  "api schema --json") printf '%s\n' '{"protocol":` + protocol + `}' ;;
  "config check") exit 0 ;;
  "status server --json") printf '%s\n' '{"running":` + runningJSON + `}' ;;
  server\ live-handoff\ --import-exe\ *) exit 0 ;;
  *) exit 1 ;;
esac
`
	writeExecutable(t, path, script)
}

func assertPathExists(t *testing.T, path string) {
	t.Helper()
	if _, err := os.Stat(path); err != nil {
		t.Fatal(err)
	}
}
