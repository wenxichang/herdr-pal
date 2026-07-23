# Herdr Pal 企业微信智能机器人 MVP 设计

## 1. 文档状态

- 日期：2026-07-23
- 状态：已完成逐节设计确认，等待最终文档审阅
- Herdr 审计基线：`2a20e90`，协议版本 `17`
- 目标企业微信能力：智能机器人 API 模式长连接
- 实现语言：Go

本文定义 Herdr Pal 第一版 MVP。它是运行在本机的独立 bridge，通过 Herdr 公共
Local Socket API 与企业微信智能机器人长连接，实现 Agent 状态通知、终端近期内容
查看、普通 prompt 和受限按键操作。

## 2. 已确认产品范围

第一版只支持：

- 企业微信智能机器人单聊。
- 一个配置指定的企业微信用户。
- 监控当前 Herdr 会话中的全部 Agent pane。
- 在单聊中使用内置命令选择控制目标和查看终端快照。
- 将不以 `/` 开头的普通文本通过 `agent.prompt` 发送给当前 Agent。
- 通过受限命令将明确的 UI 按键映射到 `agent.send_keys`。
- 在 Agent 状态变化时主动向企业微信发送通知。
- 手工启动和停止，不提供 `launchd`、systemd 或容器部署。
- Go 编译的单个可执行文件，构建时保持 `CGO_ENABLED=0`。
- 所有运行状态仅保存在内存中，重启后以最新状态重新接管。

第一版不支持：

- 企业微信群聊。
- 多用户、角色或管理员权限模型。
- 数据库、持久化队列或重启后的选择与分页恢复。
- LLM token streaming 或完整对话 transcript。
- 任意按键字符串、组合键、控制键或按键序列。
- 多媒体输入、文件上传和语音处理。
- 企业微信模板卡片交互。
- 自动批准任何 Agent 权限请求。
- 断线期间消息或通知的持久化补发。

## 3. 技术选型

### 3.1 企业微信连接

企业微信官方当前只列出 Node.js 和 Python 智能机器人长连接 SDK，没有列出 Go SDK。
Herdr Pal 使用 Go 根据公开 WebSocket JSON 协议实现最小客户端，不依赖非官方企业
微信业务 SDK。

官方协议文档：
[企业微信智能机器人长连接](https://developer.work.weixin.qq.com/document/path/101463)。

第一版只实现协议中的必要子集：

- 连接 `wss://openws.work.weixin.qq.com`。
- `aibot_subscribe` 鉴权订阅。
- `aibot_msg_callback` 文本消息回调。
- `aibot_respond_msg` 回复当前用户消息。
- `aibot_send_msg` 主动发送状态通知。
- `ping` 心跳。
- `disconnected_event` 和连接关闭处理。
- 使用 `req_id` 关联请求与响应。

### 3.2 Herdr 连接

Herdr Pal 只使用 Herdr 公共 Local Socket API 和 NDJSON 协议：

- `session.snapshot`
- `events.subscribe`
- `pane.agent_status_changed`
- pane 生命周期相关事件
- `agent.read`
- `agent.prompt`
- `agent.send_keys`

不使用 MCP、plugin startup hook、私有 TUI client socket、内部 `AppState` 或私有 Rust
模块。

## 4. 总体架构

Herdr Pal 是模块化单进程：

```text
企业微信智能机器人
        │ WebSocket
        ▼
   WeComClient
        │ IMEvent
        ▼
   CommandRouter ── PolicyGuard
        │
        ▼
   BridgeService
      ├── SessionRegistry
      ├── PanelBuffer
      ├── EventSupervisor
      └── HerdrClient
               │ NDJSON / Local Socket
               ▼
             Herdr
```

### 4.1 `WeComClient`

职责：

- 建立和维护企业微信 WebSocket 长连接。
- 发送订阅请求并校验订阅结果。
- 每 30 秒发送心跳。
- 使用 `req_id` 关联请求和响应。
- 将消息和事件回调转换为内部 `IMEvent`。
- 回复当前回调和主动推送 Markdown 消息。
- 处理连接关闭、服务端断开事件和指数退避重连。

该模块不解析 Herdr 命令，不保存当前 Agent 选择，也不实现终端内容清理。

### 4.2 `HerdrClient`

职责：

- 解析 Herdr session 和 Local Socket 地址。
- 编码、发送和解析 NDJSON 请求。
- 区分成功响应、错误响应、订阅确认和事件行。
- 维护 `events.subscribe` 长连接。
- 将底层错误转换成稳定的内部错误类型。

该模块不包含企业微信、权限或分页逻辑。

### 4.3 `SessionRegistry`

职责：

- 使用 `session.snapshot` 建立当前运行时视图。
- 按 pane、terminal 和 Agent occupant 建立索引。
- 保存 Agent 状态、标题、workspace 和 tab 展示信息。
- 保存当前用户通过 `/sel` 选择的目标。
- 应用 pane 生命周期事件。
- occupant 变化时使当前选择失效。

`SessionRegistry` 是可重建的内存缓存，不是持久化事实来源。

### 4.4 `CommandRouter`

职责：

- 严格解析内置命令。
- 将命令转换为纯内部动作，不直接访问网络或 Socket。
- 将不以 `/` 开头的文本转换为普通 prompt 动作。
- 对未知命令和非法参数返回可操作的错误。

### 4.5 `PanelBuffer`

职责：

- 管理当前选中 Agent 的终端分页缓存。
- 使用 `recent_unwrapped` 读取终端近期内容。
- 清理 ANSI、换行和明显 TUI 重绘噪音。
- 以每页 100 行组织内容。
- 在扩大读取范围时对新旧快照进行重叠校验。
- 内容无法对齐时清空缓存并回到最新页。

### 4.6 `PolicyGuard`

职责：

- 校验企业微信用户是否为唯一允许用户。
- 校验当前选择、pane、terminal 和 Agent occupant。
- 校验按键动作是否在固定白名单中。
- 使用企业微信 `msgid` 防止重复 prompt 或重复按键。
- 生成不包含完整输入和终端内容的控制动作审计记录。

### 4.7 `EventSupervisor`

职责：

- 维护 pane 生命周期订阅。
- 为当前全部 Agent pane 构造批量状态订阅。
- pane 集合或 occupant 变化后重建状态订阅。
- 检测状态迁移并抑制重复状态事件。
- 处理 Herdr Socket 关闭、重连和 snapshot 重建。

### 4.8 `BridgeService`

职责：

- 编排企业微信入站事件、命令执行和 Herdr API 调用。
- 将 Herdr 状态变化转换为企业微信主动通知。
- 协调目标失效、缓存清理和用户错误消息。

它不直接解析任何外部协议，也不包含终端清理的具体规则。

## 5. 进程和配置

### 5.1 进程入口

`cmd/herdr-pal` 负责：

- 读取并校验配置。
- 获取本机进程锁，避免相同 Bot 启动多个实例后互相踢掉长连接。
- 组装模块依赖。
- 启动企业微信和 Herdr 两侧 supervisor。
- 处理 SIGINT/SIGTERM 和优雅退出。

### 5.2 配置格式

非密钥配置使用本地 JSON：

```json
{
  "wecom": {
    "bot_id": "BOT_ID",
    "allowed_user_id": "USER_ID"
  },
  "herdr": {
    "session": "",
    "socket_path": ""
  },
  "log": {
    "level": "info"
  }
}
```

规则：

- Secret 只通过 `HERDR_PAL_WECOM_SECRET` 环境变量传入。
- 提供不包含真实标识和密钥的 `config.example.json`。
- 缺少 Bot ID、Secret 或允许用户 ID 时启动失败。
- `allowed_user_id` 使用企业微信回调中实际返回的用户标识；如果企业微信返回加密
  userid，则配置同一加密标识。
- Herdr Socket 默认按 Herdr session 规则解析，可通过配置显式覆盖。
- 配置文件和环境变量内容不得出现在普通日志中。

## 6. 企业微信入站命令协议

所有单聊文本先经过严格命令解析。以 `/` 开头但无法识别的内容返回命令错误，绝不
作为 prompt 发送。

| 输入 | 行为 |
| --- | --- |
| `/ls` | 列出最新 Agent、状态、标题和 pane 信息，并生成本次编号快照 |
| `/sel 1` | 根据最近一次 `/ls` 的编号选择目标 |
| `/con` | 读取并显示当前 Agent 最新 100 行，重置到第 0 页 |
| `/pageup` | 向更早内容移动一页 |
| `/pagedn` | 向更新内容移动一页，直到第 0 页 |
| `/keyup`、`/key up` | 发送 `up` |
| `/keydn`、`/key down` | 发送 `down` |
| `/enter`、`/key enter` | 发送 `enter` |
| `/esc`、`/key esc` | 发送 `esc` |
| `/space`、`/key space` | 发送 `space` |
| `/key X` | `X` 为单个 ASCII 字母或数字时发送该按键 |
| 普通文本 | 通过 `agent.prompt` 发送给当前 Agent |

### 6.1 `/ls` 和 `/sel`

- `/ls` 只列出当前检测到 Agent occupant 的 pane。
- 每一项至少包含编号、Agent 名称、状态、title、workspace/tab 和 pane 标识。
- 当前选择使用明确标记展示。
- `/sel N` 只解析最近一次 `/ls` 生成的编号快照。
- 如果编号不存在、pane 已关闭或 occupant 已变化，则拒绝选择并要求重新 `/ls`。
- `/sel` 成功后清空旧目标的面板缓存。

### 6.2 普通 prompt

- 必须已经选择有效目标。
- 发送前重新校验 pane、terminal 和 occupant。
- 只调用 `agent.prompt`，不调用 `pane.send_text` 或 `pane.send_input`。
- Herdr 接受请求后立即向企业微信回复“已发送”。
- 执行结果由后续状态事件通知。

### 6.3 按键动作

首版按键白名单为：

- `up`
- `down`
- `enter`
- `esc`
- `space`
- 单个 ASCII 字母 `A-Z`、`a-z`
- 单个 ASCII 数字 `0-9`

所有别名先转换为统一内部动作，再调用 `agent.send_keys`。不支持任意 key name、组合键、
控制键或按键序列。

特殊键名只接受小写形式。单个字母或数字按用户输入原样发送，保留字母大小写。

命令本身即用户的显式操作授权，不进行二次确认。`blocked` 状态永远不会自动触发按键；
只有用户明确输入受支持命令时才发送。

## 7. 终端内容和分页

### 7.1 `/con`

- 调用 `agent.read`，source 为 `recent_unwrapped`，lines 为 `100`。
- 规范化内容后显示最新 100 行。
- 将当前目标的分页索引重置为 `0`。
- 新的 `/con` 会替换旧分页缓存。

### 7.2 `/pageup`

Herdr `agent.read` 没有 offset，因此向更早内容翻页时逐步扩大读取范围：

```text
第 0 页：读取最近 100 行
第 1 页：读取最近 200 行，展示较早的 100 行
第 2 页：读取最近 300 行，展示最早的 100 行
...
第 9 页：读取最近 1000 行，展示最早的 100 行
```

扩大读取范围后，`PanelBuffer` 使用已缓存内容作为重叠锚点。如果旧缓存无法在新快照
中可靠定位，说明终端内容已经发生无法稳定对齐的变化：

1. 清空分页缓存。
2. 将页码重置为 `0`。
3. 提示用户执行 `/con` 查看最新内容。

### 7.3 `/pagedn`

- 优先从已缓存快照中渲染更新的一页。
- 到第 `0` 页后不再继续向下，并提示已经是最新内容。
- `/pagedn` 不隐式刷新最新终端内容；刷新必须显式执行 `/con`。

### 7.4 页大小和企业微信消息限制

- 逻辑页固定为 100 行。
- Herdr 当前单次最多读取 1000 行，所以第一版最多访问最近 10 页。
- 一页超过企业微信单条 Markdown 消息大小时分段发送，仍标记为同一逻辑页。
- 分段不得在 UTF-8 字符中间截断。
- 超长单行需要按字节安全切分，并在展示中标明续段。

## 8. Herdr 状态通知

Herdr Pal 监控当前会话中的全部 Agent pane。通知目标始终是配置的唯一企业微信用户，
通知不会修改当前 `/sel` 选择。

### 8.1 启动基线

启动或 Herdr 重连后：

1. 调用 `session.snapshot`。
2. 重建全部索引。
3. 记录当前状态作为基线。
4. 重建生命周期和 Agent 状态订阅。
5. 不发送历史状态通知。

### 8.2 状态策略

| 状态变化 | 行为 |
| --- | --- |
| 转为 `working` | 发送简短的开始工作通知 |
| 转为 `blocked` | 读取最近 100 行并发送阻塞通知 |
| 转为 `done` | 读取最近 100 行并发送完成通知 |
| 转为 `idle` | 仅从 `working` 或 `blocked` 转入时通知；自动输出最多 100 行 |
| 转为 `unknown` | 提示状态无法可靠识别，不宣称完成 |
| pane 关闭或 occupant 替换 | 通知目标失效；当前选择匹配时清空选择和缓存 |

自动输出规则：

- 自动读取始终使用 `recent_unwrapped`，最多 100 行。
- 不尝试发送全部新增内容。
- 每个 pane 保存当前进程内最近一次自动通知的状态和 100 行快照 hash。
- 相同状态和相同快照不重复发送。
- 自动通知不会改变手工分页缓存或页码。
- 终端内容标记为“终端近期快照”，不标记为结构化 assistant 回复。

## 9. 安全模型

### 9.1 用户边界

- 只接受 `allowed_user_id` 对应用户的单聊消息。
- 其他用户的消息拒绝处理，不执行任何 Herdr 调用。
- 群聊回调不进入业务路由。

### 9.2 目标边界

每次 prompt 或按键前必须重新验证：

- pane 仍然存在；
- terminal 标识仍然匹配；
- Agent occupant 仍然匹配；
- 当前选择未因 Herdr 重连失效。

任何一项失败都清空选择并要求重新 `/ls`、`/sel`。

### 9.3 幂等

- 使用企业微信回调中的 `msgid` 做内存去重。
- 已处理 `msgid` 使用有容量上限和 TTL 的集合保存。
- 重复回调不得再次执行 prompt 或按键。
- 状态通知使用 pane、occupant、status 和快照 hash 组合去重。

### 9.4 审计和日志

- 日志不得记录 Bot Secret、完整 prompt 或完整终端内容。
- 普通运行日志只记录连接状态、目标标识、长度、hash 和错误类型。
- 按键审计记录用户标识、pane、occupant、标准化动作、时间和执行结果。
- 不提供通过企业微信调用 `server.stop`、`pane.close` 或其他高风险 Herdr API 的入口。

## 10. 连接、重连和一致性

### 10.1 企业微信连接

- 每个进程只维护一个有效 WebSocket 连接。
- 每 30 秒发送心跳。
- 断线后使用带抖动的指数退避，1 秒起步，最大 30 秒。
- 收到 `disconnected_event` 或连接关闭后统一进入重连流程。
- 重连后不补发断线期间的通知。
- 本机进程锁防止同一配置被重复启动。

### 10.2 Herdr 连接

Herdr 断线时：

1. 将状态标记为 degraded。
2. 暂停 prompt 和按键操作。
3. 清空当前选择和分页缓存。
4. 关闭失效订阅。
5. 使用 1 秒至 30 秒的带抖动指数退避重连。

Herdr 重连后：

1. 调用 `ping` 和 `session.snapshot`。
2. 用新 snapshot 完全替换 `SessionRegistry`。
3. 以新状态建立通知基线。
4. 重建生命周期订阅。
5. 为全部当前 Agent pane 重建批量状态订阅。
6. 不重放重连前的状态或输出。

### 10.3 订阅重建

由于 `pane.agent_status_changed` 必须指定 `pane_id`，第一版使用：

- 一个生命周期订阅连接；
- 一个包含当前全部 Agent pane 状态订阅的批量连接。

pane 创建、关闭、退出、Agent detected 或 occupant 变化时重新计算订阅集合，关闭旧状态
连接并建立新连接。重建期间通过 snapshot 和状态去重避免重复通知。

第一版生命周期订阅至少包含 `pane.created`、`pane.closed`、`pane.exited`、
`pane.agent_detected` 和 `pane.updated`。

## 11. 错误处理

内部错误至少分为：

- `WeComUnavailable`
- `HerdrUnavailable`
- `ProtocolMismatch`
- `TargetNotFound`
- `OccupantChanged`
- `Unauthorized`
- `InvalidCommand`
- `InvalidKey`
- `PanelChanged`
- `OutputUnavailable`
- `DeliveryFailed`

用户错误消息必须可操作：

- Herdr 不可用：提示正在重连。
- 未选择 Agent：提示执行 `/ls` 和 `/sel`。
- Agent 已替换：提示重新选择。
- 命令格式错误：展示该命令的正确用法。
- 分页无法对齐：重置到最新页并提示执行 `/con`。
- 已到分页边界：提示已经是最新或最早可读取内容。

协议解析规则：

- JSON 允许未知字段，保持向前兼容。
- 必要字段缺失、类型错误或错误响应必须显式返回错误。
- 每个请求设置超时和 context 取消。
- 已取消请求的迟到响应只记录，不触发业务动作。
- Herdr 版本或协议不兼容时进入 degraded 状态，不继续发送输入。

## 12. 优雅退出

收到 SIGINT 或 SIGTERM 后：

1. 停止接收新的企业微信业务事件。
2. 取消未完成的 Herdr 请求。
3. 关闭 Herdr 订阅连接。
4. 关闭企业微信 WebSocket。
5. 释放本机进程锁。
6. 在有上限的退出超时内结束进程。

## 13. 代码结构

推荐目录：

```text
cmd/herdr-pal/
internal/wecom/
internal/herdr/
internal/bridge/
internal/command/
internal/session/
internal/panel/
internal/policy/
internal/config/
testdata/
```

每个模块保持单一职责。协议类型、业务动作和外部接口使用英文命名；项目文档和代码注释
使用中文。对外接口必须有符合 Go 文档工具要求的注释。

## 14. 构建与分发

项目根目录必须提供：

- `build.sh`
- `unittest.sh`

`build.sh`：

- 设置 `CGO_ENABLED=0`。
- 执行格式、静态检查和必要的生成步骤。
- 使用 `go build -trimpath` 构建 `cmd/herdr-pal`。
- 输出 `dist/herdr-pal` 单文件。
- 注入版本、Git commit 和构建时间。

`unittest.sh`：

- 执行格式检查。
- 执行 `go vet`。
- 执行全部单元测试。
- 在支持环境中执行 race test。

第一版只要求当前开发平台的本地构建。跨平台制品和发布流水线留待后续设计。

## 15. 测试策略

### 15.1 企业微信协议测试

- `aibot_subscribe` 请求和响应。
- `req_id` 关联和超时。
- 文本消息回调解析。
- 单聊和群聊过滤。
- `aibot_respond_msg` 和 `aibot_send_msg` 编码。
- 心跳、服务端断开和自动重连。
- 重复 `msgid` 不会重复执行动作。
- 未知 JSON 字段不会导致解析失败。

### 15.2 Herdr 协议测试

- NDJSON 分帧、部分读取和多行读取。
- success、error、subscription acknowledgement 和 event 解析。
- `session.snapshot` 模型转换。
- 状态订阅确认与事件解析。
- Socket 关闭、请求超时和重连。
- pane 生命周期变化后的订阅重建。

### 15.3 命令测试

使用 table-driven tests 覆盖：

- `/ls`
- `/sel N`
- `/con`
- `/pageup`
- `/pagedn`
- 所有按键别名
- `/key` 的字母、数字、特殊键和非法参数
- 未知 `/` 命令不会变成 prompt
- 普通文本会变成 prompt

### 15.4 分页和输出测试

- 每页 100 行。
- 第 0 页到第 9 页的切片正确性。
- 1000 行上限。
- 扩大读取范围时的重叠校验。
- 终端内容变化时重置分页。
- ANSI、软换行和 TUI 噪音清理。
- UTF-8 安全的消息分段。
- 自动通知只包含最近 100 行。
- 自动通知不会改变手工分页状态。

### 15.5 状态和安全测试

- 状态迁移和重复状态事件抑制。
- `blocked`、`done`、`idle` 的通知策略。
- pane 关闭和 occupant 替换使选择失效。
- 未授权用户无法执行任何 Herdr 操作。
- 按键白名单拒绝组合键和任意 key name。
- 重复企业微信回调不会重复发送 prompt 或按键。
- Herdr degraded 时暂停全部输入。

### 15.6 集成测试

- 本地 fake WebSocket Server 模拟企业微信协议。
- 本地 fake Herdr Socket Server 模拟 snapshot、订阅、事件、断线和错误。
- 真实 Herdr 集成测试通过环境变量显式启用。
- 真实 Herdr 测试不依赖企业微信网络，企业微信侧使用 fake server。

## 16. MVP 验收标准

满足以下条件视为第一版完成：

1. `build.sh` 能生成单个 `dist/herdr-pal`。
2. `unittest.sh` 能运行并通过全部测试。
3. 手工启动后能建立企业微信长连接和 Herdr Local Socket 连接。
4. 唯一允许用户能通过 `/ls`、`/sel` 选择 Agent。
5. 普通文本能通过 `agent.prompt` 发送给当前 Agent。
6. 所有已定义按键命令能通过白名单发送，非法按键被拒绝。
7. `/con` 显示最新 100 行，分页最多访问最近 1000 行。
8. `blocked` 和 `done` 通知只携带最近 100 行。
9. Agent 状态变化、pane 关闭和 occupant 替换能正确通知。
10. Herdr 或企业微信断线后能自动重连且不会重放旧动作。
11. 未授权用户、重复消息和失效目标不会产生终端输入。

## 17. 后续演进方向

后续版本可以独立评估：

- 企业微信群聊和多用户权限。
- 持久化选择、幂等键和通知 checkpoint。
- 更多安全按键与组合键。
- 模板卡片快捷操作。
- 多媒体输入和附件传输。
- `launchd` 或其他常驻服务安装方式。
- 官方 Go SDK 出现后的兼容层替换。
- 根据实际使用结果优化不同 Agent 的 TUI 清理规则。
