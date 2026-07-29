package main

import (
	"errors"
	"strings"
	"time"

	"github.com/spf13/cobra"
	"github.com/wenxichang/herdr-pal/internal/adminproto"
)

func newKeyCommand(state *commandState) *cobra.Command {
	command := withCommandHelp(
		newCommandGroup("key", "管理 Pal 机器 Key"),
		"签发、查看、启用、禁用和删除每机独立 Key，并维护 Key 允许连接的来源地址规则。",
		"  hp-cli key list\n  hp-cli key issue --principal-id user-a --machine-id office-pc --source 192.168.1.20\n  hp-cli key disable 1",
	)
	command.AddCommand(
		newKeyIssueCommand(state),
		newKeyListCommand(state),
		withCommandHelp(newCredentialIDCommand(state, "show <credential-id>", "查看一把 Key", adminproto.MethodKeyShow), "按 credential ID 查看 Key 的绑定身份、状态、来源和到期时间，不显示明文 Key。", "  hp-cli key show 1"),
		withCommandHelp(newCredentialIDCommand(state, "enable <credential-id>", "启用一把 Key", adminproto.MethodKeyEnable), "启用指定 Key，允许符合来源规则的 Pal 再次连接。", "  hp-cli key enable 1"),
		withCommandHelp(newCredentialIDCommand(state, "disable <credential-id>", "禁用一把 Key", adminproto.MethodKeyDisable), "禁用指定 Key，并立即断开使用该 Key 的在线 Pal。", "  hp-cli key disable 1"),
		newKeyDeleteCommand(state),
		newKeySourceCommand(state),
	)
	return command
}

func newKeyIssueCommand(state *commandState) *cobra.Command {
	var principalID string
	var machineID string
	var sources []string
	var expiresAt string
	command := &cobra.Command{
		Use: "issue --principal-id <userid> --machine-id <machine> --source <address>", Short: "签发一把绑定用户和机器的 Key", Args: cobra.NoArgs,
		Long: "签发新的机器 Key。--principal-id、--machine-id 和至少一个 --source 为必填；--source 可以重复，支持单 IP、CIDR 和同地址族闭区间；--expires-at 使用 RFC3339。明文 Key 只显示一次。",
		Example: "  hp-cli key issue --principal-id user-a --machine-id office-pc --source 192.168.1.20\n" +
			"  hp-cli key issue --principal-id user-a --machine-id office-pc --source 10.0.0.0/24 --expires-at 2026-12-31T16:00:00Z",
		RunE: func(command *cobra.Command, _ []string) error {
			if strings.TrimSpace(principalID) == "" || strings.TrimSpace(machineID) == "" || len(sources) == 0 {
				return errors.New("key issue 必须提供 --principal-id、--machine-id 和至少一个 --source")
			}
			params := adminproto.KeyIssueParams{PrincipalID: principalID, MachineID: machineID, Sources: append([]string(nil), sources...)}
			if strings.TrimSpace(expiresAt) != "" {
				if _, err := time.Parse(time.RFC3339, expiresAt); err != nil {
					return errors.New("--expires-at 必须是 RFC3339 时间")
				}
				params.ExpiresAt = &expiresAt
			}
			return runInvocation(command, state, Invocation{Method: adminproto.MethodKeyIssue, Params: params})
		},
	}
	command.Flags().StringVar(&principalID, "principal-id", "", "企业微信用户 ID")
	command.Flags().StringVar(&machineID, "machine-id", "", "当前运行 Herdr 的机器标识")
	command.Flags().StringArrayVar(&sources, "source", nil, "允许连接的来源地址规则，可以重复")
	command.Flags().StringVar(&expiresAt, "expires-at", "", "Key 到期时间，使用 RFC3339")
	return command
}

func newKeyListCommand(state *commandState) *cobra.Command {
	return withCommandHelp(&cobra.Command{
		Use: "list", Short: "列出全部 Key", Args: cobra.NoArgs,
		RunE: func(command *cobra.Command, _ []string) error {
			return runInvocation(command, state, Invocation{Method: adminproto.MethodKeyList, Params: adminproto.KeyListParams{}})
		},
	}, "列出服务端保存的全部 Key 摘要记录，不显示可用于连接的明文 Key。", "  hp-cli key list\n  hp-cli key list --json")
}

func newCredentialIDCommand(state *commandState, use, short string, method adminproto.Method) *cobra.Command {
	return &cobra.Command{
		Use: use, Short: short, Args: cobra.ExactArgs(1),
		RunE: func(command *cobra.Command, args []string) error {
			credentialID, err := parseCredentialID(args[0])
			if err != nil {
				return err
			}
			return runInvocation(command, state, Invocation{Method: method, Params: adminproto.CredentialIDParams{CredentialID: credentialID}})
		},
	}
}

func newKeyDeleteCommand(state *commandState) *cobra.Command {
	var confirmed bool
	command := &cobra.Command{
		Use: "delete <credential-id> --yes", Short: "永久删除一把 Key", Args: cobra.ExactArgs(1),
		Long:    "永久删除指定 Key，并立即断开对应在线 Pal。该操作不可恢复，必须显式提供 --yes。",
		Example: "  hp-cli key delete 1 --yes",
		RunE: func(command *cobra.Command, args []string) error {
			if !confirmed {
				return errors.New("key delete 必须提供 --yes")
			}
			credentialID, err := parseCredentialID(args[0])
			if err != nil {
				return err
			}
			return runInvocation(command, state, Invocation{Method: adminproto.MethodKeyDelete, Params: adminproto.KeyDeleteParams{
				CredentialID: credentialID, Confirm: true,
			}})
		},
	}
	command.Flags().BoolVar(&confirmed, "yes", false, "确认不可恢复地删除 Key")
	return command
}

func newKeySourceCommand(state *commandState) *cobra.Command {
	command := withCommandHelp(
		newCommandGroup("source", "管理 Key 的来源地址规则"),
		"查看或修改 Key 的来源地址白名单。规则支持单 IP、CIDR 和同地址族闭区间。",
		"  hp-cli key source list 1\n  hp-cli key source add 1 192.168.1.0/24\n  hp-cli key source set 1 192.168.1.20 10.0.0.1-10.0.0.5",
	)
	command.AddCommand(
		withCommandHelp(&cobra.Command{
			Use: "list <credential-id>", Short: "查看 Key 来源地址规则", Args: cobra.ExactArgs(1),
			RunE: func(command *cobra.Command, args []string) error {
				credentialID, err := parseCredentialID(args[0])
				if err != nil {
					return err
				}
				return runInvocation(command, state, Invocation{Method: adminproto.MethodKeySourceList, Params: adminproto.CredentialIDParams{CredentialID: credentialID}})
			},
		}, "查看指定 Key 当前允许的全部来源地址规则。", "  hp-cli key source list 1"),
		withCommandHelp(newKeySourceMutationCommand(state, "add <credential-id> <source>...", "追加 Key 来源地址规则", adminproto.MethodKeySourceAdd), "向指定 Key 追加一个或多个来源规则。规则支持单 IP、CIDR 和同地址族闭区间。", "  hp-cli key source add 1 192.168.1.20 10.0.0.0/24"),
		withCommandHelp(newKeySourceMutationCommand(state, "remove <credential-id> <source>...", "移除 Key 来源地址规则", adminproto.MethodKeySourceRemove), "从指定 Key 移除一个或多个完全匹配的来源规则。规则支持单 IP、CIDR 和同地址族闭区间。", "  hp-cli key source remove 1 192.168.1.20"),
		withCommandHelp(newKeySourceMutationCommand(state, "set <credential-id> <source>...", "替换 Key 来源地址规则", adminproto.MethodKeySourceSet), "使用给定规则完整替换指定 Key 的来源白名单。规则支持单 IP、CIDR 和同地址族闭区间；不再符合规则的连接会被断开。", "  hp-cli key source set 1 192.168.1.20 10.0.0.1-10.0.0.5"),
	)
	return command
}

func newKeySourceMutationCommand(state *commandState, use, short string, method adminproto.Method) *cobra.Command {
	return &cobra.Command{
		Use: use, Short: short, Args: cobra.MinimumNArgs(2),
		RunE: func(command *cobra.Command, args []string) error {
			credentialID, err := parseCredentialID(args[0])
			if err != nil {
				return err
			}
			return runInvocation(command, state, Invocation{Method: method, Params: adminproto.KeySourceMutationParams{
				CredentialID: credentialID, Sources: append([]string(nil), args[1:]...),
			}})
		},
	}
}
