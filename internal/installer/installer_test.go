package installer

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestApplyWritesValidatedClientAndHerdrConfigs(t *testing.T) {
	directory := t.TempDir()
	clientPath := filepath.Join(directory, "pal", "config.json")
	herdrPath := filepath.Join(directory, "herdr", "config.toml")
	herdrBinary := filepath.Join(directory, "bin", "herdr")
	palBinary := filepath.Join(directory, "bin", "herdr-pal")
	pluginPath := filepath.Join(directory, "pal", "plugin")
	if err := os.MkdirAll(filepath.Dir(herdrPath), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(herdrPath, []byte("onboarding = false\n\n"+managedSidecarBlock), 0o600); err != nil {
		t.Fatal(err)
	}
	checked := false
	linked := false

	result, err := Apply(context.Background(), Request{
		ClientConfigPath: clientPath,
		HerdrConfigPath:  herdrPath,
		HerdrBinaryPath:  herdrBinary,
		PalBinaryPath:    palBinary,
		PluginDirectory:  pluginPath,
		PluginVersion:    "v0.6.0",
		RelayURL:         "wss://relay.example/hprp",
		RelayKey:         validMachineKey,
	}, Options{
		Now: func() time.Time { return time.Date(2026, 7, 31, 1, 2, 3, 0, time.UTC) },
		CheckHerdrConfig: func(_ context.Context, binaryPath, targetPath string, candidate []byte) error {
			checked = true
			if binaryPath != herdrBinary || targetPath != herdrPath {
				t.Fatalf("check args = %q, %q", binaryPath, targetPath)
			}
			if strings.Contains(string(candidate), managedSidecarBegin) || !strings.Contains(string(candidate), "onboarding = false") {
				t.Fatalf("candidate was not migrated:\n%s", candidate)
			}
			return nil
		},
		LinkPlugin: func(_ context.Context, binaryPath, configPath, directory string) error {
			linked = true
			if binaryPath != herdrBinary || configPath != herdrPath || directory != pluginPath {
				t.Fatalf("link args = %q, %q, %q", binaryPath, configPath, directory)
			}
			assertFileContains(t, filepath.Join(directory, "herdr-plugin.toml"), "herdr-pal.autostart")
			assertFileNotContains(t, herdrPath, managedSidecarBegin)
			return nil
		},
	})

	if err != nil {
		t.Fatal(err)
	}
	if !checked || !linked {
		t.Fatalf("checked=%t linked=%t", checked, linked)
	}
	if result.ClientBackupPath != "" || result.HerdrBackupPath == "" || result.PluginBackupPath != "" || result.PluginDirectory != pluginPath {
		t.Fatalf("unexpected backups: %+v", result)
	}
	assertFileContains(t, clientPath, `"url": "wss://relay.example/hprp"`)
	assertFileNotContains(t, herdrPath, managedSidecarBegin)
	assertFileContains(t, filepath.Join(pluginPath, "start-herdr-pal"), palBinary)
}

func TestApplyDoesNotModifyFilesWhenHerdrValidationFails(t *testing.T) {
	directory := t.TempDir()
	clientPath := filepath.Join(directory, "config.json")
	herdrPath := filepath.Join(directory, "config.toml")
	if err := os.WriteFile(clientPath, []byte(existingClientConfig), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(herdrPath, []byte("onboarding = false\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	sentinel := errors.New("invalid herdr config")

	_, err := Apply(context.Background(), Request{
		ClientConfigPath: clientPath,
		HerdrConfigPath:  herdrPath,
		HerdrBinaryPath:  "/tmp/herdr",
		PalBinaryPath:    "/tmp/herdr-pal",
		PluginDirectory:  filepath.Join(directory, "plugin"),
		PluginVersion:    "v0.6.0",
		RelayURL:         "wss://relay.example/hprp",
		RelayKey:         validMachineKey,
	}, Options{
		CheckHerdrConfig: func(context.Context, string, string, []byte) error { return sentinel },
		LinkPlugin:       func(context.Context, string, string, string) error { return nil },
	})

	if !errors.Is(err, sentinel) {
		t.Fatalf("Apply() error = %v, want sentinel", err)
	}
	assertFileContent(t, clientPath, existingClientConfig)
	assertFileContent(t, herdrPath, "onboarding = false\n")
}

func TestApplyRestoresHerdrConfigWhenClientWriteFails(t *testing.T) {
	directory := t.TempDir()
	clientDirectory := filepath.Join(directory, "pal")
	clientPath := filepath.Join(clientDirectory, "config.json")
	herdrPath := filepath.Join(directory, "herdr", "config.toml")
	if err := os.MkdirAll(clientDirectory, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(herdrPath), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(herdrPath, []byte("onboarding = false\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	_, err := Apply(context.Background(), Request{
		ClientConfigPath: clientPath,
		HerdrConfigPath:  herdrPath,
		HerdrBinaryPath:  "/tmp/herdr",
		PalBinaryPath:    "/tmp/herdr-pal",
		PluginDirectory:  filepath.Join(directory, "plugin"),
		PluginVersion:    "v0.6.0",
		RelayURL:         "wss://relay.example/hprp",
		RelayKey:         validMachineKey,
	}, Options{
		CheckHerdrConfig: func(context.Context, string, string, []byte) error {
			if err := os.Remove(clientDirectory); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(clientDirectory, []byte("not a directory"), 0o600); err != nil {
				t.Fatal(err)
			}
			return nil
		},
		LinkPlugin: func(context.Context, string, string, string) error { return nil },
	})

	if err == nil {
		t.Fatal("Apply() should fail")
	}
	assertFileContent(t, herdrPath, "onboarding = false\n")
}

func TestApplyErrorDoesNotExposeMachineKey(t *testing.T) {
	secretKey := "hpk_9_BBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBB"
	_, err := Apply(context.Background(), Request{
		ClientConfigPath: filepath.Join(t.TempDir(), "config.json"),
		HerdrConfigPath:  filepath.Join(t.TempDir(), "config.toml"),
		HerdrBinaryPath:  "/tmp/herdr",
		PalBinaryPath:    "/tmp/herdr-pal",
		PluginDirectory:  filepath.Join(t.TempDir(), "plugin"),
		PluginVersion:    "v0.6.0",
		RelayURL:         "wss://relay.example/hprp",
		RelayKey:         secretKey,
	}, Options{
		CheckHerdrConfig: func(context.Context, string, string, []byte) error {
			return errors.New("check failed")
		},
		LinkPlugin: func(context.Context, string, string, string) error { return nil },
	})
	if err == nil {
		t.Fatal("Apply() should fail")
	}
	if strings.Contains(err.Error(), secretKey) {
		t.Fatalf("error exposes key: %v", err)
	}
}

func TestApplyRollsBackConfigsAndPluginWhenLinkFails(t *testing.T) {
	directory := t.TempDir()
	clientPath := filepath.Join(directory, "pal", "config.json")
	herdrPath := filepath.Join(directory, "herdr", "config.toml")
	pluginPath := filepath.Join(directory, "pal", "plugin")
	if err := os.MkdirAll(filepath.Dir(clientPath), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(herdrPath), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(pluginPath, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(clientPath, []byte(existingClientConfig), 0o600); err != nil {
		t.Fatal(err)
	}
	originalHerdr := "onboarding = false\n\n" + managedSidecarBlock
	if err := os.WriteFile(herdrPath, []byte(originalHerdr), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(pluginPath, "old"), []byte("old plugin"), 0o600); err != nil {
		t.Fatal(err)
	}
	sentinel := errors.New("plugin link failed")

	_, err := Apply(context.Background(), Request{
		ClientConfigPath: clientPath,
		HerdrConfigPath:  herdrPath,
		HerdrBinaryPath:  "/tmp/herdr",
		PalBinaryPath:    "/tmp/herdr-pal",
		PluginDirectory:  pluginPath,
		PluginVersion:    "v0.6.0",
		RelayURL:         "wss://relay.example/hprp",
		RelayKey:         validMachineKey,
	}, Options{
		CheckHerdrConfig: func(context.Context, string, string, []byte) error { return nil },
		LinkPlugin: func(context.Context, string, string, string) error {
			assertFileNotContains(t, herdrPath, managedSidecarBegin)
			assertFileContains(t, filepath.Join(pluginPath, "herdr-plugin.toml"), "herdr-pal.autostart")
			return sentinel
		},
	})

	if !errors.Is(err, sentinel) {
		t.Fatalf("Apply() error = %v", err)
	}
	assertFileContent(t, clientPath, existingClientConfig)
	assertFileContent(t, herdrPath, originalHerdr)
	assertFileContent(t, filepath.Join(pluginPath, "old"), "old plugin")
}

const existingClientConfig = `{
  "relay": {
    "url": "wss://old.example/hprp",
    "key": "hpk_1_AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA",
    "skip_verify": true
  },
  "herdr": {},
  "log": {"level": "info"}
}
`

func assertFileContains(t *testing.T, path, want string) {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), want) {
		t.Fatalf("%s missing %q:\n%s", path, want, data)
	}
}

func assertFileNotContains(t *testing.T, path, forbidden string) {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(data), forbidden) {
		t.Fatalf("%s contains %q:\n%s", path, forbidden, data)
	}
}
