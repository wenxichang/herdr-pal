// herdr-pal-server 是企业微信机器人与多台 Herdr Pal 客户端之间的中央 Relay。
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"syscall"

	"github.com/wenxichang/herdr-pal/internal/processlock"
	"github.com/wenxichang/herdr-pal/internal/serverapp"
	"github.com/wenxichang/herdr-pal/internal/version"
)

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	os.Exit(run(ctx, os.Args[1:], os.Stdout, os.Stderr, serverapp.Run))
}

type appExecutor func(context.Context, serverapp.Options) error

func run(ctx context.Context, args []string, stdout, stderr io.Writer, execute appExecutor) int {
	flags := flag.NewFlagSet("herdr-pal-server", flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	configPath := flags.String("config", "", "服务端 JSON 配置文件路径")
	showVersion := flags.Bool("version", false, "显示版本")
	flags.Usage = func() {
		fmt.Fprintln(stderr, "用法: herdr-pal-server -config /path/to/server.json")
		fmt.Fprintln(stderr, "      herdr-pal-server --version")
	}
	if err := flags.Parse(args); err != nil {
		fmt.Fprintln(stderr, "参数错误，请检查命令行参数。")
		flags.Usage()
		return 2
	}
	if flags.NArg() != 0 || *showVersion && *configPath != "" {
		flags.Usage()
		return 2
	}
	if *showVersion {
		fmt.Fprintln(stdout, version.String())
		return 0
	}
	if *configPath == "" {
		flags.Usage()
		return 2
	}
	err := execute(ctx, serverapp.Options{ConfigPath: *configPath, Getenv: os.Getenv, Stderr: stderr})
	if err == nil || ctx.Err() != nil && errors.Is(err, ctx.Err()) {
		return 0
	}
	if errors.Is(err, serverapp.ErrConfig) {
		fmt.Fprintln(stderr, "配置错误，请检查服务端配置文件和必填环境变量。")
		return 2
	}
	if errors.Is(err, processlock.ErrAlreadyRunning) {
		fmt.Fprintln(stderr, processlock.ErrAlreadyRunning)
		return 1
	}
	fmt.Fprintln(stderr, "Herdr Pal Server 启动或运行失败，请检查安全日志。")
	return 1
}
