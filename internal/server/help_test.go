package server

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestFileHelpProviderReadsLatestContentEveryTime(t *testing.T) {
	path := filepath.Join(t.TempDir(), "help.md")
	if err := os.WriteFile(path, []byte("第一版帮助"), 0o600); err != nil {
		t.Fatal(err)
	}
	provider, err := NewFileHelpProvider(path)
	if err != nil {
		t.Fatal(err)
	}
	first, err := provider.Read()
	if err != nil || first != "第一版帮助" {
		t.Fatalf("first Read() = %q, %v", first, err)
	}
	if err := os.WriteFile(path, []byte("第二版帮助\n立即生效"), 0o600); err != nil {
		t.Fatal(err)
	}
	second, err := provider.Read()
	if err != nil || second != "第二版帮助\n立即生效" {
		t.Fatalf("second Read() = %q, %v", second, err)
	}
}

func TestFileHelpProviderRejectsUnavailableContent(t *testing.T) {
	directory := t.TempDir()
	tests := []struct {
		name string
		path string
		data []byte
	}{
		{name: "missing", path: filepath.Join(directory, "missing.md")},
		{name: "empty", path: filepath.Join(directory, "empty.md"), data: []byte(" \n\t")},
		{name: "oversized", path: filepath.Join(directory, "large.md"), data: []byte(strings.Repeat("x", MaxHelpFileBytes+1))},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if test.data != nil {
				if err := os.WriteFile(test.path, test.data, 0o600); err != nil {
					t.Fatal(err)
				}
			}
			provider, err := NewFileHelpProvider(test.path)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := provider.Read(); !errors.Is(err, ErrHelpUnavailable) {
				t.Fatalf("Read() error = %v", err)
			}
		})
	}
}

func TestDefaultHelpUsesSidecarBundleInstallation(t *testing.T) {
	help := DefaultHelpText()
	for _, want := range []string{
		"herdr-bundle-<版本>-<系统>-<架构>.tar.gz",
		"./install.sh",
		"/ls",
		"/N",
		"Sidecar",
		"Key 会在终端回显",
	} {
		if !strings.Contains(help, want) {
			t.Errorf("default help missing %q", want)
		}
	}
	for _, old := range []string{
		"/userid",
		"/sel",
		"Key 不会显示在终端",
		"curl -fsSL https://herdr.dev/install.sh",
		"创建 config.json",
		"herdr-pal-windows-amd64.exe",
	} {
		if strings.Contains(help, old) {
			t.Errorf("default help still contains old installation text %q", old)
		}
	}
}

func TestDefaultHelpIncludesMachineRegistration(t *testing.T) {
	content := DefaultHelpText()
	for _, fragment := range []string{"/reg", "机器标识", "来源地址", "首台", "等待管理员审批", "/help"} {
		if !strings.Contains(content, fragment) {
			t.Fatalf("default help missing %q", fragment)
		}
	}
}

func TestDefaultHelpIncludesRegistrationApprovalCommands(t *testing.T) {
	content := DefaultHelpText()
	for _, command := range []string{"/ls-reg", "/apr 1 2 3", "/rej 1 2 3", "审批后编号快照立即失效"} {
		if !strings.Contains(content, command) {
			t.Fatalf("default help missing %q", command)
		}
	}
}
