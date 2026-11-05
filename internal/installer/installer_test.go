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
	checked := false

	result, err := Apply(context.Background(), Request{
		ClientConfigPath: clientPath,
		HerdrConfigPath:  herdrPath,
		HerdrBinaryPath:  herdrBinary,
		RelayURL:         "wss://relay.example/hprp",
		RelayKey:         validMachineKey,
		StartupMode:      "sidecar",
	}, Options{
		Now: func() time.Time { return time.Date(2026, 7, 31, 1, 2, 3, 0, time.UTC) },
		CheckHerdrConfig: func(_ context.Context, binaryPath, targetPath string, candidate []byte) error {
			checked = true
			if binaryPath != herdrBinary || targetPath != herdrPath {
				t.Fatalf("check args = %q, %q", binaryPath, targetPath)
			}
			if !strings.Contains(string(candidate), managedSidecarBlock) {
				t.Fatalf("candidate missing sidecar:\n%s", candidate)
			}
			return nil
		},
	})

	if err != nil {
		t.Fatal(err)
	}
	if !checked {
		t.Fatal("Herdr config was not checked")
	}
	if result.ClientBackupPath != "" || result.HerdrBackupPath != "" {
		t.Fatalf("unexpected backups: %+v", result)
	}
	assertFileContains(t, clientPath, `"url": "wss://relay.example/hprp"`)
	assertFileContains(t, herdrPath, managedSidecarBlock)
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
		RelayURL:         "wss://relay.example/hprp",
		RelayKey:         validMachineKey,
	}, Options{CheckHerdrConfig: func(context.Context, string, string, []byte) error { return sentinel }})

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
		RelayURL:         "wss://relay.example/hprp",
		RelayKey:         validMachineKey,
	}, Options{CheckHerdrConfig: func(context.Context, string, string, []byte) error {
		if err := os.Remove(clientDirectory); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(clientDirectory, []byte("not a directory"), 0o600); err != nil {
			t.Fatal(err)
		}
		return nil
	}})

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
		RelayURL:         "wss://relay.example/hprp",
		RelayKey:         secretKey,
	}, Options{CheckHerdrConfig: func(context.Context, string, string, []byte) error {
		return errors.New("check failed")
	}})
	if err == nil {
		t.Fatal("Apply() should fail")
	}
	if strings.Contains(err.Error(), secretKey) {
		t.Fatalf("error exposes key: %v", err)
	}
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

func TestApplyManualStartupMigratesManagedSidecarAndBacksUp(t *testing.T) {
	directory := t.TempDir()
	request := Request{
		ClientConfigPath: filepath.Join(directory, "pal.json"),
		HerdrConfigPath:  filepath.Join(directory, "herdr.toml"),
		HerdrBinaryPath:  "/test/herdr", RelayURL: "wss://relay.example/hprp", RelayKey: validMachineKey,
	}
	existing := "onboarding = false\n\n" + managedSidecarBlock
	if err := os.WriteFile(request.HerdrConfigPath, []byte(existing), 0o600); err != nil {
		t.Fatal(err)
	}
	options := Options{CheckHerdrConfig: func(_ context.Context, _, _ string, candidate []byte) error {
		if string(candidate) != "onboarding = false\n\n" {
			t.Fatalf("迁移候选配置错误: %q", candidate)
		}
		return nil
	}}
	result, err := Apply(context.Background(), request, options)
	if err != nil {
		t.Fatal(err)
	}
	assertFileContent(t, result.HerdrBackupPath, existing)
	assertFileContent(t, request.HerdrConfigPath, "onboarding = false\n\n")
	if _, err := Apply(context.Background(), request, options); err != nil {
		t.Fatalf("重复安装失败: %v", err)
	}
	assertFileContent(t, request.HerdrConfigPath, "onboarding = false\n\n")
}
