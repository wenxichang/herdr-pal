// hp-cli 是 herdr-pal-server 的本地 HPAP 管理工具。
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/wenxichang/herdr-pal/internal/adminclient"
	"github.com/wenxichang/herdr-pal/internal/adminproto"
	"github.com/wenxichang/herdr-pal/internal/adminserver"
	"github.com/wenxichang/herdr-pal/internal/config"
	"github.com/wenxichang/herdr-pal/internal/version"
)

var errLocalConfig = errors.New("hp-cli 本地配置错误")

// Invocation 是一次已经通过 CLI 参数校验的强类型 HPAP 调用。
type Invocation struct {
	Method adminproto.Method
	Params any
}

type parsedOptions struct {
	ConfigPath string
	JSON       bool
	Version    bool
	Invocation Invocation
}

type executor func(context.Context, string, Invocation) (any, error)

func main() {
	os.Exit(run(context.Background(), os.Args[1:], os.Stdout, os.Stderr, executeInvocation))
}

func run(ctx context.Context, args []string, stdout, stderr io.Writer, execute executor) int {
	options, err := parseArgs(args)
	if err != nil {
		fmt.Fprintln(stderr, "参数错误：", err)
		printUsage(stderr)
		return 2
	}
	if options.Version {
		fmt.Fprintln(stdout, version.String())
		return 0
	}
	configPath := options.ConfigPath
	if configPath == "" {
		configPath, err = config.DefaultServerPath()
		if err != nil {
			fmt.Fprintln(stderr, "无法确定默认服务端配置路径，请使用 -config。")
			return 2
		}
	}
	if execute == nil {
		fmt.Fprintln(stderr, "管理执行器不可用。")
		return 3
	}
	result, err := execute(ctx, configPath, options.Invocation)
	if err != nil {
		err = classifyExecutionError(err)
		var serverError *adminclient.ServerError
		switch {
		case errors.As(err, &serverError):
			fmt.Fprintln(stderr, "请求失败：", serverError)
			return 1
		case errors.Is(err, errLocalConfig), errors.Is(err, adminclient.ErrConfig):
			fmt.Fprintln(stderr, "配置错误：", err)
			return 2
		default:
			fmt.Fprintln(stderr, "Admin Socket 请求失败：", err)
			return 3
		}
	}
	if options.JSON {
		err = adminclient.FormatJSON(stdout, result)
	} else {
		err = adminclient.FormatHuman(stdout, options.Invocation.Method, result)
	}
	if err != nil {
		fmt.Fprintln(stderr, "输出失败：", err)
		return 3
	}
	return 0
}

func parseArgs(args []string) (parsedOptions, error) {
	options, commandArgs, err := extractGlobalOptions(args)
	if err != nil {
		return parsedOptions{}, err
	}
	if options.Version {
		if len(commandArgs) != 0 || options.JSON || options.ConfigPath != "" {
			return parsedOptions{}, errors.New("--version 不能与其他参数组合")
		}
		return options, nil
	}
	if len(commandArgs) < 2 {
		return parsedOptions{}, errors.New("缺少管理命令")
	}
	invocation, err := parseInvocation(commandArgs)
	if err != nil {
		return parsedOptions{}, err
	}
	options.Invocation = invocation
	return options, nil
}

func extractGlobalOptions(args []string) (parsedOptions, []string, error) {
	var options parsedOptions
	remaining := make([]string, 0, len(args))
	for index := 0; index < len(args); index++ {
		argument := args[index]
		switch {
		case argument == "--json":
			if options.JSON {
				return parsedOptions{}, nil, errors.New("--json 重复")
			}
			options.JSON = true
		case argument == "--version" || argument == "-version":
			if options.Version {
				return parsedOptions{}, nil, errors.New("--version 重复")
			}
			options.Version = true
		case argument == "-config" || argument == "--config":
			if options.ConfigPath != "" || index+1 >= len(args) {
				return parsedOptions{}, nil, errors.New("-config 无效或重复")
			}
			index++
			options.ConfigPath = args[index]
		case strings.HasPrefix(argument, "-config="):
			if options.ConfigPath != "" {
				return parsedOptions{}, nil, errors.New("-config 重复")
			}
			options.ConfigPath = strings.TrimPrefix(argument, "-config=")
		default:
			remaining = append(remaining, argument)
		}
	}
	if strings.TrimSpace(options.ConfigPath) == "" && options.ConfigPath != "" {
		return parsedOptions{}, nil, errors.New("-config 路径为空")
	}
	return options, remaining, nil
}

func parseInvocation(args []string) (Invocation, error) {
	switch args[0] {
	case "server":
		return parseServer(args[1:])
	case "key":
		return parseKey(args[1:])
	case "connection":
		return parseConnection(args[1:])
	case "session":
		return parseSession(args[1:])
	default:
		return Invocation{}, fmt.Errorf("未知命令 %q", args[0])
	}
}

func parseServer(args []string) (Invocation, error) {
	if len(args) == 1 {
		switch args[0] {
		case "status":
			return Invocation{Method: adminproto.MethodServerStatus, Params: adminproto.EmptyParams{}}, nil
		case "stop":
			return Invocation{Method: adminproto.MethodServerStop, Params: adminproto.EmptyParams{}}, nil
		}
	}
	if len(args) == 2 && args[0] == "debug" {
		switch args[1] {
		case "enable":
			return Invocation{Method: adminproto.MethodServerDebugEnable, Params: adminproto.EmptyParams{}}, nil
		case "disable":
			return Invocation{Method: adminproto.MethodServerDebugDisable, Params: adminproto.EmptyParams{}}, nil
		}
	}
	return Invocation{}, errors.New("server 命令无效")
}

func parseKey(args []string) (Invocation, error) {
	if len(args) == 0 {
		return Invocation{}, errors.New("缺少 key 子命令")
	}
	switch args[0] {
	case "issue":
		return parseKeyIssue(args[1:])
	case "list":
		if len(args) != 1 {
			return Invocation{}, errors.New("key list 不接受参数")
		}
		return Invocation{Method: adminproto.MethodKeyList, Params: adminproto.KeyListParams{}}, nil
	case "show", "enable", "disable":
		if len(args) != 2 {
			return Invocation{}, errors.New("Key 命令需要一个 credential ID")
		}
		credentialID, err := parseCredentialID(args[1])
		if err != nil {
			return Invocation{}, err
		}
		method := map[string]adminproto.Method{"show": adminproto.MethodKeyShow, "enable": adminproto.MethodKeyEnable, "disable": adminproto.MethodKeyDisable}[args[0]]
		return Invocation{Method: method, Params: adminproto.CredentialIDParams{CredentialID: credentialID}}, nil
	case "delete":
		return parseKeyDelete(args[1:])
	case "source":
		return parseKeySource(args[1:])
	default:
		return Invocation{}, errors.New("未知 key 子命令")
	}
}

func parseKeyIssue(args []string) (Invocation, error) {
	flags := flag.NewFlagSet("key issue", flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	principalID := flags.String("principal-id", "", "")
	machineID := flags.String("machine-id", "", "")
	expiresAt := flags.String("expires-at", "", "")
	var sources stringList
	flags.Var(&sources, "source", "")
	if err := flags.Parse(args); err != nil || flags.NArg() != 0 || strings.TrimSpace(*principalID) == "" || strings.TrimSpace(*machineID) == "" || len(sources) == 0 {
		return Invocation{}, errors.New("key issue 参数无效")
	}
	params := adminproto.KeyIssueParams{PrincipalID: *principalID, MachineID: *machineID, Sources: append([]string(nil), sources...)}
	if *expiresAt != "" {
		if _, err := time.Parse(time.RFC3339, *expiresAt); err != nil {
			return Invocation{}, errors.New("--expires-at 必须是 RFC3339 时间")
		}
		params.ExpiresAt = expiresAt
	}
	return Invocation{Method: adminproto.MethodKeyIssue, Params: params}, nil
}

func parseKeyDelete(args []string) (Invocation, error) {
	confirmed := false
	values := make([]string, 0, len(args))
	for _, argument := range args {
		if argument == "--yes" {
			confirmed = true
			continue
		}
		values = append(values, argument)
	}
	if !confirmed || len(values) != 1 {
		return Invocation{}, errors.New("key delete 必须提供 credential ID 和 --yes")
	}
	credentialID, err := parseCredentialID(values[0])
	if err != nil {
		return Invocation{}, err
	}
	return Invocation{Method: adminproto.MethodKeyDelete, Params: adminproto.KeyDeleteParams{CredentialID: credentialID, Confirm: true}}, nil
}

func parseKeySource(args []string) (Invocation, error) {
	if len(args) < 2 {
		return Invocation{}, errors.New("key source 命令无效")
	}
	credentialID, err := parseCredentialID(args[1])
	if err != nil {
		return Invocation{}, err
	}
	switch args[0] {
	case "list":
		if len(args) != 2 {
			return Invocation{}, errors.New("key source list 只接受 credential ID")
		}
		return Invocation{Method: adminproto.MethodKeySourceList, Params: adminproto.CredentialIDParams{CredentialID: credentialID}}, nil
	case "add", "remove", "set":
		if len(args) < 3 {
			return Invocation{}, errors.New("key source 修改至少需要一个来源")
		}
		method := map[string]adminproto.Method{"add": adminproto.MethodKeySourceAdd, "remove": adminproto.MethodKeySourceRemove, "set": adminproto.MethodKeySourceSet}[args[0]]
		return Invocation{Method: method, Params: adminproto.KeySourceMutationParams{CredentialID: credentialID, Sources: append([]string(nil), args[2:]...)}}, nil
	default:
		return Invocation{}, errors.New("未知 key source 子命令")
	}
}

func parseConnection(args []string) (Invocation, error) {
	if len(args) == 1 && args[0] == "list" {
		return Invocation{Method: adminproto.MethodConnectionList, Params: adminproto.ConnectionListParams{}}, nil
	}
	if len(args) == 2 && (args[0] == "show" || args[0] == "disconnect") && strings.TrimSpace(args[1]) != "" {
		method := adminproto.MethodConnectionShow
		if args[0] == "disconnect" {
			method = adminproto.MethodConnectionDisconnect
		}
		return Invocation{Method: method, Params: adminproto.ConnectionIDParams{ConnectionID: args[1]}}, nil
	}
	return Invocation{}, errors.New("connection 命令无效")
}

func parseSession(args []string) (Invocation, error) {
	if len(args) == 0 || args[0] != "list" {
		return Invocation{}, errors.New("session 命令无效")
	}
	flags := flag.NewFlagSet("session list", flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	principalID := flags.String("principal-id", "", "")
	machineID := flags.String("machine-id", "", "")
	if err := flags.Parse(args[1:]); err != nil || flags.NArg() != 0 {
		return Invocation{}, errors.New("session list 参数无效")
	}
	return Invocation{Method: adminproto.MethodSessionList, Params: adminproto.SessionListParams{PrincipalID: *principalID, MachineID: *machineID}}, nil
}

func parseCredentialID(value string) (uint64, error) {
	credentialID, err := strconv.ParseUint(value, 10, 64)
	if err != nil || credentialID == 0 || strconv.FormatUint(credentialID, 10) != value {
		return 0, errors.New("credential ID 必须是非零十进制整数")
	}
	return credentialID, nil
}

type stringList []string

func (values *stringList) String() string { return strings.Join(*values, ",") }
func (values *stringList) Set(value string) error {
	*values = append(*values, value)
	return nil
}

func executeInvocation(ctx context.Context, configPath string, invocation Invocation) (any, error) {
	loaded, err := config.LoadServerAdmin(configPath)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", errLocalConfig, err)
	}
	client, err := adminclient.New(adminclient.Config{SocketPath: filepath.Join(loaded.Server.StateDir, adminserver.DefaultSocketFileName)})
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

func printUsage(writer io.Writer) {
	fmt.Fprintln(writer, "用法: hp-cli [-config server-config.json] [--json] <server|key|connection|session> ...")
	fmt.Fprintln(writer, "      hp-cli --version")
}
