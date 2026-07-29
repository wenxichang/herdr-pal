// hp-cli 是 herdr-pal-server 的本地 HPAP 管理工具。
package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"strconv"

	"github.com/wenxichang/herdr-pal/internal/adminclient"
	"github.com/wenxichang/herdr-pal/internal/adminproto"
	"github.com/wenxichang/herdr-pal/internal/config"
)

var errLocalConfig = errors.New("hp-cli 本地配置错误")

// Invocation 是一次已经通过 CLI 参数校验的强类型 HPAP 调用。
type Invocation struct {
	Method adminproto.Method
	Params any
}

type executor func(context.Context, string, Invocation) (any, error)

func main() {
	os.Exit(run(context.Background(), os.Args[1:], os.Stdout, os.Stderr, executeInvocation))
}

func run(ctx context.Context, args []string, stdout, stderr io.Writer, execute executor) int {
	state := &commandState{ctx: ctx, stdout: stdout, stderr: stderr, execute: execute}
	root := newRootCommand(state)
	normalizedArgs := normalizeLegacyArgs(args)
	root.SetArgs(normalizedArgs)
	if err := root.ExecuteContext(ctx); err != nil {
		var commandError *cliError
		if errors.As(err, &commandError) {
			fmt.Fprintln(stderr, commandError.prefix, commandError.cause)
			return commandError.code
		}
		fmt.Fprintln(stderr, "参数错误：", err)
		writeNearestCommandHelp(root, normalizedArgs, stderr)
		return 2
	}
	return 0
}

func parseCredentialID(value string) (uint64, error) {
	credentialID, err := strconv.ParseUint(value, 10, 64)
	if err != nil || credentialID == 0 || strconv.FormatUint(credentialID, 10) != value {
		return 0, errors.New("credential ID 必须是非零十进制整数")
	}
	return credentialID, nil
}

func executeInvocation(ctx context.Context, configPath string, invocation Invocation) (any, error) {
	loaded, err := config.LoadServerAdmin(configPath)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", errLocalConfig, err)
	}
	client, err := adminclient.New(adminclient.Config{SocketPath: loaded.Server.AdminSocketPath})
	if err != nil {
		return nil, err
	}
	switch invocation.Method {
	case adminproto.MethodKeyList:
		return client.ListKeys(ctx, invocation.Params.(adminproto.KeyListParams))
	case adminproto.MethodConnectionList:
		return client.ListConnections(ctx, invocation.Params.(adminproto.ConnectionListParams))
	case adminproto.MethodSessionList:
		return client.ListSessions(ctx, invocation.Params.(adminproto.SessionListParams))
	}
	result := resultDestination(invocation.Method)
	if result == nil {
		return nil, fmt.Errorf("%w: 未知方法 %s", errLocalConfig, invocation.Method)
	}
	if err := client.Call(ctx, invocation.Method, invocation.Params, result); err != nil {
		return nil, err
	}
	return dereferenceResult(result), nil
}

func resultDestination(method adminproto.Method) any {
	switch method {
	case adminproto.MethodServerStatus:
		return &adminproto.ServerStatusResult{}
	case adminproto.MethodServerStop:
		return &adminproto.ServerStopResult{}
	case adminproto.MethodServerDebugEnable, adminproto.MethodServerDebugDisable:
		return &adminproto.ServerDebugResult{}
	case adminproto.MethodKeyIssue:
		return &adminproto.KeyIssueResult{}
	case adminproto.MethodKeyShow:
		return &adminproto.CredentialResult{}
	case adminproto.MethodKeyEnable, adminproto.MethodKeyDisable, adminproto.MethodKeySourceAdd, adminproto.MethodKeySourceRemove, adminproto.MethodKeySourceSet:
		return &adminproto.CredentialMutationResult{}
	case adminproto.MethodKeyDelete:
		return &adminproto.KeyDeleteResult{}
	case adminproto.MethodKeySourceList:
		return &adminproto.KeySourceListResult{}
	case adminproto.MethodConnectionShow:
		return &adminproto.ConnectionResult{}
	case adminproto.MethodConnectionDisconnect:
		return &adminproto.ConnectionDisconnectResult{}
	default:
		return nil
	}
}

func dereferenceResult(value any) any {
	switch result := value.(type) {
	case *adminproto.ServerStatusResult:
		return *result
	case *adminproto.ServerStopResult:
		return *result
	case *adminproto.ServerDebugResult:
		return *result
	case *adminproto.KeyIssueResult:
		return *result
	case *adminproto.CredentialResult:
		return *result
	case *adminproto.CredentialMutationResult:
		return *result
	case *adminproto.KeyDeleteResult:
		return *result
	case *adminproto.KeySourceListResult:
		return *result
	case *adminproto.ConnectionResult:
		return *result
	case *adminproto.ConnectionDisconnectResult:
		return *result
	default:
		return value
	}
}

func classifyExecutionError(err error) error { return err }
