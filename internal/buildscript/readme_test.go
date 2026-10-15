package buildscript

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestReadmeDocumentsSupportedHerdrProtocolRange(t *testing.T) {
	root := repositoryRoot(t)
	content, err := os.ReadFile(filepath.Join(root, "README.md"))
	if err != nil {
		t.Fatalf("读取 README.md：%v", err)
	}
	readme := string(content)
	if !strings.Contains(readme, "`17` 到 `22` 之间") {
		t.Fatal("README.md 未说明支持的 protocol 17 到 22")
	}
}
