// herdr-pal 是连接 Herdr 与即时通讯平台的桥接程序。
package main

import (
	"fmt"
	"io"
	"os"

	"github.com/wenxichang/herdr-pal/internal/version"
)

func main() {
	os.Exit(run(os.Args[1:], os.Stdout, os.Stderr))
}

func run(args []string, stdout, stderr io.Writer) int {
	if len(args) == 1 && args[0] == "--version" {
		fmt.Fprintln(stdout, version.String())
		return 0
	}

	fmt.Fprintln(stderr, "用法: herdr-pal --version")
	return 2
}
