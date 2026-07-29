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

func TestBuildScriptProducesReleaseArchitectureMatrix(t *testing.T) {
	root := repositoryRoot(t)
	temporaryRoot := t.TempDir()
	buildScript, err := os.ReadFile(filepath.Join(root, "build.sh"))
	if err != nil {
		t.Fatal(err)
	}
	temporaryScript := filepath.Join(temporaryRoot, "build.sh")
	if err := os.WriteFile(temporaryScript, buildScript, 0o755); err != nil {
		t.Fatal(err)
	}

	fakeBin := filepath.Join(temporaryRoot, "bin")
	if err := os.Mkdir(fakeBin, 0o755); err != nil {
		t.Fatal(err)
	}
	writeExecutable(t, filepath.Join(fakeBin, "gofmt"), "#!/bin/sh\nexit 0\n")
	writeExecutable(t, filepath.Join(fakeBin, "go"), `#!/bin/sh
printf '%s|%s|%s|%s\n' "${GOOS-}" "${GOARCH-}" "${CGO_ENABLED-}" "$*" >> "$BUILD_TEST_LOG"
if [ "${1-}" = "build" ]; then
	shift
	while [ "$#" -gt 0 ]; do
		if [ "$1" = "-o" ]; then
			shift
			mkdir -p "$(dirname "$1")"
			: > "$1"
			exit 0
		fi
		shift
	done
fi
exit 0
`)

	logPath := filepath.Join(temporaryRoot, "build.log")
	command := exec.Command("/bin/sh", temporaryScript)
	command.Env = append(os.Environ(),
		"PATH="+fakeBin+string(os.PathListSeparator)+os.Getenv("PATH"),
		"BUILD_TEST_LOG="+logPath,
		"VERSION=test-version",
		"COMMIT=test-commit",
		"BUILT_AT=2026-07-25T00:00:00Z",
	)
	if output, err := command.CombinedOutput(); err != nil {
		t.Fatalf("build.sh error = %v\n%s", err, output)
	}

	targets := []struct {
		name   string
		goos   string
		goarch string
	}{
		{name: "herdr-pal-darwin-amd64", goos: "darwin", goarch: "amd64"},
		{name: "herdr-pal-server-darwin-amd64", goos: "darwin", goarch: "amd64"},
		{name: "hp-cli-darwin-amd64", goos: "darwin", goarch: "amd64"},
		{name: "herdr-pal-darwin-arm64", goos: "darwin", goarch: "arm64"},
		{name: "herdr-pal-server-darwin-arm64", goos: "darwin", goarch: "arm64"},
		{name: "hp-cli-darwin-arm64", goos: "darwin", goarch: "arm64"},
		{name: "herdr-pal-linux-amd64", goos: "linux", goarch: "amd64"},
		{name: "herdr-pal-server-linux-amd64", goos: "linux", goarch: "amd64"},
		{name: "hp-cli-linux-amd64", goos: "linux", goarch: "amd64"},
		{name: "herdr-pal-linux-arm64", goos: "linux", goarch: "arm64"},
		{name: "herdr-pal-server-linux-arm64", goos: "linux", goarch: "arm64"},
		{name: "hp-cli-linux-arm64", goos: "linux", goarch: "arm64"},
		{name: "herdr-pal-windows-amd64.exe", goos: "windows", goarch: "amd64"},
	}
	for _, target := range targets {
		if _, err := os.Stat(filepath.Join(temporaryRoot, "dist", target.name)); err != nil {
			t.Errorf("missing build output %s: %v", target.name, err)
		}
	}
	for _, name := range []string{"herdr-pal", "herdr-pal-server", "hp-cli"} {
		if _, err := os.Stat(filepath.Join(temporaryRoot, "dist", name)); err != nil {
			t.Errorf("missing native convenience output %s: %v", name, err)
		}
	}
	checksumData, err := os.ReadFile(filepath.Join(temporaryRoot, "dist", "SHA256SUMS"))
	if err != nil {
		t.Fatalf("missing SHA256SUMS: %v", err)
	}
	checksumLines := strings.Split(strings.TrimSpace(string(checksumData)), "\n")
	if len(checksumLines) != len(targets) {
		t.Fatalf("SHA256SUMS line count = %d, want %d:\n%s", len(checksumLines), len(targets), checksumData)
	}
	for index, target := range targets {
		fields := strings.Fields(checksumLines[index])
		if len(fields) != 2 {
			t.Fatalf("invalid checksum line %q", checksumLines[index])
		}
		binaryData, err := os.ReadFile(filepath.Join(temporaryRoot, "dist", target.name))
		if err != nil {
			t.Fatal(err)
		}
		digest := sha256.Sum256(binaryData)
		if fields[0] != hex.EncodeToString(digest[:]) || fields[1] != target.name {
			t.Errorf("checksum line = %q, want digest for %s", checksumLines[index], target.name)
		}
	}
	logData, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatal(err)
	}
	logText := string(logData)
	for _, target := range targets {
		invocation := target.goos + "|" + target.goarch + "|0|build"
		if !strings.Contains(logText, invocation) || !strings.Contains(logText, "-o dist/"+target.name) {
			t.Errorf("%s/%s build invocation missing for %s:\n%s", target.goos, target.goarch, target.name, logText)
		}
	}
}

func TestGoModuleRequiresPatchedProductionToolchain(t *testing.T) {
	module, err := os.ReadFile(filepath.Join(repositoryRoot(t), "go.mod"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(module), "toolchain go1.26.5") {
		t.Fatalf("go.mod must require patched production toolchain go1.26.5:\n%s", module)
	}
}

func TestProjectScriptsRequirePatchedProductionToolchain(t *testing.T) {
	root := repositoryRoot(t)
	for _, name := range []string{"build.sh", "unittest.sh"} {
		content, err := os.ReadFile(filepath.Join(root, name))
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(string(content), "GOTOOLCHAIN=go1.26.5+auto") {
			t.Fatalf("%s must require GOTOOLCHAIN=go1.26.5+auto:\n%s", name, content)
		}
	}
}

func TestServerBinaryDoesNotDependOnTerminalImagePackage(t *testing.T) {
	command := exec.Command("go", "list", "-deps", "./cmd/herdr-pal-server")
	command.Dir = repositoryRoot(t)
	output, err := command.CombinedOutput()
	if err != nil {
		t.Fatalf("go list -deps error = %v\n%s", err, output)
	}
	dependencies := string(output)
	for _, forbidden := range []string{
		"github.com/wenxichang/herdr-pal/internal/terminalimage",
		"github.com/jiro4989/textimg/v3",
		"golang.org/x/image/font/opentype",
	} {
		if strings.Contains(dependencies, forbidden) {
			t.Fatalf("herdr-pal-server unexpectedly depends on %s", forbidden)
		}
	}
}

func repositoryRoot(t *testing.T) string {
	t.Helper()
	_, currentFile, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("cannot resolve test file path")
	}
	return filepath.Clean(filepath.Join(filepath.Dir(currentFile), "..", ".."))
}

func writeExecutable(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o755); err != nil {
		t.Fatal(err)
	}
}
