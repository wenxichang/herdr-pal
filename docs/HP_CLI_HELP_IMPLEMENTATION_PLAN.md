# hp-cli Cobra 帮助系统实施计划

> **执行要求：** 项目明确禁止 subagent-driven，本计划使用
> `superpowers:executing-plans` 在当前会话中逐项实施。每项功能遵循测试先行，并在提交前
> 执行完整 `./unittest.sh` 与 `./build.sh`。

**目标：** 使用 Cobra 重建 `hp-cli` 命令树，在顶层展示全部命令摘要，并为每级子命令提供
完整中文帮助，同时保持现有调用、HPAP 行为和退出码兼容。

**架构：** `main.go` 保留进程入口、Invocation 执行与错误分类；新增独立命令装配文件构造
Cobra 树，新增帮助渲染文件从同一命令树生成顶层递归摘要和分层详细帮助。所有叶子命令仍
生成现有强类型 `Invocation`，不会把 HPAP 调用散入命令定义。

**技术栈：** Go 1.26、Cobra v1.10.2、pflag、现有 `adminclient`/`adminproto`、Go `testing`。

---

## 任务 1：建立 Cobra 根命令和顶层帮助

**文件：**

- 修改：`go.mod`
- 修改：`go.sum`
- 创建：`cmd/hp-cli/command.go`
- 创建：`cmd/hp-cli/help.go`
- 创建：`cmd/hp-cli/help_test.go`
- 修改：`cmd/hp-cli/main.go`

- [x] **步骤 1：先编写顶层帮助失败测试**

在 `help_test.go` 增加以下测试，要求帮助不调用 executor，并包含全部叶子路径、全局参数和
常见示例：

```go
func TestRootHelpListsAllCommandsWithoutExecutor(t *testing.T) {
    var stdout, stderr bytes.Buffer
    called := false
    code := run(context.Background(), []string{"--help"}, &stdout, &stderr,
        func(context.Context, string, Invocation) (any, error) {
            called = true
            return nil, nil
        })
    if code != 0 || called || stderr.Len() != 0 {
        t.Fatalf("root help = code:%d called:%t stdout:%q stderr:%q", code, called, stdout.String(), stderr.String())
    }
    for _, want := range []string{
        "server status", "server debug enable", "key issue", "key source add",
        "connection disconnect", "session list", "--config", "--json", "--version",
    } {
        if !strings.Contains(stdout.String(), want) {
            t.Fatalf("root help missing %q:\n%s", want, stdout.String())
        }
    }
}
```

- [x] **步骤 2：确认测试因缺少 Cobra 帮助而失败**

运行：

```sh
go test ./cmd/hp-cli -run TestRootHelpListsAllCommandsWithoutExecutor -count=1
```

预期：失败，现有解析器把 `--help` 视为缺少管理命令，且输出不包含完整命令树。

- [x] **步骤 3：添加 Cobra 依赖并实现最小根命令**

运行：

```sh
go get github.com/spf13/cobra@v1.10.2
```

在 `command.go` 定义命令运行状态和根命令：

```go
type commandState struct {
    ctx        context.Context
    configPath string
    jsonOutput bool
    stdout     io.Writer
    stderr     io.Writer
    execute    executor
}

func newRootCommand(state *commandState) *cobra.Command {
    root := &cobra.Command{
        Use:           "hp-cli",
        Short:         "管理本机运行中的 Herdr Pal Server",
        Version:       version.String(),
        SilenceErrors: true,
        SilenceUsage:  true,
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
```

先为各构造函数建立只含完整层级和 `Short` 的命令骨架，使根帮助可以遍历全部叶子。将
`run` 改为构造根命令、设置规范化后的参数并调用 `ExecuteContext`。

- [x] **步骤 4：实现命令树驱动的帮助渲染**

在 `help.go` 实现：

```go
func renderCommandHelp(command *cobra.Command, _ []string) {
    writer := command.OutOrStdout()
    fmt.Fprintln(writer, command.Short)
    fmt.Fprintf(writer, "\n用法：\n  %s\n", command.UseLine())
    if command.Parent() == nil {
        fmt.Fprintln(writer, "\n全部命令：")
        for _, available := range availableHelpCommands(command) {
            fmt.Fprintf(writer, "  %-32s %s\n", available.CommandPath(), available.Short)
        }
    } else if command.HasAvailableSubCommands() {
        fmt.Fprintln(writer, "\n可用子命令：")
        for _, child := range command.Commands() {
            if child.IsAvailableCommand() && child.Name() != "help" {
                fmt.Fprintf(writer, "  %-18s %s\n", child.Name(), child.Short)
            }
        }
    }
    writeCommandFlags(writer, command)
    if strings.TrimSpace(command.Example) != "" {
        fmt.Fprintf(writer, "\n示例：\n%s\n", indentExamples(command.Example))
    }
}
```

`availableHelpCommands` 递归返回全部业务命令组和叶子命令，过滤 Cobra 内置 `help`；
`writeCommandFlags` 分别打印本地和继承参数；根命令 `Example` 给出状态查询、Key 签发和
会话查询示例。

- [x] **步骤 5：运行顶层帮助测试**

运行：

```sh
go test ./cmd/hp-cli -run TestRootHelpListsAllCommandsWithoutExecutor -count=1
```

预期：通过。

## 任务 2：迁移全部叶子命令并保持 Invocation 等价

**文件：**

- 创建：`cmd/hp-cli/command_key.go`
- 创建：`cmd/hp-cli/command_runtime.go`
- 修改：`cmd/hp-cli/command.go`
- 修改：`cmd/hp-cli/main_test.go`
- 修改：`cmd/hp-cli/main.go`

- [x] **步骤 1：把现有命令覆盖改为 executor 捕获测试**

将 `TestParseCommandsCoversManagementSurface` 改为调用真实 `run`，捕获每个叶子命令生成的
`Invocation`：

```go
func TestCobraCommandsCoverManagementSurface(t *testing.T) {
    tests := []struct {
        args   []string
        method adminproto.Method
    }{
        {[]string{"server", "status"}, adminproto.MethodServerStatus},
        {[]string{"server", "stop"}, adminproto.MethodServerStop},
        {[]string{"server", "debug", "enable"}, adminproto.MethodServerDebugEnable},
        {[]string{"server", "debug", "disable"}, adminproto.MethodServerDebugDisable},
        {[]string{"key", "issue", "--principal-id", "user", "--machine-id", "home", "--source", "10.0.0.1"}, adminproto.MethodKeyIssue},
        {[]string{"key", "list"}, adminproto.MethodKeyList},
        {[]string{"key", "show", "1"}, adminproto.MethodKeyShow},
        {[]string{"key", "enable", "1"}, adminproto.MethodKeyEnable},
        {[]string{"key", "disable", "1"}, adminproto.MethodKeyDisable},
        {[]string{"key", "delete", "1", "--yes"}, adminproto.MethodKeyDelete},
        {[]string{"key", "source", "list", "1"}, adminproto.MethodKeySourceList},
        {[]string{"key", "source", "add", "1", "10.0.0.1"}, adminproto.MethodKeySourceAdd},
        {[]string{"key", "source", "remove", "1", "10.0.0.1"}, adminproto.MethodKeySourceRemove},
        {[]string{"key", "source", "set", "1", "10.0.0.0/24"}, adminproto.MethodKeySourceSet},
        {[]string{"connection", "list"}, adminproto.MethodConnectionList},
        {[]string{"connection", "show", "c-1"}, adminproto.MethodConnectionShow},
        {[]string{"connection", "disconnect", "c-1"}, adminproto.MethodConnectionDisconnect},
        {[]string{"session", "list", "--principal-id", "user", "--machine-id", "home"}, adminproto.MethodSessionList},
    }
    for _, test := range tests {
        var got Invocation
        code := run(context.Background(), test.args, io.Discard, io.Discard,
            func(_ context.Context, _ string, invocation Invocation) (any, error) {
                got = invocation
                return emptyCLIResult(invocation.Method), nil
            })
        if code != 0 || got.Method != test.method {
            t.Fatalf("run(%v) = code:%d invocation:%#v", test.args, code, got)
        }
    }
}

func emptyCLIResult(method adminproto.Method) any {
    switch method {
    case adminproto.MethodServerStatus:
        return adminproto.ServerStatusResult{}
    case adminproto.MethodServerStop:
        return adminproto.ServerStopResult{}
    case adminproto.MethodServerDebugEnable, adminproto.MethodServerDebugDisable:
        return adminproto.ServerDebugResult{}
    case adminproto.MethodKeyIssue:
        return adminproto.KeyIssueResult{}
    case adminproto.MethodKeyList:
        return adminproto.KeyListResult{}
    case adminproto.MethodKeyShow:
        return adminproto.CredentialResult{}
    case adminproto.MethodKeyEnable, adminproto.MethodKeyDisable,
        adminproto.MethodKeySourceAdd, adminproto.MethodKeySourceRemove, adminproto.MethodKeySourceSet:
        return adminproto.CredentialMutationResult{}
    case adminproto.MethodKeyDelete:
        return adminproto.KeyDeleteResult{}
    case adminproto.MethodKeySourceList:
        return adminproto.KeySourceListResult{}
    case adminproto.MethodConnectionList:
        return adminproto.ConnectionListResult{}
    case adminproto.MethodConnectionShow:
        return adminproto.ConnectionResult{}
    case adminproto.MethodConnectionDisconnect:
        return adminproto.ConnectionDisconnectResult{}
    case adminproto.MethodSessionList:
        return adminproto.SessionListResult{}
    default:
        return struct{}{}
    }
}
```

- [x] **步骤 2：确认测试因命令骨架未执行 Invocation 而失败**

运行：

```sh
go test ./cmd/hp-cli -run TestCobraCommandsCoverManagementSurface -count=1
```

预期：失败，至少一个叶子命令没有调用 executor 或生成的方法不正确。

- [x] **步骤 3：实现公共 Invocation 执行函数**

在 `command.go` 增加：

```go
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
```

- [x] **步骤 4：实现 Server、Connection 和 Session 命令**

在 `command_runtime.go` 使用 `cobra.NoArgs`、`cobra.ExactArgs(1)` 构造命令。`session list`
绑定 `--principal-id`、`--machine-id`，并生成：

```go
Invocation{Method: adminproto.MethodSessionList, Params: adminproto.SessionListParams{
    PrincipalID: principalID,
    MachineID: machineID,
}}
```

`connection show/disconnect` 拒绝空 ID；`server debug enable/disable` 使用
`adminproto.EmptyParams{}`。

- [x] **步骤 5：实现 Key 和 Key Source 命令**

在 `command_key.go`：

- `key issue` 使用 `StringArrayVar` 保存重复 `--source`，手工校验去空格后的 principal、
  machine 和 source 数量，并解析 RFC3339 `--expires-at`。
- `show/enable/disable` 使用 `parseCredentialID(args[0])`。
- `delete` 使用一个位置 ID 和必需为 `true` 的 `--yes`。
- `source list` 接受一个 ID；`add/remove/set` 接受一个 ID 和至少一个来源。

每个叶子 `RunE` 只组装强类型 `Invocation` 后调用 `runInvocation`。

- [x] **步骤 6：删除旧手写解析器并运行等价测试**

从 `main.go` 删除 `parsedOptions`、`parseArgs`、`extractGlobalOptions`、`parseInvocation`、
各 `parse*` 和 `stringList`，保留 `parseCredentialID`、`executeInvocation`、结果目标转换函数。

运行：

```sh
go test ./cmd/hp-cli -run 'Test(CobraCommands|ExecuteInvocation)' -count=1
```

预期：通过。

## 任务 3：补齐分层详细帮助和历史参数兼容

**文件：**

- 修改：`cmd/hp-cli/help.go`
- 修改：`cmd/hp-cli/command.go`
- 修改：`cmd/hp-cli/help_test.go`
- 修改：`cmd/hp-cli/main_test.go`

- [x] **步骤 1：编写分层帮助和兼容性失败测试**

增加表驱动测试：

```go
func TestHierarchicalHelpForms(t *testing.T) {
    tests := []struct {
        args  []string
        wants []string
    }{
        {[]string{"help", "key"}, []string{"key issue", "source", "Key 管理"}},
        {[]string{"key", "--help"}, []string{"issue", "delete", "source"}},
        {[]string{"help", "key", "issue"}, []string{"--principal-id", "--machine-id", "--source", "RFC3339", "可以重复"}},
        {[]string{"key", "source", "add", "-h"}, []string{"<credential-id> <source>...", "单 IP", "CIDR", "闭区间"}},
    }
    for _, test := range tests {
        var stdout, stderr bytes.Buffer
        called := false
        code := run(context.Background(), test.args, &stdout, &stderr,
            func(context.Context, string, Invocation) (any, error) {
                called = true
                return nil, nil
            })
        if code != 0 || called || stderr.Len() != 0 {
            t.Fatalf("help %v = code:%d called:%t stderr:%q", test.args, code, called, stderr.String())
        }
        for _, want := range test.wants {
            if !strings.Contains(stdout.String(), want) {
                t.Fatalf("help %v missing %q:\n%s", test.args, want, stdout.String())
            }
        }
    }
}
```

另增加 `-config PATH`、`-config=PATH`、`-version` 和全局参数位于子命令后的兼容测试。

- [x] **步骤 2：确认测试因详细说明或历史参数缺失而失败**

运行：

```sh
go test ./cmd/hp-cli -run 'Test(HierarchicalHelp|LegacyGlobal)' -count=1
```

预期：失败，输出缺少叶子约束或 Cobra 拒绝历史单横线写法。

- [x] **步骤 3：补充每个命令的 Long、Args、Example 和 flag usage**

所有叶子命令使用具体 `Use`，例如：

```go
Use:   "issue --principal-id <userid> --machine-id <machine> --source <address>",
Short: "签发一把绑定用户和机器的 Key",
Long: "签发新的机器 Key。--principal-id、--machine-id 和至少一个 --source 为必填；" +
    "--source 可以重复，支持单 IP、CIDR 和同地址族闭区间；--expires-at 使用 RFC3339。",
Example: `  hp-cli key issue --principal-id user-a --machine-id office-pc --source 192.168.1.20
  hp-cli key issue --principal-id user-a --machine-id office-pc --source 10.0.0.0/24 --expires-at 2026-12-31T16:00:00Z`,
```

父命令的 `Short`、`Long` 和 `Example` 描述模块用途；帮助渲染器在 `Short` 后输出非空
`Long`，并打印位置参数用法、本地参数、继承参数和示例。

- [x] **步骤 4：规范化历史参数**

在 `command.go` 实现：

```go
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
```

不要转换其他单横线参数。

- [x] **步骤 5：运行分层帮助和兼容测试**

运行：

```sh
go test ./cmd/hp-cli -run 'Test(HierarchicalHelp|LegacyGlobal|CobraCommands)' -count=1
```

预期：通过。

## 任务 4：保持错误输出和退出码

**文件：**

- 修改：`cmd/hp-cli/command.go`
- 修改：`cmd/hp-cli/main.go`
- 修改：`cmd/hp-cli/main_test.go`
- 修改：`cmd/hp-cli/help_test.go`

- [x] **步骤 1：编写错误分层失败测试**

覆盖以下结果：

```go
func TestCobraExitCodesAndUsageScope(t *testing.T) {
    tests := []struct {
        name       string
        args       []string
        executeErr error
        code       int
        want       string
        wantHelp   string
    }{
        {"invalid argument", []string{"key", "show", "bad"}, nil, 2, "参数错误：", "hp-cli key show"},
        {"unknown key command", []string{"key", "unknown"}, nil, 2, "参数错误：", "可用子命令"},
        {"business", []string{"server", "status"}, &adminclient.ServerError{Code: adminproto.CodeCredentialNotFound}, 1, "请求失败：", ""},
        {"config", []string{"server", "status"}, errLocalConfig, 2, "配置错误：", ""},
        {"transport", []string{"server", "status"}, adminclient.ErrTransport, 3, "Admin Socket 请求失败：", ""},
    }
    for _, test := range tests {
        t.Run(test.name, func(t *testing.T) {
            var stdout, stderr bytes.Buffer
            code := run(context.Background(), test.args, &stdout, &stderr,
                func(_ context.Context, _ string, invocation Invocation) (any, error) {
                    if test.executeErr != nil {
                        return nil, test.executeErr
                    }
                    return emptyCLIResult(invocation.Method), nil
                })
            if code != test.code || !strings.Contains(stderr.String(), test.want) {
                t.Fatalf("run() = code:%d stdout:%q stderr:%q", code, stdout.String(), stderr.String())
            }
            if test.wantHelp != "" && !strings.Contains(stderr.String(), test.wantHelp) {
                t.Fatalf("stderr missing nearest help %q: %s", test.wantHelp, stderr.String())
            }
            if test.wantHelp == "" && strings.Contains(stderr.String(), "用法：") {
                t.Fatalf("non-usage error printed help: %s", stderr.String())
            }
        })
    }
}
```

- [x] **步骤 2：确认测试因 Cobra 默认错误语义不一致而失败**

运行：

```sh
go test ./cmd/hp-cli -run TestCobraExitCodesAndUsageScope -count=1
```

预期：失败，至少参数错误没有中文前缀或没有最近层级帮助。

- [x] **步骤 3：实现统一 CLI 错误类型和最近命令定位**

```go
type cliError struct {
    code   int
    prefix string
    cause  error
}

func (err *cliError) Error() string { return err.cause.Error() }

func classifyCLIError(err error) error {
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
```

`run` 对普通 Cobra 解析/Args 错误直接返回 code 2 并输出 `参数错误：`；
`nearestCommand(root, normalizedArgs)` 逐级匹配真实子命令，跳过开头 `help` 和已知全局参数，
用于把对应层级帮助写到 stderr。业务、配置和传输错误不打印用法。

- [x] **步骤 4：运行全部 hp-cli 单元测试**

运行：

```sh
go test ./cmd/hp-cli -count=1
```

预期：全部通过，且测试输出没有重复 usage 或 Cobra 英文错误前缀。

## 任务 5：真实二进制帮助与最终验证

**文件：**

- 修改：`internal/integration/admin_test.go`
- 修改：`README.md`

- [x] **步骤 1：编写不依赖 Server 的真实二进制帮助测试**

复用现有 `buildHPCLI`，增加一个不强制传配置的运行 helper，使帮助命令在不存在配置和
Admin Socket 时也成功：

```go
func TestHPCLIProcessHelpDoesNotRequireServer(t *testing.T) {
    binary := buildHPCLI(t)
    for _, args := range [][]string{
        {"--help"},
        {"help", "key"},
        {"key", "issue", "--help"},
        {"help", "key", "source", "add"},
    } {
        result := runHPCLIArgs(t, binary, args...)
        if result.exitCode != 0 || len(result.stdout) == 0 || len(result.stderr) != 0 {
            t.Fatalf("hp-cli help %v = exit:%d stdout:%q stderr:%q", args, result.exitCode, result.stdout, result.stderr)
        }
    }
}
```

- [x] **步骤 2：确认真实二进制测试先失败**

运行：

```sh
go test ./internal/integration -run TestHPCLIProcessHelpDoesNotRequireServer -count=1
```

预期：在 Cobra 实现完整前至少一个深层帮助失败；若前面任务已使其通过，则临时删除
`root.SetHelpFunc` 绑定确认该测试能捕获帮助回归，随后立即恢复。

- [x] **步骤 3：更新 README 管理帮助入口**

在 HPAP 管理段补充：

```text
hp-cli --help                    # 查看全部命令
hp-cli help key                 # 查看 Key 管理子命令
hp-cli key issue --help         # 查看签发参数和示例
```

不复制完整帮助文本，避免 README 与命令树形成第二份易失清单。

- [x] **步骤 4：运行稳定性、完整单测和构建**

```sh
go test ./cmd/hp-cli ./internal/integration -run 'Test(.*Help|Cobra|LegacyGlobal|HPCLIProcess)' -count=3
go test -race ./cmd/hp-cli ./internal/integration -run 'Test(.*Help|Cobra|LegacyGlobal|HPCLIProcess)' -count=1
./unittest.sh
./build.sh
```

预期：全部退出 `0`。

- [x] **步骤 5：提交实现**

```sh
git add go.mod go.sum cmd/hp-cli README.md internal/integration/admin_test.go docs/HP_CLI_HELP_IMPLEMENTATION_PLAN.md
git commit -m "feat: 使用 Cobra 完善 hp-cli 帮助"
```
