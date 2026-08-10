package buildscript

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestReadmeDocumentsAuditedHerdrProtocols(t *testing.T) {
	root := repositoryRoot(t)
	content, err := os.ReadFile(filepath.Join(root, "README.md"))
	if err != nil {
		t.Fatalf("读取 README.md：%v", err)
	}
	readme := string(content)
	if !strings.Contains(readme, "已审计的 `17` 或 `19`") {
		t.Fatal("README.md 未同时说明已审计的 protocol 17 和 19")
	}
	if strings.Contains(readme, "protocol 必须为 `17`") {
		t.Fatal("README.md 仍把 protocol 17 描述为唯一兼容版本")
	}
}
