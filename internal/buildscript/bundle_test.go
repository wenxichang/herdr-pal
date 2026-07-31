package buildscript

import (
	"crypto/sha256"
	"encoding/hex"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestBuildBundlePackagesReleaseMatrix(t *testing.T) {
	targets := []struct {
		name       string
		fileOutput string
	}{
		{name: "linux-amd64", fileOutput: "ELF 64-bit LSB executable, x86-64, statically linked"},
		{name: "linux-arm64", fileOutput: "ELF 64-bit LSB executable, ARM aarch64, statically linked"},
		{name: "darwin-amd64", fileOutput: "Mach-O 64-bit executable x86_64"},
		{name: "darwin-arm64", fileOutput: "Mach-O 64-bit executable arm64"},
	}
	for _, target := range targets {
		t.Run(target.name, func(t *testing.T) {
			root := prepareBundleTestRoot(t)
			fakeBin := filepath.Join(root, "fake-bin")
			if err := os.MkdirAll(fakeBin, 0o755); err != nil {
				t.Fatal(err)
			}
			writeExecutable(t, filepath.Join(fakeBin, "file"), "#!/bin/sh\nprintf '%s\\n' \"$BUNDLE_TEST_FILE_OUTPUT\"\n")
			herdrBinary := filepath.Join(root, "prebuilt-herdr")
			writeExecutable(t, herdrBinary, "#!/bin/sh\nexit 0\n")
			writeExecutable(t, filepath.Join(root, "dist", "herdr-pal-"+target.name), "#!/bin/sh\nexit 0\n")

			command := exec.Command("/bin/sh", filepath.Join(root, "packaging", "build-bundle.sh"),
				"--target", target.name,
				"--version", "v0.5.0",
				"--herdr-binary", herdrBinary,
				"--herdr-commit", "abc123",
			)
			command.Env = append(os.Environ(),
				"PATH="+fakeBin+string(os.PathListSeparator)+os.Getenv("PATH"),
				"BUNDLE_TEST_FILE_OUTPUT="+target.fileOutput,
			)
			if output, err := command.CombinedOutput(); err != nil {
				t.Fatalf("build-bundle.sh error = %v\n%s", err, output)
			}

			archiveName := "herdr-bundle-v0.5.0-" + target.name + ".tar.gz"
			archivePath := filepath.Join(root, "dist", archiveName)
			checksumPath := archivePath + ".sha256"
			assertPathExists(t, archivePath)
			assertPathExists(t, checksumPath)
			listCommand := exec.Command("tar", "-tzf", archivePath)
			listOutput, err := listCommand.CombinedOutput()
			if err != nil {
				t.Fatalf("tar list error = %v\n%s", err, listOutput)
			}
			rootName := "herdr-bundle-v0.5.0-" + target.name
			for _, want := range []string{
				rootName + "/herdr",
				rootName + "/herdr-pal",
				rootName + "/install.sh",
				rootName + "/README.md",
			} {
				if !strings.Contains(string(listOutput), want) {
					t.Errorf("archive missing %q:\n%s", want, listOutput)
				}
			}
			verifyBundleChecksum(t, archivePath, checksumPath)
			extractDirectory := filepath.Join(root, "extract")
			if err := os.MkdirAll(extractDirectory, 0o755); err != nil {
				t.Fatal(err)
			}
			extractCommand := exec.Command("tar", "-xzf", archivePath, "-C", extractDirectory)
			if output, err := extractCommand.CombinedOutput(); err != nil {
				t.Fatalf("tar extract error = %v\n%s", err, output)
			}
			for _, name := range []string{"herdr", "herdr-pal", "install.sh"} {
				info, err := os.Stat(filepath.Join(extractDirectory, rootName, name))
				if err != nil {
					t.Fatal(err)
				}
				if info.Mode().Perm() != 0o755 {
					t.Errorf("%s mode = %o, want 755", name, info.Mode().Perm())
				}
			}
			readme, err := os.ReadFile(filepath.Join(extractDirectory, rootName, "README.md"))
			if err != nil {
				t.Fatal(err)
			}
			for _, want := range []string{"v0.5.0", target.name, "abc123", "./install.sh"} {
				if !strings.Contains(string(readme), want) {
					t.Errorf("README missing %q:\n%s", want, readme)
				}
			}
		})
	}
}

func TestBuildBundleRejectsInvalidArguments(t *testing.T) {
	root := prepareBundleTestRoot(t)
	tests := [][]string{
		{"--target", "windows-amd64", "--version", "v0.5.0", "--herdr-binary", "/tmp/herdr"},
		{"--target", "darwin-arm64", "--version", "v0.5.0-dirty", "--herdr-binary", "/tmp/herdr"},
		{"--target", "darwin-arm64", "--version", "bad/version", "--herdr-binary", "/tmp/herdr"},
		{"--target", "darwin-arm64", "--version", "v0.5.0", "--herdr-binary", "/missing/herdr"},
	}
	for _, args := range tests {
		command := exec.Command("/bin/sh", append([]string{filepath.Join(root, "packaging", "build-bundle.sh")}, args...)...)
		if output, err := command.CombinedOutput(); err == nil {
			t.Fatalf("build-bundle.sh should fail for %v:\n%s", args, output)
		}
	}
}

func TestBuildBundleBuildsCleanHerdrSource(t *testing.T) {
	root := prepareBundleTestRoot(t)
	fakeBin := filepath.Join(root, "fake-bin")
	if err := os.MkdirAll(fakeBin, 0o755); err != nil {
		t.Fatal(err)
	}
	writeExecutable(t, filepath.Join(fakeBin, "file"), "#!/bin/sh\nprintf '%s\\n' 'Mach-O 64-bit executable arm64'\n")
	writeExecutable(t, filepath.Join(fakeBin, "cargo"), `#!/bin/sh
set -eu
target=''
while [ "$#" -gt 0 ]; do
  if [ "$1" = "--target" ]; then
    shift
    target=$1
  fi
  shift
done
mkdir -p "target/$target/release"
printf '%s\n' '#!/bin/sh' 'exit 0' > "target/$target/release/herdr"
chmod 0755 "target/$target/release/herdr"
`)
	writeExecutable(t, filepath.Join(root, "dist", "herdr-pal-darwin-arm64"), "#!/bin/sh\nexit 0\n")
	herdrSource := filepath.Join(root, "herdr-source")
	if err := os.MkdirAll(herdrSource, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(herdrSource, "Cargo.toml"), []byte("[package]\nname='herdr'\nversion='0.1.0'\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	runGit(t, herdrSource, "init")
	runGit(t, herdrSource, "add", "Cargo.toml")
	runGit(t, herdrSource, "-c", "user.name=Test", "-c", "user.email=test@example.com", "commit", "-m", "init")

	command := exec.Command("/bin/sh", filepath.Join(root, "packaging", "build-bundle.sh"),
		"--target", "darwin-arm64",
		"--version", "v0.5.0",
		"--herdr-source", herdrSource,
	)
	command.Env = append(os.Environ(), "PATH="+fakeBin+string(os.PathListSeparator)+os.Getenv("PATH"))
	if output, err := command.CombinedOutput(); err != nil {
		t.Fatalf("build-bundle.sh error = %v\n%s", err, output)
	}
	assertPathExists(t, filepath.Join(root, "dist", "herdr-bundle-v0.5.0-darwin-arm64.tar.gz"))
}

func TestBuildScriptBundleEntryBuildsPalThenDelegates(t *testing.T) {
	sourceRoot := repositoryRoot(t)
	root := t.TempDir()
	buildData, err := os.ReadFile(filepath.Join(sourceRoot, "build.sh"))
	if err != nil {
		t.Fatal(err)
	}
	buildPath := filepath.Join(root, "build.sh")
	if err := os.WriteFile(buildPath, buildData, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(root, "packaging"), 0o755); err != nil {
		t.Fatal(err)
	}
	writeExecutable(t, filepath.Join(root, "packaging", "build-bundle.sh"), "#!/bin/sh\nprintf '%s\\n' \"$*\" > \"$BUNDLE_ARGS_LOG\"\n")
	fakeBin := filepath.Join(root, "fake-bin")
	if err := os.MkdirAll(fakeBin, 0o755); err != nil {
		t.Fatal(err)
	}
	writeExecutable(t, filepath.Join(fakeBin, "gofmt"), "#!/bin/sh\nexit 0\n")
	writeExecutable(t, filepath.Join(fakeBin, "go"), `#!/bin/sh
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
	argsLog := filepath.Join(root, "bundle-args.log")
	command := exec.Command(buildPath, "bundle", "--target", "darwin-arm64", "--version", "v0.5.0", "--herdr-binary", "/tmp/herdr")
	command.Env = append(os.Environ(),
		"PATH="+fakeBin+string(os.PathListSeparator)+os.Getenv("PATH"),
		"BUNDLE_ARGS_LOG="+argsLog,
		"VERSION=test-version",
		"COMMIT=test-commit",
		"BUILT_AT=2026-07-31T00:00:00Z",
	)
	if output, err := command.CombinedOutput(); err != nil {
		t.Fatalf("build.sh bundle error = %v\n%s", err, output)
	}
	argsData, err := os.ReadFile(argsLog)
	if err != nil {
		t.Fatal(err)
	}
	if string(argsData) != "--target darwin-arm64 --version v0.5.0 --herdr-binary /tmp/herdr\n" {
		t.Fatalf("bundle args = %q", argsData)
	}
	assertPathExists(t, filepath.Join(root, "dist", "herdr-pal-darwin-arm64"))
}

func prepareBundleTestRoot(t *testing.T) string {
	t.Helper()
	sourceRoot := repositoryRoot(t)
	root := t.TempDir()
	for _, relative := range []string{
		filepath.Join("packaging", "build-bundle.sh"),
		filepath.Join("packaging", "bundle", "install.sh"),
		filepath.Join("packaging", "bundle", "README.md"),
	} {
		source := filepath.Join(sourceRoot, relative)
		target := filepath.Join(root, relative)
		data, err := os.ReadFile(source)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
			t.Fatal(err)
		}
		mode := os.FileMode(0o644)
		if strings.HasSuffix(relative, ".sh") {
			mode = 0o755
		}
		if err := os.WriteFile(target, data, mode); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.MkdirAll(filepath.Join(root, "dist"), 0o755); err != nil {
		t.Fatal(err)
	}
	return root
}

func verifyBundleChecksum(t *testing.T, archivePath, checksumPath string) {
	t.Helper()
	archiveData, err := os.ReadFile(archivePath)
	if err != nil {
		t.Fatal(err)
	}
	checksumData, err := os.ReadFile(checksumPath)
	if err != nil {
		t.Fatal(err)
	}
	digest := sha256.Sum256(archiveData)
	fields := strings.Fields(string(checksumData))
	if len(fields) != 2 || fields[0] != hex.EncodeToString(digest[:]) || fields[1] != filepath.Base(archivePath) {
		t.Fatalf("invalid checksum: %q", checksumData)
	}
}

func runGit(t *testing.T, directory string, args ...string) {
	t.Helper()
	command := exec.Command("git", args...)
	command.Dir = directory
	if output, err := command.CombinedOutput(); err != nil {
		t.Fatalf("git %v error = %v\n%s", args, err, output)
	}
}
