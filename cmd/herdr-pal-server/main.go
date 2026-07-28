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
	"strings"
	"syscall"

	"github.com/wenxichang/herdr-pal/internal/config"
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
	if len(args) > 0 && args[0] == "key" {
		fmt.Fprintln(stderr, "herdr-pal-server 已不再提供 Key 管理命令，请使用 hp-cli key issue。")
		return 2
	}
	flags := flag.NewFlagSet("herdr-pal-server", flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	configPath := flags.String("config", "", "服务端 JSON 配置文件路径")
	verbose := flags.Bool("verbose", false, "输出详细调试日志")
	showVersion := flags.Bool("version", false, "显示版本")
	flags.Usage = func() {
		fmt.Fprintln(stderr, "用法: herdr-pal-server [-config /path/to/server.json] [--verbose]")
		fmt.Fprintln(stderr, "      herdr-pal-server --version")
	}
	if err := flags.Parse(args); err != nil {
		fmt.Fprintln(stderr, "参数错误，请检查命令行参数。")
		flags.Usage()
		return 2
	}
	if flags.NArg() != 0 || *showVersion && (*configPath != "" || *verbose) {
		flags.Usage()
		return 2
	}
	if *showVersion {
		fmt.Fprintln(stdout, version.String())
		return 0
	}
	resolvedConfigPath := *configPath
	if resolvedConfigPath == "" {
		var err error
		resolvedConfigPath, err = config.DefaultServerPath()
		if err != nil {
			fmt.Fprintln(stderr, "无法确定默认配置文件路径，请显式指定 -config。")
			return 2
		}
	}
	err := execute(ctx, serverapp.Options{ConfigPath: resolvedConfigPath, Getenv: os.Getenv, Stderr: stderr, Verbose: *verbose})
	return finishRun(ctx, err, resolvedConfigPath, stderr)
}

func finishRun(ctx context.Context, err error, resolvedConfigPath string, stderr io.Writer) int {
	if err == nil || ctx.Err() != nil && errors.Is(err, ctx.Err()) {
		return 0
	}
	if errors.Is(err, serverapp.ErrConfig) {
		detail := strings.TrimPrefix(safeServerError(err), serverapp.ErrConfig.Error()+": ")
		fmt.Fprintf(stderr, "配置错误（%s）：%s\n", resolvedConfigPath, detail)
		return 2
	}
	if errors.Is(err, processlock.ErrAlreadyRunning) {
		fmt.Fprintln(stderr, processlock.ErrAlreadyRunning)
		return 1
	}
	fmt.Fprintf(stderr, "Herdr Pal Server 启动或运行失败：%s\n", safeServerError(err))
	return 1
}

func safeServerError(err error) string {
	message := err.Error()
	secret := os.Getenv(config.SecretEnvName)
	if strings.TrimSpace(secret) != "" {
		message = strings.ReplaceAll(message, secret, "[REDACTED]")
	}
	return message
}
