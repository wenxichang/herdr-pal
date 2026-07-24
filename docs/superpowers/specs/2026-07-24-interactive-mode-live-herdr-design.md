# Herdr Pal 交互模式与真实 Herdr 联调设计

## 1. 文档状态

- 日期：2026-07-24
- 状态：设计已确认，待实施
- Herdr 测试基线：源码 debug 版 Herdr 0.7.5，protocol 17
- 当前真实 Socket：`/Users/wxc/.config/herdr-dev/herdr.sock`

## 2. 背景与问题

当前运行中的 Herdr 来自 `/Users/wxc/Code/herdr/target/debug/herdr`。debug 构建使用
`~/.config/herdr-dev`，而 PATH 中的 Homebrew Herdr 0.7.1 使用 `~/.config/herdr`。因此，
直接执行旧 CLI 会错误地报告默认 Herdr 服务未运行。

真实 protocol 17 联调还发现了一个客户端目标类型错误：最新 Herdr 的
`agent.get`、`agent.read`、`agent.prompt` 和 `agent.send_keys` 接受 pane ID 或唯一的
Agent 名，而 Herdr Pal 当前传入 terminal ID。真实服务对 terminal ID 返回
`agent_not_found`，对同一 Agent 的 pane ID 正常返回结果。现有 fake 同时接受两种 ID，
因此未能暴露该问题。

除修复上述问题外，项目需要一个不依赖企业微信网络的本地交互入口。该入口把普通终端
模拟成简单 IM 聊天框，复用完整命令、路由、安全校验、状态通知和审计逻辑。

## 3. 目标

1. 修复 Herdr Agent API target，保证最新 protocol 17 的读取和输入可用。
2. 新增 `herdr-pal -i` 交互模式，通过 stdin/stdout 使用现有 Bridge。
3. 交互模式不要求企业微信 Bot ID、用户 ID 或 Secret。
4. 将当前运行中的真实 Herdr 纳入只读集成测试。
5. 提供双重环境变量门禁的真实 prompt 测试，并在本次实施中使用已授权测试 Agent 验证。
6. 保持普通企业微信模式的 CLI、配置格式和安全边界不变。

## 4. 非目标

- 不实现终端 TUI、光标编辑、历史搜索、补全或多行编辑器。
- 不在真实集成测试中发送 Enter、Space 或其他按键。
- 不自动扫描 `~/.config/herdr-dev` 或其他私有目录猜测 Socket。
- 不修改 Herdr 源码，也不使用 Herdr 私有 client socket 或内部状态。
- 不为交互模式增加多用户、持久化会话或远程监听端口。

## 5. 方案选择

### 5.1 Agent API target

采用 pane ID 作为所有 Agent 公共 API 的 target：

- `agent.get`
- `agent.read`
- `agent.prompt`
- `agent.send_keys`

terminal ID 继续保存在 `session.Target` 中，只用于 occupant 身份摘要和
`session.MatchesAgent` 的返回结果校验，不再作为 Agent API 路由参数。

不采用客户端内部 terminal-to-pane 缓存转换，因为该缓存会引入 occupant 替换时的陈旧
映射。不采用 Agent 名作为主 target，因为名称可能为空、重复或变化。

### 5.2 交互模式

采用 ConsoleAdapter 复用完整 Bridge。stdin 输入仍然经过：

```text
命令解析
  → 本地单用户身份校验
  → msgid 幂等
  → Agent 选择与 occupant 校验
  → Herdr 公共 Agent API
  → ConsoleAdapter 输出
```

不直接从交互循环调用 `HerdrClient`，以免绕过分页、选择、安全按键和审计。不启动本地
fake 企业微信 WebSocket，因为它只会增加运行层级，不能提供额外产品价值。

## 6. CLI 与配置

### 6.1 命令形式

```text
herdr-pal -i
herdr-pal -i -config /path/to/config.json
herdr-pal -config /path/to/config.json
herdr-pal --version
```

规则：

- `-i` 表示交互模式。
- 交互模式下 `-config` 可选。
- 普通企业微信模式仍然必须提供 `-config`。
- `-i` 与 `--version` 同时出现时返回参数错误。
- 不新增 `-socket`；Socket 仍通过公共 CLI 或配置中的 `herdr.socket_path` 解析。

### 6.2 交互配置加载

未提供 `-config` 时使用：

- 空 session；
- 空 socket override，由 PATH 中的 `herdr` 公共 CLI 发现；
- `info` 日志级别；
- 固定本地身份 `interactive-local`。

提供 `-config` 时继续严格拒绝未知 JSON 字段，但只校验 `herdr` 和 `log`。`wecom`
字段可以缺失或为空，且不会读取 `HERDR_PAL_WECOM_SECRET`。

debug Herdr 不做私有路径探测。当前开发环境通过以下任一方式连接：

```sh
PATH=/Users/wxc/Code/herdr/target/debug:$PATH ./dist/herdr-pal -i
```

或在配置中显式填写：

```json
{
  "herdr": {
    "session": "",
    "socket_path": "/Users/wxc/.config/herdr-dev/herdr.sock"
  },
  "log": {
    "level": "info"
  }
}
```

## 7. ConsoleAdapter

新增独立的本地交互适配模块，职责仅包括：

- 从 stdin 按行读取文本；
- 为每行生成单调递增的本地 request ID 和 message ID；
- 产生 `UserID=interactive-local`、`ChatType=single` 的入站消息；
- 把回调回复打印为 `[回复]`；
- 把主动状态通知打印为 `[通知]`；
- 串行化 stdout 写入并重新显示提示符。

示例：

```text
Herdr Pal 交互模式
输入 /ls 查看 Agent，按 Ctrl+C 或 Ctrl+D 退出。

herdr-pal> /ls
[回复]
可选择的 Agent：
1. Codex
   面板：w1:p1

herdr-pal>
```

主动通知到达时：

```text

[通知]
Codex 已进入 blocked 状态。
...
herdr-pal>
```

因为不引入 TUI 或 readline，通知与用户正在输入的半行文本可能在视觉上相邻；适配器只
保证每条完整输出和提示符不会由多个 goroutine 交错写入。

## 8. 生命周期与退出

- ConsoleAdapter 的输入循环与 Herdr Supervisor、消息消费循环并行运行。
- Ctrl+C 和 SIGTERM 继续通过根 context 触发现有优雅退出。
- stdin EOF 返回交互输入结束信号；应用将其视为正常退出根因，取消其他组件并释放锁。
- 输出写入失败视为交互组件失败，停止应用并返回安全错误。
- 交互模式使用独立进程锁，不与企业微信 Bot 锁冲突；锁标识由最终 Socket 路径摘要生成。
- 交互模式仍执行 protocol 17 精确门禁、两次 snapshot 和状态订阅重建。

## 9. 安全边界

- 交互输入仍经过本地单用户身份、目标选择、occupant 和按键白名单校验。
- 普通文本只调用 `agent.prompt`，显式按键只调用 `agent.send_keys`。
- 按键仍生成包含本地用户、pane、occupant hash、按键、时间和结果的结构化审计。
- stdout 可以显示终端快照，因为交互用户就是本机显式操作者；stderr 日志仍不得记录完整
  prompt、终端内容、Secret 或 Cookie。
- 交互模式不监听网络端口，也不暴露 Herdr Socket。
- 未选择 Agent、occupant 替换或 Herdr degraded 时拒绝输入。

## 10. 真实 Herdr 测试

### 10.1 默认只读测试

设置 `HERDR_PAL_INTEGRATION=1` 后：

1. 通过 PATH 中的 `herdr status server --json` 获取公共 Socket。
2. 要求 running、protocol 17 和非空 Socket。
3. 调用真实 `ping` 和 `session.snapshot`。
4. 从 snapshot 选择一个 Agent，并使用其 pane ID 调用 `agent.get` 和
   `agent.read(recent_unwrapped, 1)`。
5. 使用本地适配器执行 `/ls`、`/sel` 和 `/con`，不得向真实 Agent 输入。

没有 Agent 时明确 Skip，而不是创建或启动 Agent。

### 10.2 真实 prompt 测试

只有同时设置以下变量才运行：

```sh
HERDR_PAL_INTEGRATION=1
HERDR_PAL_LIVE_INPUT=1
HERDR_PAL_LIVE_PANE_ID=w1:p1
```

测试必须先确认指定 pane 存在且 occupant 与 snapshot 一致，然后发送一条 prompt：

```text
这是 Herdr Pal 实时集成测试。请只回复由 HERDR、PAL、LIVE、OK 四个词使用下划线连接成的字符串；不要调用工具，不要修改文件。
```

测试轮询 `agent.read(recent_unwrapped, 100)`，只判断新增输出中是否出现
`HERDR_PAL_LIVE_OK`，不打印完整终端正文。达到上限仍未出现时失败。测试不发送按键，
也不自动重试 prompt，避免重复输入。

## 11. 测试策略

### 11.1 TDD 回归测试

先收紧 fake Herdr：Agent API 只接受 pane ID 或唯一 Agent 名。现有生产代码继续传
terminal ID 时，Service 和 Notifier 测试必须按预期失败。随后修改生产代码为 pane ID，
使测试转绿。

覆盖：

- prompt、按键、读取和 occupant 查询全部使用 pane ID；
- terminal ID 不再作为 Agent API target；
- 返回的 terminal ID 仍参与 occupant 校验；
- blocked/done 通知的前后校验和读取使用 pane ID；
- fake 拒绝 terminal ID，避免再次产生假阳性。

### 11.2 交互模式单元测试

- `-i` 无配置可以启动装配流程。
- `-i -config` 不需要企业微信字段或 Secret。
- 普通模式仍拒绝缺失企业微信配置。
- stdin 每行只产生一条消息，EOF 正常退出。
- 回调回复和主动通知使用不同标签。
- 多 goroutine 输出不会交错破坏完整消息。
- Ctrl+C、EOF、组件失败和退出超时保持既有根因冻结语义。
- 交互模式按键产生审计，重复输入 ID 不会重复执行。

### 11.3 端到端测试

- fake Herdr + ConsoleAdapter 通过真实应用入口执行 `/ls`、`/sel 1`、普通 prompt、
  `/con` 和状态通知。
- stdout 包含提示符、`[回复]` 和 `[通知]`，stderr 不泄漏输入或终端内容。
- 真实 Herdr 只读测试默认可重复运行。
- 真实 prompt 测试必须显式双重授权并指定 pane。

## 12. 文档与运行示例

README 增加交互模式、debug Herdr PATH 示例、可选配置和真实输入测试警告。
`docs/HANDOFF_CONTEXT.md` 更新真实 protocol 17 联调状态，不再保留“本机仅 protocol 14”
的过期描述。

本次实施完成后的手工验收流程：

```sh
./build.sh
PATH=/Users/wxc/Code/herdr/target/debug:$PATH ./dist/herdr-pal -i
```

在提示符中依次输入：

```text
/ls
/sel 1
/con
请只回复 HERDR_PAL_MANUAL_OK，不要调用工具或修改文件。
```

观察 Agent 回复和 working/blocked/done 通知后按 Ctrl+C 退出。

## 13. 验收标准

1. 当前 debug Herdr 的真实只读集成测试通过。
2. 使用 pane ID 后 `/con` 不再返回 `agent_not_found`。
3. `herdr-pal -i` 无企业微信配置即可进入提示符。
4. 交互模式复用全部现有命令、分页、状态通知、幂等和按键审计。
5. Ctrl+C 与 Ctrl+D 均正常停止，进程锁得到释放。
6. 双重门禁测试只向指定 pane 发送一次 prompt，并观察到固定回复标记。
7. 普通企业微信模式行为和配置校验无回归。
8. `go test -race ./...`、`go vet ./...`、`./build.sh` 和 `./unittest.sh` 全部通过。
