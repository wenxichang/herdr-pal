// herdr-pal 是连接 Herdr 与即时通讯平台的桥接程序。
package main

import (
	"fmt"
	"os"

	"github.com/wenxichang/herdr-pal/internal/version"
)

func main() {
	if len(os.Args) == 2 && os.Args[1] == "--version" {
		fmt.Println(version.String())
		return
	}

	fmt.Fprintln(os.Stderr, "用法: herdr-pal --version")
	os.Exit(2)
}
