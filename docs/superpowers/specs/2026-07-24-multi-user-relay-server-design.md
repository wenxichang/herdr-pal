# 多用户 Relay Server 设计

## 1. 背景与目标

企业微信智能机器人同一 Bot ID 同时只允许一条有效长连接。现有 Herdr Pal 由每台机器
直接连接企业微信，因此无法让同一个机器人同时服务多个企业微信用户及其多台 Herdr
机器。

本次改造引入独立的 `herdr-pal-server`：server 持有 Bot ID 和 Secret，并维护唯一的
企业微信长连接；每台机器上的 `herdr-pal` 只连接本机 Herdr 和内网 server。一个 server
进程只服务一个机器人，但可以服务任意数量的企业微信用户；每个用户可以连接多台机器。

目标：

- 一个企业微信机器人连接多个用户的多台 Herdr 机器。
- server 聚合全部在线机器和 Agent 会话，提供跨机器 `/ls` 与选择。
- 普通 prompt、分页和按键仍在目标机器本地执行，不暴露 Herdr Socket。
- 所有会话列表和通知包含机器标识、本机会话序号及 panel 标题。
- 保留 `herdr-pal -i` 本机交互模式。
- Go 单文件分发，构建 `herdr-pal` 与 `herdr-pal-server` 两个二进制。

非目标：

- 不让一个 server 进程同时连接多个企业微信机器人。
- 不支持群聊、持久离线消息、断线命令重放或通知补发。
- 不提供公网安全模型、应用层 token、客户端证书或用户白名单。
- 不修改 Herdr，不暴露 Herdr 原始本地 Socket。

## 2. 信任与部署边界

server 只允许部署在可信内网。relay 客户端在握手中自行声明 `userid`，server 不提供
应用层认证，也不维护允许用户列表。任意企微单聊用户都可以执行 `/userid`；只有该
`userid` 存在在线客户端时，才有可选择的 Herdr 会话。

relay 传输只支持 `wss://`。客户端默认跳过证书链和主机名校验，因此传输具备加密，
但不保证 server 身份，不能抵御可信内网中的中间人。配置允许关闭跳过校验，供使用可信
证书的部署启用完整 TLS 校验。任何公网暴露均超出本设计安全边界。

server 未配置证书时，首次启动自动生成并持久化自签名证书。私钥权限必须为 `0600`；
配置了外部证书时，证书和私钥必须同时提供。

## 3. 总体架构

```text
企业微信
   │ 唯一机器人 WSS
   ▼
herdr-pal-server
   ├─ WeComGateway：Bot ID、Secret、企微回调和动态用户发送
   ├─ ClientHub：relay WSS 监听、握手、心跳和连接唯一性
   ├─ SessionCatalog：聚合用户、机器和完整会话快照
   ├─ ConversationRouter：全局命令、编号快照、选择与命令转发
   └─ TLSManager：外部证书加载或自签名证书生成
           │
           ├── WSS ── herdr-pal（用户 A / home-mac）── 本机 Herdr
           ├── WSS ── herdr-pal（用户 A / office-pc）─ 本机 Herdr
           └── WSS ── herdr-pal（用户 B / home-mac）── 本机 Herdr
```

`herdr-pal-server` 不连接 Herdr，也不解析 Herdr NDJSON。`herdr-pal` 继续持有
`HerdrClient`、`SessionRegistry`、`EventSupervisor`、`Service` 和 `Notifier`，负责
所有 Herdr 协议、occupant 校验、prompt wait、按键审计、分页和输出清理。

普通网络模式不再直接连接企业微信；原 `-discover-user` 模式移除，由 `/userid` 取代。
本机 `-i` 模式保持原有行为和本地命令语义。

## 4. 模块边界

### 4.1 平台中立消息模型

新增 `internal/im`，从 Bridge 中移除对 `wecom.IncomingText` 的直接依赖：

```text
IncomingText
  request_id
  message_id
  user_id
  chat_type
  content

ReplySink
  RespondMarkdown(callback_ref, content)
  SendMarkdown(content)

NotificationSink
  SendNotification(target, headline, content)
```

`Service` 依赖 `ReplySink`；`Notifier` 依赖携带目标元数据的 `NotificationSink`。通知必须
显式携带目标，不能由 server 从 Markdown 文本反向解析 pane、Agent 或标题。

### 4.2 WeComGateway

现有 `internal/wecom` 继续负责企业微信协议和唯一长连接，但移除固定
`allowed_user_id` 目标。主动发送改为：

```text
SendMarkdownTo(ctx, userID, content)
```

订阅仍只需要 Bot ID 和 Secret；入站回调提供真实 `userid`。首段回复复用企业微信
`req_id`，后续分段和主动通知按目标 `userid` 发送。

### 4.3 Relay 协议

新增 `internal/relayproto`，只负责严格 JSON 编解码、版本、长度和字段校验，不包含网络
重连、企业微信或 Herdr 逻辑。

新增 `internal/relayclient`，作为 `herdr-pal` 的网络 IM Adapter，并负责：

- 建立 WSS、握手、心跳与指数退避重连。
- 上报完整会话快照。
- 接收选择和命令请求。
- 将命令转换为平台中立 `IncomingText`。
- 将回复、后续分段和带目标通知发回 server。
- relay 断线时丢弃通知，不在重连后补发。

### 4.4 Server 组件

新增 `internal/server`：

- `ClientHub`：管理连接和每连接有界发送队列。
- `SessionCatalog`：保存当前在线机器的最新完整快照。
- `ConversationRouter`：处理企业微信命令和 relay 请求关联。
- `TLSManager`：加载或生成证书。
- `UserExecutor`：按 `userid` 串行执行选择与输入，通知不占用该串行队列。

新增 `cmd/herdr-pal-server` 作为独立入口。

## 5. 身份、连接与唯一性

连接唯一键为：

```text
ClientKey = (userid, machine_id)
```

不同用户可以使用相同 `machine_id`。同一用户的同一 `machine_id` 只能有一条连接。
第二条连接到达时，server 返回 `duplicate_client` 并关闭新连接；旧连接不受影响，不允许
新旧连接抢占。

`machine_id` 必须匹配 `[A-Za-z0-9._-]{1,64}`。`userid` 是企业微信路由标识，不作为
密码学凭据；server 日志只记录其摘要。

客户端握手：

1. 建立 WSS。
2. 发送 `client_hello`：协议版本、userid、machine_id、客户端版本。
3. server 校验协议、身份字段和唯一键。
4. server 返回 `server_hello`：连接 ID、心跳间隔、超时和快照校准间隔。
5. 客户端必须在 10 秒内发送首个完整会话快照，之后才加入可选择目录。

连接使用内部 connection ID。server 只接受当前连接记录产生的数据；连接处理 goroutine
退出后的迟到数据不得修改目录。

## 6. 会话快照与目录

每台客户端上报完整快照：

```text
SessionSnapshot
  sequence
  sessions[]
    local_index
    pane_id
    terminal_id
    occupant_hash
    agent_session_ref?
    agent
    display_agent
    title
    workspace
    tab
    status
```

`sequence` 在单个 relay 连接周期内从 1 单调递增。server 只接受大于已应用 sequence 的
快照；旧序号作为 stale snapshot 拒绝。每次更新整体替换该机器旧快照，不能增量拼接。

`local_index` 由客户端对当前 Registry 稳定排序后生成，仅用于展示。安全目标始终使用
`pane_id + occupant_hash`，不能只依赖序号。标题来自 Herdr pane title，列表和通知前均
进行 UTF-8、换行、Markdown 和长度清理。

上报时序：

- relay 接受后立即上报。
- pane、occupant、标题或状态变化后，最多 1 秒合并上报最新完整快照。
- 无变化时每 30 秒上报一次完整快照。
- 每 10 秒心跳，30 秒无响应时 server 关闭连接。

连接关闭或心跳超时时，server 原子删除该机器的全部会话，并清除所有指向该机器的用户
编号快照和当前选择。断线机器不保留离线条目。

## 7. 聚合列表与选择

server 为每个用户保存：

```text
UserRoutingState
  numbered_snapshot[] SessionRef
  selected? SessionRef

SessionRef
  userid
  machine_id
  local_index
  pane_id
  occupant_hash
```

`/ls` 读取该用户全部在线机器的最新目录，按 `machine_id`、`local_index` 和稳定目标字段
排序，并生成新的编号快照：

```text
1. [home-mac/1] Codex — herdr-pal server
   工作区：project / main · 状态：working

2. [office-pc/1] Claude — 修复登录问题
   工作区：backend / debug · 状态：blocked
```

用户输入 `/1` 或 `/sel 1` 时，server 从最近一次编号快照取出 `SessionRef`，而不是按
此刻重新排序的列表解释数字。server 先确认机器在线且 occupant 未变化，再发送
`select_request`。客户端必须从最新本地 Registry 按 `pane_id + occupant_hash` 复核并
选择；只有客户端返回成功后，server 才更新当前选择。

pane 关闭、occupant 替换、机器断线、server 重启或选择复核失败都会使选择失效。

## 8. 命令路由

server 直接处理：

- `/userid`：返回当前企业微信单聊回调中的完整 userid。
- `/ls`：生成跨机器编号快照。
- `/<NUM>`、`/sel <NUM>`：选择稳定会话。
- `/help`：显示 server 全局命令和现有控制命令。

仅处理企业微信单聊。群聊和缺失 userid 的回调在进入用户队列前拒绝，不允许查询目录或
转发输入。

server 不把这些命令转发给客户端。`/con`、`/pageup`、`/pagedn`、`/key`、`/enter`、
`/slash` 和普通 prompt 转发给当前选择所在客户端。无在线机器、没有编号快照、未选择、
机器离线或 occupant 过期时，server 直接返回可操作错误，不进行转发。

同一 userid 的全局命令和转发输入按到达顺序串行执行，确保 `/1` 与紧随其后的 prompt
不会乱序。不同用户之间互不阻塞。通知使用独立有界队列。

## 9. Relay 消息协议

所有帧为一条 WebSocket 文本消息，顶层结构：

```json
{
  "protocol": 1,
  "type": "session_snapshot",
  "request_id": "可选关联标识",
  "payload": {}
}
```

必要帧：

| 方向 | type | 用途 |
| --- | --- | --- |
| client → server | `client_hello` | 声明 userid、machine_id 和版本 |
| server → client | `server_hello` | 接受连接并下发时间参数 |
| client → server | `session_snapshot` | 完整替换机器会话目录 |
| server → client | `select_request` | 要求客户端复核并选择稳定目标 |
| client → server | `select_result` | 返回选择结果 |
| server → client | `execute_request` | 转发一条用户输入 |
| client → server | `execute_response` | 返回首段回调回复 |
| client → server | `execute_push` | 返回后续 Markdown 分段 |
| client → server | `notification` | 带稳定目标的主动通知 |
| 双向 | `ping` / `pong` | 应用层心跳 |
| 双向 | `protocol_error` | 返回稳定错误码并决定是否关闭 |

`execute_request` 包含 relay request ID、原企业微信 `message_id`、userid 和 content。
server 保存 relay request ID 到企业微信 callback `req_id` 的短期映射；收到
`execute_response` 后调用企微回调回复，`execute_push` 使用动态 userid 主动发送。

命令首段回复超时为 20 秒。超时后 server 回复“操作可能已经提交，请先检查目标会话”，
并删除请求关联；不得自动重发。迟到的首段回复不得再次使用原 callback req_id，可丢弃并
记录摘要。命令在 server 和客户端均按原企业微信 message ID 去重。

建议稳定错误码：

- `protocol_mismatch`
- `invalid_frame`
- `invalid_identity`
- `duplicate_client`
- `snapshot_stale`
- `target_not_found`
- `target_changed`
- `client_unavailable`
- `queue_full`
- `request_timeout`

错误文本不得回显 Secret、完整 prompt、终端内容或原始非法帧。

## 10. 状态通知

所有在线机器的全部 Agent 状态通知均转发，不受当前选择限制。客户端 Notifier 发送结构化
目标和内容，server 使用最新 SessionCatalog 查找本机序号和标题。目标不存在或 occupant
已变化时丢弃旧通知。

统一展示：

```text
[office-pc/1] Claude — 修复登录问题
Agent 已阻塞，需要你的处理。

<最近最多 100 行终端快照>
```

通知继续遵守现有策略：blocked、done 和必要的 idle 只读取最近 100 行；working 不发送
大段输出；unknown 不宣称成功。panel 标题必须出现在通知头部，缺失时省略标题部分。

relay 断线期间产生的通知直接丢弃。server 企业微信连接不可用时也不建立离线积压；允许
当前发送调用失败，但连接恢复后不得补发旧通知。

## 11. 重连与恢复

### 11.1 client 与 server 断开

- server 立即移除机器和会话。
- client 继续维护本机 Herdr snapshot 和订阅。
- client 不缓存命令或通知。
- client 指数退避重连。
- 重连后执行新握手，并以 sequence 1 上报当前完整快照。
- server 不恢复旧选择。

### 11.2 server 重启

- SessionCatalog、编号快照、选择、请求关联和幂等键全部清空。
- clients 自动重连并上报当前完整快照。
- 用户必须重新执行 `/ls` 和选择。
- 不补发 server 停机期间的状态或命令。

### 11.3 企业微信连接断开

- relay 客户端连接和 SessionCatalog 可以继续保留。
- server 暂时无法接收新命令或发送通知。
- 企业微信重连后从当前状态继续，不重放旧消息或通知。

## 12. 配置与命令行

server 配置：

```json
{
  "wecom": {
    "bot_id": "BOT_ID"
  },
  "server": {
    "listen": "0.0.0.0:9443",
    "cert_file": "",
    "key_file": "",
    "state_dir": ""
  },
  "log": {
    "level": "info"
  }
}
```

Secret 继续只从 `HERDR_PAL_WECOM_SECRET` 获取：

```sh
HERDR_PAL_WECOM_SECRET='...' ./dist/herdr-pal-server -config /path/server.json
```

`server.listen` 是必填字段，避免默认监听全部网卡。`state_dir` 为空时使用操作系统用户配置
目录下的 `herdr-pal-server`；自动证书文件名固定为 `relay-cert.pem` 和
`relay-key.pem`。`cert_file` 与 `key_file` 必须同时为空或同时非空。

客户端配置：

```json
{
  "relay": {
    "url": "wss://192.168.1.10:9443",
    "userid": "企业微信 userid",
    "machine_id": "home-mac",
    "skip_verify": true
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

`skip_verify` 使用可区分“未配置”和 false 的配置类型，缺省为 true。客户端拒绝
`ws://`。普通网络模式要求 relay 字段，不读取企业微信 Secret。`herdr-pal -i` 不要求
relay 字段。

原直连企微配置和 `-discover-user` 明确拒绝并提供迁移提示，不保留两套网络 IM 模式。

server 使用 Bot ID 摘要作为本机进程锁键。网络模式客户端使用规范化 Herdr Socket 摘要
作为进程锁键，确保同一台 Herdr 只有一个 `herdr-pal`。server 端同时以 ClientKey 拒绝
同一用户机器标识的第二连接。

## 13. 资源限制

首版固定限制：

- relay WebSocket 单帧最多 1 MiB。
- 每台机器最多上报 256 个会话。
- userid 最多 512 个 UTF-8 字节。
- title、workspace、tab 和展示名分别最多 512 个 UTF-8 字节。
- 每连接发送队列最多 128 帧。
- 每用户输入队列最多 64 条。
- 每连接最多 128 个在途请求。
- 企业微信 Markdown 仍按现有 20,480 字节限制安全分段。

超过限制时拒绝整个帧或请求，不能部分应用快照。

## 14. 构建与发布

`build.sh` 同时生成：

```text
dist/herdr-pal
dist/herdr-pal-server
```

两个程序支持独立 `--version`。保持 `CGO_ENABLED=0`，继续使用统一 `unittest.sh`。

## 15. 测试策略

### 15.1 协议单元测试

- 所有帧的成功、缺字段、未知字段、错误类型和尾随数据。
- 协议版本不匹配、帧大小限制和错误脱敏。
- machine_id、userid、SessionRef 和完整快照校验。
- stale sequence 和完整替换语义。

### 15.2 Catalog 与 Router 测试

- 同用户同 machine 拒绝第二连接。
- 不同用户可使用相同 machine。
- 断线立即移除会话、编号快照和选择。
- 多机器稳定排序、编号快照与 `/N` 解析。
- 列表包含 Agent、panel 标题、workspace、tab 和状态。
- occupant 变化、pane 删除和客户端选择拒绝。
- `/userid`、`/help`、无在线机器、未选择和目标离线。
- 不同用户命令、选择和通知完全隔离。
- 同用户选择与 prompt 串行，不同用户并发。

### 15.3 Relay client/server 测试

- 自动生成自签名证书及私钥权限。
- 只接受 WSS，客户端默认跳过验证并可选择启用验证。
- 握手、首快照超时、心跳和 30 秒失活处理。
- 1 秒变更合并、30 秒完整校准和重连 sequence 重置。
- 命令首段、后续分段、通知、超时和迟到回复。
- relay 断线期间通知不缓存、不重放。
- 有界队列满时安全失败。

### 15.4 端到端测试

- fake 企业微信 + server + 两个 userid + 多个 relay client。
- 同一 userid 的两台 fake Herdr 聚合 `/ls`，使用 `/1` 选择并发送 prompt。
- 不同 userid 使用相同 machine_id，路由不串线。
- panel 标题出现在跨机器列表和通知。
- 机器断线后会话立即消失，原选择失效。
- server 重启后 clients 重连、选择不恢复、通知不补发。
- prompt stalled 的单次 Enter 恢复和按键审计仍只发生在本机客户端。
- 全量 `go test -race ./...`。

## 16. 分阶段实现

该设计是一个完整产品能力，但实现按以下依赖顺序拆分：

1. 平台中立 IM 模型、动态企业微信发送和 relay 协议纯模块。
2. SessionCatalog、连接唯一性、全局命令与选择路由。
3. WSS server、TLS 自动生成和 fake relay 集成。
4. relay client、会话快照上报与 Bridge 选择控制入口。
5. 结构化通知目标、跨机器标题展示和完整端到端测试。
6. CLI、配置迁移、双二进制构建和文档更新。

每一阶段必须先写失败测试，再实现最小代码，并在提交前通过 `build.sh` 和
`unittest.sh`。
