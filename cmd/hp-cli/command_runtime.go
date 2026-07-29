package main

import (
	"errors"
	"strings"

	"github.com/spf13/cobra"
	"github.com/wenxichang/herdr-pal/internal/adminproto"
)

func newServerCommand(state *commandState) *cobra.Command {
	command := withCommandHelp(
		newCommandGroup("server", "查看或控制服务端运行状态"),
		"查看服务端、企业微信、TLS、HPAP/HPRP、连接和会话运行状态，或执行调试开关与优雅停止。",
		"  hp-cli server status\n  hp-cli server debug enable\n  hp-cli server stop",
	)
	debug := withCommandHelp(
		newCommandGroup("debug", "动态调整服务端调试日志"),
		"在不重启服务端的情况下启用或关闭详细调试日志。该设置只影响当前服务端进程。",
		"  hp-cli server debug enable\n  hp-cli server debug disable",
	)
	debug.AddCommand(
		withCommandHelp(
			newEmptyInvocationCommand(state, "enable", "启用服务端调试日志", adminproto.MethodServerDebugEnable),
			"立即启用当前服务端进程的详细调试日志，不修改配置文件。",
			"  hp-cli server debug enable",
		),
		withCommandHelp(
			newEmptyInvocationCommand(state, "disable", "关闭服务端调试日志", adminproto.MethodServerDebugDisable),
			"关闭动态调试日志，恢复配置文件指定的基础日志级别。",
			"  hp-cli server debug disable",
		),
	)
	command.AddCommand(
		withCommandHelp(
			newEmptyInvocationCommand(state, "status", "查看服务端运行状态", adminproto.MethodServerStatus),
			"显示版本、运行时间、监听地址、Admin Socket、TLS、企业微信、Key、连接和会话摘要。",
			"  hp-cli server status\n  hp-cli server status --json",
		),
		withCommandHelp(
			newEmptyInvocationCommand(state, "stop", "优雅停止服务端", adminproto.MethodServerStop),
			"请求服务端停止接收新连接，断开现有 Pal 和企业微信连接后退出。",
			"  hp-cli server stop",
		),
		debug,
	)
	return command
}

func newEmptyInvocationCommand(state *commandState, use, short string, method adminproto.Method) *cobra.Command {
	return &cobra.Command{
		Use:   use,
		Short: short,
		Args:  cobra.NoArgs,
		RunE: func(command *cobra.Command, _ []string) error {
			return runInvocation(command, state, Invocation{Method: method, Params: adminproto.EmptyParams{}})
		},
	}
}

func newConnectionCommand(state *commandState) *cobra.Command {
	command := withCommandHelp(
		newCommandGroup("connection", "查看或断开在线 Pal 连接"),
		"查询当前在线 Pal 的身份、来源、版本和快照状态，或只断开某条当前连接。断开不会禁用 Key。",
		"  hp-cli connection list\n  hp-cli connection show CONNECTION_ID\n  hp-cli connection disconnect CONNECTION_ID",
	)
	command.AddCommand(
		withCommandHelp(&cobra.Command{
			Use: "list", Short: "列出在线 Pal 连接", Args: cobra.NoArgs,
			RunE: func(command *cobra.Command, _ []string) error {
				return runInvocation(command, state, Invocation{Method: adminproto.MethodConnectionList, Params: adminproto.ConnectionListParams{}})
			},
		}, "列出当前完成 HPRP 握手的 Pal 连接，不包含机器 Key 或终端内容。", "  hp-cli connection list\n  hp-cli connection list --json"),
		withCommandHelp(
			newConnectionIDCommand(state, "show <connection-id>", "查看一条在线连接", adminproto.MethodConnectionShow),
			"按完整 connection ID 查看一条在线 Pal 连接的身份、来源、实现版本、心跳和快照状态。",
			"  hp-cli connection show CONNECTION_ID",
		),
		withCommandHelp(
			newConnectionIDCommand(state, "disconnect <connection-id>", "断开一条在线连接", adminproto.MethodConnectionDisconnect),
			"断开指定在线连接，但不禁用其 Key；Pal 可以随后自动重新连接。",
			"  hp-cli connection disconnect CONNECTION_ID",
		),
	)
	return command
}

func newConnectionIDCommand(state *commandState, use, short string, method adminproto.Method) *cobra.Command {
	return &cobra.Command{
		Use: use, Short: short, Args: cobra.ExactArgs(1),
		RunE: func(command *cobra.Command, args []string) error {
			connectionID := strings.TrimSpace(args[0])
			if connectionID == "" {
				return errors.New("connection ID 不能为空")
			}
			return runInvocation(command, state, Invocation{Method: method, Params: adminproto.ConnectionIDParams{ConnectionID: connectionID}})
		},
	}
}

func newSessionCommand(state *commandState) *cobra.Command {
	command := withCommandHelp(
		newCommandGroup("session", "查看在线 Agent 会话"),
		"查询所有在线 Pal 最新快照中的 Agent 会话，可按企业微信用户或机器标识过滤。",
		"  hp-cli session list\n  hp-cli session list --principal-id user-a --machine-id office-pc",
	)
	var principalID string
	var machineID string
	list := withCommandHelp(&cobra.Command{
		Use: "list [--principal-id <userid>] [--machine-id <machine>]", Short: "列出在线 Agent 会话", Args: cobra.NoArgs,
		RunE: func(command *cobra.Command, _ []string) error {
			return runInvocation(command, state, Invocation{Method: adminproto.MethodSessionList, Params: adminproto.SessionListParams{
				PrincipalID: principalID, MachineID: machineID,
			}})
		},
	}, "列出在线 Agent 的稳定目标、工作区、Agent 名称和状态。结果不包含终端输出。", "  hp-cli session list\n  hp-cli session list --principal-id user-a\n  hp-cli session list --machine-id office-pc --json")
	list.Flags().StringVar(&principalID, "principal-id", "", "只显示指定企业微信用户的会话")
	list.Flags().StringVar(&machineID, "machine-id", "", "只显示指定机器的会话")
	command.AddCommand(list)
	return command
}
