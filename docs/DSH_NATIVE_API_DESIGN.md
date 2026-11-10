# dsh 免插件原生 Remote API 接入设计

日期：2026-11-10

## 1. 目标

把当前“向 dsh 安装 Node 插件，再由 Pal 连接插件本机 HTTP 接口”的模式，替换为直接使用
dsh 自带的 Remote API。Pal 不再要求用户安装、升级或重启 dsh 插件，也不再需要手工配置
`dsh.token`。

本期只支持 dsh（DeepSeek Harness），不移植参考仓库中的其他 Agent 适配器。

参考实现：

- `silk-cipher/agentctrl/plugins/harness`：dsh 原生 Remote API 适配器（Rust/WASM）。
- `silk-cipher/agentctrl/crates/agentctrl/src/harness/{auth,discovery,transport,wire}.rs`：
  本机 Host 发现、认证 Cookie、HTTP RPC、Mux 传输。
- `silk-cipher/docs/research/desktop-agents/deepseek-harness.md`：协议调研记录。

## 2. 用户可见行为

### 2.1 会话目录

- 自动发现本机正在运行的 dsh web 与 dsh Desktop 宿主。
- 每个宿主只列出 `agentAvailable=true` 的会话，即当前宿主真正持有 Agent 控制权的会话。
  web 与 Desktop 各自维护控制权，因此两边的条目不会重叠。
- 不再列出无控制权的冷历史会话。历史会话只有被 dsh 恢复为 live 后才会出现。
- 每个 dsh 宿主在没有任何可控制会话时，额外提供一个“新建会话”入口；有会话时不显示。
- `/ls` 中 dsh 条目显示为 `DeepSeek Harness`，Workspace 显示 `web` 或 `desktop`。

### 2.2 `/con` 内容

- 只输出聊天正文：真实用户输入（`user/message` 且 `source.kind=user`）和助手文本块
  （`assistant/message` 的 text 块）。
- 不输出工具调用/结果、推理、系统/开发者注入、压缩摘要、图片和实时未提交文本。
- 最多最近 100 条消息，最终截取末尾 16000 个 UTF-16 代码单元。
- 清理 ANSI/控制序列。
- 存在待处理审批或问题时，在正文末尾附上当前待处理项和 `/answer` 使用提示。

### 2.3 普通文本

- 使用 dsh 原生 `session/prompt`，`mode="queue"`。
- Agent 运行中允许发送，进入 dsh 原生队列，当前任务完成后执行。
- 存在待审批事项时拒绝普通文本，提示先 `/answer`，避免把文本误当批准。
- 每个 IM 消息 ID 作为 `requestId` 提交一次；超时或网络错误不自动重试。

### 2.4 `/answer`

支持三种形式：

```text
/answer 1,2,3
/answer 5 自定义答案
/answer 纯文本答案
```

- 前导数字列表表示选项编号；剩余文本作为自定义答案。
- 多选问题使用逗号分隔编号；单选最多一个编号。
- 纯文本问题只接受文本。
- 审批只接受 `1`（允许本次）或 `2`（拒绝），不接受自定义文本，不提供“始终允许”。
- 一个原生决策包含多个问题时，逐题收集答案，最后一题回答后一次性提交。
- 待答内容变化、被取消或版本不一致时丢弃已收集答案，提示重新 `/con`。
- 提交后不自动重试；能确认采纳时回复已确认，不能确认时回复“已提交，未取得核验”。

### 2.5 `/new-session`

```text
/new-session
/new-session 标题
/new-session 标题 /绝对目录
/new-session /绝对目录
```

- 标题缺省为 `默认标题`。
- 目录缺省为用户主目录。
- 解析规则：最后一个以 `/`、`~/`、`./`、`../` 开头或等于 `~` 的参数视为目录；其余作为
  标题。这样标题可以包含空格。
- 目录必须存在且是目录，不自动创建；支持 `~` 展开。
- 创建后调用 `session/rename` 设置标题；重命名失败时保留已创建会话并明确提示。
- 创建成功后自动切换到新会话。
- 宿主选择：优先使用当前选中的 dsh 会话所属宿主；没有选中时，如果只有一个 dsh 宿主
  可用则使用它；多个宿主且无法判断时提示先 `/ls`、`/N` 选择。

## 3. 非目标

- 不移植 Codex、Claude Code 或其他 Agent 适配器。
- 不接管自定义按键；dsh 的 `/key`、`/enter` 继续拒绝。
- 不提供“始终允许”审批。
- 不把 dsh 的原始 RPC、Cookie、Socket 暴露到远程网络。
- 不启动、停止或重启 dsh；只连接已经在运行的宿主。
- 不读取 dsh 会话 JSONL 文件；正文和状态只走原生 API。
- 不展示工具参数、推理内容或隐藏上下文。
- 不提供 dsh 会话归档、删除、fork、模型选择等 UI 操作。
- 不支持 Windows 自动发现；Windows 需显式配置 `dsh.url` 与 `dsh.home`。

## 4. 协议

### 4.1 Host 身份

```go
type Host struct {
    PID        uint32
    Port       uint16
    Version    string // 0.2.1-rc.5 或 0.2.2-rc.1
    Variant    string // web | desktop
    InstallDir string
    Home       string // 含 .credentials.yaml
    Executable string
    Generation string // sha256(启动时间|可执行文件|参数)[:24]
}
```

`Generation` 在 dsh 进程重启后变化，用于让旧目标失效。

### 4.2 认证

dsh 把浏览器会话签名材料保存在 `<DSH_HOME>/.credentials.yaml` 的
`records["client-connection/browser-session"]` 记录中，格式固定为：

```yaml
version: 1
records:
  client-connection/browser-session:
    kind: grant
    payload:
      version: 1
      secret: <base64url 32 字节>
```

Pal 生成短期 Cookie：

```text
authority = "127.0.0.1:<port>"
payload   = base64url(JSON{"version":1,"authority":authority,"issuedAt":nowMs,"expiresAt":nowMs+300000})
signature = base64url(HMAC-SHA256(key, payload))
name      = "dsh-auth-" + base64url(SHA-256(authority))
value     = "v1." + payload + "." + signature
Cookie    = name + "=" + value
```

安全约束：

- 文件必须属于当前用户、权限不向组/其他用户开放、不是符号链接、大小不超过 256 KiB。
- 只接受 `version=1`、`kind=grant`、payload `version=1`、密钥长度 32。
- 未知格式直接失败，不尝试其他签名方案。
- 每次请求重新生成 Cookie，不落盘、不写日志。
- 认证材料只在内存中短暂存在。

### 4.3 HTTP RPC

```http
POST http://127.0.0.1:<port>/api/<endpoint>
Cookie: dsh-auth-...=v1....
Content-Type: application/json

{"type":"client-request","rpcId":"<uuid>","method":"<endpoint>","payload":{"args":<args>}}
```

响应：

```json
{"type":"server-response","rpcId":"<uuid>","result":{"ok":true,"value":<value>}}
```

失败：

```json
{"type":"server-response","rpcId":"<uuid>","result":{"ok":false,"error":{"code":"...","message":"..."}}}
```

约束：

- 禁用代理和重定向。
- 连接/请求超时 5 秒。
- 请求体上限 1 MiB，响应体上限 4 MiB。
- 只允许固定 endpoint 列表：
  `session/list`、`session/projections`、`session/page`、`session/prompt`、
  `session/create`、`session/rename`、`userQuestions/answer`、`$events/result`。
- `rpcId` 必须与响应一致，否则拒绝。
- HTTP 401 视为认证材料变化；触发重新发现，不自动重试写入。

### 4.4 使用的 endpoint

| endpoint | 用途 | 关键参数 |
| --- | --- | --- |
| `session/list` | 目录 | `{"_request":{}}` |
| `session/projections` | 读取投影切点、持续问题 | `{"request":{"sessionId":...}}` |
| `session/page` | 只读历史 | `{"request":{"address":{"kind":"session","sessionId":...},"throughSeq":...,"maxMessages":100}}` |
| `session/prompt` | 普通文本 | `{"request":{"sessionId":...,"requestId":...,"mode":"queue","content":[{"type":"text","text":...}]}}` |
| `session/create` | 新建会话 | `{"request":{"cwd":...}}` |
| `session/rename` | 设置标题 | `{"request":{"sessionId":...,"title":...}}` |
| `userQuestions/answer` | 回答持续问题 | `{"agentId":...,"callId":...,"answer":...}` |
| `$events/result` | 回答 waterwall 审批/问题 | `{"clientId":...,"eventId":...,"outcome":{"kind":"result","value":...}}` |

### 4.5 Mux 事件

WebSocket：

```text
ws://127.0.0.1:<port>/api/remote.mux
Cookie: dsh-auth-...=v1....
```

打开 `$events`：

```json
{"type":"open","streamId":"events","endpoint":"$events","payload":{"args":{}}}
```

服务端帧：

```json
{"streamId":"events","type":"item","value":<value>}
```

`value.type`：

- `ready`：携带 `clientId`；清空本地待答表。
- `waterfall`：携带 `eventId`、`agentId`、`event`、`request`；加入待答表。
- `cancel`：按 `eventId` 删除。
- `emit`：状态事件；可忽略，状态以 `session/list` 为准。

Mux 断线后指数退避重连；重连后以新的 `ready` 重建待答表。审批提交必须使用当前
`clientId` + `eventId`。

## 5. 模块设计

### 5.1 `internal/dsh`

保持包名不变，内部拆分：

| 文件 | 职责 |
| --- | --- |
| `host.go` | `Host` 身份、版本门禁、宿主引用 |
| `discover_darwin.go` | macOS 进程/端口/身份发现 |
| `discover_linux.go` | Linux 进程/端口/身份发现 |
| `discover_other.go` | 其他平台返回“需要显式配置” |
| `auth.go` | 凭据文件解析与 Cookie 生成 |
| `rpc.go` | HTTP RPC、错误映射、大小/超时限制 |
| `mux.go` | `$events` WebSocket、待答表、重连 |
| `client.go` | 目录、读取、发送、创建、重命名、待答、回答 |
| `transcript.go` | 原生事件到聊天正文的投影 |
| `decisions.go` | 决策模型、题目格式化、答案解析/校验 |
| `executor.go` | 单宿主 HPRP 执行器 |
| `backend.go` | 多宿主发现、生命周期和路由 |

### 5.2 目标映射

每个宿主的稳定身份：

```text
hostRef  = "harness:<pid>:<port>:<generation>"
hostSlot = "dsh-host:" + hex(sha256(hostRef))[:16]
sessionSlot = "dsh:" + hex(sha256(hostRef + "\x00" + sessionID))[:16]
sessionOccupant = "session:" + hex(sha256(host.Generation + "\x00" + sessionID))[:16]
```

- 会话的 `PaneID` 必须包含 `hostRef`，避免 web/Desktop 共享历史时同 ID 冲突。
- `OccupantKey` 在宿主重启后变化；宿主内会话消失再出现时保持稳定。
- 宿主入口只在当前没有任何可控制会话时出现在 `CurrentTargets()` 中。
- 会话状态：有待审批/待答（blocking）为 `blocked`，`running=true` 为 `working`，
  否则 `idle`。

### 5.3 目录与读取

`session/list` 的 `items`：

- 过滤 `origin=subagent`、`formatStatus=migration-required`、`agentAvailable=false`。
- 最多 256 个会话（每个宿主）。
- 标题取 `projections.values.title`，缺省为 session ID。
- Workspace 取 `basename(cwd)`，缺省为宿主变体。

读取：

1. `session/projections` 得 `asOfSeq`；非 `sequenced` 或 `asOfSeq < -1` 时返回“历史暂不可读”。
2. 快路径 `session/page` 取 `maxMessages=100` 的尾页；响应超过 RPC 上限时降级为
   `maxMessages=32` 向上分页，最多 4 页或 100 条消息。
3. 按事件顺序投影 user/assistant 文本，处理 `surfaceOp=replace`；跨页保留替换范围。
4. 过滤空文本、ANSI/控制字符，截取末尾 100 条消息和 16000 UTF-16 代码单元。仍超限时
   返回“历史过大，无法读取”，不返回半截正文。

### 5.4 审批/问题

决策来源：

- Mux `$events` 的 `waterfall`：审批 `approval/request`、问题 `user-questions/request`、
  未知交互。
- `session/projections.values.userQuestions.active`：持续问题（`call:<callId>`）。

决策模型：

```go
type Decision struct {
    NativeID  string // event:<id> 或 call:<id>
    SessionID string
    Kind      string // approval | form | unsupported
    Title     string
    Questions []Question
    Revision  string
    Blocking  bool
    Supported bool
    Reason    string
}

type Question struct {
    ID          string
    Title       string
    Description string
    Kind        string // single | multiple | text
    Options     []Option
    Required    bool
}

type Option struct {
    Value       string
    Label       string
    Description string
}
```

- 审批选项固定为 `allowed-once` / `rejected`，只发送这两个值。
- 问题数量 1..32；问题 ID 唯一；选项数有界；文本长度受限。
- 未知 `waterfall` 交互保留事实但标记 `supported=false`，不设置 blocked，提示去原生界面。
- 持续问题只在 `state=continued` 时可回答；否则标记不支持。

答案状态机（每个 dsh 会话一个）：

```go
type AnswerState struct {
    DecisionID string
    Revision   string
    QuestionIx int
    Answers    map[string]Answer
}
```

- `/answer` 到来时，如果没有活动状态，取当前会话第一个 blocking 决策；没有 blocking
  决策时取第一个 supported 的非 blocking 决策（包括 timed waterfall 问题和持续问题）。
  非 blocking 决策保持 `Blocking=false`，只表示不阻塞 Agent，不表示不可回答。
- 校验答案后存入；还有下一题则回复下一题，不提交。
- 最后一题完成后构造完整答案值并提交一次。
- 提交前复核决策 revision 和 mux 待答表；不一致则丢弃。
- Waterfall 审批/问题用 `$events/result`；持续问题用 `userQuestions/answer`。
- dsh 不会向提交成功的客户端发送原生 `cancel`；`$events/result` 成功后由本地 Mux 注销该
  待答事件并回复已确认。若本地无法确认（Mux 已换代、事件已不在），回复未确认。
- 持续问题提交前重新读取 `session/projections`，`callId` 仍存在、revision 与 supported
  未变化才提交；否则提示重新 `/con`。
- 失败/超时不清空已提交记录，不自动重发。

### 5.5 通知

单宿主执行器每秒刷新目录和待答：

- `working`：running 变化为 true。
- `blocked`：出现 blocking 决策。
- `idle`/`done`：running 变为 false 或 blocking 决策消失。
- 只在状态/决策身份变化时发送一次通知，避免每秒重复；相同 idle 状态只有 `updatedAt`
  变化时才再次通知。
- 通知仍走现有 `im.NotificationEvent` + HPRP `notification.event`，Server 会在 blocked/done
  时主动读取内容并推送。
- 待答内容出现在 `ReadTerminalSnapshot` 末尾，因此 blocked 通知触发的内容推送会包含
  审批/问题文本和 `/answer` 提示。

### 5.6 `/new-session` 的路由和选择切换

HPRP 现有 `ReplacementTarget` 要求同一 `SlotID`，不能表达“新会话是新 slot”。新增可选字段：

```go
type CommandResult struct {
    // ...
    CreatedTarget *Target `json:"created_target,omitempty"`
}
```

流程：

1. Server 把 `/new-session` 作为专用 action，解析出锚点目标。
2. 锚点优先当前选中的 dsh 会话；没有选中时使用唯一 dsh 宿主的入口或第一个会话。
3. Pal 的 dsh 执行器在锚点所属宿主上创建会话，选中新会话，并在命令结果中返回
   `created_target`。
4. Pal 在返回结果前上报一次新快照，确保新目标已进入 Server 目录。
5. Server 校验 `created_target` 与锚点同一机器、且存在于已确认快照；然后切换用户选择。
6. 旧 Server 忽略未知字段，用户仍可 `/ls` + `/N`；旧 Pal 收到 `/new-session` 返回无效命令。

新增 Catalog 方法 `RebindCreated`，只允许同一机器、不同 slot、目标已存在，不复用
`RebindSelection` 的同 slot 约束。

### 5.7 多宿主

`Backend` 负责：

- 每 2 秒发现一次宿主；新增宿主创建 `hostExecutor`，消失宿主取消并撤下目标。
- 按 `hostRef` 去重；同一宿主同一时刻只有一个执行器。
- 汇总 `CurrentTargets()`，按宿主引用和会话 ID 稳定排序。
- `SelectTarget` 按完整目标定位宿主执行器。
- `HandleMessage` / `ReadTerminalSnapshot` 路由到目标所属执行器。
- 某个宿主 API 失败时只撤下该宿主，不影响其他宿主和 Herdr。

## 6. 配置

```json
{
  "relay": { "url": "wss://...", "key": "..." },
  "herdr": { "disabled": true },
  "dsh": {
    "url": "",
    "home": "",
    "variant": "auto"
  }
}
```

- `dsh` 出现即启用 dsh 后端；`herdr.disabled=true` 时要求 `dsh` 存在。
- `url`：可选，显式 `http://127.0.0.1:端口`；为空时自动发现。
- `home`：可选，显式 `DSH_HOME`；为空时使用发现值或 `~/.dsh`。
- `variant`：`auto`（默认）、`web`、`desktop`；用于发现过滤。
- `dsh.token` 不再支持；出现时返回迁移提示，不再读取 Node 插件。

## 7. 平台与版本

- macOS：自动发现 web 与 Desktop。
- Linux：自动发现 web；Desktop 不在 Linux 支持范围。
- Windows：不自动发现；必须显式配置 `url` 和 `home`。
- 版本门禁：`0.2.1-rc.5`、`0.2.2-rc.1`。其他版本跳过并记录不兼容日志。
- dsh 认证记录格式或 RPC 协议变化时失败关闭，不猜测兼容。

## 8. 安全

- 默认拒绝未知宿主、未知会话、未选中目标和未授权企业微信用户。
- Cookie 只在本机回环使用，短期有效，绑定 `127.0.0.1:<port>`。
- 认证文件必须属于当前用户、权限受限、非符号链接。
- 日志不记录密钥、Cookie、完整审批正文或工具参数。
- 审批只允许 `allowed-once` / `rejected`，必须由用户显式 `/answer` 触发。
- 运行中的普通文本进入 dsh 原生队列；待审批时拒绝普通文本。
- 输入不自动重试；`requestId` 由 IM 消息 ID 提供，dsh 自身去重。
- RPC endpoint 白名单、请求/响应大小限制、无代理/重定向。

## 9. 删除 Node 插件

- 删除 `plugins/dsh-herdr-pal/`。
- `build.sh` 移除 npm build/test/pack。
- `unittest.sh` 移除 Node 测试。
- README、架构文档、交接文档、发布说明改为原生接入。
- 集成测试改用 Go fake native host，不再要求 Node.js。
- `internal/buildscript/documentation_test.go` 中关于 `dsh 插件` 的发布说明断言同步更新。

## 10. 测试策略

- 认证：Cookie 签名、authority 绑定、有效期、文件权限/符号链接/版本错误。
- 发现：ps/lsof/ss 输出解析、非 dsh 监听拒绝、版本门禁、多宿主、Linux/macOS 分支。
- RPC：请求帧、rpcId 关联、错误码、大小/超时、无重定向/代理、401 重新发现。
- 目录：过滤、排序、标题/工作区、宿主与会话目标身份、宿主入口条件。
- 读取：only user/assistant、source.kind=user、surface replace、ANSI、100 条/16000 单元。
- 发送：queue 模式、空文本/超长、requestId、忙碌/审批拒绝、不重试。
- 决策：审批/表单/持续问题、多题逐题、选项编号、自定义文本、revision 变化、取消。
- 新建：默认标题/目录、路径解析、创建+重命名、自动选择、创建失败回退。
- HPRP：`created_target` 编解码/校验、快照确认、Server 重新选择、旧版本兼容。
- 执行器：选择失效、宿主离线撤下、状态迁移、通知去重、分页。
- 集成：fake web + fake desktop + 生产 Pal/HPRP/Router + fake 企微，覆盖列目录、读取、
  发送、审批、多题、新建会话、宿主消失。

## 11. 风险与限制

- dsh Remote API 和认证材料是内部协议，不是稳定公开 API；版本升级可能失效。
- 只列有控制权的会话，与旧插件的“14 天历史列表”不同。
- 审批只展示 dsh 提供的工具名和原因，不展示工具参数。
- `/con` 只读已持久化事件，不展示运行中的实时增量文本。
- 极端大的会话历史可能超过 RPC 4 MiB 上限；此时返回“历史过大/宿主不可用”，不返回半截正文。
- 多宿主同名会话不做合并；按宿主分别展示，避免误发。
- `/new-session` 依赖 Server/Pal 同时升级才能自动切换；旧版本可手动 `/ls` + `/N`。
