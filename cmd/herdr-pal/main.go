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
	"strings"
	"syscall"

	"github.com/wenxichang/herdr-pal/internal/app"
	"github.com/wenxichang/herdr-pal/internal/config"
	"github.com/wenxichang/herdr-pal/internal/installer"
	"github.com/wenxichang/herdr-pal/internal/lifecycle"
	"github.com/wenxichang/herdr-pal/internal/processlock"
	"github.com/wenxichang/herdr-pal/internal/version"
)

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	os.Exit(run(ctx, os.Args[1:], os.Stdin, os.Stdout, os.Stderr, app.RunCLI))
}

type appExecutor func(context.Context, app.Options) error

type lifecycleCommandOptions struct {
	ConfigPath string
	SocketPath string
	Stdin      io.Reader
	Stdout     io.Writer
	Stderr     io.Writer
}

type lifecycleExecutor func(context.Context, lifecycleCommandOptions) error

type commandExecutors struct {
	app       appExecutor
	setup     setupExecutor
	start     lifecycleExecutor
	supervise lifecycleExecutor
	worker    lifecycleExecutor
}

func run(ctx context.Context, args []string, stdin io.Reader, stdout, stderr io.Writer, execute appExecutor) int {
	return runWithExecutors(ctx, args, stdin, stdout, stderr, execute, installer.Apply)
}

func runWithExecutors(ctx context.Context, args []string, stdin io.Reader, stdout, stderr io.Writer, execute appExecutor, setup setupExecutor) int {
	return runWithCommandExecutors(ctx, args, stdin, stdout, stderr, commandExecutors{
		app: execute, setup: setup,
		start: runLifecycleStart, supervise: runLifecycleSupervise, worker: runLifecycleWorker,
	})
}

func runWithCommandExecutors(ctx context.Context, args []string, stdin io.Reader, stdout, stderr io.Writer, executors commandExecutors) int {
	if len(args) > 0 && args[0] == "setup" {
		return runSetup(ctx, args[1:], stdin, stdout, stderr, executors.setup)
	}
	if len(args) > 0 {
		switch args[0] {
		case "start":
			return runLifecycleCLI(ctx, "start", args[1:], stdin, stdout, stderr, executors.start, false, true)
		case "__supervise":
			return runLifecycleCLI(ctx, "__supervise", args[1:], stdin, stdout, stderr, executors.supervise, true, false)
		case "__worker":
			return runLifecycleCLI(ctx, "__worker", args[1:], stdin, stdout, stderr, executors.worker, false, false)
		}
	}
	return runApplicationCLI(ctx, args, stdin, stdout, stderr, executors.app)
}

func runApplicationCLI(ctx context.Context, args []string, stdin io.Reader, stdout, stderr io.Writer, execute appExecutor) int {
	flags := flag.NewFlagSet("herdr-pal", flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	interactiveMode := flags.Bool("i", false, "进入本地交互模式")
	configPath := flags.String("config", "", "本地 JSON 配置文件路径")
	showVersion := flags.Bool("version", false, "显示版本")
	flags.Usage = func() {
		fmt.Fprintln(stderr, "用法: herdr-pal -i [-config /path/to/config.json]")
		fmt.Fprintln(stderr, "      herdr-pal [-config /path/to/config.json]")
		fmt.Fprintln(stderr, "      herdr-pal start [-config /path/to/config.json]")
		fmt.Fprintln(stderr, "      herdr-pal --version")
	}
	if err := flags.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		fmt.Fprintln(stderr, "参数错误，请检查命令行参数。")
		flags.Usage()
		return 2
	}
	if flags.NArg() != 0 {
		flags.Usage()
		return 2
	}
	if *interactiveMode && *showVersion {
		flags.Usage()
		return 2
	}
	if *showVersion {
		fmt.Fprintln(stdout, version.String())
		return 0
	}
	resolvedConfigPath := *configPath
	if !*interactiveMode && resolvedConfigPath == "" {
		var err error
		resolvedConfigPath, err = config.DefaultClientPath()
		if err != nil {
			fmt.Fprintln(stderr, "无法确定默认配置文件路径，请显式指定 -config。")
			return 2
		}
	}

	err := execute(ctx, app.Options{
		Interactive: *interactiveMode,
		ConfigPath:  resolvedConfigPath,
		Stdin:       stdin,
		Getenv:      os.Getenv,
		Stdout:      stdout,
		Stderr:      stderr,
	})
	if err == nil || ctx.Err() != nil && errors.Is(err, ctx.Err()) {
		return 0
	}
	if errors.Is(err, app.ErrConfig) {
		if detail, ok := app.ConfigErrorDetail(err); ok {
			if resolvedConfigPath == "" {
				fmt.Fprintf(stderr, "配置错误：%s\n", detail)
			} else {
				fmt.Fprintf(stderr, "配置错误（%s）：%s\n", resolvedConfigPath, detail)
			}
		} else {
			fmt.Fprintln(stderr, "配置错误，请检查配置文件和必填环境变量。")
		}
		return 2
	}
	if errors.Is(err, processlock.ErrAlreadyRunning) {
		fmt.Fprintln(stderr, processlock.ErrAlreadyRunning)
		return 1
	}
	fmt.Fprintln(stderr, "Herdr Pal 启动或运行失败，请检查 Herdr 状态和安全日志。")
	return 1
}

func runLifecycleCLI(
	ctx context.Context,
	name string,
	args []string,
	stdin io.Reader,
	stdout, stderr io.Writer,
	execute lifecycleExecutor,
	requireSocket, public bool,
) int {
	flags := flag.NewFlagSet("herdr-pal "+name, flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	configPath := flags.String("config", "", "Herdr Pal JSON 配置路径")
	var socketPath *string
	if requireSocket {
		socketPath = flags.String("socket", "", "Herdr 公共 Socket 路径")
	}
	flags.Usage = func() {
		if public {
			fmt.Fprintln(stderr, "用法: herdr-pal start [-config /path/to/config.json]")
		}
	}
	if err := flags.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		fmt.Fprintln(stderr, "Pal 生命周期命令参数错误。")
		flags.Usage()
		return 2
	}
	if flags.NArg() != 0 || requireSocket && strings.TrimSpace(*socketPath) == "" {
		fmt.Fprintln(stderr, "Pal 生命周期命令参数错误。")
		flags.Usage()
		return 2
	}
	if strings.TrimSpace(*configPath) == "" {
		var err error
		*configPath, err = config.DefaultClientPath()
		if err != nil {
			fmt.Fprintln(stderr, "无法确定默认配置文件路径，请显式指定 -config。")
			return 2
		}
	}
	if execute == nil {
		fmt.Fprintln(stderr, "Pal 生命周期执行器无效。")
		return 1
	}
	options := lifecycleCommandOptions{
		ConfigPath: *configPath,
		Stdin:      stdin,
		Stdout:     stdout,
		Stderr:     stderr,
	}
	if socketPath != nil {
		options.SocketPath = strings.TrimSpace(*socketPath)
	}
	err := execute(ctx, options)
	if err == nil || ctx.Err() != nil && errors.Is(err, ctx.Err()) {
		return 0
	}
	if errors.Is(err, app.ErrConfig) {
		if detail, ok := app.ConfigErrorDetail(err); ok {
			fmt.Fprintf(stderr, "配置错误（%s）：%s\n", options.ConfigPath, detail)
		} else {
			fmt.Fprintln(stderr, "配置错误，请检查配置文件。")
		}
		return 2
	}
	if errors.Is(err, lifecycle.ErrUnsupported) {
		fmt.Fprintln(stderr, lifecycle.ErrUnsupported)
		return 2
	}
	if errors.Is(err, processlock.ErrAlreadyRunning) {
		fmt.Fprintln(stderr, processlock.ErrAlreadyRunning)
		return 1
	}
	if public {
		fmt.Fprintln(stderr, "Pal 自动启动失败，请查看后台日志。")
	} else {
		fmt.Fprintln(stderr, "Pal 托管进程运行失败，请查看后台日志。")
	}
	return 1
}
