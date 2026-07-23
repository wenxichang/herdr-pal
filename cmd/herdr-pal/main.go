// herdr-pal 是连接 Herdr 与即时通讯平台的桥接程序。
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

	"github.com/wenxichang/herdr-pal/internal/app"
	"github.com/wenxichang/herdr-pal/internal/processlock"
	"github.com/wenxichang/herdr-pal/internal/version"
)

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	os.Exit(run(ctx, os.Args[1:], os.Stdout, os.Stderr, app.Run))
}

type appExecutor func(context.Context, app.Options) error

func run(ctx context.Context, args []string, stdout, stderr io.Writer, execute appExecutor) int {
	flags := flag.NewFlagSet("herdr-pal", flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	configPath := flags.String("config", "", "本地 JSON 配置文件路径")
	showVersion := flags.Bool("version", false, "显示版本")
	flags.Usage = func() {
		fmt.Fprintln(stderr, "用法: herdr-pal -config /path/to/config.json")
		fmt.Fprintln(stderr, "       herdr-pal --version")
	}
	if err := flags.Parse(args); err != nil {
		fmt.Fprintln(stderr, "参数错误，请使用 -config 或 --version。")
		flags.Usage()
		return 2
	}
	if flags.NArg() != 0 {
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

	err := execute(ctx, app.Options{
		ConfigPath: *configPath,
		Getenv:     os.Getenv,
		Stdout:     stdout,
		Stderr:     stderr,
	})
	if err == nil || ctx.Err() != nil && errors.Is(err, ctx.Err()) {
		return 0
	}
	if errors.Is(err, app.ErrConfig) {
		fmt.Fprintln(stderr, "配置错误，请检查配置文件和必填环境变量。")
		return 2
	}
	if errors.Is(err, processlock.ErrAlreadyRunning) {
		fmt.Fprintln(stderr, processlock.ErrAlreadyRunning)
		return 1
	}
	fmt.Fprintln(stderr, "Herdr Pal 启动或运行失败，请检查 Herdr 状态和安全日志。")
	return 1
}
