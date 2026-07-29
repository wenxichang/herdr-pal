package main

import (
	"context"
	"errors"
	"io"
	"strings"

	"github.com/spf13/cobra"
	"github.com/wenxichang/herdr-pal/internal/adminclient"
	"github.com/wenxichang/herdr-pal/internal/config"
	"github.com/wenxichang/herdr-pal/internal/version"
)

type commandState struct {
	ctx        context.Context
	configPath string
	jsonOutput bool
	stdout     io.Writer
	stderr     io.Writer
	execute    executor
}

type cliError struct {
	code   int
	prefix string
	cause  error
}

func (err *cliError) Error() string {
	if err == nil || err.cause == nil {
		return "hp-cli 执行失败"
	}
	return err.cause.Error()
}

func newRootCommand(state *commandState) *cobra.Command {
	root := &cobra.Command{
		Use:           "hp-cli",
		Short:         "管理本机运行中的 Herdr Pal Server",
		Long:          "通过本机 HPAP Admin Socket 管理运行中的 Herdr Pal Server、机器 Key、Pal 连接和 Agent 会话。",
		Version:       version.String(),
		SilenceErrors: true,
		SilenceUsage:  true,
		Example: `  hp-cli server status
  hp-cli key issue --principal-id user-a --machine-id office-pc --source 192.168.1.20
  hp-cli session list --principal-id user-a`,
	}
	root.SetOut(state.stdout)
	root.SetErr(state.stderr)
	root.SetVersionTemplate("{{.Version}}\n")
	root.CompletionOptions.DisableDefaultCmd = true
	root.PersistentFlags().StringVar(&state.configPath, "config", "", "服务端配置文件路径（兼容 -config）")
	root.PersistentFlags().BoolVar(&state.jsonOutput, "json", false, "使用 JSON 输出命令结果")
	root.AddCommand(newServerCommand(state), newKeyCommand(state), newConnectionCommand(state), newSessionCommand(state))
	root.SetHelpFunc(renderCommandHelp)
	return root
}

func normalizeLegacyArgs(args []string) []string {
	normalized := append([]string(nil), args...)
	for index, argument := range normalized {
		switch {
		case argument == "-config":
			normalized[index] = "--config"
		case strings.HasPrefix(argument, "-config="):
			normalized[index] = "--config=" + strings.TrimPrefix(argument, "-config=")
		case argument == "-version":
			normalized[index] = "--version"
		}
	}
	return normalized
}

func withCommandHelp(command *cobra.Command, long, example string) *cobra.Command {
	command.Long = long
	command.Example = example
	return command
}

func newCommandGroup(use, short string) *cobra.Command {
	return &cobra.Command{
		Use: use, Short: short, Args: cobra.NoArgs,
		RunE: func(command *cobra.Command, _ []string) error {
			return command.Help()
		},
	}
}

func runInvocation(command *cobra.Command, state *commandState, invocation Invocation) error {
	configPath := strings.TrimSpace(state.configPath)
	if configPath == "" {
		resolved, err := config.DefaultServerPath()
		if err != nil {
			return newCLIError(2, "配置错误：", err)
		}
		configPath = resolved
	}
	if state.execute == nil {
		return newCLIError(3, "Admin Socket 请求失败：", errors.New("管理执行器不可用"))
	}
	result, err := state.execute(state.ctx, configPath, invocation)
	if err != nil {
		return classifyCLIError(err)
	}
	if state.jsonOutput {
		err = adminclient.FormatJSON(command.OutOrStdout(), result)
	} else {
		err = adminclient.FormatHuman(command.OutOrStdout(), invocation.Method, result)
	}
	if err != nil {
		return newCLIError(3, "输出失败：", err)
	}
	return nil
}

func newCLIError(code int, prefix string, cause error) *cliError {
	return &cliError{code: code, prefix: prefix, cause: cause}
}

func classifyCLIError(err error) error {
	err = classifyExecutionError(err)
	var serverError *adminclient.ServerError
	switch {
	case errors.As(err, &serverError):
		return newCLIError(1, "请求失败：", err)
	case errors.Is(err, errLocalConfig), errors.Is(err, adminclient.ErrConfig):
		return newCLIError(2, "配置错误：", err)
	default:
		return newCLIError(3, "Admin Socket 请求失败：", err)
	}
}
