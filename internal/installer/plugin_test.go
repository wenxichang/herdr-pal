package installer

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestBuildPluginArtifactsUsesAbsoluteEscapedPaths(t *testing.T) {
	artifacts, err := buildPluginArtifacts("/opt/pal's/bin/herdr-pal", "/home/user's/config.json", "v0.6.0")
	if err != nil {
		t.Fatalf("buildPluginArtifacts() error = %v", err)
	}
	manifest := string(artifacts.Manifest)
	for _, want := range []string{
		`id = "herdr-pal.autostart"`,
		`version = "0.6.0"`,
		`min_herdr_version = "0.7.5"`,
		`command = ["./start-herdr-pal"]`,
	} {
		if !strings.Contains(manifest, want) {
			t.Fatalf("manifest missing %q:\n%s", want, manifest)
		}
	}
	launcher := string(artifacts.Launcher)
	for _, want := range []string{
		`exec '/opt/pal'"'"'s/bin/herdr-pal' start`,
		`-config '/home/user'"'"'s/config.json'`,
	} {
		if !strings.Contains(launcher, want) {
			t.Fatalf("launcher missing %q:\n%s", want, launcher)
		}
	}
	if strings.Contains(launcher, "hpk_") {
		t.Fatal("launcher should not contain machine key")
	}
}

func TestWritePluginDirectoryAtomicallyReplacesAndBacksUp(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "plugin")
	if err := os.MkdirAll(path, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(path, "old"), []byte("old"), 0o600); err != nil {
		t.Fatal(err)
	}
	artifacts, err := buildPluginArtifacts("/opt/herdr-pal", "/home/user/config.json", "v0.6.0")
	if err != nil {
		t.Fatal(err)
	}
	backup, err := writePluginDirectory(path, artifacts, time.Date(2026, 8, 3, 1, 2, 3, 0, time.UTC))
	if err != nil {
		t.Fatalf("writePluginDirectory() error = %v", err)
	}
	if backup == "" {
		t.Fatal("backup path is empty")
	}
	assertFileContent(t, filepath.Join(backup, "old"), "old")
	assertFileContains(t, filepath.Join(path, "herdr-plugin.toml"), "herdr-pal.autostart")
	launcherInfo, err := os.Stat(filepath.Join(path, "start-herdr-pal"))
	if err != nil {
		t.Fatal(err)
	}
	if launcherInfo.Mode().Perm() != 0o700 {
		t.Fatalf("launcher mode = %o", launcherInfo.Mode().Perm())
	}
	manifestInfo, err := os.Stat(filepath.Join(path, "herdr-plugin.toml"))
	if err != nil {
		t.Fatal(err)
	}
	if manifestInfo.Mode().Perm() != 0o600 {
		t.Fatalf("manifest mode = %o", manifestInfo.Mode().Perm())
	}
}

func TestDefaultPluginDirectoryLivesBesideClientConfig(t *testing.T) {
	clientPath := filepath.Join(t.TempDir(), "herdr-pal", "config.json")
	if got := DefaultPluginDirectory(clientPath); got != filepath.Join(filepath.Dir(clientPath), "plugin") {
		t.Fatalf("DefaultPluginDirectory() = %q", got)
	}
}

func TestRunHerdrPluginLinkUsesPublicCLIAndConfigEnvironment(t *testing.T) {
	directory := t.TempDir()
	capture := filepath.Join(directory, "capture.txt")
	herdrBinary := filepath.Join(directory, "herdr")
	script := "#!/bin/sh\nprintf '%s\\n%s\\n%s\\n%s\\n' \"$1\" \"$2\" \"$3\" \"$HERDR_CONFIG_PATH\" > " + shellSingleQuote(capture) + "\n"
	if err := os.WriteFile(herdrBinary, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	configPath := filepath.Join(directory, "config.toml")
	pluginPath := filepath.Join(directory, "plugin")

	if err := runHerdrPluginLink(context.Background(), herdrBinary, configPath, pluginPath); err != nil {
		t.Fatalf("runHerdrPluginLink() error = %v", err)
	}
	assertFileContent(t, capture, "plugin\nlink\n"+pluginPath+"\n"+configPath+"\n")
}
