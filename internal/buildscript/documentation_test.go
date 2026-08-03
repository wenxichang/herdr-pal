package buildscript

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestPublishedHelpMatchesEmbeddedDefault(t *testing.T) {
	root := repositoryRoot(t)
	published, err := os.ReadFile(filepath.Join(root, "help.md"))
	if err != nil {
		t.Fatal(err)
	}
	embedded, err := os.ReadFile(filepath.Join(root, "internal", "server", "default_help.md"))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(published, embedded) {
		t.Fatal("根目录 help.md 与服务端内嵌默认帮助不一致")
	}
}

func TestREADMEReflectsCurrentInstallerAndReleasePolicy(t *testing.T) {
	readme, err := os.ReadFile(filepath.Join(repositoryRoot(t), "README.md"))
	if err != nil {
		t.Fatal(err)
	}
	content := string(readme)
	if strings.Contains(content, "Key 输入不会回显") {
		t.Fatal("README 仍声明安装器不会回显机器 Key")
	}
	for _, expected := range []string{
		"Key 会在终端回显",
		"Startup 插件",
		"herdr-pal start",
		"~/Library/Logs/herdr-pal/herdr-pal.log",
		"~/.local/state/herdr-pal/herdr-pal.log",
		"公开 GitHub Release 当前只提供 Linux 和 macOS 的 Herdr Bundle",
		"Windows AMD64 客户端 Beta 需要从源码构建",
	} {
		if !strings.Contains(content, expected) {
			t.Fatalf("README 缺少当前发布说明 %q", expected)
		}
	}
	if strings.Contains(content, "通过 `[[sidecar]]`") {
		t.Fatal("README 仍把 Sidecar 描述为当前安装机制")
	}
}
