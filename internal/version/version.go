// Package version 提供构建版本信息。
package version

import "fmt"

var (
	// Version 是构建时注入的版本号。
	Version = "dev"
	// Commit 是构建时注入的 Git 提交短哈希。
	Commit = "unknown"
	// BuiltAt 是构建时注入的 UTC RFC3339 构建时间。
	BuiltAt = "unknown"
)

// String 返回适合命令行展示的完整构建信息。
func String() string {
	return fmt.Sprintf("%s commit=%s built_at=%s", Version, Commit, BuiltAt)
}
