package buildscript

import (
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func TestBuildScriptProducesNativeAndLinuxAMD64Binaries(t *testing.T) {
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

	for _, name := range []string{
		"herdr-pal",
		"herdr-pal-server",
		"herdr-pal-linux-amd64",
		"herdr-pal-server-linux-amd64",
	} {
		if _, err := os.Stat(filepath.Join(temporaryRoot, "dist", name)); err != nil {
			t.Errorf("missing build output %s: %v", name, err)
		}
	}
	logData, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatal(err)
	}
	logText := string(logData)
	for _, name := range []string{"herdr-pal-linux-amd64", "herdr-pal-server-linux-amd64"} {
		if !strings.Contains(logText, "linux|amd64|0|build") || !strings.Contains(logText, "-o dist/"+name) {
			t.Errorf("linux/amd64 build invocation missing for %s:\n%s", name, logText)
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
