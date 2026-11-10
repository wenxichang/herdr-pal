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
		"公开 GitHub Release 提供 Linux 和 macOS 的 Herdr Bundle 及各自校验文件",
		"Windows AMD64 客户端 Beta 需要从源码构建",
		"自动发现本机正在运行的 dsh web 与 Desktop 宿主",
		"旧 `dsh.token` 配置已移除",
	} {
		if !strings.Contains(content, expected) {
			t.Fatalf("README 缺少当前发布说明 %q", expected)
		}
	}
	for _, removed := range []string{
		"plugins/dsh-herdr-pal",
		"dsh-herdr-pal-0.1.2.tgz",
		"Node.js 22+",
	} {
		if strings.Contains(content, removed) {
			t.Fatalf("README 仍包含已移除的 dsh 插件内容 %q", removed)
		}
	}
}
